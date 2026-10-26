package nodeagent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// scriptedRegistry answers ValidateNode and RegisterNode from scripted errors.
type scriptedRegistry struct {
	pb.UnimplementedNodeRegistryServiceServer
	validateErrs, registerErrs []error // consumed in order; nil or empty after the list = success
	alwaysRefuse               error   // when set, ValidateNode returns it forever
	validates, registers       atomic.Int32
}

func pop(errs []error, n int32) error {
	if int(n) < len(errs) {
		return errs[n]
	}
	return nil
}

func (f *scriptedRegistry) ValidateNode(context.Context, *pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
	if f.alwaysRefuse != nil {
		f.validates.Add(1)
		return nil, f.alwaysRefuse
	}
	if err := pop(f.validateErrs, f.validates.Add(1)-1); err != nil {
		return nil, err
	}
	return &pb.ValidateNodeResponse{}, nil
}

func (f *scriptedRegistry) RegisterNode(context.Context, *pb.RegisterNodeRequest) (*pb.RegisterNodeResponse, error) {
	if err := pop(f.registerErrs, f.registers.Add(1)-1); err != nil {
		return nil, err
	}
	return &pb.RegisterNodeResponse{HeartbeatIntervalSeconds: 1}, nil
}

func (f *scriptedRegistry) Heartbeat(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return &pb.HeartbeatResponse{Ok: true}, nil
}

func (f *scriptedRegistry) UnregisterNode(context.Context, *pb.UnregisterNodeRequest) (*pb.UnregisterNodeResponse, error) {
	return &pb.UnregisterNodeResponse{Ok: true}, nil
}

// serve starts f on a loopback port and returns its address.
func serve(t *testing.T, f *scriptedRegistry) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpclib.NewServer()
	pb.RegisterNodeRegistryServiceServer(srv, f)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func fastAgent(cfg AgentConfig) *Agent {
	a := NewAgent(cfg)
	a.retryInitial, a.retryMax = time.Millisecond, 4*time.Millisecond
	return a
}

func denied() error {
	return status.Error(codes.PermissionDenied, `model "gemma4:latest" not permitted for this node key`)
}

func TestBackoffDoublesToCap(t *testing.T) {
	b := &backoff{initial: 10 * time.Second, max: 45 * time.Second}
	var got []time.Duration
	for range 5 {
		got = append(got, b.next())
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 45 * time.Second, 45 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays = %v, want %v", got, want)
		}
	}
	b.reset()
	if b.next() != 10*time.Second {
		t.Error("reset did not restart the sequence")
	}
}

func TestPreflightRetriesPermanentRefusal(t *testing.T) {
	f := &scriptedRegistry{validateErrs: []error{denied(), denied()}}
	a := fastAgent(AgentConfig{ProxyAddr: serve(t, f), NodeKey: "k", NodeID: "n", Engine: "ollama", EngineURL: "http://127.0.0.1:1"})
	conn, err := grpclib.NewClient(a.cfg.ProxyAddr, grpclib.WithTransportCredentials(a.dialCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	a.client = pb.NewNodeRegistryServiceClient(conn)

	if err := a.preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if got := f.validates.Load(); got != 3 {
		t.Errorf("ValidateNode calls = %d, want 3 (two refusals, then success)", got)
	}
	if got := f.registers.Load(); got != 0 {
		t.Errorf("preflight must not register, got %d RegisterNode calls", got)
	}
}

func TestPreflightSkippedOnOldProxy(t *testing.T) {
	f := &scriptedRegistry{validateErrs: []error{status.Error(codes.Unimplemented, "unknown method")}}
	a := fastAgent(AgentConfig{ProxyAddr: serve(t, f), NodeKey: "k", NodeID: "n", Engine: "ollama", EngineURL: "http://127.0.0.1:1"})
	conn, _ := grpclib.NewClient(a.cfg.ProxyAddr, grpclib.WithTransportCredentials(a.dialCredentials()))
	defer conn.Close()
	a.client = pb.NewNodeRegistryServiceClient(conn)

	if err := a.preflight(context.Background()); err != nil {
		t.Fatalf("preflight against an old proxy: %v", err)
	}
	if got := f.validates.Load(); got != 1 {
		t.Errorf("ValidateNode calls = %d, want 1", got)
	}
}

func TestRegisterUntilAcceptedKeepsRetrying(t *testing.T) {
	f := &scriptedRegistry{registerErrs: []error{denied(), status.Error(codes.Unavailable, "down")}}
	a := fastAgent(AgentConfig{ProxyAddr: serve(t, f), NodeKey: "k", NodeID: "n", Engine: "ollama", EngineURL: "http://127.0.0.1:1"})
	conn, _ := grpclib.NewClient(a.cfg.ProxyAddr, grpclib.WithTransportCredentials(a.dialCredentials()))
	defer conn.Close()
	a.client = pb.NewNodeRegistryServiceClient(conn)

	resp, err := a.registerUntilAccepted(context.Background(), nil)
	if err != nil || resp == nil {
		t.Fatalf("registerUntilAccepted = %v, %v", resp, err)
	}
	if got := f.registers.Load(); got != 3 {
		t.Errorf("RegisterNode calls = %d, want 3", got)
	}
}

func TestWaitStopsOnShutdown(t *testing.T) {
	a := fastAgent(AgentConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if a.wait(ctx, time.Minute, nil) {
		t.Error("wait should report shutdown when the context is cancelled")
	}
	a.drainChan <- struct{}{}
	if a.wait(context.Background(), time.Minute, nil) {
		t.Error("wait should report shutdown on drain")
	}
	urls := make(chan string, 1)
	urls <- "https://new.example"
	if !a.wait(context.Background(), time.Minute, urls) || a.currentEndpoint() != "https://new.example" {
		t.Error("a new tunnel address should cut the wait short and be adopted")
	}
}

// A refused node must not open a tunnel: cloudflared is never started while the proxy says no.
func TestRunStartsNoTunnelWhileRefused(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	bin := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+fakeVersion+"\ntouch "+marker+"\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &scriptedRegistry{alwaysRefuse: denied()} // never runs out, so timing cannot let it through
	a := fastAgent(AgentConfig{
		ProxyAddr: serve(t, f), NodeKey: "k", NodeID: "n", Engine: "ollama", EngineURL: "http://127.0.0.1:1",
		Expose: ExposeCloudflared, CloudflaredBin: bin,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := a.Run(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want the context deadline", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("cloudflared was started although the proxy refused the node")
	}
	if f.validates.Load() < 2 {
		t.Errorf("expected repeated pre-flight attempts, got %d", f.validates.Load())
	}
	if f.registers.Load() != 0 {
		t.Errorf("RegisterNode called %d times before pre-flight passed", f.registers.Load())
	}
}
