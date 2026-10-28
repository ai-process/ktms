package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/grpcapi"
	"github.com/ai-process/ktms/internal/safefetch"
)

// The Anthropic Messages API runs on the same GenerateText pipeline as chat
// completions: routing, fallback, limits, guardrails, cache and usage are
// shared, and whatever model answers is rendered in the Messages format.

type messagesRequest struct {
	Model      string               `json:"model"`
	System     json.RawMessage      `json:"system"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int32                `json:"max_tokens"`
	Stream     bool                 `json:"stream"`
	Tools      []anthropicTool      `json:"tools"`
	ToolChoice *anthropicToolChoice `json:"tool_choice"`
	Metadata   map[string]string    `json:"metadata"`
	// Proxy extensions, as on chat completions.
	EnableCache *bool  `json:"enable_cache,omitempty"`
	Placement   string `json:"placement,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text"`
	Source    *anthropicSource `json:"source"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Input     json.RawMessage  `json:"input"`
	ToolUseID string           `json:"tool_use_id"`
	Content   json.RawMessage  `json:"content"`
	IsError   bool             `json:"is_error"`
	Title     string           `json:"title"`
}

type anthropicSource struct {
	Type      string          `json:"type"`
	MediaType string          `json:"media_type"`
	Data      string          `json:"data"`
	URL       string          `json:"url"`
	Content   json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// anthropicBlocks reads content that is either a string or a list of blocks.
func anthropicBlocks(raw json.RawMessage, what string) ([]anthropicBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, invalid("%s must be a string or a list of content blocks", what)
	}
	return blocks, nil
}

// skippedBlocks are assistant blocks another model can neither read nor
// continue: thinking is signed by the model that wrote it, and server tools
// are not forwarded.
var skippedBlocks = map[string]bool{
	"thinking": true, "redacted_thinking": true, "server_tool_use": true, "web_search_tool_result": true,
}

func (req *messagesRequest) toProto(ctx context.Context, headerPlacement string) (*pb.GenerateTextRequest, error) {
	if len(req.Messages) == 0 {
		return nil, invalid("messages: at least one message is required")
	}
	placement := req.Placement
	if headerPlacement != "" {
		placement = headerPlacement
	}
	dec, err := route(req.Model, "", placement)
	if err != nil {
		return nil, err
	}
	out := &pb.GenerateTextRequest{
		Effort:          dec.Effort,
		ModelOverride:   dec.ModelOverride,
		MaxOutputTokens: req.MaxTokens,
		Placement:       dec.Placement,
		AutoRoute:       dec.IsAuto,
		EffortSpecified: dec.EffortSpecified,
	}
	out.Attributes, out.ActionId = attributes(req.Metadata, "")

	system, err := anthropicBlocks(req.System, "system")
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, b := range system {
		if b.Type != "text" {
			return nil, invalid("system: block type %q is not supported, only text", b.Type)
		}
		texts = append(texts, b.Text)
	}
	out.BaseSystemInstruction = strings.Join(texts, "\n\n")

	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "custom" {
			return nil, invalid("tools: server tool %q is not supported; KTMS forwards client tools only", t.Type)
		}
		if t.Name == "" {
			return nil, invalid("tools: name is required")
		}
		tool := &pb.Tool{Type: "function", Function: &pb.FunctionDefinition{Name: t.Name, Description: t.Description}}
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
			if tool.Function.Parameters, err = responseSchemaFromJSON(t.InputSchema); err != nil {
				return nil, invalid("tools: %s: invalid input_schema: %v", t.Name, err)
			}
		}
		out.Tools = append(out.Tools, tool)
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			out.ToolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_AUTO}
		case "any":
			out.ToolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_REQUIRED}
		case "none":
			out.ToolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_NONE}
		case "tool":
			if tc.Name == "" {
				return nil, invalid("tool_choice: name is required for type tool")
			}
			out.ToolChoice = &pb.ToolChoice{Mode: pb.ToolChoice_MODE_SPECIFIC, SpecificFunctionName: tc.Name}
		default:
			return nil, invalid("tool_choice: type %q is not supported", tc.Type)
		}
	}

	for i, m := range req.Messages {
		blocks, err := anthropicBlocks(m.Content, fmt.Sprintf("messages[%d].content", i))
		if err != nil {
			return nil, err
		}
		var msgs []*pb.ChatMessage
		switch m.Role {
		case "user":
			msgs, err = userMessages(ctx, blocks)
		case "assistant":
			msgs, err = assistantMessage(blocks)
		default:
			err = invalid("role %q is not supported, use user or assistant", m.Role)
		}
		if err != nil {
			return nil, prefixInvalid(fmt.Sprintf("messages[%d]", i), err)
		}
		out.Messages = append(out.Messages, msgs...)
	}
	return out, nil
}

func prefixInvalid(prefix string, err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.InvalidArgument {
		return invalid("%s: %s", prefix, st.Message())
	}
	return err
}

// userMessages turns a user turn into its tool results, each a tool message,
// then one user message with the rest. Images in tool results join that user
// message: tool messages carry text only.
func userMessages(ctx context.Context, blocks []anthropicBlock) ([]*pb.ChatMessage, error) {
	var out []*pb.ChatMessage
	var texts []string
	var parts []*pb.ContentPart
	addText := func(s string) {
		texts = append(texts, s)
		parts = append(parts, &pb.ContentPart{Part: &pb.ContentPart_Text{Text: &pb.TextPart{Text: s}}})
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			addText(b.Text)
		case "image":
			p, err := imagePart(ctx, b.Source)
			if err != nil {
				return nil, err
			}
			parts = append(parts, p)
		case "document":
			p, text, err := documentPart(ctx, b)
			if err != nil {
				return nil, err
			}
			if p != nil {
				parts = append(parts, p)
			} else {
				addText(text)
			}
		case "tool_result":
			inner, err := anthropicBlocks(b.Content, "tool_result.content")
			if err != nil {
				return nil, err
			}
			var result []string
			for _, ib := range inner {
				switch ib.Type {
				case "text":
					result = append(result, ib.Text)
				case "image":
					p, err := imagePart(ctx, ib.Source)
					if err != nil {
						return nil, err
					}
					parts = append(parts, p)
				default:
					return nil, invalid("tool_result: block type %q is not supported", ib.Type)
				}
			}
			text := strings.Join(result, "\n")
			if b.IsError {
				text = "Error: " + text
			}
			out = append(out, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_TOOL, Text: text, ToolCallId: b.ToolUseID})
		default:
			return nil, invalid("block type %q is not supported in a user message", b.Type)
		}
	}
	if len(parts) > 0 {
		out = append(out, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: strings.Join(texts, "\n\n"), Parts: parts})
	}
	return out, nil
}

func assistantMessage(blocks []anthropicBlock) ([]*pb.ChatMessage, error) {
	msg := &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_ASSISTANT}
	var texts []string
	for _, b := range blocks {
		switch {
		case b.Type == "text":
			texts = append(texts, b.Text)
		case b.Type == "tool_use":
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			msg.ToolCalls = append(msg.ToolCalls, &pb.ToolCall{
				Id: b.ID, Type: "function", Function: &pb.FunctionCall{Name: b.Name, Arguments: args},
			})
		case skippedBlocks[b.Type]:
		default:
			return nil, invalid("block type %q is not supported in an assistant message", b.Type)
		}
	}
	msg.Text = strings.Join(texts, "\n\n")
	if msg.Text == "" && len(msg.ToolCalls) == 0 {
		return nil, nil
	}
	return []*pb.ChatMessage{msg}, nil
}

// mediaBytes reads a base64 or https source; url sources go through safefetch
// like image_url on chat completions.
func mediaBytes(ctx context.Context, src *anthropicSource, what string) (mime string, data []byte, err error) {
	if src == nil {
		return "", nil, invalid("%s: source is required", what)
	}
	switch src.Type {
	case "base64":
		if data, err = base64.StdEncoding.DecodeString(src.Data); err != nil {
			return "", nil, invalid("%s: invalid base64 data: %v", what, err)
		}
		mime = src.MediaType
	case "url":
		if !strings.HasPrefix(src.URL, "https://") {
			return "", nil, invalid("%s: url %q must be https", what, src.URL)
		}
		if data, mime, err = safefetch.FetchURL(ctx, src.URL, maxRequestBytes); err != nil {
			return "", nil, invalid("%s: failed to fetch %s: %v", what, src.URL, err)
		}
	default:
		return "", nil, invalid("%s: source type %q is not supported, use base64 or url", what, src.Type)
	}
	if err := safefetch.ValidateMediaMimeType(mime); err != nil {
		return "", nil, invalid("%s: %v", what, err)
	}
	return mime, data, nil
}

func imagePart(ctx context.Context, src *anthropicSource) (*pb.ContentPart, error) {
	mime, data, err := mediaBytes(ctx, src, "image")
	if err != nil {
		return nil, err
	}
	return &pb.ContentPart{Part: &pb.ContentPart_Image{Image: &pb.ImagePart{
		MimeType: mime, Data: data, Url: src.URL, Detail: "auto",
	}}}, nil
}

// documentPart returns a document part, or the text of a plain-text or
// content-block document.
func documentPart(ctx context.Context, b anthropicBlock) (*pb.ContentPart, string, error) {
	if b.Source != nil {
		switch b.Source.Type {
		case "text":
			return nil, b.Source.Data, nil
		case "content":
			inner, err := anthropicBlocks(b.Source.Content, "document.source.content")
			if err != nil {
				return nil, "", err
			}
			var texts []string
			for _, ib := range inner {
				if ib.Type != "text" {
					return nil, "", invalid("document: content block type %q is not supported", ib.Type)
				}
				texts = append(texts, ib.Text)
			}
			return nil, strings.Join(texts, "\n\n"), nil
		}
	}
	mime, data, err := mediaBytes(ctx, b.Source, "document")
	if err != nil {
		return nil, "", err
	}
	return &pb.ContentPart{Part: &pb.ContentPart_Document{Document: &pb.DocumentPart{
		MimeType: mime, Data: data, FileName: b.Title,
	}}}, "", nil
}

func (a *OpenAIAPI) messages(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var req messagesRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.MaxTokens < 1 {
		return invalid("max_tokens: must be at least 1")
	}
	preq, err := req.toProto(ctx, getPlacementHeader(r))
	if err != nil {
		return err
	}
	if req.EnableCache != nil {
		preq.EnableCache = *req.EnableCache
	} else if h := getCacheHeader(r); h != "" {
		preq.EnableCache = h == "true" || h == "1"
	}

	id := "msg_" + randomID()
	if req.Stream {
		return a.streamMessages(ctx, w, preq, id)
	}
	resp, err := a.proxy.GenerateText(ctx, preq)
	if err != nil {
		return err
	}
	setRouteHeaders(w, resp)
	setCacheHeader(w, preq.EnableCache, resp.Cached)

	var text, finish string
	var calls []*pb.ToolCall
	if len(resp.Details) > 0 {
		d := resp.Details[0]
		text, finish, calls = d.Text, d.FinishReason, d.ToolCalls
	} else if len(resp.Choices) > 0 {
		text = resp.Choices[0]
	}
	content := []map[string]any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range calls {
		content = append(content, map[string]any{
			"type": "tool_use", "id": toolUseID(tc.GetId()), "name": tc.GetFunction().GetName(),
			"input": toolInput(tc.GetFunction().GetArguments()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": resp.ResolvedModel,
		"content": content, "stop_reason": stopReason(finish, len(calls) > 0), "stop_sequence": nil,
		"usage": anthropicUsage(resp.GetUsage(), 0),
	})
	return nil
}

// toolUseID keeps the vendor's call id, which the client sends back in its
// tool_result, and makes one up for vendors that give none.
func toolUseID(id string) string {
	if id == "" {
		return "toolu_" + randomID()
	}
	return id
}

// toolInput is a tool call's arguments as the JSON object Messages clients
// expect; arguments that are not one become {}.
func toolInput(args string) json.RawMessage {
	var obj map[string]json.RawMessage
	if strings.TrimSpace(args) == "" || json.Unmarshal([]byte(args), &obj) != nil || obj == nil {
		if strings.TrimSpace(args) != "" {
			log.Warn().Int("bytes", len(args)).Msg("messages: tool call arguments are not a JSON object; sending {}")
		}
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

func stopReason(finish string, toolCalls bool) string {
	switch {
	case finish == "length":
		return "max_tokens"
	case finish == "content_filter":
		return "refusal"
	case toolCalls || finish == "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// anthropicUsage splits the cached tokens out of the input count, as the
// Messages API reports them; estimate stands in before the vendor reports.
func anthropicUsage(u *pb.TokensUsage, estimate int64) map[string]any {
	input := u.GetInput() - u.GetCacheRead() - u.GetCacheWrite()
	if u == nil {
		input = estimate
	}
	return map[string]any{
		"input_tokens":                max(input, 0),
		"output_tokens":               u.GetOutput(),
		"cache_read_input_tokens":     u.GetCacheRead(),
		"cache_creation_input_tokens": u.GetCacheWrite(),
	}
}

func (a *OpenAIAPI) streamMessages(ctx context.Context, w http.ResponseWriter, preq *pb.GenerateTextRequest, id string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return status.Error(codes.Internal, "streaming unsupported by response writer")
	}
	s := &messagesStream{
		ctx: ctx, w: w, flusher: flusher, id: id, block: -1,
		estimate: grpcapi.EstimateTokens(preq), enableCache: preq.EnableCache, toolBlocks: map[int32]bool{},
	}
	err := a.proxy.GenerateTextStream(&pb.GenerateTextStreamRequest{Request: preq}, s)
	if err != nil && !s.started {
		return err
	}
	if err == nil {
		s.start(nil)
		s.finish()
		return nil
	}
	// Headers are out: the error goes in the stream, as the Messages API sends it.
	st, _ := status.FromError(err)
	code, _ := httpStatus(st.Code())
	msg := st.Message()
	if st.Code() == codes.Internal {
		msg = "internal error"
	}
	s.event("error", map[string]any{"type": "error", "error": map[string]any{"type": anthropicErrorType(code), "message": msg}})
	return nil
}

// messagesStream renders GenerateText chunks as Messages SSE events. Text
// and each tool call are content blocks, opened and closed in order.
type messagesStream struct {
	grpc.ServerStream
	ctx         context.Context
	w           http.ResponseWriter
	flusher     http.Flusher
	id          string
	estimate    int64
	enableCache bool
	cacheHit    bool
	started     bool
	model       string
	block       int  // index of the last block opened
	open        bool // whether that block is still open
	blockTool   bool
	tool        int32 // vendor index of the open tool call
	toolBlocks  map[int32]bool
	toolCalls   bool
	finishedAs  string
	usage       *pb.TokensUsage
}

func (s *messagesStream) Context() context.Context { return s.ctx }

func (s *messagesStream) event(name string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	s.flusher.Flush()
}

func (s *messagesStream) start(chunk *pb.GenerateTextChunk) {
	if s.started {
		return
	}
	if chunk != nil {
		setRouteHeaders(s.w, chunk)
		s.model = chunk.ResolvedModel
	}
	setCacheHeader(s.w, s.enableCache, s.cacheHit)
	startEventStream(s.w, s.flusher)
	s.started = true
	s.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "model": s.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": anthropicUsage(nil, s.estimate),
	}})
	s.event("ping", map[string]any{"type": "ping"})
}

func (s *messagesStream) openBlock(block map[string]any, tool bool, index int32) {
	s.closeBlock()
	s.block++
	s.open, s.blockTool, s.tool = true, tool, index
	s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.block, "content_block": block})
}

func (s *messagesStream) closeBlock() {
	if !s.open {
		return
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.block})
	s.open = false
}

func (s *messagesStream) delta(delta map[string]any) {
	s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.block, "delta": delta})
}

func (s *messagesStream) Send(chunk *pb.GenerateTextChunk) error {
	if chunk.Cached {
		s.cacheHit = true
	}
	s.start(chunk)
	if chunk.Delta != "" {
		if !s.open || s.blockTool {
			s.openBlock(map[string]any{"type": "text", "text": ""}, false, -1)
		}
		s.delta(map[string]any{"type": "text_delta", "text": chunk.Delta})
	}
	for _, tc := range chunk.ToolCallChunks {
		if !s.open || !s.blockTool || s.tool != tc.Index {
			if s.toolBlocks[tc.Index] {
				return status.Errorf(codes.Internal, "tool call %d continued after the next content block began", tc.Index)
			}
			s.toolBlocks[tc.Index] = true
			s.toolCalls = true
			s.openBlock(map[string]any{"type": "tool_use", "id": toolUseID(tc.Id), "name": tc.Name, "input": map[string]any{}}, true, tc.Index)
		}
		if tc.ArgumentsDelta != "" {
			s.delta(map[string]any{"type": "input_json_delta", "partial_json": tc.ArgumentsDelta})
		}
	}
	if chunk.FinishReason != "" {
		s.finishedAs = chunk.FinishReason
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	return nil
}

func (s *messagesStream) finish() {
	s.closeBlock()
	usage := anthropicUsage(s.usage, s.estimate)
	s.event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReason(s.finishedAs, s.toolCalls), "stop_sequence": nil},
		"usage": usage})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

func writeAnthropicError(w http.ResponseWriter, err error) {
	code, _, msg, _ := apiError(w, err)
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{
		"type": anthropicErrorType(code), "message": msg,
	}})
}

// anthropicErrorType names the Messages API error for an HTTP status; the
// statuses themselves are those of the HTTP error table.
func anthropicErrorType(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	default:
		return "api_error"
	}
}

// countTokens estimates like the token budgets do (a rune count), adding the
// tool definitions and tool calls the estimate leaves out.
func (a *OpenAIAPI) countTokens(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var req messagesRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	preq, err := req.toProto(ctx, "")
	if err != nil {
		return err
	}
	n := grpcapi.EstimateTokens(preq)
	extra := 0
	for _, t := range req.Tools {
		extra += utf8.RuneCountInString(t.Name) + utf8.RuneCountInString(t.Description) + utf8.RuneCount(t.InputSchema)
	}
	for _, m := range preq.Messages {
		for _, tc := range m.ToolCalls {
			extra += utf8.RuneCountInString(tc.GetFunction().GetName()) + utf8.RuneCountInString(tc.GetFunction().GetArguments())
		}
	}
	setObservabilityHeader(w, "Token-Count", "estimate")
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n + int64(extra/4)})
	return nil
}
