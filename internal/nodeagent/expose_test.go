package nodeagent

import (
	"context"
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

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
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
sleep 300
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
	urls, err := a.startExposure(context.Background())
	if err != nil || urls != nil {
		t.Fatalf("urls=%v err=%v; --expose=none must start nothing", urls, err)
	}
	if a.cfg.NodeAuthToken != "t" || a.currentEndpoint() != "https://mine.example" {
		t.Fatal("none mode must leave the operator's token and endpoint alone")
	}
}

func TestExposeUnknownModeAndMissingBinary(t *testing.T) {
	if _, err := NewAgent(AgentConfig{Expose: "ngrok"}).startExposure(context.Background()); err == nil {
		t.Fatal("unknown --expose must be rejected")
	}
	t.Setenv("PATH", t.TempDir())
	_, err := NewAgent(AgentConfig{Expose: ExposeCloudflared}).startExposure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cloudflared not found") {
		t.Fatalf("err = %v, want a hint about installing cloudflared", err)
	}
}
