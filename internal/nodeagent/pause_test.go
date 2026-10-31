package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// levelRegistry records the levels heartbeats carry and reports a settable
// count of proxy requests still running on the node.
type levelRegistry struct {
	pb.UnimplementedNodeRegistryServiceServer
	mu     sync.Mutex
	levels []uint32
	active int32
	beats  chan uint32
}

func (f *levelRegistry) RegisterNode(_ context.Context, r *pb.RegisterNodeRequest) (*pb.RegisterNodeResponse, error) {
	return &pb.RegisterNodeResponse{AssignedNodeId: r.NodeId, HeartbeatIntervalSeconds: 1}, nil
}

func (f *levelRegistry) ValidateNode(context.Context, *pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
	return &pb.ValidateNodeResponse{}, nil
}

func (f *levelRegistry) Heartbeat(_ context.Context, r *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lvl := uint32(100)
	if r.CapacityPercent != nil {
		lvl = r.GetCapacityPercent()
	}
	f.levels = append(f.levels, lvl)
	select {
	case f.beats <- lvl:
	default:
	}
	return &pb.HeartbeatResponse{Ok: true, CapacityApplied: true, ActiveProxyRequests: f.active}, nil
}

func (f *levelRegistry) UnregisterNode(context.Context, *pb.UnregisterNodeRequest) (*pb.UnregisterNodeResponse, error) {
	return &pb.UnregisterNodeResponse{Ok: true}, nil
}

func (f *levelRegistry) setActive(n int32) {
	f.mu.Lock()
	f.active = n
	f.mu.Unlock()
}

// residentOllama keeps one model and logs every keep_alive call it gets.
type residentOllama struct {
	mu    sync.Mutex
	warm  bool
	calls []string // "m:1b=0" or "m:1b=-1"
}

func (o *residentOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch r.URL.Path {
	case "/api/tags":
		fmt.Fprint(w, `{"models":[{"name":"m:1b","model":"m:1b"}]}`)
	case "/api/ps":
		if o.warm {
			fmt.Fprint(w, `{"models":[{"name":"m:1b","model":"m:1b"}]}`)
		} else {
			fmt.Fprint(w, `{"models":[]}`)
		}
	case "/api/generate":
		var body struct {
			Model     string `json:"model"`
			KeepAlive int    `json:"keep_alive"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		o.calls = append(o.calls, fmt.Sprintf("%s=%d", body.Model, body.KeepAlive))
		o.warm = body.KeepAlive != 0
		fmt.Fprint(w, `{"done":true}`)
	default:
		http.NotFound(w, r)
	}
}

func (o *residentOllama) log() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.calls...)
}

func startScheduledAgent(t *testing.T, schedule string, clock *fakeClock) (*levelRegistry, *residentOllama) {
	t.Helper()
	_, reg, ollama := startAgent(t, schedule, clock)
	return reg, ollama
}

// startAgent runs an agent against a fake proxy and Ollama; opts adjust its config.
func startAgent(t *testing.T, schedule string, clock *fakeClock, opts ...func(*AgentConfig)) (*Agent, *levelRegistry, *residentOllama) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "schedule")
	if err := os.WriteFile(file, []byte(schedule), 0o644); err != nil {
		t.Fatal(err)
	}
	ollama := &residentOllama{warm: true}
	engine := httptest.NewServer(ollama)
	t.Cleanup(engine.Close)

	reg := &levelRegistry{beats: make(chan uint32, 64)}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer()
	pb.RegisterNodeRegistryServiceServer(srv, reg)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	cfg := AgentConfig{
		ProxyAddr: lis.Addr().String(), NodeKey: "k", NodeID: "n1", NodeName: "n1",
		Engine: "ollama", EngineURL: engine.URL, Endpoint: engine.URL, OnlyModels: []string{"m:1b"},
		BaseWeight: 100, MaxConcurrent: 1, ScheduleFile: file, Warmup: 5 * time.Minute,
	}
	for _, o := range opts {
		o(&cfg)
	}
	a := NewAgent(cfg)
	a.now = clock.Now
	a.levelEvery = 10 * time.Millisecond
	a.pauseGrace = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return a, reg, ollama
}

func waitLevel(t *testing.T, reg *levelRegistry, want uint32, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case l := <-reg.beats:
			if l == want {
				return
			}
		case <-deadline:
			t.Fatalf("no heartbeat at %d%% within %v", want, within)
		}
	}
}

func waitCalls(t *testing.T, o *residentOllama, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Join(o.log(), " ") == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("engine calls = %v, want %q", o.log(), want)
}

func TestScheduledPauseUnloadsAfterRequestsAndWarmsBeforeWindow(t *testing.T) {
	day := func(clock string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-20 "+clock, time.Local)
		return v
	}
	clock := &fakeClock{t: day("09:59")}
	reg, ollama := startScheduledAgent(t, "10:00-23:00 0\n", clock)
	reg.setActive(1)
	waitLevel(t, reg, 100, 5*time.Second)

	// The switch reaches the proxy at once, well inside the 1s heartbeat interval.
	clock.Set(day("10:00"))
	waitLevel(t, reg, 0, 500*time.Millisecond)

	// A proxy request still running keeps the model loaded through later beats.
	waitLevel(t, reg, 0, 3*time.Second)
	if calls := ollama.log(); len(calls) != 0 {
		t.Fatalf("engine called while a request was running: %v", calls)
	}
	reg.setActive(0)
	waitCalls(t, ollama, "m:1b=0")

	// Warm-up starts --warmup before the window, the level change at the window.
	clock.Set(day("22:56"))
	waitCalls(t, ollama, "m:1b=0 m:1b=-1")
	clock.Set(day("23:00"))
	waitLevel(t, reg, 100, 500*time.Millisecond)
}

func TestAgentStartedInsidePauseNeverReportsFull(t *testing.T) {
	noon, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-20 12:00", time.Local)
	reg, _ := startScheduledAgent(t, "10:00-23:00 0\n", &fakeClock{t: noon})
	waitLevel(t, reg, 0, 5*time.Second)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, l := range reg.levels {
		if l != 0 {
			t.Fatalf("heartbeat levels %v: an agent started in a pause must never report more than 0%%", reg.levels)
		}
	}
}

func TestBrokenScheduleEditKeepsCurrentSchedule(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schedule")
	if err := os.WriteFile(file, []byte("00:00-24:00 50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(AgentConfig{ScheduleFile: file})
	a.refreshLevel()
	if a.pause.level != 50 {
		t.Fatalf("level = %d, want 50", a.pause.level)
	}
	if err := os.WriteFile(file, []byte("00:00-24:00 50\nbroken line here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a.refreshLevel() || a.pause.level != 50 || a.pause.sched == nil {
		t.Fatalf("after a broken edit: level %d, schedule kept %v", a.pause.level, a.pause.sched != nil)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if !a.refreshLevel() || a.pause.level != 100 {
		t.Fatalf("after removing the file: level %d, want 100", a.pause.level)
	}
}

func TestGatewayRefusesWhilePaused(t *testing.T) {
	gw, srv, hits := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(completionJSON))
	}))
	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer access-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	gw.SetAccepting(false)
	if code := post(); code != http.StatusServiceUnavailable || hits.Load() != 0 {
		t.Fatalf("paused: status %d, engine hits %d; want 503 and none", code, hits.Load())
	}
	gw.SetAccepting(true)
	if code := post(); code != http.StatusOK {
		t.Fatalf("resumed: status %d, want 200", code)
	}
}

func TestInvalidScheduleRefusedAtStart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schedule")
	if err := os.WriteFile(file, []byte("10:00-12:00 0\n11:00-13:00 50\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(AgentConfig{ProxyAddr: "127.0.0.1:1", ScheduleFile: file})
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("Run = %v, want the overlap error", err)
	}
}

func TestSameSizeEditIsPickedUp(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schedule")
	write := func(text string) {
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("00:00-24:00 50\n")
	fi, _ := os.Stat(file)
	a := NewAgent(AgentConfig{ScheduleFile: file})
	a.refreshLevel()
	write("00:00-24:00 30\n")
	if err := os.Chtimes(file, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if !a.refreshLevel() || a.pause.level != 30 {
		t.Fatalf("level = %d after a same-size edit with the old mtime, want 30", a.pause.level)
	}
}

type failingController struct{ calls int }

func (f *failingController) Inspect(context.Context, string, string) ([]*pb.NodeModelSpec, error) {
	return nil, nil
}
func (f *failingController) Warm(context.Context, string, string, string) error { return nil }
func (f *failingController) Unload(context.Context, string, string, string) error {
	f.calls++
	return fmt.Errorf("engine down")
}

func TestPauseWaitsForFreshCountAndBacksOffFailures(t *testing.T) {
	file := filepath.Join(t.TempDir(), "schedule")
	if err := os.WriteFile(file, []byte("10:00-23:00 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)}
	ctl := &failingController{}
	a := NewAgent(AgentConfig{ScheduleFile: file})
	a.inspector, a.now, a.pauseGrace = ctl, clock.Now, 0
	a.refreshLevel()
	warm := []*pb.NodeModelSpec{{ModelId: "m", RemoteModelName: "m", IsWarm: true}}
	a.afterHeartbeat(&pb.HeartbeatResponse{Ok: true}, warm) // active 0, before the pause

	clock.Set(time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local))
	a.refreshLevel()
	a.stepPause()
	if a.pause.busy {
		t.Fatal("unload started on a request count from before the pause")
	}
	a.afterHeartbeat(&pb.HeartbeatResponse{Ok: true}, warm)
	a.finishEngineCall(<-a.engineDone)
	if ctl.calls != 1 {
		t.Fatalf("unload calls = %d, want 1", ctl.calls)
	}
	a.stepPause()
	if a.pause.busy {
		t.Fatal("failed unload retried at once")
	}
	clock.Set(clock.Now().Add(engineRetry))
	a.stepPause()
	a.finishEngineCall(<-a.engineDone)
	if ctl.calls != 2 {
		t.Fatalf("unload calls = %d after the retry delay, want 2", ctl.calls)
	}
}
