package nodeagent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// fakeOllama answers the three endpoints the doctor reads.
func fakeOllama(t *testing.T, tags, ps string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.0.0"}`))
		case "/api/tags":
			_, _ = w.Write([]byte(tags))
		case "/api/ps":
			_, _ = w.Write([]byte(ps))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func newDoctor(cfg AgentConfig) *Doctor {
	if cfg.Engine == "" {
		cfg.Engine = "ollama"
	}
	d := &Doctor{Cfg: cfg}
	d.defaults()
	return d
}

func one(t *testing.T, fs []Finding, check string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no %q finding in %+v", check, fs)
	return Finding{}
}

func TestDoctorEngineDown(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	_, fs, up := newDoctor(AgentConfig{EngineURL: url}).checkEngine(context.Background())
	if up {
		t.Fatal("a dead engine must not be reported as up")
	}
	f := one(t, fs, "Engine")
	if f.Level != LevelProblem || !strings.Contains(f.Message, "does not answer") || !strings.Contains(f.Fix, "11435") {
		t.Fatalf("got %+v", f)
	}
}

func TestDoctorEngineRefusesAgent(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer s.Close()
	_, fs, _ := newDoctor(AgentConfig{EngineURL: s.URL}).checkEngine(context.Background())
	if f := one(t, fs, "Engine"); f.Level != LevelProblem || !strings.Contains(f.Fix, "NODE_AUTH_TOKEN") {
		t.Fatalf("got %+v", f)
	}
}

func TestDoctorModels(t *testing.T) {
	tags := `{"models":[{"name":"gemma4:26b","model":"gemma4:26b"},{"name":"tiny:latest","model":"tiny:latest"}]}`
	ps := `{"models":[{"name":"gemma4:26b","model":"gemma4:26b"}]}`
	s := fakeOllama(t, tags, ps)
	ctx := context.Background()

	d := newDoctor(AgentConfig{EngineURL: s.URL})
	models, fs, _ := d.checkEngine(ctx)
	if f := one(t, fs, "Engine"); f.Level != LevelOK {
		t.Fatalf("engine: %+v", f)
	}

	cases := []struct {
		model string
		level Level
		want  string
	}{
		{"", LevelOK, "2 model(s)"},
		{"gemma4:26b", LevelOK, "installed and loaded"},
		{"tiny", LevelWarn, "not loaded in memory"}, // ":latest" is implied, and it is cold
		{"qwen3:30b", LevelProblem, "not installed"},
	}
	for _, c := range cases {
		d.Model = c.model
		f := one(t, d.checkModel(models), "Model")
		if f.Level != c.level || !strings.Contains(f.Message, c.want) {
			t.Errorf("model %q: got %+v, want level %d containing %q", c.model, f, c.level, c.want)
		}
	}
	d.Model = "qwen3:30b"
	if f := one(t, d.checkModel(models), "Model"); !strings.Contains(f.Fix, "ollama pull qwen3:30b") {
		t.Errorf("missing model should say how to pull it: %+v", f)
	}

	empty := one(t, newDoctor(AgentConfig{EngineURL: s.URL}).checkModel(nil), "Model")
	if empty.Level != LevelProblem || !strings.Contains(empty.Message, "no models") {
		t.Fatalf("no models: %+v", empty)
	}
}

func TestDoctorAcceleration(t *testing.T) {
	ctx := context.Background()
	run := func(ps, goos, goarch string, intel bool) []Finding {
		s := fakeOllama(t, `{"models":[]}`, ps)
		d := newDoctor(AgentConfig{EngineURL: s.URL})
		d.GOOS, d.GOARCH = goos, goarch
		d.LookPath = func(string) (string, error) { return "/usr/local/bin/ollama", nil }
		d.MachOArchs = func(string) ([]string, error) {
			if intel {
				return []string{"x86_64"}, nil
			}
			return []string{"x86_64", "arm64"}, nil
		}
		return d.checkAcceleration(ctx)
	}
	cpu := `{"models":[{"name":"gemma4:26b","size":15000,"size_vram":0}]}`
	half := `{"models":[{"name":"gemma4:26b","size":15000,"size_vram":7500}]}`
	full := `{"models":[{"name":"gemma4:26b","size":15000,"size_vram":15000}]}`

	f := one(t, run(cpu, "darwin", "arm64", true), "GPU")
	if f.Level != LevelProblem || !strings.Contains(f.Message, "Rosetta") || !strings.Contains(f.Message, "/usr/local/bin/ollama") {
		t.Fatalf("Intel Ollama on Apple Silicon: %+v", f)
	}
	f = one(t, run(cpu, "darwin", "arm64", false), "GPU")
	if f.Level != LevelProblem || strings.Contains(f.Message, "/usr/local/bin/ollama") {
		t.Fatalf("CPU-only with a universal binary should not blame PATH's Ollama: %+v", f)
	}
	f = one(t, run(cpu, "linux", "amd64", false), "GPU")
	if f.Level != LevelProblem || strings.Contains(f.Message, "Rosetta") || !strings.Contains(f.Fix, "NVIDIA") {
		t.Fatalf("CPU-only on Linux: %+v", f)
	}
	f = one(t, run(half, "linux", "amd64", false), "GPU")
	if f.Level != LevelWarn || !strings.Contains(f.Message, "50%") {
		t.Fatalf("partial offload: %+v", f)
	}
	if f = one(t, run(full, "darwin", "arm64", false), "GPU"); f.Level != LevelOK {
		t.Fatalf("fully on GPU: %+v", f)
	}
	// Nothing loaded yet: an Intel-only Ollama on PATH is still worth a warning on Apple Silicon.
	f = one(t, run(`{"models":[]}`, "darwin", "arm64", true), "GPU")
	if f.Level != LevelWarn || !strings.Contains(f.Message, "Intel-only") {
		t.Fatalf("idle Intel Ollama: %+v", f)
	}
	if got := run(`{"models":[]}`, "linux", "amd64", false); len(got) != 0 {
		t.Fatalf("nothing to say on an idle Linux box, got %+v", got)
	}
}

// ---- proxy ----------------------------------------------------------------

type heartbeatFn func(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error)

type fakeProxy struct {
	pb.UnimplementedNodeRegistryServiceServer
	hb       heartbeatFn
	validate func(*pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) // nil = an old proxy
}

func (f *fakeProxy) ValidateNode(_ context.Context, req *pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
	if f.validate == nil {
		return nil, status.Error(codes.Unimplemented, "unknown method ValidateNode")
	}
	return f.validate(req)
}

func (f *fakeProxy) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	return f.hb(ctx, req)
}

func serveProxy(t *testing.T, hb heartbeatFn, creds credentials.TransportCredentials) string {
	t.Helper()
	return serveFakeProxy(t, &fakeProxy{hb: hb}, creds)
}

func serveFakeProxy(t *testing.T, fp *fakeProxy, creds credentials.TransportCredentials) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var opts []grpclib.ServerOption
	if creds != nil {
		opts = append(opts, grpclib.Creds(creds))
	}
	srv := grpclib.NewServer(opts...)
	pb.RegisterNodeRegistryServiceServer(srv, fp)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func serverTLS(t *testing.T) credentials.TransportCredentials {
	t.Helper()
	s := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(s.Close)
	return credentials.NewServerTLSFromCert(&s.TLS.Certificates[0])
}

func TestDoctorProxyAndKey(t *testing.T) {
	ctx := context.Background()
	denied := func(code codes.Code, msg string) heartbeatFn {
		return func(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
			return nil, status.Error(code, msg)
		}
	}
	cases := []struct {
		name      string
		hb        heartbeatFn
		keyLevel  Level
		wantInKey string
	}{
		{"unknown key", denied(codes.Unauthenticated, "unauthenticated"), LevelProblem, "wrong, revoked or expired"},
		{"key of another node", denied(codes.PermissionDenied, `key is bound to node_id "node-1"`), LevelProblem, `belongs to node "node-1"`},
		{"key of this node", denied(codes.PermissionDenied, `key is bound to node_id "node-9"`), LevelOK, "bound to this node id"},
		{"not a node key", denied(codes.PermissionDenied, "requires scope node"), LevelProblem, "not a node key"},
		{"unbound key", func(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
			return &pb.HeartbeatResponse{ReRegisterRequired: true}, nil
		}, LevelOK, "accepts the node key"},
	}
	for _, c := range cases {
		addr := serveProxy(t, c.hb, nil)
		fs := newDoctor(AgentConfig{ProxyAddr: addr, NodeKey: "k", NodeID: "node-9"}).checkProxy(ctx)
		if p := one(t, fs, "Proxy"); p.Level != LevelOK {
			t.Errorf("%s: proxy should be reachable: %+v", c.name, p)
		}
		if k := one(t, fs, "Node key"); k.Level != c.keyLevel || !strings.Contains(k.Message, c.wantInKey) {
			t.Errorf("%s: key finding %+v, want level %d containing %q", c.name, k, c.keyLevel, c.wantInKey)
		}
	}
}

func TestDoctorProbeNeverUsesRealNodeID(t *testing.T) {
	var got string
	addr := serveProxy(t, func(_ context.Context, r *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
		got = r.GetNodeId()
		return &pb.HeartbeatResponse{ReRegisterRequired: true}, nil
	}, nil)
	newDoctor(AgentConfig{ProxyAddr: addr, NodeKey: "k", NodeID: "node-9"}).checkProxy(context.Background())
	if got == "node-9" || got == "" {
		t.Fatalf("probe heartbeat used node id %q; a real id would overwrite a live node's heartbeat", got)
	}
}

func TestDoctorProxyUnreachable(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	fs := newDoctor(AgentConfig{ProxyAddr: addr, NodeKey: "k"}).checkProxy(context.Background())
	if f := one(t, fs, "Proxy"); f.Level != LevelProblem || !strings.Contains(f.Message, "Cannot connect") {
		t.Fatalf("got %+v", f)
	}
	fs = newDoctor(AgentConfig{ProxyAddr: "no-such-host.invalid:443", NodeKey: "k"}).checkProxy(context.Background())
	if f := one(t, fs, "Proxy"); f.Level != LevelProblem || !strings.Contains(f.Message, "cannot be resolved") {
		t.Fatalf("DNS: got %+v", f)
	}
	fs = newDoctor(AgentConfig{ProxyAddr: "missing-port", NodeKey: "k"}).checkProxy(context.Background())
	if f := one(t, fs, "Proxy"); !strings.Contains(f.Message, "host:port") {
		t.Fatalf("address shape: got %+v", f)
	}
}

func TestDoctorTLSMismatchHints(t *testing.T) {
	ctx := context.Background()
	ok := func(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
		return &pb.HeartbeatResponse{ReRegisterRequired: true}, nil
	}

	plain := serveProxy(t, ok, nil)
	f := one(t, newDoctor(AgentConfig{ProxyAddr: plain, NodeKey: "k", TLS: true}).checkProxy(ctx), "Proxy")
	if f.Level != LevelProblem || !strings.Contains(f.Message, "Drop --tls") {
		t.Errorf("TLS client against a plaintext server: %+v", f)
	}

	secure := serveProxy(t, ok, serverTLS(t))
	f = one(t, newDoctor(AgentConfig{ProxyAddr: secure, NodeKey: "k", TLS: false}).checkProxy(ctx), "Proxy")
	if f.Level != LevelProblem || !strings.Contains(f.Message, "Add --tls") {
		t.Errorf("plaintext client against a TLS server: %+v", f)
	}

	f = one(t, newDoctor(AgentConfig{ProxyAddr: secure, NodeKey: "k", TLS: true}).checkProxy(ctx), "Proxy")
	if f.Level != LevelProblem || !strings.Contains(f.Message, "does not trust") {
		t.Errorf("self-signed certificate: %+v", f)
	}
}

// ---- tunnel and endpoint --------------------------------------------------

func TestDoctorTunnel(t *testing.T) {
	ctx := context.Background()
	mk := func() *Doctor {
		d := newDoctor(AgentConfig{Expose: ExposeCloudflared})
		d.LookPath = func(string) (string, error) { return "/usr/local/bin/cloudflared", nil }
		d.RunCmd = func(context.Context, string, ...string) error { return nil }
		d.DialTCP = func(context.Context, string) error { return nil }
		return d
	}
	if f := one(t, mk().checkTunnel(ctx), "Tunnel"); f.Level != LevelOK {
		t.Fatalf("healthy: %+v", f)
	}

	d := mk()
	d.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if f := one(t, d.checkTunnel(ctx), "Tunnel"); f.Level != LevelProblem || !strings.Contains(f.Message, "not installed") {
		t.Fatalf("missing binary: %+v", f)
	}

	d = mk()
	d.RunCmd = func(context.Context, string, ...string) error { return errors.New("killed: 9") }
	if f := one(t, d.checkTunnel(ctx), "Tunnel"); f.Level != LevelProblem || !strings.Contains(f.Fix, "Gatekeeper") {
		t.Fatalf("blocked binary: %+v", f)
	}

	d = mk()
	d.DialTCP = func(_ context.Context, addr string) error {
		if !strings.HasSuffix(addr, ":7844") {
			t.Errorf("tunnel check dialed %s, want port 7844", addr)
		}
		return errors.New("i/o timeout")
	}
	if f := one(t, d.checkTunnel(ctx), "Tunnel"); f.Level != LevelProblem || !strings.Contains(f.Message, "7844") {
		t.Fatalf("blocked port: %+v", f)
	}
}

func TestDoctorEndpoint(t *testing.T) {
	ctx := context.Background()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	f := one(t, newDoctor(AgentConfig{EngineURL: "http://127.0.0.1:1", Endpoint: s.URL}).checkEndpoint(ctx), "Endpoint")
	if f.Level != LevelOK {
		t.Fatalf("a 401 still proves the address is reachable: %+v", f)
	}
	s.Close()
	f = one(t, newDoctor(AgentConfig{EngineURL: "http://127.0.0.1:1", Endpoint: s.URL}).checkEndpoint(ctx), "Endpoint")
	if f.Level != LevelProblem || !strings.Contains(f.Message, "does not answer from here") {
		t.Fatalf("dead endpoint: %+v", f)
	}
	if got := newDoctor(AgentConfig{EngineURL: "http://x", Endpoint: "http://x"}).checkEndpoint(ctx); len(got) != 0 {
		t.Fatalf("endpoint equal to the engine URL has nothing extra to check: %+v", got)
	}
}

func TestRender(t *testing.T) {
	var b bytes.Buffer
	n := Render(&b, []Finding{
		{"Engine", LevelOK, "fine", "ignored for ok"},
		{"GPU", LevelWarn, "slow", "do a"},
		{"Node key", LevelProblem, "bad", "do b"},
	})
	out := b.String()
	if n != 1 {
		t.Fatalf("problems = %d", n)
	}
	for _, want := range []string{"[ ok ] Engine: fine", "[warn] GPU: slow", "What to do: do a", "[FAIL] Node key: bad", "What to do: do b", "1 problem found."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ignored for ok") {
		t.Errorf("OK lines should not print a fix:\n%s", out)
	}
	b.Reset()
	if Render(&b, []Finding{{"Engine", LevelOK, "fine", ""}}) != 0 || !strings.Contains(b.String(), "No problems found.") {
		t.Fatalf("clean report: %s", b.String())
	}
}

func TestMachOArchsReadsRealBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs a Mach-O binary")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	archs, err := machoArchs(exe)
	if err != nil || len(archs) == 0 {
		t.Fatalf("archs = %v, err = %v", archs, err)
	}
	want := map[string]string{"arm64": "arm64", "amd64": "x86_64"}[runtime.GOARCH]
	if archs[0] != want {
		t.Fatalf("archs = %v, want %s", archs, want)
	}
	if _, err := machoArchs(os.Args[0] + ".does-not-exist"); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestDoctorSkipsModelChecksWhenEngineIsDown(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	d := newDoctor(AgentConfig{EngineURL: url, ProxyAddr: "127.0.0.1:9", NodeKey: "k"})
	d.Model = "tiny:1b"
	for _, f := range d.Run(context.Background()) {
		if f.Check == "Model" || f.Check == "GPU" {
			t.Fatalf("%s check ran against a dead engine: %+v", f.Check, f)
		}
	}
}

func TestDoctorDoesNotDialDirectlyBehindAProxy(t *testing.T) {
	addr := serveProxy(t, func(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
		return &pb.HeartbeatResponse{ReRegisterRequired: true}, nil
	}, nil)
	d := newDoctor(AgentConfig{ProxyAddr: addr, NodeKey: "k", NodeID: "n"})
	d.UsesProxy = func(string) bool { return true }
	d.DialTCP = func(context.Context, string) error {
		return errors.New("direct dial blocked by the corporate firewall")
	}
	fs := d.checkProxy(context.Background())
	if f := one(t, fs, "Proxy"); f.Level != LevelOK {
		t.Fatalf("a blocked direct dial must not fail the check when a proxy is in use: %+v", f)
	}

	d.UsesProxy = func(string) bool { return false }
	if f := one(t, d.checkProxy(context.Background()), "Proxy"); f.Level != LevelProblem {
		t.Fatalf("without a proxy the direct dial failure is real: %+v", f)
	}
}

func TestDoctorRegistration(t *testing.T) {
	models := []*pb.NodeModelSpec{{ModelId: "tiny:1b"}, {ModelId: "gemma4:latest"}}
	cases := []struct {
		name     string
		validate func(*pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error)
		level    Level
		want     string
	}{
		{"all accepted", func(*pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
			return &pb.ValidateNodeResponse{AcceptedModels: []string{"tiny:1b", "gemma4:latest"}}, nil
		}, LevelOK, "would accept this node with 2 model(s)"},
		{"extra model ignored", func(*pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
			return &pb.ValidateNodeResponse{AcceptedModels: []string{"tiny:1b"}, DroppedModels: []string{"gemma4:latest"}}, nil
		}, LevelWarn, "does not allow gemma4:latest"},
		{"refused with the proxy's text", func(*pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
			return nil, status.Error(codes.PermissionDenied, `none of the models ["gemma4:latest"] is permitted for this node key`)
		}, LevelProblem, "gemma4:latest"},
		{"old proxy", nil, LevelWarn, "too old"},
	}
	for _, c := range cases {
		addr := serveFakeProxy(t, &fakeProxy{validate: c.validate}, nil)
		d := newDoctor(AgentConfig{ProxyAddr: addr, NodeKey: "k", NodeID: "node-9"})
		f := one(t, d.checkRegistration(context.Background(), models), "Registration")
		if f.Level != c.level || !strings.Contains(f.Message, c.want) {
			t.Errorf("%s: got %+v, want level %d containing %q", c.name, f, c.level, c.want)
		}
	}
}

func TestDoctorRunReportsRefusedRegistration(t *testing.T) {
	engine := fakeOllama(t, `{"models":[{"name":"gemma4:latest","model":"gemma4:latest"}]}`, `{"models":[]}`)
	addr := serveFakeProxy(t, &fakeProxy{
		hb: func(context.Context, *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
			return &pb.HeartbeatResponse{ReRegisterRequired: true}, nil
		},
		validate: func(*pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
			return nil, status.Error(codes.PermissionDenied, `model "gemma4:latest" not permitted`)
		},
	}, nil)
	d := newDoctor(AgentConfig{EngineURL: engine.URL, ProxyAddr: addr, NodeKey: "k", NodeID: "n"})
	d.UsesProxy = func(string) bool { return true }
	var buf bytes.Buffer
	if Render(&buf, d.Run(context.Background())) == 0 {
		t.Fatalf("the doctor said all is well though registration is refused:\n%s", buf.String())
	}
}
