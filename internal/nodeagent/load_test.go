package nodeagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const vllmMetrics = `# HELP vllm:num_requests_running Number of requests currently running on GPU.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{engine="0",model_name="qwen"} 3.0
vllm:num_requests_running{engine="0",model_name="llama"} 1.0
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{engine="0",model_name="qwen"} 5.0
vllm:num_requests_waiting_by_reason{reason="capacity"} 99
`

func metricsServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVLLMLoadSumsSeries(t *testing.T) {
	url := metricsServer(t, http.StatusOK, vllmMetrics)
	l, err := (&VLLMInspector{}).Load(context.Background(), url+"/v1", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if l.Running != 4 || l.Waiting != 5 {
		t.Fatalf("load = %+v, want running 4 (two models), waiting 5", l)
	}
}

func TestTGIAndLlamaCppLoad(t *testing.T) {
	tgi := metricsServer(t, http.StatusOK, "tgi_batch_current_size 2\ntgi_queue_size 7\n")
	if l, err := (&TGIInspector{}).Load(context.Background(), tgi, ""); err != nil || l.Running != 2 || l.Waiting != 7 {
		t.Fatalf("tgi load = %+v, %v", l, err)
	}
	llama := metricsServer(t, http.StatusOK, "llamacpp:requests_processing 1\nllamacpp:requests_deferred 4\n")
	if l, err := (&LlamaCppInspector{}).Load(context.Background(), llama, ""); err != nil || l.Running != 1 || l.Waiting != 4 {
		t.Fatalf("llama.cpp load = %+v, %v", l, err)
	}
}

// llama.cpp serves /metrics only with --metrics; without it the heartbeat
// must fall back rather than report a fake empty queue.
func TestLoadFailsWithoutMetrics(t *testing.T) {
	off := metricsServer(t, http.StatusNotImplemented, "")
	if _, err := (&LlamaCppInspector{}).Load(context.Background(), off, ""); err == nil {
		t.Fatal("a 501 /metrics must be an error")
	}
	unrelated := metricsServer(t, http.StatusOK, "process_cpu_seconds_total 12\n")
	if _, err := (&VLLMInspector{}).Load(context.Background(), unrelated, ""); err == nil {
		t.Fatal("metrics without the queue gauges must be an error")
	}
}

func TestAgentLoadFallsBackToGateway(t *testing.T) {
	gw := NewGateway("http://127.0.0.1:1", "k", "")
	gw.inflight.Add(3)
	a := &Agent{inspector: &OllamaInspector{}, gateway: gw}
	if active, queued := a.load(context.Background()); active != 3 || queued != 0 {
		t.Fatalf("load = %d/%d, want the gateway's 3 in flight and no queue", active, queued)
	}

	a = &Agent{inspector: &OllamaInspector{}}
	if active, queued := a.load(context.Background()); active != 0 || queued != 0 {
		t.Fatalf("load without a source = %d/%d, want zeros", active, queued)
	}
}

func TestEngineLoadWinsOverGateway(t *testing.T) {
	url := metricsServer(t, http.StatusOK, vllmMetrics)
	gw := NewGateway(url, "k", "")
	gw.inflight.Add(1)
	a := &Agent{inspector: &VLLMInspector{}, gateway: gw, cfg: AgentConfig{EngineURL: url}}
	if active, queued := a.load(context.Background()); active != 4 || queued != 5 {
		t.Fatalf("load = %d/%d, want the engine's own 4/5", active, queued)
	}
}

func TestParseSample(t *testing.T) {
	for _, tc := range []struct {
		line  string
		name  string
		value float64
		ok    bool
	}{
		{`up 1`, "up", 1, true},
		{`m{a="x y",b="}"} 2.5 1700000000`, "m", 2.5, true},
		{`# HELP m help`, "", 0, false},
		{`m{a="1"} NaNx`, "", 0, false},
	} {
		name, v, ok := parseSample(tc.line)
		if ok != tc.ok || name != tc.name || v != tc.value {
			t.Errorf("parseSample(%q) = %q %v %v", tc.line, name, v, ok)
		}
	}
}
