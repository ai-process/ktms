package nodeagent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ai-process/ktms/internal/registry"
)

const freeTokenKey = "ft-test-key"

// freeTokenServer answers like `ft serve --api-key`: every route but /health
// needs the key, as a bearer or in x-api-key. Bodies follow FreeToken's
// server/control_api.py, stats.py and openai_api.py.
func freeTokenServer(t *testing.T, health, stats string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" && r.Header.Get("Authorization") != "Bearer "+freeTokenKey && r.Header.Get("x-api-key") != freeTokenKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
			return
		}
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(health))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3.6-35b","object":"model","root":"/models/qwen","max_model_len":32768,"context_length":32768}]}`))
		case "/v1/stats":
			_, _ = w.Write([]byte(stats))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

const (
	ftHealthOK      = `{"status":"ok","model":"qwen3.6-35b","instance_id":"a","uptime_s":12,"maintenance":"serving","version":"0.4.0"}`
	ftHealthLoading = `{"status":"loading","phase":"weights","progress":{"done_bytes":1,"total_bytes":9},"model":"qwen3.6-35b","instance_id":"a"}`
	ftHealthError   = `{"status":"error","message":"backend worker died","instance_id":"a"}`
)

func ftStats(active int, modalities string) string {
	return `{"instance_id":"a","model":{"id":"qwen3.6-35b","ctx":32768,"attn":"hybrid_linear","moe":true,"input_modalities":` + modalities +
		`},"uptime_s":12,"kv":null,"throughput":{"decode_tps":0,"prefill_tps":0},"requests":{"active":` + strconv.Itoa(active) +
		`,"completed":7,"p95_ms":900,"ttft_mean_ms":120,"prompt_tokens_total":1,"completion_tokens_total":2}}`
}

func TestFreeTokenInspect(t *testing.T) {
	url := freeTokenServer(t, ftHealthOK, ftStats(0, `["text","image"]`))
	ms, err := GetInspector("FreeToken").Inspect(context.Background(), url+"/v1", freeTokenKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 {
		t.Fatalf("models %v", ms)
	}
	m := ms[0]
	if m.GetModelId() != "qwen3.6-35b" || m.GetContextLength() != 32768 || !m.GetIsWarm() {
		t.Fatalf("model %+v", m)
	}
	// Capabilities belong to the registry model: any here would make a --judge-model one not judge-only.
	if len(m.GetCapabilities()) != 0 {
		t.Fatalf("capabilities %v", m.GetCapabilities())
	}

	if _, err := GetInspector("freetoken").Inspect(context.Background(), url, "wrong"); err == nil {
		t.Fatal("a wrong key must fail the inspection")
	}
}

func TestFreeTokenInspectLoadingAndError(t *testing.T) {
	url := freeTokenServer(t, ftHealthLoading, ftStats(0, `["text"]`))
	ms, err := (&FreeTokenInspector{}).Inspect(context.Background(), url, freeTokenKey)
	if err != nil || len(ms) != 1 || ms[0].GetIsWarm() {
		t.Fatalf("loading: %v, %v", ms, err)
	}
	url = freeTokenServer(t, ftHealthError, ftStats(0, `["text"]`))
	if _, err := (&FreeTokenInspector{}).Inspect(context.Background(), url, freeTokenKey); !errors.Is(err, ErrFreeTokenFailed) || !strings.Contains(err.Error(), "backend worker died") {
		t.Fatalf("a failed server must report no models: %v", err)
	}

	// A proxy in front of ft serve may answer /health with 503 while it loads: cold, not gone.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(ftHealthLoading))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_length":8192}]}`))
	}))
	t.Cleanup(s.Close)
	if ms, err := (&FreeTokenInspector{}).Inspect(context.Background(), s.URL, ""); err != nil || len(ms) != 1 || ms[0].GetIsWarm() {
		t.Fatalf("503 loading: %v, %v", ms, err)
	}
}

func TestFreeTokenLoad(t *testing.T) {
	for _, c := range []struct {
		max, active      int
		running, waiting uint32
	}{
		{0, 0, 0, 0},
		{0, 3, 3, 0},
		{0, 6, 4, 2}, // default --max-running-requests 4
		{2, 6, 2, 4},
	} {
		url := freeTokenServer(t, ftHealthOK, ftStats(c.active, `["text"]`))
		l, err := (&FreeTokenInspector{MaxRunning: uint32(c.max)}).Load(context.Background(), url+"/v1", freeTokenKey)
		if err != nil || l.Running != c.running || l.Waiting != c.waiting {
			t.Errorf("max %d active %d: %+v, %v", c.max, c.active, l, err)
		}
	}

	url := freeTokenServer(t, ftHealthOK, `{"model":{}}`)
	if _, err := (&FreeTokenInspector{}).Load(context.Background(), url, freeTokenKey); err == nil {
		t.Fatal("stats without requests must not read as an idle engine")
	}
}

// The queue splits at the engine's own limit, not at what the proxy may send.
func TestNewAgentFreeTokenLimit(t *testing.T) {
	a := NewAgent(AgentConfig{Engine: registry.EngineFreeToken, MaxConcurrent: 16, EngineMaxRunning: 2})
	if ft, ok := a.inspector.(*FreeTokenInspector); !ok || ft.MaxRunning != 2 {
		t.Fatalf("inspector %#v", a.inspector)
	}
	if _, ok := a.inspector.(LoadProber); !ok {
		t.Fatal("FreeToken must report its queue")
	}
}

func TestDoctorFreeToken(t *testing.T) {
	ctx := context.Background()
	d := newDoctor(AgentConfig{Engine: registry.EngineFreeToken, EngineURL: "http://127.0.0.1:1"})
	d.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	_, fs, up := d.checkEngine(ctx)
	if f := one(t, fs, "Engine"); up || !strings.Contains(f.Fix, "Install FreeToken") {
		t.Fatalf("down without ft: %+v", f)
	}
	d.LookPath = func(string) (string, error) { return "/usr/local/bin/ft", nil }
	_, fs, _ = d.checkEngine(ctx)
	if f := one(t, fs, "Engine"); !strings.Contains(f.Fix, "ft serve --model") {
		t.Fatalf("down with ft: %+v", f)
	}

	url := freeTokenServer(t, ftHealthLoading, ftStats(0, `["text"]`))
	d = newDoctor(AgentConfig{Engine: registry.EngineFreeToken, EngineURL: url, EngineToken: "wrong"})
	_, fs, up = d.checkEngine(ctx)
	if f := one(t, fs, "Engine"); up || !strings.Contains(f.Fix, "FREETOKEN_API_KEY") {
		t.Fatalf("wrong key: %+v", f)
	}

	d = newDoctor(AgentConfig{Engine: registry.EngineFreeToken, EngineURL: url, EngineToken: freeTokenKey})
	models, fs, up := d.checkEngine(ctx)
	if !up {
		t.Fatalf("engine: %+v", fs)
	}
	if f := one(t, d.checkModel(models, false), "Model"); f.Level != LevelWarn || !strings.Contains(f.Fix, "/health") {
		t.Fatalf("loading, no --model: %+v", f)
	}
	d.Model = "qwen3.6-35b"
	if f := one(t, d.checkModel(models, false), "Model"); f.Level != LevelWarn || !strings.Contains(f.Fix, "/health") {
		t.Fatalf("loading: %+v", f)
	}
	failed := freeTokenServer(t, ftHealthError, ftStats(0, `["text"]`))
	_, fs, up = newDoctor(AgentConfig{Engine: registry.EngineFreeToken, EngineURL: failed, EngineToken: freeTokenKey}).checkEngine(ctx)
	if f := one(t, fs, "Engine"); up || !strings.Contains(f.Fix, "Restart ft serve") {
		t.Fatalf("failed backend: %+v", f)
	}

	d.Model = "llama"
	if f := one(t, d.checkModel(models, false), "Model"); f.Level != LevelProblem || !strings.Contains(f.Fix, "--served-model-name") {
		t.Fatalf("other model: %+v", f)
	}
}
