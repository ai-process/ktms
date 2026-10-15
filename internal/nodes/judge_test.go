package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/llm/adapters"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

func TestPoolAdapter_Judge_Success(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	// Mock node decisions server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		prob := 0.85
		conf := 0.95
		resp := map[string]any{
			"model": "jepa-node-v1",
			"answers": []map[string]any{
				{
					"name":        "q1",
					"type":        "noul",
					"probability": prob,
					"confidence":  conf,
				},
				{
					"name":   "q2",
					"type":   "choice",
					"choice": "opt_a",
					"probabilities": map[string]float64{
						"opt_a": 0.9,
						"opt_b": 0.1,
					},
				},
				{
					"name":  "q3",
					"type":  "score",
					"score": 4.0,
				},
			},
			"usage": map[string]int{
				"input_tokens":  25,
				"output_tokens": 0,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	meta := &NodeMeta{
		NodeID:        "node-judge-1",
		Name:          "Decision Node 1",
		Endpoint:      srv.URL,
		Engine:        "decisions",
		AuthToken:     "test-token",
		BaseWeight:    100,
		MaxConcurrent: 4,
		Models: []*pb.NodeModelSpec{
			{
				ModelId:         "jepa-fast",
				RemoteModelName: "jepa-node-v1",
				ContextLength:   4096,
				IsWarm:          true,
				Capabilities:    []string{registry.CapabilityJudge},
			},
		},
	}
	if err := mgr.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: meta.NodeID, WarmModels: []string{"jepa-fast"}}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	model := &proxydb.Model{
		ID:           "jepa-fast",
		Vendor:       registry.VendorLocal,
		Capabilities: []string{registry.CapabilityJudge},
	}
	sel := NewSelector(mgr)
	adapter := NewPoolAdapter(sel, nil, model, 30*time.Second, nil)

	req := &llm.JudgeRequest{
		StateJSON: `{"context": "testing"}`,
		Questions: map[string]*llm.JudgeQuestion{
			"q1": {
				Type:         llm.JudgeNoul,
				Instructions: "Is this valid?",
			},
			"q2": {
				Type:         llm.JudgeChoice,
				Instructions: "Which option?",
				Options:      map[string]string{"opt_a": "A", "opt_b": "B"},
			},
			"q3": {
				Type:         llm.JudgeScore,
				Instructions: "Score from 1 to 5",
				Levels:       []string{"1", "2", "3", "4", "5"},
			},
		},
	}
	callMeta := make(map[string]string)
	res, err := adapter.Judge(ctx, req, "user-123", callMeta)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if res.Model != "jepa-node-v1" {
		t.Errorf("got model %q, want jepa-node-v1", res.Model)
	}
	if ans, ok := res.Answers["q1"]; !ok || ans.Noul != 0.85 {
		t.Errorf("q1 answer unexpected: %+v", ans)
	}
	if ans, ok := res.Answers["q2"]; !ok || ans.Choice != "opt_a" {
		t.Errorf("q2 answer unexpected: %+v", ans)
	}
	if ans, ok := res.Answers["q3"]; !ok || ans.Score != 4.0 {
		t.Errorf("q3 answer unexpected: %+v", ans)
	}
	if callMeta["node_id"] != "node-judge-1" {
		t.Errorf("callMeta[node_id] = %q, want node-judge-1", callMeta["node_id"])
	}
}

func TestPoolAdapter_Judge_NotSupportedOnTextModel(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	sel := NewSelector(mgr)

	model := &proxydb.Model{
		ID:           "llama-chat",
		Vendor:       registry.VendorLocal,
		Capabilities: []string{"generate"},
	}
	adapter := NewPoolAdapter(sel, nil, model, 30*time.Second, nil)

	req := &llm.JudgeRequest{
		StateJSON: `{"test": true}`,
		Questions: map[string]*llm.JudgeQuestion{
			"q": {Type: llm.JudgeNoul, Instructions: "Check"},
		},
	}
	_, err := adapter.Judge(ctx, req, "user-1", nil)
	if !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Fatalf("expected ErrJudgmentUnsupported, got %v", err)
	}
}

func TestPoolAdapter_GenerateText_UnsupportedOnJudgeOnlyModel(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	sel := NewSelector(mgr)

	model := &proxydb.Model{
		ID:           "jepa-only",
		Vendor:       registry.VendorLocal,
		Capabilities: []string{registry.CapabilityJudge},
	}
	adapter := NewPoolAdapter(sel, nil, model, 30*time.Second, nil)

	_, err := adapter.GenerateText(ctx, nil)
	if !errors.Is(err, adapters.ErrTextGenerationUnsupported) {
		t.Fatalf("expected ErrTextGenerationUnsupported, got %v", err)
	}

	err = adapter.GenerateTextStream(ctx, nil, func(chunk llm.StreamChunk) error { return nil })
	if !errors.Is(err, adapters.ErrTextGenerationUnsupported) {
		t.Fatalf("expected ErrTextGenerationUnsupported, got %v", err)
	}
}

func TestPoolAdapter_Judge_CircuitBreakerClassification(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	var reqCount atomic.Int32
	var statusToReturn atomic.Int32
	statusToReturn.Store(http.StatusBadRequest)
	var delayMs atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		if d := delayMs.Load(); d > 0 {
			select {
			case <-time.After(time.Duration(d) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(int(statusToReturn.Load()))
		_, _ = io.WriteString(w, `{"error": "simulated"}`)
	}))
	defer srv.Close()

	meta := &NodeMeta{
		NodeID:        "node-breaker-test",
		Name:          "Breaker Node",
		Endpoint:      srv.URL,
		Engine:        "decisions",
		BaseWeight:    100,
		MaxConcurrent: 4,
		Models: []*pb.NodeModelSpec{
			{
				ModelId:      "jepa-breaker",
				IsWarm:       true,
				Capabilities: []string{registry.CapabilityJudge},
			},
		},
	}
	if err := mgr.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: meta.NodeID, WarmModels: []string{"jepa-breaker"}}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	model := &proxydb.Model{
		ID:           "jepa-breaker",
		Vendor:       registry.VendorLocal,
		Capabilities: []string{registry.CapabilityJudge},
	}
	sel := NewSelector(mgr)
	adapter := NewPoolAdapter(sel, nil, model, 30*time.Second, nil)
	req := &llm.JudgeRequest{
		StateJSON: `{}`,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul}},
	}

	// 1. 400 Client error does not trip circuit breaker
	statusToReturn.Store(http.StatusBadRequest)
	for i := 0; i < 4; i++ {
		_, err := adapter.Judge(ctx, req, "u", nil)
		if err == nil {
			t.Fatalf("expected 400 error, got nil")
		}
	}
	if reqCount.Load() != 4 {
		t.Fatalf("expected 4 requests reached server, got %d", reqCount.Load())
	}
	nodes, err := mgr.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 || nodes[0].State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
		t.Fatalf("400 errors tripped circuit breaker unexpectedly")
	}

	// 2. Caller context deadline exceeded in flight does not trip circuit breaker
	delayMs.Store(200)
	preTimeoutCount := reqCount.Load()
	for i := 0; i < 4; i++ {
		timeoutCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		_, err := adapter.Judge(timeoutCtx, req, "u", nil)
		cancel()
		if err == nil {
			t.Fatalf("expected deadline exceeded error, got nil")
		}
	}
	if reqCount.Load() <= preTimeoutCount {
		t.Fatalf("expected requests to reach server during timeout test, count = %d", reqCount.Load())
	}
	nodes, err = mgr.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 || nodes[0].State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
		t.Fatalf("caller timeout tripped circuit breaker unexpectedly")
	}
	delayMs.Store(0)

	// 3. 404 maps to ErrJudgmentUnsupported and does NOT trip circuit breaker for the node
	statusToReturn.Store(http.StatusNotFound)
	for i := 0; i < 4; i++ {
		_, err := adapter.Judge(ctx, req, "u", nil)
		if !errors.Is(err, llm.ErrJudgmentUnsupported) {
			t.Fatalf("expected ErrJudgmentUnsupported on 404, got %v", err)
		}
	}
	nodes, err = mgr.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 || nodes[0].State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
		t.Fatalf("404 errors should not trip circuit breaker for the node")
	}

	// 4. 500 Server error trips circuit breaker (threshold = 3)
	statusToReturn.Store(http.StatusInternalServerError)
	for i := 0; i < 3; i++ {
		_, err := adapter.Judge(ctx, req, "u", nil)
		if err == nil {
			t.Fatalf("expected 500 error, got nil")
		}
	}
	nodes, err = mgr.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 || nodes[0].State != pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
		t.Fatalf("500 errors should have tripped circuit breaker, got state %v", nodes[0].State)
	}
}

func TestSelector_CapabilityFiltering(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	// Node 1: Judge only
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID:        "node-judge-only",
		Endpoint:      "http://127.0.0.1:8001",
		BaseWeight:    100,
		MaxConcurrent: 4,
		Models: []*pb.NodeModelSpec{
			{ModelId: "shared-model", Capabilities: []string{registry.CapabilityJudge}, IsWarm: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: "node-judge-only", WarmModels: []string{"shared-model"}}); err != nil {
		t.Fatal(err)
	}

	// Node 2: Chat generation
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID:        "node-chat-only",
		Endpoint:      "http://127.0.0.1:8002",
		BaseWeight:    100,
		MaxConcurrent: 4,
		Models: []*pb.NodeModelSpec{
			{ModelId: "shared-model", Capabilities: []string{"generate"}, IsWarm: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: "node-chat-only", WarmModels: []string{"shared-model"}}); err != nil {
		t.Fatal(err)
	}

	sel := NewSelector(mgr)

	// Selecting for "judge" capability must return node-judge-only
	candJudge, err := sel.SelectNodeForCapability(ctx, "shared-model", registry.CapabilityJudge)
	if err != nil {
		t.Fatalf("SelectNodeForCapability(judge): %v", err)
	}
	if candJudge.Snapshot.Meta.NodeID != "node-judge-only" {
		t.Errorf("got node %q, want node-judge-only", candJudge.Snapshot.Meta.NodeID)
	}

	// Selecting for "" (standard text generation) must return node-chat-only
	candChat, err := sel.SelectNode(ctx, "shared-model")
	if err != nil {
		t.Fatalf("SelectNode(text): %v", err)
	}
	if candChat.Snapshot.Meta.NodeID != "node-chat-only" {
		t.Errorf("got node %q, want node-chat-only", candChat.Snapshot.Meta.NodeID)
	}
}

func TestPoolAdapter_EvaluateRollouts_Success(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions/rollouts" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":             "jepa-node-v1",
			"best_candidate_id": "c1",
			"evaluations": []map[string]any{
				{
					"id":              "c1",
					"rank":            1,
					"composite_score": 0.95,
					"answers": []map[string]any{
						{"name": "q1", "type": "score", "score": 0.95},
					},
				},
			},
			"usage": map[string]any{"input_tokens": 120, "output_tokens": 0},
		})
	}))
	defer srv.Close()

	if err := mgr.Register(ctx, &NodeMeta{
		NodeID:        "node-rollout-1",
		Endpoint:      srv.URL,
		BaseWeight:    100,
		MaxConcurrent: 2,
		Models: []*pb.NodeModelSpec{
			{ModelId: "jepa-model", Capabilities: []string{registry.CapabilityJudge}, IsWarm: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: "node-rollout-1", WarmModels: []string{"jepa-model"}}); err != nil {
		t.Fatal(err)
	}

	sel := NewSelector(mgr)
	modelSpec := &proxydb.Model{
		ID:           "jepa-model",
		Capabilities: []string{registry.CapabilityJudge},
	}
	adapter := NewPoolAdapter(sel, nil, modelSpec, 5*time.Second, nil)

	req := &llm.EvaluateRolloutsRequest{
		StateJSON: `{"s":1}`,
		Candidates: []*llm.RolloutCandidate{
			{ID: "c1", ActionJSON: `{"a":1}`},
		},
		Criteria: map[string]*llm.JudgeQuestion{
			"q1": {Type: llm.JudgeScore, Instructions: "Rate", Levels: []string{"bad", "good"}},
		},
	}

	res, err := adapter.EvaluateRollouts(ctx, req, "user-123", nil)
	if err != nil {
		t.Fatalf("EvaluateRollouts: %v", err)
	}
	if res.BestCandidateID != "c1" || len(res.Evaluations) != 1 {
		t.Fatalf("unexpected evaluations: %+v", res)
	}
}

func TestPoolAdapter_EvaluateRollouts_404_YieldsUnsupported(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if err := mgr.Register(ctx, &NodeMeta{
		NodeID:        "node-rollout-2",
		Endpoint:      srv.URL,
		BaseWeight:    100,
		MaxConcurrent: 2,
		Models: []*pb.NodeModelSpec{
			{ModelId: "jepa-model", Capabilities: []string{registry.CapabilityJudge}, IsWarm: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: "node-rollout-2", WarmModels: []string{"jepa-model"}}); err != nil {
		t.Fatal(err)
	}

	sel := NewSelector(mgr)
	modelSpec := &proxydb.Model{
		ID:           "jepa-model",
		Capabilities: []string{registry.CapabilityJudge},
	}
	adapter := NewPoolAdapter(sel, nil, modelSpec, 5*time.Second, nil)

	req := &llm.EvaluateRolloutsRequest{
		StateJSON: `{"s":1}`,
		Candidates: []*llm.RolloutCandidate{
			{ID: "c1", ActionJSON: `{"a":1}`},
		},
		Criteria: map[string]*llm.JudgeQuestion{
			"q1": {Type: llm.JudgeScore, Instructions: "Rate", Levels: []string{"bad", "good"}},
		},
	}

	_, err := adapter.EvaluateRollouts(ctx, req, "user-123", nil)
	if !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Fatalf("err = %v, want ErrJudgmentUnsupported", err)
	}
	nodes, err := mgr.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 || nodes[0].State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
		t.Errorf("404 unexpectedly tripped node circuit breaker")
	}
}
