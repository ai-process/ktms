package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// messagesCall sends the key the way the Anthropic SDKs do, in x-api-key.
func messagesCall(h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func anthropicErr(t *testing.T, rec *httptest.ResponseRecorder) (typ, msg string) {
	t.Helper()
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Type != "error" {
		t.Fatalf("not an Anthropic error body: %s", rec.Body)
	}
	return out.Error.Type, out.Error.Message
}

func TestMessagesMapsRequest(t *testing.T) {
	fp, h, key := newTestAPI(t)
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
	rec := messagesCall(h, "/v1/messages?beta=true", key, `{
	  "model":"auto:high","max_tokens":32000,
	  "system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}],
	  "metadata":{"user_id":"u-42"},
	  "temperature":1,"thinking":{"type":"enabled","budget_tokens":1024},
	  "tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],
	  "tool_choice":{"type":"any"},
	  "messages":[
	    {"role":"user","content":"read main.go"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"hm","signature":"sig"},
	      {"type":"text","text":"Reading."},
	      {"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"main.go"}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"package main"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+png+`"}}]},
	      {"type":"text","text":"<system-reminder>x</system-reminder>"},
	      {"type":"text","text":"explain it"}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	r := fp.text
	if r.Effort != pb.Effort_EFFORT_HIGH || !r.AutoRoute || r.MaxOutputTokens != 32000 {
		t.Errorf("routing: %+v", r)
	}
	if r.BaseSystemInstruction != "You are Claude Code.\n\nBe brief." || r.Attributes["user_id"] != "u-42" {
		t.Errorf("system %q attrs %v", r.BaseSystemInstruction, r.Attributes)
	}
	if len(r.Tools) != 1 || r.Tools[0].Function.Name != "Read" || r.Tools[0].Function.Parameters.Properties["path"].Type != "string" ||
		r.ToolChoice.GetMode() != pb.ToolChoice_MODE_REQUIRED {
		t.Errorf("tools %v choice %v", r.Tools, r.ToolChoice)
	}
	if len(r.Messages) != 4 {
		t.Fatalf("messages: %v", r.Messages)
	}
	asst, tool, user := r.Messages[1], r.Messages[2], r.Messages[3]
	if asst.Text != "Reading." || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Id != "toolu_1" ||
		asst.ToolCalls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("assistant: %v", asst)
	}
	if tool.Role != pb.MessageRole_MESSAGE_ROLE_TOOL || tool.ToolCallId != "toolu_1" || tool.Text != "package main" {
		t.Errorf("tool result: %v", tool)
	}
	// The tool result's image rides in the user message after it.
	if user.Role != pb.MessageRole_MESSAGE_ROLE_USER || user.Text != "<system-reminder>x</system-reminder>\n\nexplain it" ||
		len(user.Parts) != 3 || user.Parts[0].GetImage().GetMimeType() != "image/png" {
		t.Errorf("user: %v", user)
	}
}

func TestMessagesResponse(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.respDetails = []*pb.Choice{{
		Text: "Let me look.",
		ToolCalls: []*pb.ToolCall{
			{Id: "call_9", Function: &pb.FunctionCall{Name: "Read", Arguments: `{"path":"a.go"}`}},
			{Function: &pb.FunctionCall{Name: "Glob", Arguments: ""}},
		},
		FinishReason: "tool_calls",
	}}
	rec := messagesCall(h, "/v1/messages", key, `{"model":"m1","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "msg_") || out.Type != "message" || out.Role != "assistant" || out.Model != "m1" ||
		out.StopReason != "tool_use" || len(out.Content) != 3 {
		t.Fatalf("message: %s", rec.Body)
	}
	if out.Content[0].Text != "Let me look." || out.Content[1].ID != "call_9" || string(out.Content[1].Input) != `{"path":"a.go"}` ||
		!strings.HasPrefix(out.Content[2].ID, "toolu_") || string(out.Content[2].Input) != "{}" {
		t.Errorf("content: %s", rec.Body)
	}
	if out.Usage["input_tokens"] != 3 || out.Usage["output_tokens"] != 2 {
		t.Errorf("usage: %v", out.Usage)
	}
	if rec.Header().Get("X-KTMS-Vendor") != "openai" || rec.Header().Get("X-KTMS-Rule") != "default" {
		t.Errorf("headers: %v", rec.Header())
	}
}

func TestMessagesUsageSplitsCache(t *testing.T) {
	u := anthropicUsage(&pb.TokensUsage{Input: 100, Output: 7, CacheRead: 60, CacheWrite: 30}, 0)
	if u["input_tokens"] != int64(10) || u["cache_read_input_tokens"] != int64(60) || u["cache_creation_input_tokens"] != int64(30) {
		t.Errorf("usage %v", u)
	}
	if u := anthropicUsage(nil, 42); u["input_tokens"] != int64(42) || u["output_tokens"] != int64(0) {
		t.Errorf("estimate %v", u)
	}
}

// sseEvents returns the event names of a stream, in order.
func sseEvents(body string) []string {
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, name)
		}
	}
	return names
}

func TestMessagesStream(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{
		{Delta: "Checking", ResolvedModel: "m1", ResolvedVendor: "openai", MatchedRule: "default"},
		{Delta: " now."},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Id: "call_a", Name: "Read", ArgumentsDelta: `{"path":`}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, ArgumentsDelta: `"a.go"}`}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, Name: "Glob", ArgumentsDelta: `{}`}}},
		{FinishReason: "tool_calls", Usage: &pb.TokensUsage{Input: 50, Output: 9, CacheRead: 40}},
	}
	rec := messagesCall(h, "/v1/messages", key, `{"model":"auto","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" || rec.Header().Get("X-KTMS-Vendor") != "openai" {
		t.Fatalf("headers %v: %s", rec.Header(), rec.Body)
	}
	b := rec.Body.String()
	want := []string{"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop"}
	if got := sseEvents(b); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v\n%s", got, b)
	}
	for _, s := range []string{
		`"model":"m1"`,
		`{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`,
		`"delta":{"text":" now.","type":"text_delta"}`,
		`"content_block":{"id":"call_a","input":{},"name":"Read","type":"tool_use"},"index":1`,
		`"delta":{"partial_json":"\"a.go\"}","type":"input_json_delta"},"index":1`,
		`"name":"Glob"`,
		`"stop_reason":"tool_use"`,
		`"cache_read_input_tokens":40,"input_tokens":10,"output_tokens":9`,
	} {
		if !strings.Contains(b, s) {
			t.Errorf("stream missing %s:\n%s", s, b)
		}
	}
}

func TestMessagesStreamToolOutOfOrder(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Id: "a", Name: "Read"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, Id: "b", Name: "Glob"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, ArgumentsDelta: "{}"}}},
	}
	rec := messagesCall(h, "/v1/messages", key, `{"max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if ev := sseEvents(rec.Body.String()); ev[len(ev)-1] != "error" {
		t.Errorf("events %v", ev)
	}
}

func TestMessagesStreamErrors(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.textErr = status.Error(codes.ResourceExhausted, "throttled:rpm")
	rec := messagesCall(h, "/v1/messages", key, `{"max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if typ, _ := anthropicErr(t, rec); rec.Code != 429 || typ != "rate_limit_error" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("before the stream: %d %s %v", rec.Code, typ, rec.Header())
	}

	fp.textErr = nil
	fp.emptyStream = true
	rec = messagesCall(h, "/v1/messages", key, `{"max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if got := strings.Join(sseEvents(rec.Body.String()), ","); got != "message_start,ping,message_delta,message_stop" {
		t.Errorf("no chunks: %s", got)
	}
}

func TestMessagesErrors(t *testing.T) {
	fp, h, key := newTestAPI(t)
	cases := []struct {
		key, body, typ string
		code           int
	}{
		{"", `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, "authentication_error", 401},
		{key, `{"messages":[{"role":"user","content":"x"}]}`, "invalid_request_error", 400},
		{key, `{"max_tokens":1,"messages":[]}`, "invalid_request_error", 400},
		{key, `{"max_tokens":1,"messages":[{"role":"system","content":"x"}]}`, "invalid_request_error", 400},
		{key, `{"max_tokens":1,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"x"}]}`, "invalid_request_error", 400},
		{key, `{"max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"http://169.254.169.254/x.png"}}]}]}`, "invalid_request_error", 400},
		{key, `{"max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"f"}}]}]}`, "invalid_request_error", 400},
	}
	for _, c := range cases {
		rec := messagesCall(h, "/v1/messages", c.key, c.body)
		if typ, msg := anthropicErr(t, rec); rec.Code != c.code || typ != c.typ {
			t.Errorf("%s: %d %s %q", c.body, rec.Code, typ, msg)
		}
	}

	// Authorization works as well as x-api-key, as ANTHROPIC_AUTH_TOKEN sends it.
	if rec := do(h, "POST", "/v1/messages", key, `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`); rec.Code != 200 {
		t.Errorf("bearer: %d %s", rec.Code, rec.Body)
	}
	// x-api-key is for the Messages API only.
	if rec := messagesCall(h, "/v1/chat/completions", key, `{"messages":[{"role":"user","content":"x"}]}`); rec.Code != 401 {
		t.Errorf("x-api-key on chat completions: %d", rec.Code)
	}

	fp.textErr = status.Error(codes.Unavailable, "vendors_unavailable")
	rec := messagesCall(h, "/v1/messages", key, `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	if typ, msg := anthropicErr(t, rec); rec.Code != 503 || typ != "overloaded_error" || msg != "vendors_unavailable" {
		t.Errorf("unavailable: %d %s %s", rec.Code, typ, msg)
	}
}

func TestClaudeCodeUser(t *testing.T) {
	for _, c := range []struct{ in, user, session string }{
		{`{"device_id":"dev1","account_uuid":"","session_id":"s1"}`, "dev1", "s1"},
		{`{"device_id":"dev1","account_uuid":"acc1","session_id":"s2"}`, "acc1", "s2"},
		{"user_abc_account_acc1_session_s3", "user_abc_account_acc1", "s3"},
		{"u-42", "", ""},
		{"{not json", "", ""},
	} {
		if u, s := claudeCodeUser(c.in); u != c.user || s != c.session {
			t.Errorf("%s: %q %q", c.in, u, s)
		}
	}
	fp, h, key := newTestAPI(t)
	messagesCall(h, "/v1/messages", key, `{"max_tokens":1,"metadata":{"user_id":"{\"device_id\":\"dev1\",\"account_uuid\":\"\",\"session_id\":\"s1\"}"},"messages":[{"role":"user","content":"x"}]}`)
	if fp.text.Attributes["user_id"] != "dev1" || fp.text.Attributes["session_id"] != "s1" {
		t.Errorf("attributes %v", fp.text.Attributes)
	}
}

func TestMessagesStreamToolDeltas(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Id: "a", Name: "Re"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Name: "ad"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, ArgumentsDelta: `{"path":"a.go"}`}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, Id: "b", Name: "Glob", ArgumentsDelta: "null"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 2, Id: "c", Name: "Ls"}}},
		{FinishReason: "tool_calls"},
	}
	rec := messagesCall(h, "/v1/messages", key, `{"max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	b := rec.Body.String()
	for _, s := range []string{
		`"content_block":{"id":"a","input":{},"name":"Read","type":"tool_use"},"index":0`,
		`"content_block":{"id":"b","input":{},"name":"Glob","type":"tool_use"},"index":1`,
		`"content_block":{"id":"c","input":{},"name":"Ls","type":"tool_use"},"index":2`,
		`{"index":2,"type":"content_block_stop"}`,
	} {
		if !strings.Contains(b, s) {
			t.Errorf("stream missing %s:\n%s", s, b)
		}
	}
	if strings.Contains(b, `"partial_json":"null"`) {
		t.Errorf("null arguments were streamed:\n%s", b)
	}
}

func TestMessagesStreamErrorEvent(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{{Delta: "part"}}
	fp.streamErr = status.Error(codes.Unavailable, "vendor_error")
	rec := messagesCall(h, "/v1/messages", key, `{"max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	b := rec.Body.String()
	if ev := sseEvents(b); ev[len(ev)-1] != "error" ||
		!strings.Contains(b, `"error":{"message":"vendor_error","type":"api_error"}`) {
		t.Errorf("stream error, as the non-stream 502:\n%s", b)
	}
}

func TestCountTokens(t *testing.T) {
	_, h, key := newTestAPI(t)
	rec := messagesCall(h, "/v1/messages/count_tokens", key, `{"model":"auto",
	  "system":"`+strings.Repeat("s", 400)+`",
	  "tools":[{"name":"Read","input_schema":{"type":"object"}}],
	  "messages":[{"role":"user","content":[{"type":"text","text":"`+strings.Repeat("u", 400)+`"},
	    {"type":"image","source":{"type":"url","url":"https://127.0.0.1:1/never-fetched.png"}}]}]}`)
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// 800 runes, the tool, and the image's 1000.
	if out.InputTokens < 1200 || out.InputTokens > 1220 || rec.Header().Get("X-KTMS-Token-Count") != "estimate" {
		t.Errorf("count %d headers %v", out.InputTokens, rec.Header())
	}
}
