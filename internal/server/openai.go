package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/panicsafe"
)

const maxRequestBytes = 16 << 20

// OpenAIAPI serves the data plane over HTTP in the OpenAI wire format. It calls
// the same handlers as gRPC after the same key and scope check, so routing,
// throttling and usage reporting behave identically on both transports.
type OpenAIAPI struct {
	proxy    pb.LLMProxyServiceServer
	verifier *apikeys.Verifier
}

func NewOpenAIAPI(proxy pb.LLMProxyServiceServer, verifier *apikeys.Verifier) *OpenAIAPI {
	return &OpenAIAPI{proxy: proxy, verifier: verifier}
}

// Register mounts the routes under /v1.
func (a *OpenAIAPI) Register(mux *http.ServeMux) {
	chat := method(pb.LLMProxyService_GenerateText_FullMethodName)
	speech := method(pb.LLMProxyService_SynthesizeSpeech_FullMethodName)
	image := method(pb.LLMProxyService_GenerateImage_FullMethodName)
	judge := method(pb.LLMProxyService_Judge_FullMethodName)
	models := method(pb.LLMProxyService_ListModels_FullMethodName)

	mux.HandleFunc("POST /v1/chat/completions", a.wrap(chat, a.chatCompletions))
	mux.HandleFunc("POST /v1/audio/speech", a.wrap(speech, a.audioSpeech))
	mux.HandleFunc("POST /v1/images/generations", a.wrap(image, a.imageGenerations))
	mux.HandleFunc("POST /v1/judge", a.wrap(judge, a.judge))
	mux.HandleFunc("GET /v1/models", a.wrap(models, a.listModels))
	mux.HandleFunc("GET /v1/models/{id...}", a.wrap(models, a.getModel))
}

type apiHandler func(ctx context.Context, w http.ResponseWriter, r *http.Request) error

// wrap authenticates, recovers panics and turns returned errors into
// OpenAI-style error bodies.
func (a *OpenAIAPI) wrap(fullMethod string, h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var err error
		defer func() {
			if rec := recover(); rec != nil {
				panicsafe.Report("http handler "+fullMethod, rec)
				err = status.Error(codes.Internal, "internal error")
				writeAPIError(w, err)
			}
			rpcLogEvent(err).Str("method", fullMethod).Str("transport", "http").
				Dur("duration", time.Since(start)).Msg("http request")
		}()

		md := metadata.MD{}
		if auth := r.Header.Get("Authorization"); auth != "" {
			md.Set(metadataKeyAuthorization, auth)
		}
		ctx := metadata.NewIncomingContext(r.Context(), md)
		id, aerr := authorize(ctx, a.verifier, fullMethod)
		if aerr != nil {
			err = aerr
			writeAPIError(w, err)
			return
		}
		if err = h(apikeys.WithIdentity(ctx, id), w, r); err != nil {
			writeAPIError(w, err)
		}
	}
}

// ---- chat completions ----

type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []chatMessage     `json:"messages"`
	Stream              bool              `json:"stream"`
	StreamOptions       *streamOptions    `json:"stream_options"`
	MaxTokens           int32             `json:"max_tokens"`
	MaxCompletionTokens int32             `json:"max_completion_tokens"`
	ResponseFormat      *responseFormat   `json:"response_format"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	User                string            `json:"user"`
	Metadata            map[string]string `json:"metadata"`
	N                   *int              `json:"n"`
	Tools               json.RawMessage   `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	Functions           json.RawMessage   `json:"functions"`
	// Proxy extension: lets a caller turn on grounded Google answers.
	EnableGoogleSearch bool `json:"enable_google_search"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string                 `json:"type"`
	Function chatFunctionDefinition `json:"function"`
}

type chatFunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

func invalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

func decodeBody(r *http.Request, dst any) error {
	body := http.MaxBytesReader(nil, r.Body, maxRequestBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return invalid("request body is empty")
		}
		return invalid("invalid JSON body: %v", err)
	}
	return nil
}

// text flattens string or [{"type":"text","text":...}] content.
func (m chatMessage) text() (string, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return "", invalid("message content must be a string or a list of text parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", invalid("content part type %q is not supported (text only)", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// route resolves the OpenAI "model" field. "auto" (or empty) leaves the choice
// to the rules, "auto:low|medium|high" also picks the effort, anything else is
// an exact registry model id.
func route(model, reasoningEffort string) (effort pb.Effort, override string, err error) {
	effort = pb.Effort_EFFORT_MEDIUM
	if reasoningEffort != "" {
		if effort, err = parseEffort(reasoningEffort); err != nil {
			return 0, "", err
		}
	}
	switch {
	case model == "" || model == "auto":
	case strings.HasPrefix(model, "auto:"):
		if effort, err = parseEffort(strings.TrimPrefix(model, "auto:")); err != nil {
			return 0, "", err
		}
	default:
		override = model
	}
	return effort, override, nil
}

func parseEffort(s string) (pb.Effort, error) {
	switch strings.ToLower(s) {
	case "low", "minimal":
		return pb.Effort_EFFORT_LOW, nil
	case "medium":
		return pb.Effort_EFFORT_MEDIUM, nil
	case "high":
		return pb.Effort_EFFORT_HIGH, nil
	}
	return 0, invalid("unknown effort %q (use low, medium or high)", s)
}

// attributes builds the routing attributes from OpenAI's metadata and user
// fields. metadata.action_id is lifted out as the usage action label.
func attributes(metadata map[string]string, user string) (attrs map[string]string, actionID string) {
	attrs = make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		attrs[k] = v
	}
	if user != "" && attrs["user_id"] == "" {
		attrs["user_id"] = user
	}
	actionID = attrs["action_id"]
	delete(attrs, "action_id")
	return attrs, actionID
}

func (req *chatRequest) toProto() (*pb.GenerateTextRequest, error) {
	if req.N != nil && *req.N != 1 {
		return nil, invalid("n must be 1")
	}
	if len(req.Messages) == 0 {
		return nil, invalid("messages is required")
	}
	effort, override, err := route(req.Model, req.ReasoningEffort)
	if err != nil {
		return nil, err
	}

	var tools []*pb.Tool
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		var rawTools []chatTool
		if err := json.Unmarshal(req.Tools, &rawTools); err != nil {
			return nil, invalid("tools must be a list of tool objects")
		}
		for _, rt := range rawTools {
			t := &pb.Tool{
				Type: rt.Type,
				Function: &pb.FunctionDefinition{
					Name:        rt.Function.Name,
					Description: rt.Function.Description,
				},
			}
			if rt.Function.Strict != nil {
				t.Function.Strict = *rt.Function.Strict
			}
			if len(rt.Function.Parameters) > 0 && string(rt.Function.Parameters) != "null" {
				params, err := responseSchemaFromJSON(rt.Function.Parameters)
				if err != nil {
					return nil, invalid("invalid tool parameters schema: %v", err)
				}
				t.Function.Parameters = params
			}
			tools = append(tools, t)
		}
	}

	var toolChoice *pb.ToolChoice
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var s string
		if err := json.Unmarshal(req.ToolChoice, &s); err == nil {
			switch s {
			case "auto":
				toolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_AUTO}
			case "none":
				toolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_NONE}
			case "required":
				toolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_REQUIRED}
			default:
				return nil, invalid("unsupported tool_choice %q", s)
			}
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &obj); err != nil {
				return nil, invalid("tool_choice must be a string or object")
			}
			toolChoice = &pb.ToolChoice{
				Mode:                 pb.ToolChoice_MODE_SPECIFIC,
				SpecificFunctionName: obj.Function.Name,
			}
		}
	}

	out := &pb.GenerateTextRequest{
		Effort:             effort,
		ModelOverride:      override,
		MaxOutputTokens:    req.MaxCompletionTokens,
		EnableGoogleSearch: req.EnableGoogleSearch,
		Tools:              tools,
		ToolChoice:         toolChoice,
	}
	if out.MaxOutputTokens == 0 {
		out.MaxOutputTokens = req.MaxTokens
	}
	if out.MaxOutputTokens < 0 {
		return nil, invalid("max_tokens must not be negative")
	}
	out.Attributes, out.ActionId = attributes(req.Metadata, req.User)

	var system []string
	for i, m := range req.Messages {
		text, err := m.text()
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, text)
		case "user":
			out.Messages = append(out.Messages, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: text})
		case "assistant":
			var tcs []*pb.ToolCall
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, &pb.ToolCall{
					Id:   tc.ID,
					Type: tc.Type,
					Function: &pb.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
			out.Messages = append(out.Messages, &pb.ChatMessage{
				Role:      pb.MessageRole_MESSAGE_ROLE_ASSISTANT,
				Text:      text,
				ToolCalls: tcs,
			})
		case "tool":
			out.Messages = append(out.Messages, &pb.ChatMessage{
				Role:       pb.MessageRole_MESSAGE_ROLE_TOOL,
				Text:       text,
				ToolCallId: m.ToolCallID,
			})
		default:
			return nil, invalid("messages[%d]: role %q is not supported", i, m.Role)
		}
	}
	out.BaseSystemInstruction = strings.Join(system, "\n\n")

	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "", "text":
		case "json_object":
			out.RequestInstruction = "Respond with a single valid JSON object and nothing else."
		case "json_schema":
			if rf.JSONSchema == nil || len(rf.JSONSchema.Schema) == 0 {
				return nil, invalid("response_format.json_schema.schema is required")
			}
			if out.ResponseSchema, err = responseSchemaFromJSON(rf.JSONSchema.Schema); err != nil {
				return nil, invalid("%v", err)
			}
		default:
			return nil, invalid("response_format.type %q is not supported", rf.Type)
		}
	}
	return out, nil
}

func (a *OpenAIAPI) chatCompletions(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var req chatRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	preq, err := req.toProto()
	if err != nil {
		return err
	}

	id, created := "chatcmpl-"+randomID(), time.Now().Unix()
	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		return a.streamChat(ctx, w, preq, id, created, includeUsage)
	}

	resp, err := a.proxy.GenerateText(ctx, preq)
	if err != nil {
		return err
	}
	setResolvedHeaders(w, resp.ResolvedVendor, resp.MatchedRule)

	usage := usageJSON(resp.GetUsage())
	choices := make([]map[string]any, 0, len(resp.Choices))
	if len(resp.Details) > 0 {
		for i, d := range resp.Details {
			msg := map[string]any{"role": "assistant"}
			if d.Text != "" || len(d.ToolCalls) == 0 {
				msg["content"] = d.Text
			} else {
				msg["content"] = nil
			}
			if len(d.ToolCalls) > 0 {
				var tcs []map[string]any
				for _, tc := range d.ToolCalls {
					tcs = append(tcs, map[string]any{
						"id":   tc.Id,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						},
					})
				}
				msg["tool_calls"] = tcs
			}
			finish := d.FinishReason
			if finish == "" {
				finish = "stop"
			}
			choices = append(choices, map[string]any{
				"index":         i,
				"message":       msg,
				"finish_reason": finish,
			})
		}
	} else {
		for i, c := range resp.Choices {
			choices = append(choices, map[string]any{
				"index":         i,
				"message":       map[string]any{"role": "assistant", "content": c},
				"finish_reason": "stop",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "chat.completion", "created": created,
		"model": resp.ResolvedModel, "choices": choices, "usage": usage,
	})
	return nil
}

func (a *OpenAIAPI) streamChat(ctx context.Context, w http.ResponseWriter, preq *pb.GenerateTextRequest,
	id string, created int64, includeUsage bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return status.Error(codes.Internal, "streaming unsupported by response writer")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	s := &httpStreamAdapter{
		ctx:          ctx,
		w:            w,
		flusher:      flusher,
		id:           id,
		created:      created,
		includeUsage: includeUsage,
	}

	err := a.proxy.GenerateTextStream(&pb.GenerateTextStreamRequest{Request: preq}, s)
	if err != nil {
		errData, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "api_error",
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errData)
	}

	if s.lastUsage != nil && includeUsage {
		usageBody := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   s.resolvedModel,
			"choices": []any{},
			"usage":   usageJSON(s.lastUsage),
		}
		b, _ := json.Marshal(usageBody)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
	return nil
}

type httpStreamAdapter struct {
	grpc.ServerStream
	ctx           context.Context
	w             http.ResponseWriter
	flusher       http.Flusher
	id            string
	created       int64
	resolvedModel string
	includeUsage  bool
	lastUsage     *pb.TokensUsage
}

func (s *httpStreamAdapter) Context() context.Context {
	return s.ctx
}

func (s *httpStreamAdapter) Send(chunk *pb.GenerateTextChunk) error {
	if chunk.ResolvedModel != "" {
		s.resolvedModel = chunk.ResolvedModel
	}
	if chunk.Usage != nil {
		s.lastUsage = chunk.Usage
	}

	delta := map[string]any{}
	if chunk.Delta != "" {
		delta["content"] = chunk.Delta
	}
	if len(chunk.ToolCallChunks) > 0 {
		var tcDeltas []map[string]any
		for _, tc := range chunk.ToolCallChunks {
			tcMap := map[string]any{
				"index": tc.Index,
			}
			if tc.Id != "" {
				tcMap["id"] = tc.Id
			}
			if tc.Type != "" {
				tcMap["type"] = tc.Type
			}
			fnMap := map[string]any{}
			if tc.Name != "" {
				fnMap["name"] = tc.Name
			}
			if tc.ArgumentsDelta != "" {
				fnMap["arguments"] = tc.ArgumentsDelta
			}
			tcMap["function"] = fnMap
			tcDeltas = append(tcDeltas, tcMap)
		}
		delta["tool_calls"] = tcDeltas
	}

	var finishReason any
	if chunk.FinishReason != "" {
		finishReason = chunk.FinishReason
	}

	body := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.resolvedModel,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	b, _ := json.Marshal(body)
	_, err := fmt.Fprintf(s.w, "data: %s\n\n", b)
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func usageJSON(u *pb.TokensUsage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.GetInput(),
		"completion_tokens": u.GetOutput(),
		"total_tokens":      u.GetInput() + u.GetOutput(),
	}
}

func setResolvedHeaders(w http.ResponseWriter, vendor, rule string) {
	if vendor != "" {
		w.Header().Set("X-LLM-Proxy-Vendor", vendor)
	}
	if rule != "" {
		w.Header().Set("X-LLM-Proxy-Rule", rule)
	}
}

// ---- speech ----

type speechRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
	// Proxy extensions: pronunciation language and usage labels.
	Language string            `json:"language"`
	Metadata map[string]string `json:"metadata"`
	User     string            `json:"user"`
}

func (a *OpenAIAPI) audioSpeech(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var req speechRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.Input == "" {
		return invalid("input is required")
	}
	_, override, err := route(req.Model, "")
	if err != nil {
		return err
	}
	attrs, actionID := attributes(req.Metadata, req.User)
	resp, err := a.proxy.SynthesizeSpeech(ctx, &pb.SynthesizeSpeechRequest{
		Text: req.Input, Language: req.Language, Attributes: attrs, ActionId: actionID, ModelOverride: override,
	})
	if err != nil {
		return err
	}
	setResolvedHeaders(w, resp.ResolvedVendor, resp.MatchedRule)
	w.Header().Set("X-LLM-Proxy-Model", resp.ResolvedModel)
	mime := resp.MimeType
	if mime == "" {
		mime = "audio/ogg"
	}
	w.Header().Set("Content-Type", mime)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Audio)
	return nil
}

// ---- images ----

type imageRequest struct {
	Model          string            `json:"model"`
	Prompt         string            `json:"prompt"`
	N              *int              `json:"n"`
	ResponseFormat string            `json:"response_format"`
	Metadata       map[string]string `json:"metadata"`
	User           string            `json:"user"`
}

func (a *OpenAIAPI) imageGenerations(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var req imageRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.Prompt == "" {
		return invalid("prompt is required")
	}
	if req.N != nil && *req.N != 1 {
		return invalid("n must be 1")
	}
	if req.ResponseFormat == "url" {
		return invalid("response_format url is not supported; images are returned as b64_json")
	}
	_, override, err := route(req.Model, "")
	if err != nil {
		return err
	}
	attrs, actionID := attributes(req.Metadata, req.User)
	resp, err := a.proxy.GenerateImage(ctx, &pb.GenerateImageRequest{
		Prompt: req.Prompt, Attributes: attrs, ActionId: actionID, ModelOverride: override,
	})
	if err != nil {
		return err
	}
	setResolvedHeaders(w, resp.ResolvedVendor, resp.MatchedRule)
	w.Header().Set("X-LLM-Proxy-Model", resp.ResolvedModel)
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(resp.Image)}},
	})
	return nil
}

// ---- judge (proxy extension, protobuf JSON) ----

func (a *OpenAIAPI) judge(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	if err != nil {
		return invalid("cannot read body: %v", err)
	}
	var req pb.JudgeRequest
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, &req); err != nil {
		return invalid("invalid JudgeRequest: %v", err)
	}
	resp, err := a.proxy.Judge(ctx, &req)
	if err != nil {
		return err
	}
	setResolvedHeaders(w, resp.ResolvedVendor, resp.MatchedRule)
	out, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(resp)
	if err != nil {
		return status.Error(codes.Internal, "cannot encode response")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	return nil
}

// ---- models ----

func modelJSON(m *pb.ModelInfo) map[string]any {
	efforts := make([]string, 0, len(m.Efforts))
	for _, e := range m.Efforts {
		efforts = append(efforts, strings.ToLower(strings.TrimPrefix(e.String(), "EFFORT_")))
	}
	return map[string]any{
		"id": m.Id, "object": "model", "created": 0, "owned_by": m.Vendor,
		"efforts": efforts, "capabilities": m.Capabilities,
	}
}

func (a *OpenAIAPI) listModels(ctx context.Context, w http.ResponseWriter, _ *http.Request) error {
	resp, err := a.proxy.ListModels(ctx, &pb.ListModelsRequest{})
	if err != nil {
		return err
	}
	data := make([]map[string]any, 0, len(resp.Models))
	for _, m := range resp.Models {
		data = append(data, modelJSON(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
	return nil
}

func (a *OpenAIAPI) getModel(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	resp, err := a.proxy.ListModels(ctx, &pb.ListModelsRequest{})
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	for _, m := range resp.Models {
		if m.Id == id {
			writeJSON(w, http.StatusOK, modelJSON(m))
			return nil
		}
	}
	return status.Errorf(codes.NotFound, "model %q not found", id)
}

// ---- errors ----

var reasonRe = regexp.MustCompile(`^[a-z_]+(:[a-z_]+)?$`)

// httpStatus maps a gRPC code to the closest HTTP status and OpenAI error type.
func httpStatus(c codes.Code) (int, string) {
	switch c {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest, "invalid_request_error"
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "authentication_error"
	case codes.PermissionDenied:
		return http.StatusForbidden, "permission_error"
	case codes.NotFound:
		return http.StatusNotFound, "invalid_request_error"
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, "rate_limit_error"
	case codes.Unavailable:
		return http.StatusServiceUnavailable, "server_error"
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, "server_error"
	case codes.Canceled:
		return 499, "server_error"
	case codes.Unimplemented:
		return http.StatusNotImplemented, "server_error"
	default:
		return http.StatusInternalServerError, "server_error"
	}
}

func writeAPIError(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code, typ := httpStatus(st.Code())
	msg := st.Message()
	// Machine-readable reasons ("throttled:rpm", "no_capable_model") become the code.
	var errCode any
	if reasonRe.MatchString(msg) {
		errCode = msg
	}
	if st.Code() == codes.Internal && errCode == nil {
		msg = "internal error"
	}
	if st.Code() == codes.ResourceExhausted {
		w.Header().Set("Retry-After", "1")
	}
	if msg == "vendor_error" {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": nil, "code": errCode,
	}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, `{"error":{"message":"encode failure","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		log.Warn().Err(err).Msg("random id")
	}
	return hex.EncodeToString(b)
}
