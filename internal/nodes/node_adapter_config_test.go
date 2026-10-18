package nodes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// captureNodeBody runs one completion through a PoolAdapter against a fake node
// and returns the JSON body the node received.
func captureNodeBody(t *testing.T, engine string, capabilities []string) map[string]json.RawMessage {
	t.Helper()
	ctx := context.Background()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"gemma","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	mgr, _ := setupTestManager(t)
	nodeID, modelID := "node-cfg", "local-gemma"
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID: nodeID, Name: "n", Endpoint: srv.URL, Engine: engine, BaseWeight: 10, MaxConcurrent: 2,
		Models: []*pb.NodeModelSpec{{ModelId: modelID, RemoteModelName: "gemma", IsWarm: true}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: nodeID}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	m := &proxydb.Model{ID: modelID, Vendor: registry.VendorLocal, Capabilities: capabilities}
	pa := NewPoolAdapter(NewSelector(mgr), nil, m, time.Minute, nil)
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	if _, err := pa.GenerateText(ctx, chat); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("node got non-JSON body %q: %v", body, err)
	}
	return got
}

func TestPoolAdapter_ThinkingOffFollowsNodeEngine(t *testing.T) {
	got := captureNodeBody(t, "ollama", []string{registry.CapabilityNoThinking})
	if string(got["reasoning_effort"]) != `"none"` {
		t.Fatalf("ollama node: reasoning_effort = %s, want \"none\"", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Fatal("ollama node must not get the DeepSeek thinking switch")
	}

	got = captureNodeBody(t, "vllm", []string{registry.CapabilityNoThinking})
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("vllm node got reasoning_effort: %s", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Fatal("vllm node got a thinking switch it does not understand")
	}
}

func TestPoolAdapter_NoCapabilityNoSwitch(t *testing.T) {
	got := captureNodeBody(t, "ollama", nil)
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("model without no_thinking got reasoning_effort: %s", got["reasoning_effort"])
	}
}
