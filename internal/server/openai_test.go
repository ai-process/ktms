package server

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/audio"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/router"
)

type fakeProxy struct {
	pb.UnimplementedGatewayServiceServer
	text           *pb.GenerateTextRequest
	textErr        error
	respDetails    []*pb.Choice
	respCached     bool
	respPlacement  string
	respRouteMode  string
	respJudgeScore float64
	respJudgeModel string
	streamChunks   []*pb.GenerateTextChunk
	emptyStream    bool
	media          map[string]string
	speechErr      error
	judgeReq       *pb.JudgeRequest
	embedReq       *pb.GetEmbeddingsRequest
	embedResp      *pb.GetEmbeddingsResponse
	embedErr       error
	rolloutReq     *pb.EvaluateRolloutsRequest
}

func (f *fakeProxy) GenerateText(_ context.Context, r *pb.GenerateTextRequest) (*pb.GenerateTextResponse, error) {
	f.text = r
	if f.textErr != nil {
		return nil, f.textErr
	}
	resp := &pb.GenerateTextResponse{
		Choices: []string{"hello"}, Usage: &pb.TokensUsage{Input: 3, Output: 2},
		ResolvedModel: "m1", ResolvedVendor: "openai", MatchedRule: "default",
		Cached:     f.respCached,
		Placement:  f.respPlacement,
		RouteMode:  f.respRouteMode,
		JudgeScore: f.respJudgeScore,
		JudgeModel: f.respJudgeModel,
	}
	if len(f.respDetails) > 0 {
		resp.Details = f.respDetails
	}
	return resp, nil
}

func (f *fakeProxy) GenerateTextStream(r *pb.GenerateTextStreamRequest, s pb.GatewayService_GenerateTextStreamServer) error {
	f.text = r.Request
	if f.textErr != nil {
		return f.textErr
	}
	if f.emptyStream {
		return nil
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
		Cached:         f.respCached,
		Placement:      f.respPlacement,
		RouteMode:      f.respRouteMode,
		JudgeScore:     f.respJudgeScore,
		JudgeModel:     f.respJudgeModel,
	}); err != nil {
		return err
	}
	return s.Send(&pb.GenerateTextChunk{
		FinishReason:  "stop",
		Usage:         &pb.TokensUsage{Input: 3, Output: 2},
		ResolvedModel: "m1",
		Cached:        f.respCached,
	})
}

func (f *fakeProxy) ListModels(context.Context, *pb.ListModelsRequest) (*pb.ListModelsResponse, error) {
	return &pb.ListModelsResponse{Models: []*pb.ModelInfo{
		{Id: "m1", Vendor: "openai", Efforts: []pb.Effort{pb.Effort_EFFORT_LOW}, Capabilities: []string{"tts"}},
	}}, nil
}

func (f *fakeProxy) SynthesizeSpeech(_ context.Context, r *pb.SynthesizeSpeechRequest) (*pb.SynthesizeSpeechResponse, error) {
	if f.speechErr != nil {
		return nil, f.speechErr
	}
	f.media = map[string]string{"text": r.Text, "language": r.Language, "override": r.ModelOverride}
	return &pb.SynthesizeSpeechResponse{Audio: []byte("OggS"), MimeType: "audio/ogg", ResolvedModel: "tts1"}, nil
}

func (f *fakeProxy) GenerateImage(_ context.Context, r *pb.GenerateImageRequest) (*pb.GenerateImageResponse, error) {
	f.media = map[string]string{"prompt": r.Prompt}
	return &pb.GenerateImageResponse{Image: []byte("png"), MimeType: "image/png"}, nil
}

func (f *fakeProxy) Judge(_ context.Context, r *pb.JudgeRequest) (*pb.JudgeResponse, error) {
	f.judgeReq = r
	answers := make(map[string]*pb.JudgeAnswer, len(r.Questions))
	for id, q := range r.Questions {
		switch q.Type {
		case pb.JudgeType_JUDGE_TYPE_NOUL:
			answers[id] = &pb.JudgeAnswer{Type: q.Type, Noul: 0.9}
		case pb.JudgeType_JUDGE_TYPE_CHOICE:
			answers[id] = &pb.JudgeAnswer{Type: q.Type, Choice: "opt1", Confidence: 0.95}
		case pb.JudgeType_JUDGE_TYPE_SCORE:
			answers[id] = &pb.JudgeAnswer{
				Type:          q.Type,
				Score:         2.5,
				Probabilities: map[string]float64{"0": 0.2, "1": 0.8},
				Confidence:    0.85,
			}
		}
	}
	if len(answers) == 0 {
		answers["q"] = &pb.JudgeAnswer{Type: pb.JudgeType_JUDGE_TYPE_NOUL, Noul: 0.9}
	}
	model := "jev"
	if r.ModelOverride != "" {
		model = r.ModelOverride
	}
	return &pb.JudgeResponse{
		Answers:       answers,
		ResolvedModel: model,
		Usage:         &pb.TokensUsage{Input: 15, Output: 0},
	}, nil
}

func (f *fakeProxy) GetEmbeddings(_ context.Context, r *pb.GetEmbeddingsRequest) (*pb.GetEmbeddingsResponse, error) {
	f.embedReq = r
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	if f.embedResp != nil {
		return f.embedResp, nil
	}
	return &pb.GetEmbeddingsResponse{
		Data: []*pb.Embedding{
			{Index: 0, Values: []float32{0.1, 0.2, 0.3}},
		},
		ResolvedModel:  "text-embedding-3-small",
		ResolvedVendor: "openai",
		MatchedRule:    "default",
		Usage:          &pb.TokensUsage{Input: 5, Output: 0},
	}, nil
}

func (f *fakeProxy) EvaluateRollouts(_ context.Context, r *pb.EvaluateRolloutsRequest) (*pb.EvaluateRolloutsResponse, error) {
	f.rolloutReq = r
	model := "jev"
	if r.ModelOverride != "" {
		model = r.ModelOverride
	}
	evals := make([]*pb.CandidateEvaluation, 0, len(r.Candidates))
	for i, c := range r.Candidates {
		answers := make(map[string]*pb.JudgeAnswer, len(r.Criteria))
		for id, q := range r.Criteria {
			switch q.Type {
			case pb.JudgeType_JUDGE_TYPE_NOUL:
				answers[id] = &pb.JudgeAnswer{Type: q.Type, Noul: 0.9}
			case pb.JudgeType_JUDGE_TYPE_CHOICE:
				answers[id] = &pb.JudgeAnswer{Type: q.Type, Choice: "opt1", Confidence: 0.95}
			case pb.JudgeType_JUDGE_TYPE_SCORE:
				answers[id] = &pb.JudgeAnswer{
					Type:          q.Type,
					Score:         2.5,
					Probabilities: map[string]float64{"0": 0.2, "1": 0.8},
					Confidence:    0.85,
				}
			}
		}
		evals = append(evals, &pb.CandidateEvaluation{
			Id:             c.Id,
			Rank:           int32(i + 1),
			CompositeScore: 1.0 - float64(i)*0.2,
			Answers:        answers,
		})
	}
	bestID := ""
	if len(evals) > 0 {
		bestID = evals[0].Id
	}
	return &pb.EvaluateRolloutsResponse{
		Evaluations:     evals,
		BestCandidateId: bestID,
		ResolvedModel:   model,
		ResolvedVendor:  "openai",
		MatchedRule:     "default",
		Usage:           &pb.TokensUsage{Input: 40, Output: 0},
	}, nil
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
				Role      string  `json:"role"`
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
		{status.Error(codes.InvalidArgument, "guardrail_violation: PII detected (email)"), 400, "guardrail_violation"},
		{status.Error(codes.Unimplemented, "ffmpeg_missing: ffmpeg is required for Gemini speech synthesis: install ffmpeg or use the 'coyl/llm-proxy:audio' container image"), 501, "ffmpeg_missing"},
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

func TestSpendLimitErrorMapping(t *testing.T) {
	fp, h, key := newTestAPI(t)
	body := `{"messages":[{"role":"user","content":"x"}]}`

	fp.textErr = router.ToStatus(&router.ChainExhausted{AllThrottled: true, ThrottleKind: "spend_key_daily"})
	rec := do(h, "POST", "/v1/chat/completions", key, body)
	e := errBody(t, rec)
	if rec.Code != http.StatusTooManyRequests || e["code"] != "insufficient_quota" || e["type"] != "insufficient_quota" {
		t.Fatalf("got %d %v", rec.Code, e)
	}
	if e["message"] != "throttled:spend_key_daily" {
		t.Errorf("message = %v, want the tripped limit named", e["message"])
	}
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || secs < 1 || secs > 24*60*60 {
		t.Errorf("Retry-After = %q, want seconds until UTC midnight", rec.Header().Get("Retry-After"))
	}

	fp.textErr = router.ToStatus(&router.ChainExhausted{AllThrottled: true, ThrottleKind: "spend_unavailable"})
	rec = do(h, "POST", "/v1/chat/completions", key, body)
	if e := errBody(t, rec); rec.Code != http.StatusServiceUnavailable || e["code"] != "throttled:spend_unavailable" {
		t.Errorf("fail-closed refusal: got %d %v", rec.Code, e)
	}
}

func TestVendorCooldownSetsRetryAfter(t *testing.T) {
	fp, h, key := newTestAPI(t)
	body := `{"messages":[{"role":"user","content":"x"}]}`

	fp.textErr = router.ToStatus(&router.ChainExhausted{Last: &router.CoolingDown{Model: "m", Remaining: 2500 * time.Millisecond}, RetryAfter: 2500 * time.Millisecond})
	rec := do(h, "POST", "/v1/chat/completions", key, body)
	if e := errBody(t, rec); rec.Code != http.StatusServiceUnavailable || e["code"] != "vendors_unavailable" {
		t.Fatalf("got %d %v", rec.Code, e)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want the paused model's 3s", got)
	}

	fp.textErr = router.ToStatus(&router.ChainExhausted{AllCircuitBroken: true})
	rec = do(h, "POST", "/v1/chat/completions", key, body)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("open circuits carry no vendor wait, got Retry-After %q", got)
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

func TestChatStreamZeroChunks(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.emptyStream = true
	rec := do(h, "POST", "/v1/chat/completions", key,
		`{"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type %q, want text/event-stream", ct)
	}
	b := rec.Body.String()
	if !strings.Contains(b, "data: [DONE]\n\n") {
		t.Errorf("expected data: [DONE], got body %q", b)
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

	fp.speechErr = router.ToStatus(audio.ErrFFmpegMissing)
	rec = do(h, "POST", "/v1/audio/speech", key, `{"model":"gemini-flash","input":"speak this"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("speech without ffmpeg code = %d, want 501", rec.Code)
	}
	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v", err)
	}
	wantMsg := "ffmpeg_missing: ffmpeg is required for Gemini speech synthesis: install ffmpeg or use the 'coyl/llm-proxy:audio' container image"
	if errResp.Error.Message != wantMsg {
		t.Errorf("speech error message = %q, want %q", errResp.Error.Message, wantMsg)
	}
	if errResp.Error.Code != "ffmpeg_missing" {
		t.Errorf("speech error code = %q, want ffmpeg_missing", errResp.Error.Code)
	}
	fp.speechErr = nil

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

func TestDecisions(t *testing.T) {
	proxy, h, key := newTestAPI(t)
	body := `{
		"model": "gpt-6-luna",
		"input": "User wants refund",
		"user": "user-123",
		"metadata": {"team": "ops"},
		"questions": [
			{"name": "is_urgent", "type": "predicate", "instructions": "Urgent?"},
			{"name": "dept", "type": "choice", "instructions": "Which dept?", "choices": [{"value":"a","description":"A"},{"value":"b","description":"B"}]},
			{"name": "severity", "type": "score", "instructions": "Rate", "levels": ["low", "high"]}
		]
	}`
	rec := do(h, "POST", "/v1/decisions", key, body)
	if rec.Code != 200 {
		t.Fatalf("decisions returned status %d: %s", rec.Code, rec.Body)
	}
	var out decisionsResponseOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal decisions response: %v", err)
	}
	if out.Model != "gpt-6-luna" {
		t.Errorf("model = %q, want gpt-6-luna", out.Model)
	}
	if len(out.Answers) != 3 {
		t.Fatalf("answers count = %d, want 3", len(out.Answers))
	}
	if out.Answers[0].Name != "is_urgent" || out.Answers[0].Type != "predicate" || *out.Answers[0].Probability != 0.9 {
		t.Errorf("answer 0 = %+v", out.Answers[0])
	}
	if out.Answers[1].Name != "dept" || out.Answers[1].Type != "choice" || out.Answers[1].Choice != "opt1" {
		t.Errorf("answer 1 = %+v", out.Answers[1])
	}
	if out.Answers[2].Name != "severity" || out.Answers[2].Type != "score" || *out.Answers[2].Score != 2.5 {
		t.Errorf("answer 2 = %+v", out.Answers[2])
	}
	if out.Answers[2].Probabilities["low"] != 0.2 || out.Answers[2].Probabilities["high"] != 0.8 {
		t.Errorf("mapped probabilities = %+v, want low: 0.2, high: 0.8", out.Answers[2].Probabilities)
	}
	if proxy.judgeReq == nil || proxy.judgeReq.Attributes["user_id"] != "user-123" || proxy.judgeReq.Attributes["team"] != "ops" {
		t.Errorf("judge request attributes = %+v, want user_id: user-123, team: ops", proxy.judgeReq.GetAttributes())
	}
	if out.Usage["input_tokens"] != 15 {
		t.Errorf("usage = %+v", out.Usage)
	}

	// Test object-style levels mapping
	objLevelsReq := `{
		"input": "ok",
		"questions": [
			{"name": "severity_obj", "type": "score", "instructions": "Rate", "levels": [{"label": "bad", "description": "poor"}, {"label": "good", "description": "fine"}]}
		]
	}`
	recObj := do(h, "POST", "/v1/decisions", key, objLevelsReq)
	if recObj.Code != 200 {
		t.Fatalf("obj levels status = %d: %s", recObj.Code, recObj.Body)
	}
	var outObj decisionsResponseOut
	_ = json.Unmarshal(recObj.Body.Bytes(), &outObj)
	if outObj.Answers[0].Probabilities["bad"] != 0.2 || outObj.Answers[0].Probabilities["good"] != 0.8 {
		t.Errorf("mapped probabilities = %+v, want bad: 0.2, good: 0.8", outObj.Answers[0].Probabilities)
	}

	if rec := do(h, "POST", "/v1/decisions", key, `{"questions":[]}`); rec.Code != 400 {
		t.Errorf("empty input = %d, want 400", rec.Code)
	}
	if rec := do(h, "POST", "/v1/decisions", key, `{"input":"ok","questions":[{"name":"q","type":"unknown","instructions":"x"}]}`); rec.Code != 400 {
		t.Errorf("unknown type = %d, want 400", rec.Code)
	}
	// Duplicate question name
	dupQ := `{"input":"ok","questions":[{"name":"q","type":"predicate","instructions":"x"},{"name":"q","type":"predicate","instructions":"y"}]}`
	if rec := do(h, "POST", "/v1/decisions", key, dupQ); rec.Code != 400 {
		t.Errorf("dup question name = %d, want 400", rec.Code)
	}
	// Duplicate choice values
	dupChoice := `{"input":"ok","questions":[{"name":"q","type":"choice","instructions":"x","choices":[{"value":"a","description":"A"},{"value":"a","description":"B"}]}]}`
	if rec := do(h, "POST", "/v1/decisions", key, dupChoice); rec.Code != 400 {
		t.Errorf("dup choice value = %d, want 400", rec.Code)
	}
	// Duplicate level labels
	dupLevels := `{"input":"ok","questions":[{"name":"q","type":"score","instructions":"x","levels":["low","low"]}]}`
	if rec := do(h, "POST", "/v1/decisions", key, dupLevels); rec.Code != 400 {
		t.Errorf("dup level label = %d, want 400", rec.Code)
	}

	// Test model "auto" and "auto:low" route with empty override
	autoReq := `{"model":"auto","input":"ok","questions":[{"name":"q","type":"predicate","instructions":"test"}]}`
	recAuto := do(h, "POST", "/v1/decisions", key, autoReq)
	if recAuto.Code != 200 {
		t.Fatalf("auto model status = %d: %s", recAuto.Code, recAuto.Body)
	}
	if proxy.judgeReq.ModelOverride != "" {
		t.Errorf("model override for auto = %q, want empty", proxy.judgeReq.ModelOverride)
	}

	autoLowReq := `{"model":"auto:low","input":"ok","questions":[{"name":"q","type":"predicate","instructions":"test"}]}`
	recAutoLow := do(h, "POST", "/v1/decisions", key, autoLowReq)
	if recAutoLow.Code != 200 {
		t.Fatalf("auto:low status = %d: %s", recAutoLow.Code, recAutoLow.Body)
	}
	if proxy.judgeReq.ModelOverride != "" {
		t.Errorf("model override for auto:low = %q, want empty", proxy.judgeReq.ModelOverride)
	}

	if rec := do(h, "POST", "/v1/decisions", key, `{"model":"auto:invalid","input":"ok","questions":[{"name":"q","type":"predicate","instructions":"test"}]}`); rec.Code != 400 {
		t.Errorf("auto:invalid status = %d, want 400", rec.Code)
	}
}

func TestDecisionsRollouts(t *testing.T) {
	proxy, h, key := newTestAPI(t)

	body := `{
		"model": "gpt-6-luna",
		"state": {"board": "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"},
		"user": "user-456",
		"metadata": {"task": "chess-bot"},
		"candidates": [
			{"id": "cand_1", "action": {"move": "e4"}, "action_text": "King's pawn opening"},
			{"id": "cand_2", "action": {"move": "d4"}, "action_text": "Queen's pawn opening"}
		],
		"criteria": [
			{"name": "quality", "type": "score", "instructions": "Evaluate rollout move quality", "levels": ["blunder", "inaccuracy", "good", "best"]},
			{"name": "safety", "type": "predicate", "instructions": "Is move tactically safe?"}
		]
	}`

	rec := do(h, "POST", "/v1/decisions/rollouts", key, body)
	if rec.Code != 200 {
		t.Fatalf("rollouts returned status %d: %s", rec.Code, rec.Body)
	}

	var out decisionsRolloutResponseOut
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal rollouts response: %v", err)
	}
	if out.Model != "gpt-6-luna" {
		t.Errorf("model = %q, want gpt-6-luna", out.Model)
	}
	if out.BestCandidateID != "cand_1" {
		t.Errorf("best candidate = %q, want cand_1", out.BestCandidateID)
	}
	if len(out.Evaluations) != 2 {
		t.Fatalf("evaluations count = %d, want 2", len(out.Evaluations))
	}
	if out.Evaluations[0].ID != "cand_1" || out.Evaluations[0].Rank != 1 {
		t.Errorf("evaluations[0] = %+v, want cand_1 with rank 1", out.Evaluations[0])
	}
	if out.Evaluations[1].ID != "cand_2" || out.Evaluations[1].Rank != 2 {
		t.Errorf("evaluations[1] = %+v, want cand_2 with rank 2", out.Evaluations[1])
	}
	if rec.Header().Get("X-LLM-Proxy-Model") != "gpt-6-luna" {
		t.Errorf("X-LLM-Proxy-Model = %q, want gpt-6-luna", rec.Header().Get("X-LLM-Proxy-Model"))
	}
	if rec.Header().Get("X-LLM-Proxy-Vendor") != "openai" {
		t.Errorf("X-LLM-Proxy-Vendor = %q, want openai", rec.Header().Get("X-LLM-Proxy-Vendor"))
	}
	if proxy.rolloutReq == nil || proxy.rolloutReq.Attributes["user_id"] != "user-456" || proxy.rolloutReq.Attributes["task"] != "chess-bot" {
		t.Errorf("rollout request attributes = %+v", proxy.rolloutReq.GetAttributes())
	}

	// Empty candidates error
	if recEmpty := do(h, "POST", "/v1/decisions/rollouts", key, `{"candidates":[]}`); recEmpty.Code != 400 {
		t.Errorf("empty candidates status = %d, want 400", recEmpty.Code)
	}

	// Missing candidate id
	if recNoID := do(h, "POST", "/v1/decisions/rollouts", key, `{"candidates":[{"action":{"a":1}}]}`); recNoID.Code != 400 {
		t.Errorf("missing candidate id status = %d, want 400", recNoID.Code)
	}

	// Duplicate criteria name
	dupCriteria := `{"candidates":[{"id":"c1","action":{"a":1}}],"criteria":[{"name":"q","type":"predicate","instructions":"a"},{"name":"q","type":"predicate","instructions":"b"}]}`
	if recDup := do(h, "POST", "/v1/decisions/rollouts", key, dupCriteria); recDup.Code != 400 {
		t.Errorf("duplicate criteria status = %d, want 400", recDup.Code)
	}

	// Default criteria without explicit criteria field populates answers
	noCriteriaBody := `{"candidates":[{"id":"c1","action":{"move":"e4"}}]}`
	recNoCrit := do(h, "POST", "/v1/decisions/rollouts", key, noCriteriaBody)
	if recNoCrit.Code != 200 {
		t.Fatalf("no criteria status = %d: %s", recNoCrit.Code, recNoCrit.Body)
	}
	var outNoCrit decisionsRolloutResponseOut
	if err := json.Unmarshal(recNoCrit.Body.Bytes(), &outNoCrit); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(outNoCrit.Evaluations) == 0 || len(outNoCrit.Evaluations[0].Answers) == 0 {
		t.Errorf("expected default reward answer, got %+v", outNoCrit.Evaluations)
	}

	// Unsupported candidate media type
	unsupportedMedia := `{"candidates":[{"id":"c1","media":[{"type":"unknown_media_type"}]}]}`
	if recBadMedia := do(h, "POST", "/v1/decisions/rollouts", key, unsupportedMedia); recBadMedia.Code != 400 {
		t.Errorf("unsupported candidate media status = %d, want 400", recBadMedia.Code)
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

func TestChatCompletions_MultipartContent(t *testing.T) {
	proxy, h, key := newTestAPI(t)

	imgData := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))
	audioData := base64.StdEncoding.EncodeToString([]byte("fake-audio-data"))
	fileData := base64.StdEncoding.EncodeToString([]byte("fake-pdf-data"))

	body := fmt.Sprintf(`{
		"model": "gpt-4o",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Inspect these files:"},
					{"type": "image_url", "image_url": {"url": "data:image/png;base64,%s", "detail": "high"}},
					{"type": "input_audio", "input_audio": {"data": "%s", "format": "wav"}},
					{"type": "file", "file": {"file_data": "%s", "mime_type": "application/pdf", "filename": "doc.pdf"}}
				]
			}
		]
	}`, imgData, audioData, fileData)

	rec := do(h, "POST", "/v1/chat/completions", key, body)
	if rec.Code != 200 {
		t.Fatalf("chat completions status = %d: %s", rec.Code, rec.Body)
	}

	if proxy.text == nil {
		t.Fatal("proxy received no request")
	}
	msgs := proxy.text.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.GetRole() != pb.MessageRole_MESSAGE_ROLE_USER {
		t.Errorf("role = %v, want USER", m.GetRole())
	}
	if len(m.GetParts()) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(m.GetParts()))
	}

	// 1. Text part
	txtPart := m.GetParts()[0].GetText()
	if txtPart == nil || txtPart.GetText() != "Inspect these files:" {
		t.Errorf("text part = %+v", txtPart)
	}

	// 2. Image part
	imgPart := m.GetParts()[1].GetImage()
	if imgPart == nil || imgPart.GetMimeType() != "image/png" || imgPart.GetDetail() != "high" || string(imgPart.GetData()) != "fake-png-data" {
		t.Errorf("image part = %+v", imgPart)
	}

	// 3. Audio part
	audioPart := m.GetParts()[2].GetAudio()
	if audioPart == nil || audioPart.GetFormat() != "wav" || string(audioPart.GetData()) != "fake-audio-data" {
		t.Errorf("audio part = %+v", audioPart)
	}

	// 4. Document part
	docPart := m.GetParts()[3].GetDocument()
	if docPart == nil || docPart.GetMimeType() != "application/pdf" || docPart.GetFileName() != "doc.pdf" || string(docPart.GetData()) != "fake-pdf-data" {
		t.Errorf("doc part = %+v", docPart)
	}
}

func TestChatCompletions_SSRFRejection(t *testing.T) {
	_, h, key := newTestAPI(t)

	// Disallowed scheme http://
	bodyHttp := `{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "http://example.com/test.png"}}]}]
	}`
	recHttp := do(h, "POST", "/v1/chat/completions", key, bodyHttp)
	if recHttp.Code != 400 {
		t.Errorf("expected 400 for http scheme, got %d: %s", recHttp.Code, recHttp.Body)
	}

	// Private loopback IP in https://
	bodyLoopback := `{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "https://127.0.0.1/test.png"}}]}]
	}`
	recLoopback := do(h, "POST", "/v1/chat/completions", key, bodyLoopback)
	if recLoopback.Code != 400 {
		t.Errorf("expected 400 for loopback IP, got %d: %s", recLoopback.Code, recLoopback.Body)
	}
}

func TestChatCompletions_MultimodalNonUserRejected(t *testing.T) {
	_, h, key := newTestAPI(t)

	body := `{
		"model": "gpt-4o",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "image_url", "image_url": {"url": "data:image/png;base64,YWJj"}}
				]
			}
		]
	}`
	rec := do(h, "POST", "/v1/chat/completions", key, body)
	if rec.Code != 400 {
		t.Errorf("expected 400 for multimodal assistant message, got %d: %s", rec.Code, rec.Body)
	}
}

func TestChatCompletions_Caching(t *testing.T) {
	fp, h, key := newTestAPI(t)

	// 1. JSON enable_cache = true, MISS
	fp.respCached = false
	body1 := `{"model":"auto:low","messages":[{"role":"user","content":"hi"}],"enable_cache":true}`
	rec1 := do(h, "POST", "/v1/chat/completions", key, body1)
	if rec1.Code != 200 {
		t.Fatalf("code = %d", rec1.Code)
	}
	if !fp.text.EnableCache {
		t.Errorf("expected EnableCache to be true in proto request")
	}
	if got := rec1.Header().Get("X-LLM-Proxy-Cache"); got != "MISS" {
		t.Errorf("got cache header %q, want MISS", got)
	}

	// 2. JSON enable_cache = true, HIT
	fp.respCached = true
	rec2 := do(h, "POST", "/v1/chat/completions", key, body1)
	if rec2.Code != 200 {
		t.Fatalf("code = %d", rec2.Code)
	}
	if got := rec2.Header().Get("X-LLM-Proxy-Cache"); got != "HIT" {
		t.Errorf("got cache header %q, want HIT", got)
	}

	// 3. Header X-LLM-Proxy-Cache: true, HIT
	body3 := `{"model":"auto:low","messages":[{"role":"user","content":"hi"}]}`
	req3 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body3))
	req3.Header.Set("Authorization", "Bearer "+key)
	req3.Header.Set("X-LLM-Proxy-Cache", "true")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("code = %d", rec3.Code)
	}
	if !fp.text.EnableCache {
		t.Errorf("expected EnableCache to be true via header")
	}
	if got := rec3.Header().Get("X-LLM-Proxy-Cache"); got != "HIT" {
		t.Errorf("got cache header %q, want HIT", got)
	}

	// 4. Streaming with enable_cache = true, HIT
	body4 := `{"model":"auto:low","messages":[{"role":"user","content":"hi"}],"stream":true,"enable_cache":true}`
	rec4 := do(h, "POST", "/v1/chat/completions", key, body4)
	if rec4.Code != 200 {
		t.Fatalf("code = %d", rec4.Code)
	}
	if got := rec4.Header().Get("X-LLM-Proxy-Cache"); got != "HIT" {
		t.Errorf("got streaming cache header %q, want HIT", got)
	}

	// 5. Streaming with enable_cache = true, MISS
	fp.respCached = false
	rec5 := do(h, "POST", "/v1/chat/completions", key, body4)
	if rec5.Code != 200 {
		t.Fatalf("code = %d", rec5.Code)
	}
	if got := rec5.Header().Get("X-LLM-Proxy-Cache"); got != "MISS" {
		t.Errorf("got streaming cache header %q, want MISS", got)
	}
}

func TestChatCompletions_AutoRoutingAndPlacement(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.respPlacement = router.PlacementNodesFirst
	fp.respRouteMode = "auto-judge"
	fp.respJudgeScore = 0.88
	fp.respJudgeModel = "jev-latest"

	// 1. model: "auto" defaults to nodes_first placement and auto_route=true
	body1 := `{"model":"auto","messages":[{"role":"user","content":"explain quantum computing"}]}`
	rec1 := do(h, "POST", "/v1/chat/completions", key, body1)
	if rec1.Code != 200 {
		t.Fatalf("code = %d: %s", rec1.Code, rec1.Body)
	}
	if !fp.text.AutoRoute {
		t.Errorf("expected AutoRoute=true in proto request")
	}
	if fp.text.Placement != router.PlacementNodesFirst {
		t.Errorf("expected Placement=nodes_first, got %s", fp.text.Placement)
	}
	if got := rec1.Header().Get("X-LLM-Proxy-Placement"); got != router.PlacementNodesFirst {
		t.Errorf("expected X-LLM-Proxy-Placement %q, got %q", router.PlacementNodesFirst, got)
	}
	if got := rec1.Header().Get("X-LLM-Proxy-Route-Mode"); got != "auto-judge" {
		t.Errorf("expected X-LLM-Proxy-Route-Mode auto-judge, got %q", got)
	}
	if got := rec1.Header().Get("X-LLM-Proxy-Judge-Score"); got != "0.88" {
		t.Errorf("expected X-LLM-Proxy-Judge-Score 0.88, got %q", got)
	}
	if got := rec1.Header().Get("X-LLM-Proxy-Judge-Model"); got != "jev-latest" {
		t.Errorf("expected X-LLM-Proxy-Judge-Model jev-latest, got %q", got)
	}

	// 2. model: "auto:nodes-only" parses placement
	body2 := `{"model":"auto:nodes-only","messages":[{"role":"user","content":"hi"}]}`
	rec2 := do(h, "POST", "/v1/chat/completions", key, body2)
	if rec2.Code != 200 {
		t.Fatalf("code = %d: %s", rec2.Code, rec2.Body)
	}
	if fp.text.Placement != router.PlacementNodesOnly {
		t.Errorf("expected Placement=nodes_only, got %s", fp.text.Placement)
	}

	// 3. model: "auto:cloud-first" parses placement
	body3 := `{"model":"auto:cloud-first","messages":[{"role":"user","content":"hi"}]}`
	rec3 := do(h, "POST", "/v1/chat/completions", key, body3)
	if rec3.Code != 200 {
		t.Fatalf("code = %d: %s", rec3.Code, rec3.Body)
	}
	if fp.text.Placement != router.PlacementCloudFirst {
		t.Errorf("expected Placement=cloud_first, got %s", fp.text.Placement)
	}

	// 4. model: "auto:nodes-first:high" parses effort and placement
	body4 := `{"model":"auto:nodes-first:high","messages":[{"role":"user","content":"hi"}]}`
	rec4 := do(h, "POST", "/v1/chat/completions", key, body4)
	if rec4.Code != 200 {
		t.Fatalf("code = %d: %s", rec4.Code, rec4.Body)
	}
	if fp.text.Effort != pb.Effort_EFFORT_HIGH {
		t.Errorf("expected effort high, got %v", fp.text.Effort)
	}
	if !fp.text.EffortSpecified {
		t.Errorf("expected EffortSpecified=true")
	}
	if fp.text.Placement != router.PlacementNodesFirst {
		t.Errorf("expected Placement=nodes_first, got %s", fp.text.Placement)
	}

	// 5. reasoning_effort pins effort and marks EffortSpecified=true
	body5 := `{"model":"auto","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`
	rec5 := do(h, "POST", "/v1/chat/completions", key, body5)
	if rec5.Code != 200 {
		t.Fatalf("code = %d: %s", rec5.Code, rec5.Body)
	}
	if fp.text.Effort != pb.Effort_EFFORT_LOW {
		t.Errorf("expected effort low from reasoning_effort, got %v", fp.text.Effort)
	}
	if !fp.text.EffortSpecified {
		t.Errorf("expected EffortSpecified=true when reasoning_effort set")
	}

	// 6. JSON body placement field
	body6 := `{"model":"auto","placement":"nodes_only","messages":[{"role":"user","content":"hi"}]}`
	rec6 := do(h, "POST", "/v1/chat/completions", key, body6)
	if rec6.Code != 200 {
		t.Fatalf("code = %d: %s", rec6.Code, rec6.Body)
	}
	if fp.text.Placement != router.PlacementNodesOnly {
		t.Errorf("expected Placement=nodes_only from body, got %s", fp.text.Placement)
	}

	// 7. X-LLM-Proxy-Placement header overrides body and model
	body7 := `{"model":"auto:nodes-first","placement":"nodes_first","messages":[{"role":"user","content":"hi"}]}`
	req7 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body7))
	req7.Header.Set("Authorization", "Bearer "+key)
	req7.Header.Set("X-LLM-Proxy-Placement", "cloud_only")
	rec7 := httptest.NewRecorder()
	h.ServeHTTP(rec7, req7)
	if rec7.Code != 200 {
		t.Fatalf("code = %d: %s", rec7.Code, rec7.Body)
	}
	if fp.text.Placement != router.PlacementCloudOnly {
		t.Errorf("expected Placement=cloud_only from header override, got %s", fp.text.Placement)
	}

	// 8. Streaming response headers
	body8 := `{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec8 := do(h, "POST", "/v1/chat/completions", key, body8)
	if rec8.Code != 200 {
		t.Fatalf("code = %d: %s", rec8.Code, rec8.Body)
	}
	if got := rec8.Header().Get("X-LLM-Proxy-Placement"); got != router.PlacementNodesFirst {
		t.Errorf("expected stream X-LLM-Proxy-Placement %q, got %q", router.PlacementNodesFirst, got)
	}
	if got := rec8.Header().Get("X-LLM-Proxy-Route-Mode"); got != "auto-judge" {
		t.Errorf("expected stream X-LLM-Proxy-Route-Mode auto-judge, got %q", got)
	}

	// 9. Negative parsing tests: reject invalid auto specifiers
	invalidBodies := []string{
		`{"model":"auto:","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"auto:low:high","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"auto:nodes-only:cloud-only","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"auto:unknown-spec","messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, inv := range invalidBodies {
		r := do(h, "POST", "/v1/chat/completions", key, inv)
		if r.Code != 400 {
			t.Errorf("expected 400 for invalid body %s, got %d: %s", inv, r.Code, r.Body)
		}
	}

	// 10. Placement rejected for non-chat endpoint (speech)
	speechBody := `{"model":"auto:nodes-only","input":"speak this"}`
	recSpeech := do(h, "POST", "/v1/audio/speech", key, speechBody)
	if recSpeech.Code != 400 {
		t.Errorf("expected 400 for speech with placement, got %d: %s", recSpeech.Code, recSpeech.Body)
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	fp, h, key := newTestAPI(t)

	// 1. Single string input with float encoding (default)
	body1 := `{"model":"text-embedding-3-small","input":"hello world"}`
	rec1 := do(h, "POST", "/v1/embeddings", key, body1)
	if rec1.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec1.Code, rec1.Body)
	}
	if fp.embedReq == nil || len(fp.embedReq.Inputs) != 1 || fp.embedReq.Inputs[0] != "hello world" {
		t.Fatalf("unexpected embedReq inputs: %v", fp.embedReq)
	}
	if fp.embedReq.Model != "text-embedding-3-small" {
		t.Errorf("expected model text-embedding-3-small, got %s", fp.embedReq.Model)
	}
	if rec1.Header().Get("X-LLM-Proxy-Model") != "text-embedding-3-small" {
		t.Errorf("expected header X-LLM-Proxy-Model text-embedding-3-small, got %s", rec1.Header().Get("X-LLM-Proxy-Model"))
	}
	if rec1.Header().Get("X-LLM-Proxy-Vendor") != "openai" {
		t.Errorf("expected header X-LLM-Proxy-Vendor openai, got %s", rec1.Header().Get("X-LLM-Proxy-Vendor"))
	}
	if rec1.Header().Get("X-LLM-Proxy-Rule") != "default" {
		t.Errorf("expected header X-LLM-Proxy-Rule default, got %s", rec1.Header().Get("X-LLM-Proxy-Rule"))
	}

	var resp1 struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp1.Object != "list" || len(resp1.Data) != 1 {
		t.Fatalf("unexpected resp structure: %+v", resp1)
	}
	if resp1.Data[0].Index != 0 || len(resp1.Data[0].Embedding) != 3 {
		t.Fatalf("unexpected embedding data: %+v", resp1.Data[0])
	}
	if resp1.Usage.PromptTokens != 5 || resp1.Usage.TotalTokens != 5 {
		t.Errorf("unexpected usage: %+v", resp1.Usage)
	}

	// 2. Batch input with base64 encoding and metadata/user
	fp.embedResp = &pb.GetEmbeddingsResponse{
		Data: []*pb.Embedding{
			{Index: 0, Values: []float32{1.5, -2.5}},
			{Index: 1, Values: []float32{0.0, 4.2}},
		},
		ResolvedModel:  "text-embedding-3-large",
		ResolvedVendor: "openai",
		MatchedRule:    "custom-emb",
		Usage:          &pb.TokensUsage{Input: 12, Output: 0},
	}
	body2 := `{
		"model": "text-embedding-3-large",
		"input": ["doc1", "doc2"],
		"encoding_format": "base64",
		"dimensions": 2,
		"user": "alice",
		"metadata": {"action_id": "act-123", "dept": "engineering"}
	}`
	rec2 := do(h, "POST", "/v1/embeddings", key, body2)
	if rec2.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body)
	}
	if fp.embedReq.Dimensions != 2 {
		t.Errorf("expected dimensions 2, got %d", fp.embedReq.Dimensions)
	}
	if fp.embedReq.ActionId != "act-123" {
		t.Errorf("expected action_id act-123, got %s", fp.embedReq.ActionId)
	}
	if fp.embedReq.Attributes["user_id"] != "alice" || fp.embedReq.Attributes["dept"] != "engineering" {
		t.Errorf("unexpected attributes: %v", fp.embedReq.Attributes)
	}

	var resp2 struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string `json:"object"`
			Index     int    `json:"index"`
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("failed to decode base64 response: %v", err)
	}
	if len(resp2.Data) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp2.Data))
	}
	decodeFloats := func(s string) []float32 {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("invalid base64: %v", err)
		}
		floats := make([]float32, len(raw)/4)
		for i := range floats {
			bits := binary.LittleEndian.Uint32(raw[i*4:])
			floats[i] = math.Float32frombits(bits)
		}
		return floats
	}
	f0 := decodeFloats(resp2.Data[0].Embedding)
	if len(f0) != 2 || f0[0] != 1.5 || f0[1] != -2.5 {
		t.Errorf("unexpected floats for item 0: %v", f0)
	}
	f1 := decodeFloats(resp2.Data[1].Embedding)
	if len(f1) != 2 || f1[0] != 0.0 || f1[1] != 4.2 {
		t.Errorf("unexpected floats for item 1: %v", f1)
	}

	// 3. Validation / error cases
	fp.embedResp = nil
	errCases := []struct {
		name string
		body string
		code int
	}{
		{"missing model", `{"input":"hi"}`, 400},
		{"auto model forbidden", `{"model":"auto","input":"hi"}`, 400},
		{"empty string input", `{"model":"m","input":""}`, 400},
		{"empty array input", `{"model":"m","input":[]}`, 400},
		{"empty item in array", `{"model":"m","input":["hello",""]}`, 400},
		{"token input rejected", `{"model":"m","input":[1,2,3]}`, 400},
		{"invalid encoding format", `{"model":"m","input":"hi","encoding_format":"hex"}`, 400},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			r := do(h, "POST", "/v1/embeddings", key, tc.body)
			if r.Code != tc.code {
				t.Errorf("expected status %d for %s, got %d: %s", tc.code, tc.name, r.Code, r.Body)
			}
		})
	}

	// 4. Upstream error mapping
	fp.embedErr = status.Error(codes.FailedPrecondition, "no_capable_model")
	recErr := do(h, "POST", "/v1/embeddings", key, `{"model":"m","input":"hi"}`)
	if recErr.Code != 400 {
		t.Errorf("expected 400 for FailedPrecondition, got %d: %s", recErr.Code, recErr.Body)
	}
}

func TestDecisionsMultimodalInput(t *testing.T) {
	proxy, h, key := newTestAPI(t)
	png := base64.StdEncoding.EncodeToString([]byte("png"))
	wav := base64.StdEncoding.EncodeToString([]byte("wav"))
	body := `{"input":[
		{"type":"input_text","text":"Is the lane clear?"},
		{"type":"input_image","image_url":"data:image/png;base64,` + png + `","detail":"low"},
		{"type":"image_url","image_url":{"url":"https://cdn.example.com/cam.jpg","detail":"high"}},
		{"type":"input_audio","input_audio":{"data":"` + wav + `","format":"wav"}},
		{"type":"input_embedding","embedding":[0.5,-1],"model":"v-jepa-2"}
	],"questions":[{"name":"q","type":"predicate","instructions":"clear?"}]}`
	if rec := do(h, "POST", "/v1/decisions", key, body); rec.Code != 200 {
		t.Fatalf("decisions: %d %s", rec.Code, rec.Body)
	}
	req := proxy.judgeReq
	if req.GetStateJson() != "" || len(req.GetStateMedia()) != 5 {
		t.Fatalf("state_json = %q, media = %d parts, want empty and 5", req.GetStateJson(), len(req.GetStateMedia()))
	}
	m := req.GetStateMedia()
	if m[0].GetText().GetText() != "Is the lane clear?" {
		t.Errorf("text part = %v", m[0])
	}
	if img := m[1].GetImage(); img.GetMimeType() != "image/png" || string(img.GetData()) != "png" || img.GetDetail() != "low" || img.GetUrl() != "" {
		t.Errorf("data URI image = %v", img)
	}
	if img := m[2].GetImage(); img.GetUrl() != "https://cdn.example.com/cam.jpg" || img.GetDetail() != "high" || len(img.GetData()) != 0 {
		t.Errorf("https image = %v", img)
	}
	if aud := m[3].GetAudio(); string(aud.GetData()) != "wav" || aud.GetFormat() != "wav" {
		t.Errorf("audio = %v", aud)
	}
	if emb := m[4].GetEmbedding(); len(emb.GetValues()) != 2 || emb.GetValues()[1] != -1 || emb.GetModel() != "v-jepa-2" {
		t.Errorf("embedding = %v", emb)
	}

	// An array state that is not made of known part types stays verbatim JSON.
	plain := `{"input":[{"type":"car","speed":3}],"questions":[{"name":"q","type":"predicate","instructions":"fast?"}]}`
	if rec := do(h, "POST", "/v1/decisions", key, plain); rec.Code != 200 {
		t.Fatalf("plain array: %d %s", rec.Code, rec.Body)
	}
	if proxy.judgeReq.GetStateJson() != `[{"type":"car","speed":3}]` || len(proxy.judgeReq.GetStateMedia()) != 0 {
		t.Errorf("plain array state = %q, media = %v", proxy.judgeReq.GetStateJson(), proxy.judgeReq.GetStateMedia())
	}

	bad := map[string]string{
		"http image":     `[{"type":"input_image","image_url":"http://x/a.png"}]`,
		"bad data uri":   `[{"type":"input_image","image_url":"data:image/png;base64,%%%"}]`,
		"image no url":   `[{"type":"input_image"}]`,
		"audio no data":  `[{"type":"input_audio","input_audio":{"format":"wav"}}]`,
		"audio bad b64":  `[{"type":"input_audio","input_audio":{"data":"%%%","format":"wav"}}]`,
		"embedding text": `[{"type":"input_embedding","embedding":["a"]}]`,
	}
	for name, input := range bad {
		rec := do(h, "POST", "/v1/decisions", key, `{"input":`+input+`,"questions":[{"name":"q","type":"predicate","instructions":"x"}]}`)
		if rec.Code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
}

func TestHeaderCompatibilityAndDeprecation(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.respPlacement = "nodes_first"
	fp.respRouteMode = "auto-effort"

	// 1. Inbound request using legacy X-LLM-Proxy-Placement
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LLM-Proxy-Placement", "cloud_only")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if fp.text.Placement != "cloud_only" {
		t.Errorf("expected placement cloud_only, got %s", fp.text.Placement)
	}

	// Verify dual emission of response headers
	if rec.Header().Get("X-KTMS-Vendor") != "openai" || rec.Header().Get("X-LLM-Proxy-Vendor") != "openai" {
		t.Errorf("expected vendor headers openai, got ktms=%q, legacy=%q",
			rec.Header().Get("X-KTMS-Vendor"), rec.Header().Get("X-LLM-Proxy-Vendor"))
	}
	if rec.Header().Get("X-KTMS-Rule") != "default" || rec.Header().Get("X-LLM-Proxy-Rule") != "default" {
		t.Errorf("expected rule headers default, got ktms=%q, legacy=%q",
			rec.Header().Get("X-KTMS-Rule"), rec.Header().Get("X-LLM-Proxy-Rule"))
	}
	if rec.Header().Get("X-KTMS-Placement") != "nodes_first" || rec.Header().Get("X-LLM-Proxy-Placement") != "nodes_first" {
		t.Errorf("expected placement headers nodes_first, got ktms=%q, legacy=%q",
			rec.Header().Get("X-KTMS-Placement"), rec.Header().Get("X-LLM-Proxy-Placement"))
	}
	if rec.Header().Get("X-KTMS-Route-Mode") != "auto-effort" || rec.Header().Get("X-LLM-Proxy-Route-Mode") != "auto-effort" {
		t.Errorf("expected route mode auto-effort, got ktms=%q, legacy=%q",
			rec.Header().Get("X-KTMS-Route-Mode"), rec.Header().Get("X-LLM-Proxy-Route-Mode"))
	}

	// Verify RFC 8594 / RFC 7234 Deprecation headers
	if rec.Header().Get("Deprecation") != "true" {
		t.Errorf("expected Deprecation: true, got %q", rec.Header().Get("Deprecation"))
	}
	if rec.Header().Get("Sunset") != "Sun, 01 Mar 2027 00:00:00 GMT" {
		t.Errorf("expected Sunset header, got %q", rec.Header().Get("Sunset"))
	}
	if !strings.Contains(rec.Header().Get("Warning"), "299 - \"X-LLM-Proxy-* response headers are deprecated") {
		t.Errorf("expected Warning header with deprecation message, got %q", rec.Header().Get("Warning"))
	}

	// 2. Inbound request using new X-KTMS-Placement
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer "+key)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-KTMS-Placement", "nodes_only")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec2.Code, rec2.Body.String())
	}
	if fp.text.Placement != "nodes_only" {
		t.Errorf("expected placement nodes_only, got %s", fp.text.Placement)
	}
}

