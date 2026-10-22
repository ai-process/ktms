package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{defaults: timing{Chunks: 3}}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTimingDirective(t *testing.T) {
	s := &server{defaults: timing{Latency: time.Second, Chunks: 20, ChunkDelay: 15 * time.Millisecond}}
	cases := []struct {
		name, body string
		want       timing
	}{
		{"defaults", `{"messages":[{"content":"hi"}]}`, s.defaults},
		{"string content", `{"messages":[{"content":"ping [mock latency_ms=50 chunks=4]"}]}`,
			timing{Latency: 50 * time.Millisecond, Chunks: 4, ChunkDelay: 15 * time.Millisecond}},
		{"array content", `{"messages":[{"content":[{"type":"text","text":"[mock chunk_delay_ms=0 latency_ms=0]"}]}]}`,
			timing{Chunks: 20}},
		{"zero chunks clamps to one", `{"messages":[{"content":"[mock chunks=0]"}]}`,
			timing{Latency: time.Second, Chunks: 1, ChunkDelay: 15 * time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req chatRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			if got := s.timingFor(&req); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNonStreaming(t *testing.T) {
	s, ts := newTestServer(t)
	resp := post(t, ts.URL, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content == "" || body.Usage.TotalTokens == 0 {
		t.Fatalf("unexpected body %+v", body)
	}
	if s.requests.Load() != 1 || s.streams.Load() != 0 {
		t.Fatalf("counters requests=%d streams=%d", s.requests.Load(), s.streams.Load())
	}
}

func TestStreaming(t *testing.T) {
	s, ts := newTestServer(t)
	resp := post(t, ts.URL, `{"model":"m","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"[mock chunks=5 chunk_delay_ms=1]"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			events = append(events, data)
		}
	}
	// 5 content chunks, a finish chunk, a usage chunk, [DONE].
	if len(events) != 8 || events[7] != "[DONE]" {
		t.Fatalf("got %d events: %v", len(events), events)
	}
	if !strings.Contains(events[6], `"total_tokens":21`) {
		t.Fatalf("usage chunk %s", events[6])
	}
	if s.streams.Load() != 1 {
		t.Fatalf("streams=%d", s.streams.Load())
	}
}

func TestInvalidBody(t *testing.T) {
	s, ts := newTestServer(t)
	if resp := post(t, ts.URL, `{`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if s.requests.Load() != 0 {
		t.Fatal("invalid request must not be counted")
	}
}

func TestRequireKey(t *testing.T) {
	s := &server{defaults: timing{Chunks: 1}, requireKey: "vk"}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	body := `{"model":"m","messages":[{"content":"hi"}]}`
	if resp := post(t, ts.URL, body); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer vk")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("right key: status %d", resp.StatusCode)
	}
}
