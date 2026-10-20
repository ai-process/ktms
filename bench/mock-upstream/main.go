// Command mock-upstream is a deterministic OpenAI-compatible upstream for
// gateway benchmarks: fixed response bodies, configurable latency, SSE streaming.
//
// Timing is configured by env defaults and can be overridden per request by a
// directive inside any message, e.g. "[mock latency_ms=50 chunks=20 chunk_delay_ms=15]".
// The directive lives in the prompt so it survives any proxy, unlike headers.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type timing struct {
	Latency    time.Duration // before the response (or the first chunk)
	Chunks     int           // content chunks in a streamed response
	ChunkDelay time.Duration // between streamed content chunks
}

type chatRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type server struct {
	defaults timing
	requests atomic.Int64
	streams  atomic.Int64
}

var directiveRE = regexp.MustCompile(`\[mock((?:\s+[a-z_]+=\d+)*)\s*\]`)

var words = strings.Fields("The quick brown fox jumps over the lazy dog while the gateway relays every token")

func main() {
	s := &server{defaults: timing{
		Latency:    envMs("MOCK_LATENCY_MS", 0),
		Chunks:     envInt("MOCK_CHUNKS", 20),
		ChunkDelay: envMs("MOCK_CHUNK_DELAY_MS", 15),
	}}
	addr := ":" + envStr("PORT", "8000")
	srv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mock-upstream listening on %s (defaults %+v)", addr, s.defaults)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]int64{"requests": s.requests.Load(), "streams": s.streams.Load()})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "gpt-4o-mini", "object": "model", "created": 1700000000, "owned_by": "mock"},
		}})
	})
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	return mux
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid JSON body","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	s.requests.Add(1)
	t := s.timingFor(&req)
	model := req.Model
	if model == "" {
		model = "gpt-4o-mini"
	}
	if !sleep(r, t.Latency) {
		return
	}
	if req.Stream {
		s.streams.Add(1)
		stream(w, r, t, model, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}
	writeJSON(w, map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": strings.Join(words[:10], " ")},
			"finish_reason": "stop",
		}},
		"usage": usage(10),
	})
}

// stream writes the SSE headers together with the first chunk, so a client's
// time-to-first-byte equals time-to-first-token through any proxy.
func stream(w http.ResponseWriter, r *http.Request, t timing, model string, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	created := time.Now().Unix()
	chunk := func(delta map[string]string, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	for i := 0; i < t.Chunks; i++ {
		if i > 0 && !sleep(r, t.ChunkDelay) {
			return
		}
		delta := map[string]string{"content": " " + words[i%len(words)]}
		if i == 0 {
			delta["role"] = "assistant"
		}
		if !writeEvent(w, flusher, chunk(delta, nil)) {
			return
		}
	}
	if !writeEvent(w, flusher, chunk(map[string]string{}, "stop")) {
		return
	}
	if includeUsage {
		final := chunk(nil, nil)
		final["choices"] = []any{}
		final["usage"] = usage(t.Chunks)
		if !writeEvent(w, flusher, final) {
			return
		}
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return
	}
	flusher.Flush()
}

// timingFor applies the last directive found in the messages over the defaults.
func (s *server) timingFor(req *chatRequest) timing {
	t := s.defaults
	for _, m := range req.Messages {
		match := directiveRE.FindStringSubmatch(messageText(m.Content))
		if match == nil {
			continue
		}
		for _, kv := range strings.Fields(match[1]) {
			k, v, _ := strings.Cut(kv, "=")
			n, err := strconv.Atoi(v)
			if err != nil {
				continue
			}
			switch k {
			case "latency_ms":
				t.Latency = time.Duration(n) * time.Millisecond
			case "chunks":
				t.Chunks = max(n, 1)
			case "chunk_delay_ms":
				t.ChunkDelay = time.Duration(n) * time.Millisecond
			}
		}
	}
	return t
}

// messageText flattens string content and the text parts of array content.
func messageText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, " ")
}

func usage(completion int) map[string]int {
	return map[string]int{"prompt_tokens": 16, "completion_tokens": completion, "total_tokens": 16 + completion}
}

func writeEvent(w http.ResponseWriter, f http.Flusher, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal chunk: %v", err)
		return false
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return false // client went away
	}
	f.Flush()
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// sleep waits for d unless the client disconnects first.
func sleep(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < 0 {
		return def
	}
	return n
}

func envMs(key string, def int) time.Duration {
	return time.Duration(envInt(key, def)) * time.Millisecond
}
