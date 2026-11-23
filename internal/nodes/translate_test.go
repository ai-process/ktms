package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func registerTranslateNode(t *testing.T, mgr *Manager, endpoint, engine string, caps []string) {
	t.Helper()
	ctx := context.Background()
	meta := &NodeMeta{
		NodeID: "node-" + engine, Name: engine, Endpoint: endpoint, Engine: engine, AuthToken: "test-token",
		BaseWeight: 100, MaxConcurrent: 2,
		Models: []*pb.NodeModelSpec{{ModelId: "opus-mt-de", IsWarm: true, Capabilities: caps}},
	}
	if err := mgr.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: meta.NodeID, WarmModels: []string{"opus-mt-de"}}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}

func translateAdapter(mgr *Manager) *PoolAdapter {
	model := &proxydb.Model{ID: "opus-mt-de", Vendor: registry.VendorLocal, Capabilities: []string{registry.CapabilityTranslate}}
	return NewPoolAdapter(NewSelector(mgr), nil, model, 30*time.Second, nil)
}

func TestPoolAdapter_Translate_Native(t *testing.T) {
	mgr, _ := setupTestManager(t)
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/translate" || r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		_, _ = io.WriteString(w, `{"model":"opus-mt-en-de","translations":[{"text":"Gutes Hotel","detected_source_language":"en"}],"usage":{"input_tokens":4,"output_tokens":5,"characters":10}}`)
	}))
	defer srv.Close()
	registerTranslateNode(t, mgr, srv.URL, registry.EngineMT, []string{registry.CapabilityTranslate})

	meta := map[string]string{}
	res, err := translateAdapter(mgr).Translate(context.Background(), &llm.TranslateRequest{Texts: []string{"Good hotel"}, TargetLanguage: "de"}, "svc:x", meta)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Translations[0].Text != "Gutes Hotel" || res.Translations[0].DetectedSourceLanguage != "en" ||
		res.Usage.Characters != 10 || res.Usage.Output != 5 || res.Model != "opus-mt-de" || meta["node_id"] != "node-mt" {
		t.Errorf("result = %+v, meta %v", res, meta)
	}
	if !strings.Contains(body.Load().(string), `"target_language":"de"`) {
		t.Errorf("engine got %s", body.Load())
	}
}

func TestPoolAdapter_Translate_UnsupportedPairSparesBreaker(t *testing.T) {
	mgr, _ := setupTestManager(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"unsupported_language_pair","message":"no model for xx to de"}}`)
	}))
	defer srv.Close()
	registerTranslateNode(t, mgr, srv.URL, registry.EngineMT, []string{registry.CapabilityTranslate})
	a := translateAdapter(mgr)
	for i := 0; i < 6; i++ {
		_, err := a.Translate(context.Background(), &llm.TranslateRequest{Texts: []string{"x"}, SourceLanguage: "xx", TargetLanguage: "de"}, "svc:x", nil)
		if !errors.Is(err, llm.ErrUnsupportedLanguagePair) || !llm.IsRetryableError(err) {
			t.Fatalf("err = %v, want unsupported_language_pair that moves the chain on", err)
		}
	}
	if ok, _ := Serves(context.Background(), mgr, "opus-mt-de", registry.CapabilityTranslate); !ok {
		t.Error("an unsupported pair took the node out")
	}
}

func TestPoolAdapter_Translate_ChatNodeByPrompt(t *testing.T) {
	mgr, _ := setupTestManager(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		content, _ := json.Marshal(map[string]any{"translations": []any{map[string]any{"id": 0, "source_language": "en", "text": "Gutes Hotel"}}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion", "model": "gemma",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": string(content)}}},
			"usage":   map[string]any{"prompt_tokens": 50, "completion_tokens": 9},
		})
	}))
	defer srv.Close()
	registerTranslateNode(t, mgr, srv.URL, "ollama", nil)

	res, err := translateAdapter(mgr).Translate(context.Background(), &llm.TranslateRequest{Texts: []string{"Good hotel"}, TargetLanguage: "de"}, "svc:x", map[string]string{})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Translations[0].Text != "Gutes Hotel" || calls.Load() != 1 || res.Usage.Input != 50 {
		t.Errorf("result = %+v after %d chat calls", res, calls.Load())
	}
}
