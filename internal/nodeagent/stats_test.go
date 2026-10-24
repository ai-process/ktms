package nodeagent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func pinnedStore(t *testing.T) (*StatsStore, *time.Location) {
	t.Helper()
	loc := berlin(t)
	st, err := OpenStats(filepath.Join(t.TempDir(), "stats"), true)
	if err != nil {
		t.Fatal(err)
	}
	st.loc = loc
	return st, loc
}

func TestStatsStoreDaysDedupAndGzip(t *testing.T) {
	st, loc := pinnedStore(t)
	day1 := time.Date(2026, 10, 23, 23, 30, 0, 0, loc)
	day2 := time.Date(2026, 10, 24, 0, 30, 0, 0, loc)
	recs := []RequestRecord{
		{RequestID: "a", StartedAt: day1, DurationMs: 1000, Outcome: "ok"},
		{RequestID: "b", StartedAt: day2, DurationMs: 2000, Outcome: "ok"},
	}
	if err := st.AppendRequests(recs); err != nil {
		t.Fatal(err)
	}
	// A crash between the write and the cursor repeats a record.
	if err := st.AppendRequests(recs[1:]); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"requests-2026-10-23.jsonl", "requests-2026-10-24.jsonl"} {
		if _, err := os.Stat(filepath.Join(st.Dir(), f)); err != nil {
			t.Fatalf("missing %s: a request belongs to the local day it started", f)
		}
	}
	from, to := day1.Add(-time.Hour), day2.Add(time.Hour)
	got, err := st.Requests(from, to)
	if err != nil || len(got) != 2 || got[0].RequestID != "a" || got[1].RequestID != "b" {
		t.Fatalf("Requests = %+v (%v), want a, b once each", got, err)
	}

	// Closing day 1 gzips it; a record pulled late for it joins the archive.
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
	if err := st.Maintain(now, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendRequests([]RequestRecord{{RequestID: "late", StartedAt: day1.Add(time.Minute), Outcome: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Maintain(now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), "requests-2026-10-23.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a closed day must be gzipped")
	}
	got, err = st.Requests(from, to)
	if err != nil || len(got) != 3 {
		t.Fatalf("after gzip: %+v (%v), want a, late, b", got, err)
	}

	// Retention drops whole days.
	if err := st.Maintain(now.Add(24*time.Hour), 36*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Requests(from, to); len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("after retention: %+v, want only day 2", got)
	}
}

func TestStatsStoreCursorAndSamples(t *testing.T) {
	st, loc := pinnedStore(t)
	if c, err := st.Cursor(); err != nil || c != "" {
		t.Fatalf("fresh cursor = %q (%v)", c, err)
	}
	if _, ok := st.CursorAge(time.Now()); ok {
		t.Fatal("no cursor yet, but an age")
	}
	if err := st.SetCursor("1729760000000-3"); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Cursor(); c != "1729760000000-3" {
		t.Fatalf("cursor = %q", c)
	}
	at := time.Date(2026, 10, 24, 10, 0, 0, 0, loc)
	for i := range 3 {
		if err := st.AppendSample(Sample{At: at.Add(time.Duration(i) * time.Minute), State: SampleStatePaused}); err != nil {
			t.Fatal(err)
		}
	}
	if s, err := st.Samples(at, at.Add(2*time.Minute)); err != nil || len(s) != 2 {
		t.Fatalf("Samples = %d (%v), want 2 in range", len(s), err)
	}
}

func TestParseRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "36h": 36 * time.Hour, "0": 0} {
		if got, err := ParseRetention(in); err != nil || got != want {
			t.Errorf("ParseRetention(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "-1d", "-3h"} {
		if _, err := ParseRetention(in); err == nil {
			t.Errorf("ParseRetention(%q) accepted", in)
		}
	}
}

// usageRegistry serves pages of a fixed log and records the cursors asked for.
type usageRegistry struct {
	levelRegistry
	mu      sync.Mutex
	log     []*pb.NodeUsageRecord
	cursors []string
	gapOnce bool
}

func (u *usageRegistry) PullNodeUsage(_ context.Context, r *pb.PullNodeUsageRequest) (*pb.PullNodeUsageResponse, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cursors = append(u.cursors, r.GetAfterId())
	start := 0
	for i, rec := range u.log {
		if rec.GetId() == r.GetAfterId() {
			start = i + 1
		}
	}
	end := min(start+2, len(u.log)) // two per page, to exercise paging
	resp := &pb.PullNodeUsageResponse{Records: u.log[start:end], NextId: r.GetAfterId(), More: end < len(u.log)}
	if end > start {
		resp.NextId = u.log[end-1].GetId()
	}
	if u.gapOnce && r.GetAfterId() == "" {
		resp.GapUntil = timestamppb.New(time.UnixMilli(1_800_000_000_000))
		u.gapOnce = false
	}
	return resp, nil
}

func TestAgentPullsUsageLogAndSamples(t *testing.T) {
	reg := &usageRegistry{levelRegistry: levelRegistry{beats: make(chan uint32, 64)}, gapOnce: true}
	at := time.Now().Add(-time.Minute)
	for i := range 5 {
		reg.log = append(reg.log, &pb.NodeUsageRecord{
			Id: fmt.Sprintf("1800000000%03d-0", i), RequestId: fmt.Sprint("r", i), StartedAt: timestamppb.New(at),
			DurationMs: 1000, ModelId: "m:1b", Kind: "chat", InputTokens: 10, OutputTokens: 5, Outcome: "ok", CapacityPercent: 100,
		})
	}
	dir := filepath.Join(t.TempDir(), "stats")
	stopAgent := runAgentWith(t, reg, func(c *AgentConfig) { c.StatsDir = dir }, func(a *Agent) { a.statsEvery = 50 * time.Millisecond })
	defer stopAgent()

	st, _ := OpenStats(dir, false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		recs, _ := st.Requests(at.Add(-time.Hour), at.Add(time.Hour))
		samples, _ := st.Samples(at.Add(-time.Hour), time.Now().Add(time.Hour))
		if len(recs) == 5 && len(samples) > 0 {
			if c, _ := st.Cursor(); c != "1800000000004-0" {
				t.Fatalf("cursor = %q, want the last record's id", c)
			}
			if recs[4].InputTokens != 10 || recs[4].Model != "m:1b" || recs[4].Level != 100 {
				t.Fatalf("record = %+v", recs[4])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 10s: %d records, %d samples", len(recs), len(samples))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gaps, _ := st.Gaps(time.Time{}, time.Now().Add(time.Hour)); len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want the one the proxy reported", gaps)
	}
}

func TestAgentWithOldProxyKeepsSamples(t *testing.T) {
	reg := &levelRegistry{beats: make(chan uint32, 64)} // no PullNodeUsage: Unimplemented
	dir := filepath.Join(t.TempDir(), "stats")
	stopAgent := runAgentWith(t, reg, func(c *AgentConfig) { c.StatsDir = dir }, func(a *Agent) { a.statsEvery = 50 * time.Millisecond })
	defer stopAgent()
	st, _ := OpenStats(dir, false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s, _ := st.Samples(time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); len(s) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no samples written against a proxy without the usage log")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runAgentWith starts an agent against reg and returns a stop function.
func runAgentWith(t *testing.T, reg pb.NodeRegistryServiceServer, cfg func(*AgentConfig), tune func(*Agent)) func() {
	t.Helper()
	engine := &residentOllama{warm: true}
	srvEngine := newEngineServer(t, engine)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer()
	pb.RegisterNodeRegistryServiceServer(srv, reg)
	go func() { _ = srv.Serve(lis) }()
	c := AgentConfig{
		ProxyAddr: lis.Addr().String(), NodeKey: "k", NodeID: "n1", NodeName: "n1",
		Engine: "ollama", EngineURL: srvEngine, Endpoint: srvEngine, OnlyModels: []string{"m:1b"},
		BaseWeight: 100, MaxConcurrent: 1,
	}
	cfg(&c)
	a := NewAgent(c)
	tune(a)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop")
		}
		srv.Stop()
	}
}

func newEngineServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
