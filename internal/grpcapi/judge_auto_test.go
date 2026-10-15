package grpcapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
)

func catchAllRule(name, effort string, chain ...string) *proxydb.Rule {
	return &proxydb.Rule{
		Name:     name,
		Effort:   effort,
		Priority: 1000,
		Enabled:  true,
		Use:      chain,
	}
}

func TestAutoRoute_JudgeSelection(t *testing.T) {
	judgeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Answer with high effort
		_, _ = w.Write([]byte(`{
			"model": "jev-latest",
			"answers": {
				"complexity": {
					"type": "choice",
					"choice": "high",
					"confidence": 0.95
				}
			},
			"usage": {"input_tokens": 12, "output_tokens": 0}
		}`))
	})
	judgeSrv := httptest.NewServer(judgeHandler)
	defer judgeSrv.Close()

	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"heavy answer"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	textSrv := httptest.NewServer(textHandler)
	defer textSrv.Close()

	models := []*proxydb.Model{
		testModel("m-low", registry.VendorOpenAI, textSrv.URL, registry.EffortLow),
		testModel("m-med", registry.VendorOpenAI, textSrv.URL, registry.EffortMedium),
		testModel("m-high", registry.VendorOpenAI, textSrv.URL, registry.EffortHigh),
		judgeModel("jev-latest", judgeSrv.URL),
	}

	rules := []*proxydb.Rule{
		catchAllRule("r-low", registry.EffortLow, "m-low"),
		catchAllRule("r-med", registry.EffortMedium, "m-med"),
		catchAllRule("r-high", registry.EffortHigh, "m-high"),
		judgeRule("jev-latest"),
	}

	s := newJudgeServer(t, models, rules, nil)

	req := &pb.GenerateTextRequest{
		Messages:  []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "Write a compiler in Go"}},
		AutoRoute: true,
	}

	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}

	if resp.ResolvedModel != "m-high" {
		t.Errorf("got resolved model %q, want m-high", resp.ResolvedModel)
	}
	if resp.RouteMode != router.RouteModeAutoJudge {
		t.Errorf("got route mode %q, want %q", resp.RouteMode, router.RouteModeAutoJudge)
	}
	if resp.JudgeScore != 0.95 {
		t.Errorf("got judge score %f, want 0.95", resp.JudgeScore)
	}
	if resp.JudgeModel != "jev-latest" {
		t.Errorf("got judge model %q, want jev-latest", resp.JudgeModel)
	}
}

func TestAutoRoute_JudgeFails_FallsBackToMedium(t *testing.T) {
	judgeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "judge error", http.StatusInternalServerError)
	})
	judgeSrv := httptest.NewServer(judgeHandler)
	defer judgeSrv.Close()

	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fallback answer"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	textSrv := httptest.NewServer(textHandler)
	defer textSrv.Close()

	models := []*proxydb.Model{
		testModel("m-low", registry.VendorOpenAI, textSrv.URL, registry.EffortLow),
		testModel("m-med", registry.VendorOpenAI, textSrv.URL, registry.EffortMedium),
		testModel("m-high", registry.VendorOpenAI, textSrv.URL, registry.EffortHigh),
		judgeModel("jev-latest", judgeSrv.URL),
	}

	rules := []*proxydb.Rule{
		catchAllRule("r-low", registry.EffortLow, "m-low"),
		catchAllRule("r-med", registry.EffortMedium, "m-med"),
		catchAllRule("r-high", registry.EffortHigh, "m-high"),
		judgeRule("jev-latest"),
	}

	s := newJudgeServer(t, models, rules, nil)

	req := &pb.GenerateTextRequest{
		Messages:  []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "Write something"}},
		AutoRoute: true,
	}

	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}

	if resp.ResolvedModel != "m-med" {
		t.Errorf("got resolved model %q, want m-med", resp.ResolvedModel)
	}
	if resp.RouteMode != router.RouteModeAutoDefault {
		t.Errorf("got route mode %q, want %q", resp.RouteMode, router.RouteModeAutoDefault)
	}
}

func TestAutoRoute_PlacementNodesOnly(t *testing.T) {
	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"local answer"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	textSrv := httptest.NewServer(textHandler)
	defer textSrv.Close()

	models := []*proxydb.Model{
		testModel("cloud-m1", registry.VendorOpenAI, textSrv.URL, registry.EffortMedium),
		testModel("node-m2", registry.VendorOllama, textSrv.URL, registry.EffortMedium),
	}

	rules := []*proxydb.Rule{
		catchAllRule("r-med", registry.EffortMedium, "cloud-m1", "node-m2"),
	}

	keys := map[string]map[string]string{
		"key1": {registry.VendorOpenAI: "sk-test", registry.VendorOllama: "ollama-test"},
	}
	s := NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, nil)}, nil)

	req := &pb.GenerateTextRequest{
		Effort:    pb.Effort_EFFORT_MEDIUM,
		Messages:  []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "Hi"}},
		Placement: router.PlacementNodesOnly,
	}

	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}

	if resp.ResolvedModel != "node-m2" {
		t.Errorf("got resolved model %q, want node-m2", resp.ResolvedModel)
	}
	if resp.Placement != router.PlacementNodesOnly {
		t.Errorf("got placement %q, want %q", resp.Placement, router.PlacementNodesOnly)
	}
}

func TestAutoRoute_JudgeTimeoutFallsBack(t *testing.T) {
	judgeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep beyond judgeAutoTimeout (250ms)
		time.Sleep(350 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	judgeSrv := httptest.NewServer(judgeHandler)
	defer judgeSrv.Close()

	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"timeout fallback"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	textSrv := httptest.NewServer(textHandler)
	defer textSrv.Close()

	models := []*proxydb.Model{
		testModel("m-med", registry.VendorOpenAI, textSrv.URL, registry.EffortMedium),
		judgeModel("jev-latest", judgeSrv.URL),
	}
	rules := []*proxydb.Rule{
		catchAllRule("r-med", registry.EffortMedium, "m-med"),
		judgeRule("jev-latest"),
	}

	s := newJudgeServer(t, models, rules, nil)

	req := &pb.GenerateTextRequest{
		Messages:  []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "Slow judge prompt"}},
		AutoRoute: true,
	}

	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}
	if resp.ResolvedModel != "m-med" {
		t.Errorf("got model %q, want m-med", resp.ResolvedModel)
	}
	if resp.RouteMode != router.RouteModeAutoDefault {
		t.Errorf("got route mode %q, want auto-default", resp.RouteMode)
	}
}

func TestAutoRoute_EffortSpecifiedSkipsJudge(t *testing.T) {
	var judgeCalled bool
	judgeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeCalled = true
		w.WriteHeader(http.StatusOK)
	})
	judgeSrv := httptest.NewServer(judgeHandler)
	defer judgeSrv.Close()

	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"direct answer"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	textSrv := httptest.NewServer(textHandler)
	defer textSrv.Close()

	models := []*proxydb.Model{
		testModel("m-low", registry.VendorOpenAI, textSrv.URL, registry.EffortLow),
		judgeModel("jev-latest", judgeSrv.URL),
	}
	rules := []*proxydb.Rule{
		catchAllRule("r-low", registry.EffortLow, "m-low"),
		judgeRule("jev-latest"),
	}

	s := newJudgeServer(t, models, rules, nil)

	req := &pb.GenerateTextRequest{
		Effort:          pb.Effort_EFFORT_LOW,
		Messages:        []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "Direct low"}},
		AutoRoute:       true,
		EffortSpecified: true,
	}

	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}
	if judgeCalled {
		t.Errorf("expected judge NOT to be called when effort is explicitly specified")
	}
	if resp.RouteMode != router.RouteModeAutoEffort {
		t.Errorf("got route mode %q, want auto-effort", resp.RouteMode)
	}
	if resp.ResolvedModel != "m-low" {
		t.Errorf("got resolved model %q, want m-low", resp.ResolvedModel)
	}
}

func TestAutoRoute_JudgeWithPlacement(t *testing.T) {
	judgeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-latest",
			"answers": {
				"complexity": {
					"type": "choice",
					"choice": "high",
					"confidence": 0.99
				}
			}
		}`))
	})
	judgeSrv := httptest.NewServer(judgeHandler)
	defer judgeSrv.Close()

	textHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"node high"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	textSrv := httptest.NewServer(textHandler)
	defer textSrv.Close()

	models := []*proxydb.Model{
		testModel("cloud-high", registry.VendorOpenAI, textSrv.URL, registry.EffortHigh),
		testModel("node-high", registry.VendorOllama, textSrv.URL, registry.EffortHigh),
		judgeModel("jev-latest", judgeSrv.URL),
	}
	rules := []*proxydb.Rule{
		catchAllRule("r-high", registry.EffortHigh, "cloud-high", "node-high"),
		judgeRule("jev-latest"),
	}

	keys := map[string]map[string]string{
		"key1": {
			registry.VendorTypeSafe: "ts-test",
			registry.VendorOpenAI:   "sk-test",
			registry.VendorOllama:   "ollama-test",
		},
	}
	s := NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, nil)}, nil)

	req := &pb.GenerateTextRequest{
		Messages:  []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "Heavy node prompt"}},
		AutoRoute: true,
		Placement: router.PlacementNodesOnly,
	}

	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}
	if resp.ResolvedModel != "node-high" {
		t.Errorf("got model %q, want node-high (placement nodes_only applied)", resp.ResolvedModel)
	}
	if resp.RouteMode != router.RouteModeAutoJudge {
		t.Errorf("got route mode %q, want auto-judge", resp.RouteMode)
	}
	if resp.Placement != router.PlacementNodesOnly {
		t.Errorf("got placement %q, want nodes_only", resp.Placement)
	}
}
