package grpcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"

	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/throttle"
)

// fakeVendor is an httptest server speaking the OpenAI chat-completions
// shape — it covers the openai AND deepseek adapter paths.
type fakeVendor struct {
	mu           sync.Mutex
	calls        int
	failFirstN   int    // respond 500 to the first N calls
	finishReason string // default "stop"
	reply        string
	toolCalls    []map[string]any
}

func (f *fakeVendor) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call <= f.failFirstN {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
		return
	}
	bodyBytes, _ := io.ReadAll(r.Body)
	var body struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(bodyBytes, &body)

	if body.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"cmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"`+f.reply+`"},"finish_reason":null}]}`)
		if ok {
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"cmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
		if ok {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if ok {
			flusher.Flush()
		}
		return
	}

	finish := f.finishReason
	if finish == "" {
		if len(f.toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	msg := map[string]any{"role": "assistant", "content": f.reply}
	if len(f.toolCalls) > 0 {
		msg["tool_calls"] = f.toolCalls
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     "cmpl-1",
		"object": "chat.completion",
		"model":  "fake",
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	})
}

type mockStreamServer struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*pb.GenerateTextChunk
}

func (m *mockStreamServer) Context() context.Context {
	return m.ctx
}

func (m *mockStreamServer) Send(chunk *pb.GenerateTextChunk) error {
	m.chunks = append(m.chunks, chunk)
	return nil
}

func (f *fakeVendor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeTracker records every usage event the tracking adapter reports.
type fakeTracker struct {
	mu     sync.Mutex
	events []trackedEvent
}

type trackedEvent struct {
	userID, actionID, vendor, model string
	status                          usagev1.Status
	meta                            map[string]string
}

func (f *fakeTracker) RecordUsage(userID, actionID, vendor, model string,
	inputTokens, outputTokens int, durationMs int64, status usagev1.Status, meta map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, trackedEvent{userID, actionID, vendor, model, status, meta})
	return nil
}

func (f *fakeTracker) list() []trackedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]trackedEvent(nil), f.events...)
}

func testModel(id, vendor, endpoint string, efforts ...string) *proxydb.Model {
	return &proxydb.Model{ID: id, Vendor: vendor, Endpoint: endpoint, Efforts: efforts, Enabled: true}
}

type fixedSnapshot struct{ snap *registry.Snapshot }

func (f fixedSnapshot) Snapshot() *registry.Snapshot { return f.snap }

func testIdentity() context.Context {
	return apikeys.WithIdentity(context.Background(),
		&apikeys.Identity{KeyID: "key1", Name: "generation", Scopes: []string{apikeys.ScopeGenerate}})
}

func newTestServer(t *testing.T, models []*proxydb.Model, rules []*proxydb.Rule, tracker *fakeTracker) *ProxyServer {
	t.Helper()
	keys := map[string]map[string]string{
		"key1": {registry.VendorOpenAI: "sk-test", registry.VendorDeepSeek: "sk-deep"},
	}
	var tr *fakeTracker
	if tracker != nil {
		tr = tracker
	}
	var snap *registry.Snapshot
	if tr != nil {
		snap = registry.NewSnapshotForTest(models, rules, keys, tr)
	} else {
		snap = registry.NewSnapshotForTest(models, rules, keys, nil)
	}
	return NewProxyServer(fixedSnapshot{snap}, nil)
}

func catchAllRules(use ...string) []*proxydb.Rule {
	return []*proxydb.Rule{{
		Name: "any-default", Priority: 0, Effort: "",
		Attributes: map[string]string{}, Enabled: true, Use: use,
	}}
}

func TestGenerateTextHappyPath(t *testing.T) {
	vendor := &fakeVendor{reply: "bonjour"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	tracker := &fakeTracker{}
	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, srv.URL,
		registry.EffortLow, registry.EffortMedium, registry.EffortHigh)}
	s := newTestServer(t, models, catchAllRules("m1"), tracker)

	resp, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Effort:     pb.Effort_EFFORT_LOW,
		Attributes: map[string]string{"subject": "french", "user_id": "u-42"},
		Messages:   []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "say hi"}},
		ActionId:   "test.hi",
	})
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0] != "bonjour" {
		t.Fatalf("choices = %v", resp.Choices)
	}
	if resp.Usage.Input != 11 || resp.Usage.Output != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.ResolvedModel != "m1" || resp.ResolvedVendor != registry.VendorOpenAI || resp.MatchedRule != "any-default" {
		t.Fatalf("resolution = %s/%s rule=%s", resp.ResolvedModel, resp.ResolvedVendor, resp.MatchedRule)
	}

	events := tracker.list()
	if len(events) != 1 {
		t.Fatalf("tracked %d events, want 1", len(events))
	}
	e := events[0]
	if e.userID != "u-42" || e.actionID != "test.hi" || e.vendor != registry.VendorOpenAI || e.model != "m1" {
		t.Fatalf("event = %+v", e)
	}
	if e.meta["subject"] != "french" || e.meta["api_key"] != "generation" ||
		e.meta["rule"] != "any-default" || e.meta["effort"] != "low" {
		t.Fatalf("meta = %v", e.meta)
	}
}

func TestGenerateTextStreamHappyPath(t *testing.T) {
	vendor := &fakeVendor{reply: "bonjour stream"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	tracker := &fakeTracker{}
	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, srv.URL,
		registry.EffortLow, registry.EffortMedium, registry.EffortHigh)}
	s := newTestServer(t, models, catchAllRules("m1"), tracker)

	stream := &mockStreamServer{ctx: testIdentity()}
	err := s.GenerateTextStream(&pb.GenerateTextStreamRequest{
		Request: &pb.GenerateTextRequest{
			Effort:     pb.Effort_EFFORT_LOW,
			Attributes: map[string]string{"subject": "french", "user_id": "u-42"},
			Messages:   []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "say hi"}},
			ActionId:   "test.stream.hi",
		},
	}, stream)
	if err != nil {
		t.Fatalf("GenerateTextStream: %v", err)
	}
	if len(stream.chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(stream.chunks))
	}
	if stream.chunks[0].Delta != "bonjour stream" {
		t.Errorf("expected chunk delta 'bonjour stream', got %q", stream.chunks[0].Delta)
	}
	lastChunk := stream.chunks[len(stream.chunks)-1]
	if lastChunk.FinishReason != "stop" {
		t.Errorf("expected finish_reason stop, got %q", lastChunk.FinishReason)
	}
	if lastChunk.Usage == nil || lastChunk.Usage.Input != 11 || lastChunk.Usage.Output != 7 {
		t.Errorf("unexpected usage: %+v", lastChunk.Usage)
	}

	events := tracker.list()
	if len(events) != 1 {
		t.Fatalf("tracked %d events, want 1", len(events))
	}
	e := events[0]
	if e.userID != "u-42" || e.actionID != "test.stream.hi" || e.vendor != registry.VendorOpenAI || e.model != "m1" {
		t.Fatalf("event = %+v", e)
	}
}

func TestGenerateTextStreamChainFallback(t *testing.T) {
	broken := &fakeVendor{failFirstN: 1000}
	healthy := &fakeVendor{reply: "stream rescued"}
	srvBroken := httptest.NewServer(http.HandlerFunc(broken.handler))
	defer srvBroken.Close()
	srvHealthy := httptest.NewServer(http.HandlerFunc(healthy.handler))
	defer srvHealthy.Close()

	tracker := &fakeTracker{}
	models := []*proxydb.Model{
		testModel("first", registry.VendorDeepSeek, srvBroken.URL, registry.EffortHigh),
		testModel("second", registry.VendorOpenAI, srvHealthy.URL, registry.EffortHigh),
	}
	s := newTestServer(t, models, catchAllRules("first", "second"), tracker)

	stream := &mockStreamServer{ctx: testIdentity()}
	err := s.GenerateTextStream(&pb.GenerateTextStreamRequest{
		Request: &pb.GenerateTextRequest{
			Effort:   pb.Effort_EFFORT_HIGH,
			Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hello"}},
		},
	}, stream)
	if err != nil {
		t.Fatalf("GenerateTextStream: %v", err)
	}
	if len(stream.chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(stream.chunks))
	}
	if stream.chunks[0].ResolvedModel != "second" || stream.chunks[0].Delta != "stream rescued" {
		t.Errorf("expected fallback to second, got %+v", stream.chunks[0])
	}
}

func TestGenerateTextWithTools(t *testing.T) {
	vendor := &fakeVendor{
		toolCalls: []map[string]any{
			{
				"id":   "call_999",
				"type": "function",
				"function": map[string]any{
					"name":      "calc",
					"arguments": "{\"x\":1}",
				},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	tracker := &fakeTracker{}
	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, srv.URL, registry.EffortLow)}
	s := newTestServer(t, models, catchAllRules("m1"), tracker)

	resp, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Effort: pb.Effort_EFFORT_LOW,
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "calc 1+1"},
		},
		Tools: []*pb.Tool{
			{
				Type: "function",
				Function: &pb.FunctionDefinition{
					Name:        "calc",
					Description: "Calculate math",
				},
			},
		},
		ToolChoice: &pb.ToolChoice{
			Mode: pb.ToolChoice_MODE_AUTO,
		},
	})
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if len(resp.Details) != 1 {
		t.Fatalf("expected 1 detail choice, got %d", len(resp.Details))
	}
	detail := resp.Details[0]
	if len(detail.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(detail.ToolCalls))
	}
	if detail.ToolCalls[0].Id != "call_999" || detail.ToolCalls[0].Function.Name != "calc" {
		t.Errorf("unexpected tool call: %+v", detail.ToolCalls[0])
	}
	if detail.FinishReason != "tool_calls" {
		t.Errorf("expected finish reason tool_calls, got %s", detail.FinishReason)
	}
}

func TestGenerateTextChainFallback(t *testing.T) {
	broken := &fakeVendor{failFirstN: 1000}
	healthy := &fakeVendor{reply: "rescued"}
	srvBroken := httptest.NewServer(http.HandlerFunc(broken.handler))
	defer srvBroken.Close()
	srvHealthy := httptest.NewServer(http.HandlerFunc(healthy.handler))
	defer srvHealthy.Close()

	tracker := &fakeTracker{}
	models := []*proxydb.Model{
		testModel("first", registry.VendorDeepSeek, srvBroken.URL, registry.EffortHigh),
		testModel("second", registry.VendorOpenAI, srvHealthy.URL, registry.EffortHigh),
	}
	s := newTestServer(t, models, catchAllRules("first", "second"), tracker)

	resp, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Effort:   pb.Effort_EFFORT_HIGH,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if resp.ResolvedModel != "second" || resp.Choices[0] != "rescued" {
		t.Fatalf("resolved %s, choices %v", resp.ResolvedModel, resp.Choices)
	}
	if broken.count() == 0 {
		t.Fatal("first model was never tried")
	}
	// Every vendor call is a usage event, including the failed fallback tries.
	var failed, succeeded int
	for _, e := range tracker.list() {
		switch e.status {
		case usagev1.Status_STATUS_ERROR:
			failed++
		case usagev1.Status_STATUS_SUCCESS:
			succeeded++
		}
	}
	if failed == 0 || succeeded != 1 {
		t.Fatalf("failed=%d succeeded=%d", failed, succeeded)
	}
}

func TestGenerateTextTruncation(t *testing.T) {
	vendor := &fakeVendor{reply: "cut off", finishReason: "length"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, srv.URL, registry.EffortLow)}
	rules := []*proxydb.Rule{
		{Name: "low", Priority: 0, Effort: registry.EffortLow, Attributes: map[string]string{}, Enabled: true, Use: []string{"m1"}},
	}
	s := newTestServer(t, models, rules, nil)

	_, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Effort:          pb.Effort_EFFORT_LOW,
		Messages:        []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "long story"}},
		MaxOutputTokens: 100,
	})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "output_truncated" {
		t.Fatalf("err = %v, want FailedPrecondition output_truncated", err)
	}
}

func TestGenerateTextNoCapableModelForSearch(t *testing.T) {
	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, "http://unused", registry.EffortLow)}
	s := newTestServer(t, models, catchAllRules("m1"), nil)

	_, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Effort:             pb.Effort_EFFORT_LOW,
		Messages:           []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
		EnableGoogleSearch: true,
	})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "no_capable_model" {
		t.Fatalf("err = %v, want FailedPrecondition no_capable_model", err)
	}
}

func TestGenerateTextModelOverride(t *testing.T) {
	vendor := &fakeVendor{reply: "direct"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	models := []*proxydb.Model{
		testModel("routed", registry.VendorOpenAI, srv.URL, registry.EffortLow),
		testModel("special", registry.VendorDeepSeek, srv.URL, registry.EffortHigh),
	}
	s := newTestServer(t, models, catchAllRules("routed"), nil)

	resp, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		ModelOverride: "special",
		Messages:      []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
	})
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if resp.ResolvedModel != "special" || resp.MatchedRule != "" {
		t.Fatalf("resolved %s rule %q", resp.ResolvedModel, resp.MatchedRule)
	}

	_, err = s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		ModelOverride: "ghost",
		Messages:      []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown override: %v, want InvalidArgument", err)
	}
}

func TestGenerateTextValidation(t *testing.T) {
	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, "http://unused", registry.EffortLow)}
	s := newTestServer(t, models, catchAllRules("m1"), nil)

	// No effort, no override.
	_, err := s.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing effort: %v", err)
	}

	// No content at all.
	_, err = s.GenerateText(testIdentity(), &pb.GenerateTextRequest{Effort: pb.Effort_EFFORT_LOW})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty request: %v", err)
	}

	// Unconfigured snapshot.
	empty := NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(nil, nil, nil, nil)}, nil)
	_, err = empty.GenerateText(testIdentity(), &pb.GenerateTextRequest{
		Effort:   pb.Effort_EFFORT_LOW,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
	})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "not_configured" {
		t.Fatalf("unconfigured: %v", err)
	}
}

func TestSchemaRoundTrip(t *testing.T) {
	protoSchema := &pb.ResponseSchema{
		Type: "object",
		Properties: map[string]*pb.SchemaProperty{
			"words": {
				Type:  "array",
				Items: &pb.SchemaProperty{Type: "string", Description: "a word", Nullable: true},
			},
			"count": {Type: "integer"},
		},
		Required: []string{"words"},
	}
	got := schemaFromProto(protoSchema)
	if string(got.Type) != "object" || len(got.Properties) != 2 {
		t.Fatalf("schema = %+v", got)
	}
	words := got.Properties["words"]
	if string(words.Type) != "array" || words.Items == nil || !words.Items.Nullable {
		t.Fatalf("words = %+v", words)
	}
	if len(got.Required) != 1 || got.Required[0] != "words" {
		t.Fatalf("required = %v", got.Required)
	}
	if schemaFromProto(nil) != nil {
		t.Fatal("nil schema must stay nil")
	}
}

func TestEstimateTokens(t *testing.T) {
	req := &pb.GenerateTextRequest{
		BaseSystemInstruction: "abcd",                                // 4 runes
		Messages:              []*pb.ChatMessage{{Text: "abcdefgh"}}, // 8 runes
	}
	if got := estimateTokens(req); got != 4 { // 12/4 + 1
		t.Fatalf("estimate = %d, want 4", got)
	}
}

func TestGenerateTextAllThrottled(t *testing.T) {
	vendor := &fakeVendor{reply: "should not matter"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	m := testModel("m1", registry.VendorOpenAI, srv.URL, registry.EffortLow)
	m.RPM = 1
	keys := map[string]map[string]string{"key1": {registry.VendorOpenAI: "sk-test"}}
	snap := registry.NewSnapshotForTest([]*proxydb.Model{m}, catchAllRules("m1"), keys, nil)
	s := NewProxyServer(fixedSnapshot{snap}, throttle.New(client, "t"))

	req := &pb.GenerateTextRequest{
		Effort:   pb.Effort_EFFORT_LOW,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
	}
	if _, err := s.GenerateText(testIdentity(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := s.GenerateText(testIdentity(), req)
	if status.Code(err) != codes.ResourceExhausted || status.Convert(err).Message() != "throttled:rpm" {
		t.Fatalf("err = %v, want ResourceExhausted throttled:rpm", err)
	}
}

func TestUsageProject(t *testing.T) {
	// A caller fronting many tenants names the project; a multi-tenant caller needs
	// this or every tenant collapses into one project.
	if got := UsageProject(map[string]string{"api_key": "chatbot", "usage_project": "app-uuid-1"}); got != "app-uuid-1" {
		t.Errorf("usage_project should win, got %q", got)
	}
	if got := UsageProject(map[string]string{"api_key": "generation"}); got != "generation" {
		t.Errorf("api key is the fallback, got %q", got)
	}
	// An empty value must not blank out attribution.
	if got := UsageProject(map[string]string{"api_key": "generation", "usage_project": ""}); got != "generation" {
		t.Errorf("empty usage_project should fall back, got %q", got)
	}
	if got := UsageProject(nil); got != "" {
		t.Errorf("no meta means no project, got %q", got)
	}
}

func TestGenerateText_ServiceCallerBudgetCheck(t *testing.T) {
	vendor := &fakeVendor{reply: "test response"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	m := testModel("m1", registry.VendorOpenAI, srv.URL, registry.EffortLow)
	m.DailyTokensPerUser = 1
	keys := map[string]map[string]string{"key1": {registry.VendorOpenAI: "sk-test"}}
	snap := registry.NewSnapshotForTest([]*proxydb.Model{m}, catchAllRules("m1"), keys, nil)
	s := NewProxyServer(fixedSnapshot{snap}, throttle.New(client, "t"))

	req := &pb.GenerateTextRequest{
		Effort:   pb.Effort_EFFORT_LOW,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "12345678"}},
	}
	_, err := s.GenerateText(testIdentity(), req)
	if status.Code(err) != codes.ResourceExhausted || status.Convert(err).Message() != "throttled:budget_user" {
		t.Fatalf("err = %v, want ResourceExhausted throttled:budget_user", err)
	}
}
