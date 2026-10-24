package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/openai/openai-go/v3"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/llm/adapters"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func usageLogAt(t *testing.T, retention time.Duration) (*UsageLog, *miniredis.Miniredis, func(time.Time)) {
	t.Helper()
	mgr, mr := setupTestManager(t)
	ul := NewUsageLog(mgr.store, retention)
	set := func(at time.Time) {
		mr.SetTime(at) // stream ids come from Redis's clock, the cutoff from ours
		ul.now = func() time.Time { return at }
	}
	set(time.UnixMilli(1_800_000_000_000))
	return ul, mr, set
}

func TestUsageLogPagesInOrder(t *testing.T) {
	ctx := context.Background()
	ul, _, _ := usageLogAt(t, time.Hour)
	for i := range 3 {
		if err := ul.Append(ctx, "n", UsageRecord{RequestID: fmt.Sprint("r", i), Outcome: usageOutcomeOK, InputTokens: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := ul.Pull(ctx, "n", "", 2)
	if err != nil || len(page.Records) != 2 || !page.More || page.Records[0].RequestID != "r0" {
		t.Fatalf("first page = %+v (%v), want r0, r1 and more", page, err)
	}
	page, err = ul.Pull(ctx, "n", page.NextID, 2)
	if err != nil || len(page.Records) != 1 || page.More || page.Records[0].RequestID != "r2" || page.Records[0].InputTokens != 2 {
		t.Fatalf("second page = %+v (%v), want r2 alone", page, err)
	}
	last := page.NextID
	page, err = ul.Pull(ctx, "n", last, 0)
	if err != nil || len(page.Records) != 0 || page.NextID != last || !page.GapUntil.IsZero() {
		t.Fatalf("caught-up page = %+v (%v), want empty with the same cursor", page, err)
	}
	if _, err := ul.Pull(ctx, "n", "yesterday", 0); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor: err = %v, want ErrBadCursor", err)
	}
	if page, _ := ul.Pull(ctx, "other", "", 0); len(page.Records) != 0 {
		t.Fatalf("another node sees %d records", len(page.Records))
	}
}

func TestUsageLogTrimsAndReportsGap(t *testing.T) {
	ctx := context.Background()
	ul, _, set := usageLogAt(t, time.Hour)
	start := ul.now()
	if err := ul.Append(ctx, "n", UsageRecord{RequestID: "old-1"}); err != nil {
		t.Fatal(err)
	}
	first, _ := ul.Pull(ctx, "n", "", 0)
	cursor := first.NextID
	if err := ul.Append(ctx, "n", UsageRecord{RequestID: "old-2"}); err != nil {
		t.Fatal(err)
	}

	set(start.Add(2 * time.Hour))
	if err := ul.Append(ctx, "n", UsageRecord{RequestID: "new"}); err != nil {
		t.Fatal(err)
	}
	all, _ := ul.Pull(ctx, "n", "", 0)
	if len(all.Records) != 1 || all.Records[0].RequestID != "new" {
		t.Fatalf("after the retention the log holds %+v, want only the new record", all.Records)
	}
	page, err := ul.Pull(ctx, "n", cursor, 0)
	if err != nil || len(page.Records) != 1 || page.GapUntil.IsZero() {
		t.Fatalf("pull from a trimmed cursor = %+v (%v), want the new record and a gap", page, err)
	}
	if !page.GapUntil.Equal(start.Add(2 * time.Hour)) {
		t.Errorf("gap until %v, want the oldest kept record at %v", page.GapUntil, start.Add(2*time.Hour))
	}

	// A cursor at the oldest kept record lost nothing, however old.
	set(start.Add(5 * time.Hour))
	if page, _ := ul.Pull(ctx, "n", all.NextID, 0); !page.GapUntil.IsZero() {
		t.Errorf("cursor at the oldest record: gap until %v, want none", page.GapUntil)
	}
}

func TestUsageLogOff(t *testing.T) {
	ul, mr, _ := usageLogAt(t, 0)
	if err := ul.Append(context.Background(), "n", UsageRecord{RequestID: "r"}); err != nil {
		t.Fatal(err)
	}
	if len(mr.Keys()) != 0 {
		t.Fatalf("a disabled log wrote %v", mr.Keys())
	}
}

func TestCallOutcome(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	upstream, cancelUp := context.WithCancelCause(context.Background())
	cancelUp(circuitbreaker.ErrUpstreamTimeout)
	bg := context.Background()
	for _, c := range []struct {
		name          string
		ctx           context.Context
		err           error
		outcome, code string
	}{
		{"ok", bg, nil, usageOutcomeOK, ""},
		{"caller left", canceled, context.Canceled, usageOutcomeCancelled, ""},
		{"deadline", bg, context.DeadlineExceeded, usageOutcomeTimeout, ""},
		{"upstream timeout beats cancel", upstream, context.Canceled, usageOutcomeTimeout, ""},
		{"engine status", bg, &openai.Error{StatusCode: 503}, usageOutcomeError, "http_503"},
		{"decision status", bg, &adapters.OpenAIDecisionError{Status: 422}, usageOutcomeError, "http_422"},
		{"other", bg, errors.New("boom"), usageOutcomeError, ""},
	} {
		if o, code := callOutcome(c.ctx, c.err); o != c.outcome || code != c.code {
			t.Errorf("%s: outcome %q code %q, want %q %q", c.name, o, code, c.outcome, c.code)
		}
	}
}

// A call through the pool lands in the node's log with what the engine reported.
func TestPoolAdapterLogsCalls(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gemma","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`))
	}))
	t.Cleanup(srv.Close)

	mgr, _ := setupTestManager(t)
	mgr.usage = NewUsageLog(mgr.store, time.Hour)
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID: "n", Name: "n", Endpoint: srv.URL, Engine: "ollama", BaseWeight: 10, MaxConcurrent: 2,
		Models: []*pb.NodeModelSpec{{ModelId: "local-gemma", RemoteModelName: "gemma", IsWarm: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: "n", CapacityPercent: pct(50)}); err != nil {
		t.Fatal(err)
	}
	pa := NewPoolAdapter(NewSelector(mgr), nil, &proxydb.Model{ID: "local-gemma", Vendor: registry.VendorLocal}, time.Minute, nil)
	pa.async = func(f func()) { f() }
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	if _, err := pa.GenerateText(ctx, chat); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	fail.Store(true)
	if _, err := pa.GenerateText(ctx, chat); err == nil {
		t.Fatal("want the 503 back")
	}

	page, err := mgr.Usage().Pull(ctx, "n", "", 0)
	if err != nil || len(page.Records) != 2 {
		t.Fatalf("log = %+v (%v), want two records", page, err)
	}
	ok, bad := page.Records[0], page.Records[1]
	if ok.Kind != usageKindChat || ok.Outcome != usageOutcomeOK || ok.ModelID != "local-gemma" || ok.InputTokens != 12 || ok.OutputTokens != 5 || ok.CapacityPercent != 50 || ok.RequestID == "" || ok.StartedAt.IsZero() {
		t.Errorf("ok record = %+v", ok)
	}
	if bad.Outcome != usageOutcomeError || bad.ErrorCode != "http_503" || bad.RequestID == ok.RequestID {
		t.Errorf("failed record = %+v", bad)
	}
}

func TestUsageLogExpiresAndDrops(t *testing.T) {
	ctx := context.Background()
	ul, mr, _ := usageLogAt(t, time.Hour)
	if err := ul.Append(ctx, "n", UsageRecord{RequestID: "r"}); err != nil {
		t.Fatal(err)
	}
	key := ul.store.keys.NodeUsage("n")
	if ttl := mr.TTL(key); ttl != time.Hour {
		t.Fatalf("TTL = %v, want the retention: an idle node's log must expire", ttl)
	}
	if err := ul.Drop(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(key) {
		t.Fatal("Drop left the log in place")
	}
}

func TestUsageLogBoundsPendingWrites(t *testing.T) {
	ul, _, _ := usageLogAt(t, time.Hour)
	for i := range maxUsageWrites {
		if !ul.reserve() {
			t.Fatalf("write %d refused below the bound", i)
		}
	}
	if ul.reserve() {
		t.Fatal("a write beyond the bound was accepted")
	}
	ul.release()
	if !ul.reserve() {
		t.Fatal("a released slot was not reusable")
	}
}
