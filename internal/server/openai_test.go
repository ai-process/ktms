package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

type fakeProxy struct {
	pb.UnimplementedLLMProxyServiceServer
	text         *pb.GenerateTextRequest
	textErr      error
	respDetails  []*pb.Choice
	streamChunks []*pb.GenerateTextChunk
	media        map[string]string
}

func (f *fakeProxy) GenerateText(_ context.Context, r *pb.GenerateTextRequest) (*pb.GenerateTextResponse, error) {
	f.text = r
	if f.textErr != nil {
		return nil, f.textErr
	}
	resp := &pb.GenerateTextResponse{
		Choices: []string{"hello"}, Usage: &pb.TokensUsage{Input: 3, Output: 2},
		ResolvedModel: "m1", ResolvedVendor: "openai", MatchedRule: "default",
	}
	if len(f.respDetails) > 0 {
		resp.Details = f.respDetails
	}
	return resp, nil
}

func (f *fakeProxy) GenerateTextStream(r *pb.GenerateTextStreamRequest, s pb.LLMProxyService_GenerateTextStreamServer) error {
	f.text = r.Request
	if f.textErr != nil {
		return f.textErr
	}
	if len(f.streamChunks) > 0 {
		for _, chunk := range f.streamChunks {
			if err := s.Send(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.Send(&pb.GenerateTextChunk{
		Delta:          "hello",
		ResolvedModel:  "m1",
		ResolvedVendor: "openai",
		MatchedRule:    "default",
	}); err != nil {
		return err
	}
	return s.Send(&pb.GenerateTextChunk{
		FinishReason:  "stop",
		Usage:         &pb.TokensUsage{Input: 3, Output: 2},
		ResolvedModel: "m1",
	})
}

func (f *fakeProxy) ListModels(context.Context, *pb.ListModelsRequest) (*pb.ListModelsResponse, error) {
	return &pb.ListModelsResponse{Models: []*pb.ModelInfo{
		{Id: "m1", Vendor: "openai", Efforts: []pb.Effort{pb.Effort_EFFORT_LOW}, Capabilities: []string{"tts"}},
	}}, nil
}

func (f *fakeProxy) SynthesizeSpeech(_ context.Context, r *pb.SynthesizeSpeechRequest) (*pb.SynthesizeSpeechResponse, error) {
	f.media = map[string]string{"text": r.Text, "language": r.Language, "override": r.ModelOverride}
	return &pb.SynthesizeSpeechResponse{Audio: []byte("OggS"), MimeType: "audio/ogg", ResolvedModel: "tts1"}, nil
}

func (f *fakeProxy) GenerateImage(_ context.Context, r *pb.GenerateImageRequest) (*pb.GenerateImageResponse, error) {
	f.media = map[string]string{"prompt": r.Prompt}
	return &pb.GenerateImageResponse{Image: []byte("png"), MimeType: "image/png"}, nil
}

func (f *fakeProxy) Judge(_ context.Context, r *pb.JudgeRequest) (*pb.JudgeResponse, error) {
	return &pb.JudgeResponse{Answers: map[string]*pb.JudgeAnswer{
		"q": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Noul: 0.9},
	}, ResolvedModel: "jev"}, nil
}

func newTestAPI(t *testing.T) (*fakeProxy, http.Handler, string) {
	t.Helper()
	store := &fakeStore{rows: map[string]*proxydb.APIKey{}}
	key := mintKey(t, store, "client", apikeys.ScopeGenerate)
	adminOnly := mintKey(t, store, "adm", apikeys.ScopeAdmin)
	_ = adminOnly
	fp := &fakeProxy{}
	mux := http.NewServeMux()
	NewOpenAIAPI(fp, apikeys.NewVerifier(store, "")).Register(mux)
	return fp, mux, key
}

func do(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Error == nil {
		t.Fatalf("not an OpenAI error body: %s", rec.Body.String())
	}
	return out.Error
}

func TestRequiresKey(t *testing.T) {
	_, h, key := newTestAPI(t)
	if rec := do(h, "POST", "/v1/chat/completions", "", `{}`); rec.Code != 401 {
		t.Fatalf("missing key = %d", rec.Code)
	}
	if rec := do(h, "POST", "/v1/chat/completions", "llm_nope", `{}`); rec.Code != 401 {
		t.Fatalf("bad key = %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/models", key, ``); rec.Code != 200 {
		t.Fatalf("good key = %d", rec.Code)
	}
}

func TestChatMapsRequestAndResponse(t *testing.T) {
	fp, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/chat/completions", key, `{
	  "model":"auto:high",
	  "messages":[
	    {"role":"system","content":"be brief"},
	    {"role":"developer","content":[{"type":"text","text":"in French"}]},
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":"yo"},
	    {"role":"user","content":"again"}],
	  "max_completion_tokens":50,"user":"u1",
	  "metadata":{"subject":"fr","action_id":"greet"},
	  "temperature":0.2}`)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	r := fp.text
	if r.Effort != pb.Effort_EFFORT_HIGH || r.ModelOverride != "" {
		t.Errorf("routing: effort=%v override=%q", r.Effort, r.ModelOverride)
	}
	if r.BaseSystemInstruction != "be brief\n\nin French" || len(r.Messages) != 3 {
		t.Errorf("messages: %q %d", r.BaseSystemInstruction, len(r.Messages))
	}
	if r.MaxOutputTokens != 50 || r.ActionId != "greet" ||
		r.Attributes["user_id"] != "u1" || r.Attributes["subject"] != "fr" || r.Attributes["action_id"] != "" {
		t.Errorf("attrs: %+v action=%q max=%d", r.Attributes, r.ActionId, r.MaxOutputTokens)
	}

	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message      map[string]string `json:"message"`
			FinishReason string            `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Model != "m1" || out.Choices[0].Message["content"] != "hello" ||
		out.Choices[0].FinishReason != "stop" || out.Usage["total_tokens"] != 5 {
		t.Errorf("response: %s", rec.Body)
	}
	if rec.Header().Get("X-LLM-Proxy-Vendor") != "openai" || rec.Header().Get("X-LLM-Proxy-Rule") != "default" {
		t.Errorf("headers: %v", rec.Header())
	}
}

func TestChatModelIsOverride(t *testing.T) {
	fp, h, key := newTestAPI(t)
	do(h, "POST", "/v1/chat/completions", key,
		`{"model":"gemini-x","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	if fp.text.ModelOverride != "gemini-x" || fp.text.Effort != pb.Effort_EFFORT_LOW {
		t.Errorf("got %+v", fp.text)
	}
}

func TestChatJSONSchema(t *testing.T) {
	fp, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/chat/completions", key, `{"model":"auto","messages":[{"role":"user","content":"x"}],
	 "response_format":{"type":"json_schema","json_schema":{"name":"n","strict":true,"schema":{
	   "type":"object","required":["tags"],"additionalProperties":false,
	   "properties":{"tags":{"type":"array","items":{"type":"string"}},
	                 "note":{"type":["string","null"],"description":"d"}}}}}}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s := fp.text.ResponseSchema
	if s.Type != "object" || s.Required[0] != "tags" || s.Properties["tags"].Items.Type != "string" ||
		s.Properties["note"].Type != "string" || !s.Properties["note"].Nullable || s.Properties["note"].Description != "d" {
		t.Errorf("schema: %v", s)
	}
}

func TestChatRejectsUnsupported(t *testing.T) {
	_, h, key := newTestAPI(t)
	cases := map[string]string{
		"n":          `{"messages":[{"role":"user","content":"x"}],"n":2}`,
		"image part": `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
		"no msgs":    `{"messages":[]}`,
		"bad json":   `{`,
		"bad effort": `{"model":"auto:max","messages":[{"role":"user","content":"x"}]}`,
		"functions":  `{"messages":[{"role":"user","content":"x"}],"functions":[{"name":"f"}]}`,
	}
	for name, body := range cases {
		rec := do(h, "POST", "/v1/chat/completions", key, body)
		if rec.Code != 400 || errBody(t, rec)["type"] != "invalid_request_error" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestChatToolsAndToolRoles(t *testing.T) {
	fp, h, key := newTestAPI(t)
	body := `{
		"model": "m1",
		"messages": [
			{"role": "user", "content": "What is the weather?"},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Tokyo\"}"}}]},
			{"role": "tool", "content": "{\"temp\": 22}", "tool_call_id": "call_1"}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "get_weather",
					"description": "Get current weather",
					"parameters": {
						"type": "object",
						"properties": {
							"city": {"type": "string"}
						},
						"required": ["city"]
					}
				}
			}
		],
		"tool_choice": "auto"
	}`

	fp.respDetails = []*pb.Choice{
		{
			ToolCalls: []*pb.ToolCall{
				{
					Id:   "call_2",
					Type: "function",
					Function: &pb.FunctionCall{
						Name:      "get_weather",
						Arguments: "{\"city\":\"Osaka\"}",
					},
				},
			},
			FinishReason: "tool_calls",
		},
	}

	rec := do(h, "POST", "/v1/chat/completions", key, body)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(fp.text.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(fp.text.Messages))
	}
	if fp.text.Messages[2].Role != pb.MessageRole_MESSAGE_ROLE_TOOL {
		t.Errorf("expected MESSAGE_ROLE_TOOL, got %v", fp.text.Messages[2].Role)
	}
	if fp.text.Messages[2].ToolCallId != "call_1" {
		t.Errorf("expected tool_call_id call_1, got %v", fp.text.Messages[2].ToolCallId)
	}
	if len(fp.text.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(fp.text.Tools))
	}
	if fp.text.Tools[0].Function.Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", fp.text.Tools[0].Function.Name)
	}
	if fp.text.ToolChoice == nil || fp.text.ToolChoice.Mode != pb.ToolChoice_MODE_AUTO {
		t.Errorf("expected tool choice mode auto, got %v", fp.text.ToolChoice)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("unexpected response choices: %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_2" || tc.Function.Name != "get_weather" || tc.Function.Arguments != "{\"city\":\"Osaka\"}" {
		t.Errorf("unexpected tool call in resp: %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("unexpected finish_reason: %s", resp.Choices[0].FinishReason)
	}
}

func TestErrorMapping(t *testing.T) {
	fp, h, key := newTestAPI(t)
	body := `{"messages":[{"role":"user","content":"x"}]}`
	cases := []struct {
		err  error
		code int
		want string
	}{
		{status.Error(codes.ResourceExhausted, "throttled:rpm"), 429, "throttled:rpm"},
		{status.Error(codes.Unavailable, "vendors_unavailable"), 503, "vendors_unavailable"},
		{status.Error(codes.FailedPrecondition, "output_truncated"), 400, "output_truncated"},
		{status.Error(codes.Internal, "vendor_error"), 502, "vendor_error"},
		{status.Error(codes.Internal, "pq: password for user x"), 500, ""},
	}
	for _, c := range cases {
		fp.textErr = c.err
		rec := do(h, "POST", "/v1/chat/completions", key, body)
		e := errBody(t, rec)
		got, _ := e["code"].(string)
		if rec.Code != c.code || got != c.want {
			t.Errorf("%v: got %d code=%q", c.err, rec.Code, got)
		}
		if strings.Contains(e["message"].(string), "password") {
			t.Errorf("internal detail leaked: %v", e["message"])
		}
	}
}

func TestChatStream(t *testing.T) {
	_, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/chat/completions", key,
		`{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	if rec.Header().Get("X-LLM-Proxy-Vendor") != "openai" || rec.Header().Get("X-LLM-Proxy-Rule") != "default" {
		t.Errorf("headers: %v", rec.Header())
	}
	b := rec.Body.String()
	for _, want := range []string{`"content":"hello"`, `"finish_reason":"stop"`, `"total_tokens":5`, "data: [DONE]"} {
		if !strings.Contains(b, want) {
			t.Errorf("stream missing %s:\n%s", want, b)
		}
	}
}

func TestChatStreamPreStreamError(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.textErr = status.Error(codes.ResourceExhausted, "throttled:rpm")
	rec := do(h, "POST", "/v1/chat/completions", key,
		`{"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != 429 {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	e := errBody(t, rec)
	if e["code"] != "throttled:rpm" {
		t.Errorf("expected throttled:rpm, got %v", e["code"])
	}
}

func TestChatStreamWithTools(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{
		{
			ResolvedModel: "m1",
			ToolCallChunks: []*pb.ToolCallChunk{
				{
					Index:          0,
					Id:             "call_abc",
					Type:           "function",
					Name:           "fetch_data",
					ArgumentsDelta: "{\"q\":",
				},
			},
		},
		{
			ResolvedModel: "m1",
			ToolCallChunks: []*pb.ToolCallChunk{
				{
					Index:          0,
					ArgumentsDelta: "\"test\"}",
				},
			},
		},
		{
			ResolvedModel: "m1",
			FinishReason:  "tool_calls",
			Usage:         &pb.TokensUsage{Input: 10, Output: 15},
		},
	}

	rec := do(h, "POST", "/v1/chat/completions", key,
		`{"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"x"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	b := rec.Body.String()
	for _, want := range []string{
		`"id":"call_abc"`,
		`"name":"fetch_data"`,
		`"arguments":"{\"q\":"`,
		`"arguments":"\"test\"}"`,
		`"finish_reason":"tool_calls"`,
		`"total_tokens":25`,
		"data: [DONE]",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("stream missing %s:\n%s", want, b)
		}
	}
}

func TestModels(t *testing.T) {
	_, h, key := newTestAPI(t)
	rec := do(h, "GET", "/v1/models", key, "")
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Object != "list" ||
		list.Data[0]["id"] != "m1" || list.Data[0]["owned_by"] != "openai" {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec := do(h, "GET", "/v1/models/m1", key, ""); rec.Code != 200 {
		t.Errorf("get: %d", rec.Code)
	}
	if rec := do(h, "GET", "/v1/models/nope", key, ""); rec.Code != 404 {
		t.Errorf("missing: %d", rec.Code)
	}
}

func TestSpeechAndImage(t *testing.T) {
	fp, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/audio/speech", key, `{"model":"tts1","input":"hola","voice":"alloy","language":"Spanish"}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/ogg" || rec.Body.String() != "OggS" ||
		fp.media["override"] != "tts1" || fp.media["language"] != "Spanish" {
		t.Errorf("speech: %d %v", rec.Code, fp.media)
	}
	if rec := do(h, "POST", "/v1/audio/speech", key, `{"model":"auto"}`); rec.Code != 400 {
		t.Errorf("speech without input = %d", rec.Code)
	}

	rec = do(h, "POST", "/v1/images/generations", key, `{"prompt":"a cat","n":1}`)
	var img struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &img); err != nil || rec.Code != 200 ||
		img.Data[0]["b64_json"] != base64.StdEncoding.EncodeToString([]byte("png")) {
		t.Errorf("image: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/images/generations", key, `{"prompt":"x","response_format":"url"}`); rec.Code != 400 {
		t.Errorf("url format = %d", rec.Code)
	}
}

func TestJudge(t *testing.T) {
	_, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/judge", key,
		`{"state_json":"{}","questions":{"q":{"type":"JUDGE_TYPE_NOUL","instructions":"ok?"}}}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"noul":0.9`) {
		t.Errorf("judge: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/judge", key, `{"bogus":1}`); rec.Code != 400 {
		t.Errorf("unknown field = %d", rec.Code)
	}
}

func TestAdminKeyCannotGenerate(t *testing.T) {
	store := &fakeStore{rows: map[string]*proxydb.APIKey{}}
	key := mintKey(t, store, "reader", "other")
	mux := http.NewServeMux()
	NewOpenAIAPI(&fakeProxy{}, apikeys.NewVerifier(store, "")).Register(mux)
	if rec := do(mux, "GET", "/v1/models", key, ""); rec.Code != 401 && rec.Code != 403 {
		t.Errorf("scopeless key got %d", rec.Code)
	}
}
