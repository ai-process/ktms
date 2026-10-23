package usagereport

import (
	"context"
	"errors"
	"github.com/ai-process/ktms/internal/llm"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ai-process/ktms/internal/observability"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// fakeCollector records batches; while down it fails every call.
type fakeCollector struct {
	usagev1.UsageServiceClient
	mu     sync.Mutex
	down   bool
	refuse error // returned instead of delivering, like an auth refusal
	hang   bool
	events map[string]int // event id -> times delivered
	calls  int
}

func newFake() *fakeCollector { return &fakeCollector{events: map[string]int{}} }

func (f *fakeCollector) RecordUsageEvents(ctx context.Context, in *usagev1.RecordUsageEventsRequest, _ ...grpc.CallOption) (*usagev1.RecordUsageEventsResponse, error) {
	f.mu.Lock()
	f.calls++
	down, refuse, hang := f.down, f.refuse, f.hang
	f.mu.Unlock()
	if refuse != nil {
		return nil, refuse
	}
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if down {
		return nil, errors.New("connection refused")
	}
	if len(in.GetEvents()) > maxBatch {
		return nil, errors.New("batch over the collector's limit")
	}
	for i, ev := range in.GetEvents() {
		if ev.GetModel() == "refused" { // like tokenmeter: one bad event fails the batch
			return nil, status.Errorf(codes.InvalidArgument, "invalid event at index %d: model refused", i)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range in.GetEvents() {
		f.events[ev.GetEventId()]++
	}
	return &usagev1.RecordUsageEventsResponse{CreatedCount: int32(len(in.GetEvents()))}, nil
}

func (f *fakeCollector) delivered() (unique, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.events {
		unique++
		total += n
	}
	return unique, total
}

func record(t *testing.T, c *UsageCollectorClient, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := c.RecordUsage("u", "a", "openai", "m", llm.TokensUsage{Input: 1, Output: 1}, 1, 0, usagev1.Status_STATUS_SUCCESS, nil); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
}

func waitDelivered(t *testing.T, f *fakeCollector, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if u, _ := f.delivered(); u == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	u, _ := f.delivered()
	t.Fatalf("delivered %d events, want %d", u, want)
}

func TestEventsAreBatched(t *testing.T) {
	f := newFake()
	c := newClient(f, "p", 100)
	defer c.Close()
	record(t, c, 20)
	waitDelivered(t, f, 20, 3*time.Second)
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls > 2 {
		t.Fatalf("20 events took %d calls, want them batched", calls)
	}
}

// Events recorded while the collector is down are held and delivered once it
// is back, exactly once each.
func TestCollectorOutageLosesNothing(t *testing.T) {
	f := newFake()
	f.down = true
	c := newClient(f, "p", 100)
	defer c.Close()
	record(t, c, 30)
	time.Sleep(1500 * time.Millisecond) // at least one failed flush
	if u, _ := f.delivered(); u != 0 {
		t.Fatalf("delivered %d events while down", u)
	}
	f.mu.Lock()
	f.down = false
	f.mu.Unlock()
	waitDelivered(t, f, 30, 5*time.Second)
	if u, total := f.delivered(); total != u {
		t.Fatalf("%d deliveries for %d events: retries must not duplicate", total, u)
	}
}

func TestFullQueueDropsAndCounts(t *testing.T) {
	f := newFake()
	f.hang = true // the sender is stuck, so nothing leaves the queue
	c := newClient(f, "p", 5)
	before := testutil.ToFloat64(observability.UsageEventsDroppedTotal.WithLabelValues("queue_full"))
	start := time.Now()
	record(t, c, 600) // the sender holds up to maxBatch (500), the queue 5 more
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("recording took %v: a stuck collector must never block requests", elapsed)
	}
	dropped := testutil.ToFloat64(observability.UsageEventsDroppedTotal.WithLabelValues("queue_full")) - before
	if dropped < 90 {
		t.Fatalf("dropped %v events, want at least 90 of 600 with room for 505", dropped)
	}
	f.mu.Lock()
	f.hang = false
	f.mu.Unlock()
	_ = c.Close()
}

func TestCloseFlushesQueuedEvents(t *testing.T) {
	f := newFake()
	c := newClient(f, "p", 100)
	record(t, c, 7)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.delivered(); u != 7 {
		t.Fatalf("delivered %d events by Close, want 7", u)
	}
}

// One event the collector refuses must cost only itself, not block delivery.
func TestRejectedEventDoesNotBlockTheRest(t *testing.T) {
	f := newFake()
	c := newClient(f, "p", 100)
	defer c.Close()
	before := testutil.ToFloat64(observability.UsageEventsDroppedTotal.WithLabelValues("rejected"))
	record(t, c, 3)
	if err := c.RecordUsage("u", "a", "openai", "refused", llm.TokensUsage{Input: 1, Output: 1}, 1, 0, usagev1.Status_STATUS_SUCCESS, nil); err != nil {
		t.Fatal(err)
	}
	record(t, c, 3)
	waitDelivered(t, f, 6, 3*time.Second)
	if got := testutil.ToFloat64(observability.UsageEventsDroppedTotal.WithLabelValues("rejected")) - before; got != 1 {
		t.Fatalf("rejected drops = %v, want 1", got)
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls > 3 {
		t.Fatalf("%d calls: the named index should be dropped and the rest resent at once", calls)
	}
}

func TestRejectedIndex(t *testing.T) {
	err := status.Error(codes.InvalidArgument, "invalid event at index 12: model too long")
	if i, ok := rejectedIndex(err, 20); !ok || i != 12 {
		t.Fatalf("rejectedIndex = %d, %v", i, ok)
	}
	if _, ok := rejectedIndex(err, 5); ok {
		t.Fatal("an index outside the batch must not be trusted")
	}
	if _, ok := rejectedIndex(status.Error(codes.InvalidArgument, "bad"), 5); ok {
		t.Fatal("no index, no match")
	}
}

func TestValidationMirrorsCollector(t *testing.T) {
	c := newClient(newFake(), "p", 10)
	defer c.Close()
	long := strings.Repeat("m", maxModelLength+1)
	if err := c.RecordUsage("u", "a", "openai", long, llm.TokensUsage{Input: 1, Output: 1}, 1, 0, usagev1.Status_STATUS_SUCCESS, nil); err == nil {
		t.Fatal("a model name the collector would refuse must fail at RecordUsage")
	}
	if err := c.RecordUsage("u", "a", "openai", "m", llm.TokensUsage{Input: 1, Output: 1}, 1, -5, usagev1.Status_STATUS_SUCCESS, nil); err == nil {
		t.Fatal("a negative cost must fail at RecordUsage")
	}
}

func TestRecordAfterCloseIsCounted(t *testing.T) {
	c := newClient(newFake(), "p", 10)
	_ = c.Close()
	before := testutil.ToFloat64(observability.UsageEventsDroppedTotal.WithLabelValues("closed"))
	record(t, c, 2)
	if got := testutil.ToFloat64(observability.UsageEventsDroppedTotal.WithLabelValues("closed")) - before; got != 2 {
		t.Fatalf("closed drops = %v, want 2", got)
	}
}

func TestCloseDoesNotWaitOutAHungSend(t *testing.T) {
	f := newFake()
	f.hang = true
	c := newClient(f, "p", 10)
	record(t, c, 1)
	time.Sleep(1100 * time.Millisecond) // the flush is now stuck in the hung send
	start := time.Now()
	_ = c.Close()
	if elapsed := time.Since(start); elapsed > closeDeadline+time.Second {
		t.Fatalf("Close took %v, want at most about closeDeadline", elapsed)
	}
}

// tokenCollector is a real gRPC sink that remembers the authorization header.
type tokenCollector struct {
	usagev1.UnimplementedUsageServiceServer
	got chan string
}

func (s *tokenCollector) RecordUsageEvents(ctx context.Context, in *usagev1.RecordUsageEventsRequest) (*usagev1.RecordUsageEventsResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.got <- strings.Join(md.Get("authorization"), ",")
	return &usagev1.RecordUsageEventsResponse{CreatedCount: int32(len(in.GetEvents()))}, nil
}

func TestTokenIsSentAsBearer(t *testing.T) {
	for _, tc := range []struct{ token, want string }{{"s3cret", "Bearer s3cret"}, {"", ""}} {
		t.Run("token="+tc.token, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			sink := &tokenCollector{got: make(chan string, 8)}
			srv := grpc.NewServer()
			usagev1.RegisterUsageServiceServer(srv, sink)
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)

			c, err := NewUsageCollectorClient(lis.Addr().String(), "p", false, tc.token)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			record(t, c, 1)
			select {
			case got := <-sink.got:
				if got != tc.want {
					t.Errorf("authorization %q, want %q", got, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no batch reached the sink")
			}
		})
	}
}

// An auth refusal is not the events' fault: they are held, not dropped, and
// delivered once the token is fixed.
func TestAuthRefusalHoldsEvents(t *testing.T) {
	for _, code := range []codes.Code{codes.Unauthenticated, codes.PermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			f := newFake()
			f.refuse = status.Error(code, "refused")
			c := newClient(f, "p", 100)
			defer c.Close()
			if c.refusal(f.refuse) == "" {
				t.Fatalf("%v is not reported as an auth refusal", code)
			}
			record(t, c, 5)
			time.Sleep(1500 * time.Millisecond) // at least one refused flush
			if u, _ := f.delivered(); u != 0 {
				t.Fatalf("delivered %d events while refused", u)
			}
			f.mu.Lock()
			f.refuse = nil
			f.mu.Unlock()
			waitDelivered(t, f, 5, 5*time.Second)
		})
	}
}

func TestFitMetaKeepsEventsWithinTheCollectorLimit(t *testing.T) {
	small := map[string]string{"rule": "r1", "team": "t"}
	if got := fitMeta(small, "a"); len(got) != 2 {
		t.Fatalf("small meta changed: %v", got)
	}
	big := map[string]string{"rule": "r1", "prompt": strings.Repeat("x", maxMetaBytes), "": "empty key"}
	got := fitMeta(big, "a")
	if _, ok := got["prompt"]; ok {
		t.Error("an entry over the limit was kept")
	}
	if _, ok := got[""]; ok {
		t.Error("an empty key was kept")
	}
	if got["rule"] != "r1" {
		t.Errorf("small entries must survive: %v", got)
	}
	if big["prompt"] == "" {
		t.Error("the caller's map was modified")
	}
}

// Cache counts reach the event within tokens_input, so light-tokenmeter never
// refuses the event (and its batch) over an adapter's inconsistent report.
func TestCacheTokensStayWithinInput(t *testing.T) {
	for _, tc := range []struct {
		usage       llm.TokensUsage
		read, write int64
	}{
		{llm.TokensUsage{Input: 13000, CacheRead: 10000, CacheWrite: 2000}, 10000, 2000},
		{llm.TokensUsage{Input: 100, CacheRead: 80, CacheWrite: 80}, 80, 20},
		{llm.TokensUsage{Input: 10, CacheRead: 50}, 10, 0},
		{llm.TokensUsage{Input: 10, CacheRead: -1, CacheWrite: -1}, 0, 0},
		{llm.TokensUsage{CacheRead: 5}, 0, 0},
	} {
		got := tc.usage.Clamped()
		if int64(got.CacheRead) != tc.read || int64(got.CacheWrite) != tc.write {
			t.Errorf("Clamped(%+v) = %d, %d; want %d, %d", tc.usage, got.CacheRead, got.CacheWrite, tc.read, tc.write)
		}
	}
}
