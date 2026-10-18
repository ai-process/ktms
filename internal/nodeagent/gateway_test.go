package nodeagent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/llm/adapters"
)

const completionJSON = `{"id":"c1","object":"chat.completion","created":1,"model":"gemma","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func newTestGateway(t *testing.T, engine http.Handler) (*Gateway, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		engine.ServeHTTP(w, r)
	}))
	t.Cleanup(eng.Close)
	gw := NewGateway(eng.URL, "access-key", "engine-secret")
	gw.keepaliveAfter, gw.keepaliveEvery = 40*time.Millisecond, 20*time.Millisecond
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return gw, srv, &hits
}

func sdkClient(url, key string) openai.Client {
	return openai.NewClient(option.WithBaseURL(url+"/v1"), option.WithAPIKey(key), option.WithMaxRetries(0))
}

func chatParams() openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model:    "gemma",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("go")},
	}
}

func TestGatewayRejectsBadKeyWithoutTouchingEngine(t *testing.T) {
	_, srv, hits := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, auth := range []string{"", "Bearer wrong", "access-key", "Basic access-key"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("engine was reached %d times without a valid key", hits.Load())
	}
}

func TestGatewayAllowlistAndKeySwap(t *testing.T) {
	var gotAuth atomic.Value
	_, srv, hits := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completionJSON)
	}))

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/tags", 404},
		{"POST", "/api/pull", 404},
		{"DELETE", "/api/delete", 404},
		{"GET", "/v1/chat/completions", 405},
		{"POST", "/v1/models", 405},
		{"GET", "/v1/decisions", 405},
	} {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer access-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked routes reached the engine %d times", hits.Load())
	}

	cl := sdkClient(srv.URL, "access-key")
	if _, err := cl.Chat.Completions.New(context.Background(), chatParams()); err != nil {
		t.Fatalf("allowed call failed: %v", err)
	}
	if got := gotAuth.Load(); got != "Bearer engine-secret" {
		t.Fatalf("engine saw Authorization %q; the access key must be swapped for the engine token", got)
	}
}

func TestGatewayStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	_, srv, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"g","choices":[{"index":0,"delta":{"content":"he"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"g","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	cl := sdkClient(srv.URL, "access-key")
	st := cl.Chat.Completions.NewStreaming(context.Background(), chatParams())
	defer st.Close()

	if !st.Next() {
		t.Fatalf("no first chunk: %v", st.Err())
	}
	if got := st.Current().Choices[0].Delta.Content; got != "he" {
		t.Fatalf("first chunk %q, want it before the engine finished", got)
	}
	close(release)
	var rest string
	for st.Next() {
		rest += st.Current().Choices[0].Delta.Content
	}
	if st.Err() != nil || rest != "llo" {
		t.Fatalf("rest = %q, err = %v", rest, st.Err())
	}
}

func TestGatewayKeepaliveSlowNonStream(t *testing.T) {
	_, srv, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // several keepalive periods
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, completionJSON)
	}))
	// Raw read: the first byte must arrive long before the engine answers.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"g"}`))
	req.Header.Set("Authorization", "Bearer access-key")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("response headers took %v; keepalive should commit them early", d)
	}

	cl := sdkClient(srv.URL, "access-key")
	got, err := cl.Chat.Completions.New(context.Background(), chatParams())
	if err != nil {
		t.Fatalf("SDK rejected keepalive-padded completion: %v", err)
	}
	if got.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
}

func TestGatewayKeepaliveSlowStream(t *testing.T) {
	_, srv, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"g","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	cl := sdkClient(srv.URL, "access-key")
	st := cl.Chat.Completions.NewStreaming(context.Background(), chatParams())
	defer st.Close()
	var text string
	for st.Next() {
		if c := st.Current().Choices; len(c) > 0 { // keepalive chunks carry none
			text += c[0].Delta.Content
		}
	}
	if st.Err() != nil || text != "ok" {
		t.Fatalf("text = %q, err = %v (keepalive chunks must be ignored by the client)", text, st.Err())
	}
}

func TestGatewayLateEngineErrorIsStillAnError(t *testing.T) {
	_, srv, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"model crashed"}`)
	}))
	cl := sdkClient(srv.URL, "access-key")

	// The status is already 200, so the SDK sees an empty completion; the
	// adapter the proxy uses turns that into an error.
	ad := adapters.NewOpenAIAdapter(&cl, "gemma", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	if _, err := ad.GenerateText(context.Background(), chat); err == nil {
		t.Fatal("non-stream: late engine failure was reported as success")
	}
	st := cl.Chat.Completions.NewStreaming(context.Background(), chatParams())
	defer st.Close()
	for st.Next() {
	}
	if st.Err() == nil {
		t.Fatal("stream: late engine failure was reported as a clean end")
	}
}

func TestGatewayFastEngineErrorKeepsStatus(t *testing.T) {
	_, srv, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer access-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want the engine's 404 so the proxy sees the real class of error", resp.StatusCode)
	}
}

func TestGatewayHealthzNeedsNoKey(t *testing.T) {
	_, srv, hits := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || hits.Load() != 0 {
		t.Fatalf("healthz status %d, engine hits %d", resp.StatusCode, hits.Load())
	}
}

func TestGatewayAllowsDecisions(t *testing.T) {
	var gotAuth atomic.Value
	_, srv, hits := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jepa","answers":[]}`)
	}))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/decisions", strings.NewReader(`{"model":"jepa","questions":[]}`))
	req.Header.Set("Authorization", "Bearer access-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/decisions status %d, want 200", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("engine was reached %d times, want 1", hits.Load())
	}
	if gotAuth.Load() != "Bearer engine-secret" {
		t.Fatalf("engine saw %q, want Bearer engine-secret", gotAuth.Load())
	}
}
