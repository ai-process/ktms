package nodeagent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

type fakeRegistry struct {
	pb.UnimplementedNodeRegistryServiceServer
	mu   sync.Mutex
	regs chan *pb.RegisterNodeRequest
}

func (f *fakeRegistry) RegisterNode(_ context.Context, r *pb.RegisterNodeRequest) (*pb.RegisterNodeResponse, error) {
	f.regs <- r
	return &pb.RegisterNodeResponse{AssignedNodeId: r.NodeId, HeartbeatIntervalSeconds: 1}, nil
}

func (f *fakeRegistry) Heartbeat(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return &pb.HeartbeatResponse{Ok: true}, nil
}

func (f *fakeRegistry) UnregisterNode(context.Context, *pb.UnregisterNodeRequest) (*pb.UnregisterNodeResponse, error) {
	return &pb.UnregisterNodeResponse{Ok: true}, nil
}

// fakeCloudflared prints a different address on each launch; the first run
// exits after a moment, like a dropped tunnel.
func fakeCloudflared(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
n=$(cat "` + dir + `/runs" 2>/dev/null || echo 0)
n=$((n+1))
echo $n > "` + dir + `/runs"
echo "INF |  https://run-$n-abc.trycloudflare.com  |" >&2
if [ "$n" = "1" ]; then sleep 1; exit 1; fi
exec sleep 300
`
	path := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExposeRegistersTunnelAndFollowsRestarts(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"gemma:26b"}]}`))
	}))
	defer engine.Close()

	reg := &fakeRegistry{regs: make(chan *pb.RegisterNodeRequest, 8)}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer()
	pb.RegisterNodeRegistryServiceServer(srv, reg)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	a := NewAgent(AgentConfig{
		ProxyAddr: lis.Addr().String(), NodeKey: "k", NodeID: "n1", NodeName: "n1",
		Engine: "ollama", EngineURL: engine.URL, Endpoint: engine.URL,
		EngineToken: "engine-secret", Expose: ExposeCloudflared, CloudflaredBin: fakeCloudflared(t),
		BaseWeight: 100, MaxConcurrent: 2,
	})
	a.probe = func(context.Context, string) error { return nil } // no real DNS in tests

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("agent did not stop")
		}
	}()

	next := func() *pb.RegisterNodeRequest {
		select {
		case r := <-reg.regs:
			return r
		case <-time.After(15 * time.Second):
			t.Fatal("timed out waiting for registration")
			return nil
		}
	}

	first := next()
	if !strings.HasPrefix(first.Endpoint, "https://run-1-") {
		t.Fatalf("first registration endpoint %q, want the first tunnel address", first.Endpoint)
	}
	if first.NodeAuthToken == "" || first.NodeAuthToken == "engine-secret" {
		t.Fatalf("registered token %q must be a generated gateway key, not the engine's own", first.NodeAuthToken)
	}
	if len(first.Models) != 1 || first.Models[0].ModelId != "gemma:26b" {
		t.Fatalf("models = %v", first.Models)
	}

	second := next()
	if !strings.HasPrefix(second.Endpoint, "https://run-2-") {
		t.Fatalf("after the tunnel restarted, endpoint %q, want the new address", second.Endpoint)
	}
	if second.NodeAuthToken != first.NodeAuthToken {
		t.Fatal("gateway key changed across a tunnel restart; the gateway keeps one key per process")
	}
}

func TestExposeNoneKeepsOperatorEndpointAndToken(t *testing.T) {
	a := NewAgent(AgentConfig{Endpoint: "https://mine.example", NodeAuthToken: "t", Expose: ExposeNone})
	urls, _, err := a.startExposure(context.Background())
	if err != nil || urls != nil {
		t.Fatalf("urls=%v err=%v; --expose=none must start nothing", urls, err)
	}
	if a.cfg.NodeAuthToken != "t" || a.currentEndpoint() != "https://mine.example" {
		t.Fatal("none mode must leave the operator's token and endpoint alone")
	}
}

func TestExposeUnknownModeAndMissingBinary(t *testing.T) {
	if _, _, err := NewAgent(AgentConfig{Expose: "ngrok"}).startExposure(context.Background()); err == nil {
		t.Fatal("unknown --expose must be rejected")
	}
	t.Setenv("PATH", t.TempDir())
	_, _, err := NewAgent(AgentConfig{Expose: ExposeCloudflared}).startExposure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cloudflared not found") {
		t.Fatalf("err = %v, want a hint about installing cloudflared", err)
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShutdownSignalDuringTunnelWaitReturns(t *testing.T) {
	// cloudflared that never produces an address
	a := NewAgent(AgentConfig{
		ProxyAddr: "127.0.0.1:1", NodeKey: "k", NodeID: "n", Engine: "ollama",
		EngineURL: "http://127.0.0.1:1", Expose: ExposeCloudflared, CloudflaredBin: writeScript(t, "exec sleep 300\n"),
	})
	a.probe = func(context.Context, string) error { return nil }
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()

	time.Sleep(300 * time.Millisecond)
	a.drainChan <- struct{}{} // what the SIGTERM handler does
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want a clean exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored the shutdown request while waiting for the tunnel")
	}
}

func TestRunReapsCloudflaredBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	bin := writeScript(t, `echo $$ > "`+pidFile+`"
echo "https://x-abc.trycloudflare.com" >&2
exec sleep 300
`)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"models":[]}`)) }))
	defer engine.Close()
	reg := &fakeRegistry{regs: make(chan *pb.RegisterNodeRequest, 8)}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer()
	pb.RegisterNodeRegistryServiceServer(srv, reg)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	a := NewAgent(AgentConfig{
		ProxyAddr: lis.Addr().String(), NodeKey: "k", NodeID: "n", Engine: "ollama",
		EngineURL: engine.URL, Expose: ExposeCloudflared, CloudflaredBin: bin, BaseWeight: 1, MaxConcurrent: 1,
	})
	a.probe = func(context.Context, string) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-reg.regs:
	case <-time.After(15 * time.Second):
		t.Fatal("agent never registered")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("agent did not stop")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("cloudflared (pid %d) is still running after Run returned", pid)
	}
}
