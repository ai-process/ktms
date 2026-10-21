package usagereport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"

	"github.com/ai-process/ktms/internal/observability"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// fakeCollector records batches; while down it fails every call.
type fakeCollector struct {
	usagev1.UsageServiceClient
	mu     sync.Mutex
	down   bool
	hang   bool
	events map[string]int // event id -> times delivered
	calls  int
}

func newFake() *fakeCollector { return &fakeCollector{events: map[string]int{}} }

func (f *fakeCollector) RecordUsageEvents(ctx context.Context, in *usagev1.RecordUsageEventsRequest, _ ...grpc.CallOption) (*usagev1.RecordUsageEventsResponse, error) {
	f.mu.Lock()
	f.calls++
	down, hang := f.down, f.hang
	f.mu.Unlock()
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
		if err := c.RecordUsage("u", "a", "openai", "m", 1, 1, 1, 0, usagev1.Status_STATUS_SUCCESS, nil); err != nil {
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
