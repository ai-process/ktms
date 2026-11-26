package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/grpcapi"
	"github.com/ai-process/ktms/internal/safefetch"
)

// The OpenAI Responses API runs on the same GenerateText pipeline as chat
// completions. KTMS keeps no response state: each request carries its whole
// input, as Codex CLI and the Agents SDK send it.

type responsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"`
	Instructions       string              `json:"instructions"`
	Tools              []json.RawMessage   `json:"tools"`
	ToolChoice         json.RawMessage     `json:"tool_choice"`
	Text               *responsesText      `json:"text"`
	MaxOutputTokens    int32               `json:"max_output_tokens"`
	Temperature        *float64            `json:"temperature"`
	TopP               *float64            `json:"top_p"`
	Reasoning          *responsesReasoning `json:"reasoning"`
	User               string              `json:"user"`
	Metadata           map[string]string   `json:"metadata"`
	Stream             bool                `json:"stream"`
	Store              *bool               `json:"store"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls"`
	PreviousResponseID string              `json:"previous_response_id"`
	Conversation       json.RawMessage     `json:"conversation"`
	Background         bool                `json:"background"`
	Prompt             json.RawMessage     `json:"prompt"`
	// Proxy extensions, as on chat completions.
	EnableCache        *bool  `json:"enable_cache,omitempty"`
	Placement          string `json:"placement,omitempty"`
	DataClassification string `json:"data_classification,omitempty"`

	ignored []string        // fields taken but not acted on, for X-KTMS-Ignored-Params
	custom  map[string]bool // custom (freeform) tools, sent to the model as functions
}

// responsesHandled are the top-level fields toProto reads; any other field
// is reported in X-KTMS-Ignored-Params.
var responsesHandled = map[string]bool{
	"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true, "text": true,
	"max_output_tokens": true, "temperature": true, "top_p": true, "reasoning": true, "user": true,
	"metadata": true, "stream": true, "store": true, "parallel_tool_calls": true, "previous_response_id": true,
	"conversation": true, "background": true, "prompt": true,
	"enable_cache": true, "placement": true, "data_classification": true,
}

type responsesText struct {
	Format    *responsesFormat `json:"format"`
	Verbosity string           `json:"verbosity"`
}

type responsesFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
}

type responsesReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
	Format      *struct {
		Type       string `json:"type"`
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	} `json:"format"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
}

type responsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Refusal  string `json:"refusal"`
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
	Detail   string `json:"detail"`
	FileData string `json:"file_data"`
	FileURL  string `json:"file_url"`
	Filename string `json:"filename"`
}

// customToolSchema is how a freeform tool reaches models that only call
// functions: its text is the one string argument.
const customToolSchema = `{"type":"object","properties":{"input":{"type":"string","description":"The tool input as plain text."}},"required":["input"]}`

func decodeResponsesRequest(r *http.Request) (*responsesRequest, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	if err != nil {
		return nil, invalid("invalid JSON body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, invalid("request body is empty")
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, invalid("invalid JSON body: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, invalid("request body must be a JSON object")
	}
	for k, v := range fields {
		if !responsesHandled[k] && string(v) != "null" {
			req.ignored = append(req.ignored, k)
		}
	}
	return &req, nil
}

// responsesEffort maps the Responses effort levels onto KTMS's three.
func responsesEffort(e string) string {
	switch strings.ToLower(e) {
	case "none", "minimal":
		return "low"
	case "xhigh":
		return "high"
	}
	return e
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func (req *responsesRequest) toProto(ctx context.Context, headerPlacement string) (*pb.GenerateTextRequest, error) {
	switch {
	case req.PreviousResponseID != "":
		return nil, invalid("previous_response_id is not supported: KTMS keeps no response state, send the whole conversation in input")
	case !isNull(req.Conversation):
		return nil, invalid("conversation is not supported: KTMS keeps no response state, send the whole conversation in input")
	case req.Background:
		return nil, invalid("background is not supported; use stream for long responses")
	case !isNull(req.Prompt):
		return nil, invalid("prompt is not supported: KTMS has no stored prompts, send instructions and input")
	case req.MaxOutputTokens < 0:
		return nil, invalid("max_output_tokens must not be negative")
	}
	placement := req.Placement
	if headerPlacement != "" {
		placement = headerPlacement
	}
	var effort string
	if req.Reasoning != nil {
		effort = responsesEffort(req.Reasoning.Effort)
		if req.Reasoning.Summary != "" {
			req.ignored = append(req.ignored, "reasoning.summary")
		}
	}
	dec, err := route(req.Model, effort, placement)
	if err != nil {
		return nil, err
	}
	out := &pb.GenerateTextRequest{
		Effort:             dec.Effort,
		ModelOverride:      dec.ModelOverride,
		MaxOutputTokens:    req.MaxOutputTokens,
		Placement:          dec.Placement,
		AutoRoute:          dec.IsAuto,
		EffortSpecified:    dec.EffortSpecified,
		Needs:              dec.Needs,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		DataClassification: req.DataClassification,
	}
	out.Attributes, out.ActionId = attributes(req.Metadata, req.User)
	if req.Store != nil && *req.Store {
		req.ignored = append(req.ignored, "store")
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		req.ignored = append(req.ignored, "parallel_tool_calls")
	}

	if out.Tools, err = req.tools(); err != nil {
		return nil, err
	}
	if out.ToolChoice, err = responsesToolChoice(req.ToolChoice); err != nil {
		return nil, err
	}
	if t := req.Text; t != nil {
		if t.Verbosity != "" {
			req.ignored = append(req.ignored, "text.verbosity")
		}
		if f := t.Format; f != nil {
			switch f.Type {
			case "", "text":
			case "json_object":
				out.RequestInstruction = "Respond with a single valid JSON object and nothing else."
			case "json_schema":
				if isNull(f.Schema) {
					return nil, invalid("text.format.schema is required for json_schema")
				}
				if out.ResponseSchema, err = responseSchemaFromJSON(f.Schema); err != nil {
					return nil, invalid("text.format.schema: %v", err)
				}
			default:
				return nil, invalid("text.format.type %q is not supported", f.Type)
			}
		}
	}

	in := &responsesInput{ctx: ctx}
	if req.Instructions != "" {
		in.system = append(in.system, req.Instructions)
	}
	if err := in.read(req.Input); err != nil {
		return nil, err
	}
	if len(in.msgs) == 0 {
		return nil, invalid("input: at least one message is required")
	}
	out.Messages = in.msgs
	out.BaseSystemInstruction = strings.Join(in.system, "\n\n")
	slices.Sort(req.ignored)
	return out, nil
}

// tools converts function and custom tools. Hosted OpenAI tools (web_search,
// file_search, mcp…) are dropped and reported: Codex sends web_search by
// default, and failing the whole request for it would break every session.
func (req *responsesRequest) tools() ([]*pb.Tool, error) {
	var out []*pb.Tool
	for i, raw := range req.Tools {
		var t responsesTool
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, invalid("tools[%d] must be a tool object", i)
		}
		switch t.Type {
		case "function", "custom":
		case "":
			return nil, invalid("tools[%d].type is required", i)
		default:
			req.ignored = append(req.ignored, "tools."+t.Type)
			continue
		}
		if t.Name == "" {
			return nil, invalid("tools[%d].name is required", i)
		}
		tool := &pb.Tool{Type: "function", Function: &pb.FunctionDefinition{Name: t.Name, Description: t.Description}}
		params := t.Parameters
		if t.Type == "custom" {
			if req.custom == nil {
				req.custom = map[string]bool{}
			}
			req.custom[t.Name] = true
			params = json.RawMessage(customToolSchema)
			if f := t.Format; f != nil && f.Type == "grammar" && f.Definition != "" {
				tool.Function.Description += fmt.Sprintf("\n\nThe input follows this %s grammar:\n%s", f.Syntax, f.Definition)
			}
		} else if t.Strict != nil {
			tool.Function.Strict = *t.Strict
		}
		if !isNull(params) {
			var err error
			if tool.Function.Parameters, err = responseSchemaFromJSON(params); err != nil {
				return nil, invalid("tools[%d]: invalid parameters schema: %v", i, err)
			}
		}
		out = append(out, tool)
	}
	return out, nil
}

func responsesToolChoice(raw json.RawMessage) (*pb.ToolChoice, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return &pb.ToolChoice{Mode: pb.ToolChoice_MODE_AUTO}, nil
		case "none":
			return &pb.ToolChoice{Mode: pb.ToolChoice_MODE_NONE}, nil
		case "required":
			return &pb.ToolChoice{Mode: pb.ToolChoice_MODE_REQUIRED}, nil
		}
		return nil, invalid("tool_choice %q is not supported", s)
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, invalid("tool_choice must be a string or an object")
	}
	if obj.Type != "function" && obj.Type != "custom" {
		return nil, invalid("tool_choice.type %q is not supported, use function or custom", obj.Type)
	}
	if obj.Name == "" {
		return nil, invalid("tool_choice.name is required")
	}
	return &pb.ToolChoice{Mode: pb.ToolChoice_MODE_SPECIFIC, SpecificFunctionName: obj.Name}, nil
}

// responsesInput turns input items into chat messages. Tool calls join the
// assistant message before them; images in tool outputs follow the outputs
// as a user message, since tool messages carry text only.
type responsesInput struct {
	ctx    context.Context
	system []string
	msgs   []*pb.ChatMessage
	media  []*pb.ContentPart
}

func (in *responsesInput) read(raw json.RawMessage) error {
	if isNull(raw) {
		return invalid("input is required")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		in.msgs = append(in.msgs, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: s})
		return nil
	}
	var items []responsesItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return invalid("input must be a string or a list of items")
	}
	for i, it := range items {
		if err := in.item(it); err != nil {
			return prefixInvalid(fmt.Sprintf("input[%d]", i), err)
		}
	}
	in.flushMedia()
	return nil
}

func (in *responsesInput) item(it responsesItem) error {
	typ := it.Type
	if typ == "" && it.Role != "" {
		typ = "message"
	}
	if typ != "function_call_output" && typ != "custom_tool_call_output" {
		in.flushMedia()
	}
	switch typ {
	case "message":
		return in.message(it)
	case "function_call", "custom_tool_call":
		args := it.Arguments
		if typ == "custom_tool_call" {
			b, err := json.Marshal(map[string]string{"input": it.Input})
			if err != nil {
				return err
			}
			args = string(b)
		}
		if args == "" {
			args = "{}"
		}
		call := &pb.ToolCall{Id: it.CallID, Type: "function", Function: &pb.FunctionCall{Name: it.Name, Arguments: args}}
		if n := len(in.msgs); n > 0 && in.msgs[n-1].Role == pb.MessageRole_MESSAGE_ROLE_ASSISTANT {
			in.msgs[n-1].ToolCalls = append(in.msgs[n-1].ToolCalls, call)
		} else {
			in.msgs = append(in.msgs, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_ASSISTANT, ToolCalls: []*pb.ToolCall{call}})
		}
		return nil
	case "function_call_output", "custom_tool_call_output":
		text, err := in.toolOutput(it.Output)
		if err != nil {
			return err
		}
		in.msgs = append(in.msgs, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_TOOL, Text: text, ToolCallId: it.CallID})
		return nil
	case "reasoning":
		// Reasoning is signed or encrypted by the model that wrote it; no other model can continue it.
		return nil
	case "item_reference":
		return invalid("item_reference is not supported: KTMS keeps no response state, send the item itself")
	case "":
		return invalid("type is required")
	default:
		return invalid("item type %q is not supported", typ)
	}
}

func (in *responsesInput) message(it responsesItem) error {
	text, parts, err := in.parts(it.Content)
	if err != nil {
		return err
	}
	switch it.Role {
	case "system", "developer":
		if hasMediaParts(parts) {
			return invalid("%s messages take text only", it.Role)
		}
		in.system = append(in.system, text)
	case "user":
		in.msgs = append(in.msgs, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: text, Parts: parts})
	case "assistant":
		if hasMediaParts(parts) {
			return invalid("assistant messages take text only")
		}
		in.msgs = append(in.msgs, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_ASSISTANT, Text: text})
	default:
		return invalid("role %q is not supported, use user, assistant, system or developer", it.Role)
	}
	return nil
}

// parts reads message content: a string or a list of input and output parts.
func (in *responsesInput) parts(raw json.RawMessage) (string, []*pb.ContentPart, error) {
	if isNull(raw) {
		return "", nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var list []responsesPart
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", nil, invalid("content must be a string or a list of parts")
	}
	var texts []string
	var parts []*pb.ContentPart
	for _, p := range list {
		switch p.Type {
		case "input_text", "output_text", "text", "refusal":
			t := p.Text
			if p.Type == "refusal" {
				t = p.Refusal
			}
			texts = append(texts, t)
			parts = append(parts, &pb.ContentPart{Part: &pb.ContentPart_Text{Text: &pb.TextPart{Text: t}}})
		case "input_image":
			if p.FileID != "" || p.ImageURL == "" {
				return "", nil, invalid("input_image needs image_url: KTMS has no file storage for file_id")
			}
			part, err := imageURLPart(in.ctx, p.ImageURL, p.Detail)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, part)
		case "input_file":
			part, err := responsesFile(p)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, part)
		default:
			return "", nil, invalid("content part type %q is not supported", p.Type)
		}
	}
	return strings.Join(texts, "\n\n"), parts, nil
}

// responsesFile reads input_file data, a data URI or bare base64.
func responsesFile(p responsesPart) (*pb.ContentPart, error) {
	if p.FileData == "" {
		return nil, invalid("input_file needs file_data: KTMS has no file storage for file_id or file_url")
	}
	mime, data := "application/pdf", []byte(nil)
	var err error
	if strings.HasPrefix(p.FileData, "data:") {
		if mime, data, err = safefetch.ParseDataURI(p.FileData); err != nil {
			return nil, invalid("input_file: invalid data URI: %v", err)
		}
	} else if data, err = base64.StdEncoding.DecodeString(p.FileData); err != nil {
		return nil, invalid("input_file: invalid base64 file_data: %v", err)
	}
	if err := safefetch.ValidateMediaMimeType(mime); err != nil {
		return nil, invalid("input_file: %v", err)
	}
	return &pb.ContentPart{Part: &pb.ContentPart_Document{Document: &pb.DocumentPart{
		MimeType: mime, Data: data, FileName: p.Filename,
	}}}, nil
}

// toolOutput reads a function call's output: a string or a list of parts,
// whose media wait for the user message after the outputs.
func (in *responsesInput) toolOutput(raw json.RawMessage) (string, error) {
	text, parts, err := in.parts(raw)
	if err != nil {
		return "", prefixInvalid("output", err)
	}
	for _, p := range parts {
		if p.GetText() == nil {
			in.media = append(in.media, p)
		}
	}
	return text, nil
}

func (in *responsesInput) flushMedia() {
	if len(in.media) == 0 {
		return
	}
	in.msgs = append(in.msgs, &pb.ChatMessage{Role: pb.MessageRole_MESSAGE_ROLE_USER, Parts: in.media})
	in.media = nil
}

// ---- response ----

// responseShape holds what every response object of one request repeats.
type responseShape struct {
	id      string
	created int64
	req     *responsesRequest
}

func (sh *responseShape) object(model, state string, output []map[string]any, usage, incomplete, errObj any) map[string]any {
	req := sh.req
	var effort any
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		effort = req.Reasoning.Effort
	}
	format := any(map[string]any{"type": "text"})
	if req.Text != nil && req.Text.Format != nil {
		format = req.Text.Format
	}
	var toolChoice any = "auto"
	if !isNull(req.ToolChoice) {
		toolChoice = req.ToolChoice
	}
	tools := req.Tools
	if tools == nil {
		tools = []json.RawMessage{}
	}
	metadata := req.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	parallel := req.ParallelToolCalls == nil || *req.ParallelToolCalls
	return map[string]any{
		"id": sh.id, "object": "response", "created_at": sh.created, "status": state, "model": model,
		"output": output, "usage": usage, "error": errObj, "incomplete_details": incomplete,
		"instructions": nullIfEmpty(req.Instructions), "max_output_tokens": nullIfZero(req.MaxOutputTokens),
		"parallel_tool_calls": parallel, "previous_response_id": nil, "store": false,
		"reasoning":   map[string]any{"effort": effort, "summary": nil},
		"temperature": req.Temperature, "top_p": req.TopP, "text": map[string]any{"format": format},
		"tool_choice": toolChoice, "tools": tools, "truncation": "disabled",
		"user": nullIfEmpty(req.User), "metadata": metadata,
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(n int32) any {
	if n == 0 {
		return nil
	}
	return n
}

// responseState is the response status for a finish reason, with the
// incomplete_details the Responses API gives when output was cut.
func responseState(finish string) (string, any) {
	switch finish {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	}
	return "completed", nil
}

func responsesUsage(u *pb.TokensUsage, estimate int64) map[string]any {
	input := u.GetInput()
	if u == nil {
		input = estimate
	}
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": u.GetCacheRead()},
		"output_tokens":         u.GetOutput(),
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          input + u.GetOutput(),
	}
}

func callID(id string) string {
	if id == "" {
		return "call_" + randomID()
	}
	return id
}

func orEmptyObject(args string) string {
	if strings.TrimSpace(args) == "" || args == "null" {
		return "{}"
	}
	return args
}

// customInput is the text of a custom tool call, which the model sent as
// the one argument of a function.
func customInput(args string) string {
	var v struct {
		Input *string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &v) == nil && v.Input != nil {
		return *v.Input
	}
	return args
}

func messageItem(id, text, state string) map[string]any {
	content := []any{}
	if state == "completed" {
		content = append(content, outputTextPart(text))
	}
	return map[string]any{"type": "message", "id": id, "status": state, "role": "assistant", "content": content}
}

func outputTextPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
}

func (sh *responseShape) toolItem(id, call, name, args, state string) map[string]any {
	if sh.req.custom[name] {
		input := ""
		if state == "completed" {
			input = customInput(args)
		}
		return map[string]any{"type": "custom_tool_call", "id": id, "call_id": call, "name": name, "input": input, "status": state}
	}
	return map[string]any{"type": "function_call", "id": id, "call_id": call, "name": name, "arguments": args, "status": state}
}

func (sh *responseShape) toolItemID(name string) string {
	if sh.req.custom[name] {
		return "ctc_" + randomID()
	}
	return "fc_" + randomID()
}

func (a *OpenAIAPI) responses(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	req, err := decodeResponsesRequest(r)
	if err != nil {
		return err
	}
	preq, err := req.toProto(ctx, getPlacementHeader(r))
	if err != nil {
		return err
	}
	if err := applyNeedsHeader(r, preq); err != nil {
		return err
	}
	preq.EnableCache = cacheEnabled(r, req.EnableCache)

	sh := &responseShape{id: "resp_" + randomID(), created: time.Now().Unix(), req: req}
	if req.Stream {
		return a.streamResponses(ctx, w, preq, sh)
	}
	resp, err := a.proxy.GenerateText(ctx, preq)
	if err != nil {
		return err
	}
	setRouteHeaders(w, resp)
	for _, f := range req.ignored {
		addIgnoredParam(w, f)
	}
	setCacheHeader(w, preq.EnableCache, resp.Cached)

	var text, finish string
	var calls []*pb.ToolCall
	if len(resp.Details) > 0 {
		d := resp.Details[0]
		text, finish, calls = d.Text, d.FinishReason, d.ToolCalls
	} else if len(resp.Choices) > 0 {
		text = resp.Choices[0]
	}
	output := []map[string]any{}
	if text != "" {
		output = append(output, messageItem("msg_"+randomID(), text, "completed"))
	}
	for _, tc := range calls {
		name := tc.GetFunction().GetName()
		output = append(output, sh.toolItem(sh.toolItemID(name), callID(tc.GetId()), name,
			orEmptyObject(tc.GetFunction().GetArguments()), "completed"))
	}
	state, incomplete := responseState(finish)
	writeJSON(w, http.StatusOK, sh.object(resp.ResolvedModel, state, output, responsesUsage(resp.GetUsage(), 0), incomplete, nil))
	return nil
}

func responseNotStored(context.Context, http.ResponseWriter, *http.Request) error {
	return status.Error(codes.NotFound, "responses are not stored: KTMS keeps no response state, send the whole conversation with each request")
}

func (a *OpenAIAPI) streamResponses(ctx context.Context, w http.ResponseWriter, preq *pb.GenerateTextRequest, sh *responseShape) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return status.Error(codes.Internal, "streaming unsupported by response writer")
	}
	s := &responsesStream{
		ctx: ctx, w: w, flusher: flusher, sh: sh, estimate: grpcapi.EstimateTokens(preq),
		enableCache: preq.EnableCache, toolItems: map[int32]bool{}, output: []map[string]any{},
	}
	err := a.proxy.GenerateTextStream(&pb.GenerateTextStreamRequest{Request: preq}, s)
	if err != nil && !s.started {
		return err
	}
	if err == nil {
		if err = s.start(nil); err == nil {
			err = s.finish()
		}
		if err != nil {
			log.Debug().Err(err).Msg("responses: the client left before the stream ended")
		}
		return nil
	}
	// Headers are out: the error ends the stream as response.failed.
	_, typ, msg, errCode := apiError(w, err)
	if c, ok := errCode.(string); ok {
		typ = c
	}
	failed := sh.object(s.model, "failed", s.output, responsesUsage(s.usage, s.estimate), nil, map[string]any{"code": typ, "message": msg})
	if werr := s.event("response.failed", map[string]any{"response": failed}); werr != nil {
		log.Debug().Err(werr).Msg("responses: the client left before the stream error")
	}
	return nil
}

// responsesStream renders GenerateText chunks as Responses SSE events. Text
// and each tool call are output items, opened and closed in order; a tool
// item opens once its name is complete, at its first argument delta.
type responsesStream struct {
	grpc.ServerStream
	ctx         context.Context
	w           http.ResponseWriter
	flusher     http.Flusher
	sh          *responseShape
	estimate    int64
	enableCache bool
	cacheHit    bool
	started     bool
	model       string
	seq         int
	output      []map[string]any // closed items
	cur         map[string]any   // the open item, nil when none
	curText     bool             // whether it is a message
	buf         strings.Builder  // its text or arguments so far
	tool        int32            // vendor index of the open or pending tool call
	pending     *pb.ToolCallChunk
	toolItems   map[int32]bool
	finishedAs  string
	usage       *pb.TokensUsage
}

func (s *responsesStream) Context() context.Context { return s.ctx }

// event writes one SSE event; its error stops the vendor stream once the
// client is gone.
func (s *responsesStream) event(name string, data map[string]any) error {
	data["type"] = name
	data["sequence_number"] = s.seq
	s.seq++
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *responsesStream) start(chunk *pb.GenerateTextChunk) error {
	if s.started {
		return nil
	}
	if chunk != nil {
		setRouteHeaders(s.w, chunk)
		s.model = chunk.ResolvedModel
	}
	for _, f := range s.sh.req.ignored {
		addIgnoredParam(s.w, f)
	}
	setCacheHeader(s.w, s.enableCache, s.cacheHit)
	startEventStream(s.w, s.flusher)
	s.started = true
	created := s.sh.object(s.model, "in_progress", []map[string]any{}, nil, nil, nil)
	if err := s.event("response.created", map[string]any{"response": created}); err != nil {
		return err
	}
	return s.event("response.in_progress", map[string]any{"response": created})
}

func (s *responsesStream) index() int { return len(s.output) }

func (s *responsesStream) openText() error {
	if err := s.closeItem(); err != nil {
		return err
	}
	s.cur, s.curText = messageItem("msg_"+randomID(), "", "in_progress"), true
	if err := s.event("response.output_item.added", map[string]any{"output_index": s.index(), "item": s.cur}); err != nil {
		return err
	}
	return s.event("response.content_part.added", map[string]any{
		"item_id": s.cur["id"], "output_index": s.index(), "content_index": 0, "part": outputTextPart(""),
	})
}

func (s *responsesStream) openTool(p *pb.ToolCallChunk) error {
	if err := s.closeItem(); err != nil {
		return err
	}
	s.cur, s.curText = s.sh.toolItem(s.sh.toolItemID(p.Name), callID(p.Id), p.Name, "", "in_progress"), false
	return s.event("response.output_item.added", map[string]any{"output_index": s.index(), "item": s.cur})
}

func (s *responsesStream) flushPending() error {
	p := s.pending
	if p == nil {
		return nil
	}
	s.pending = nil
	return s.openTool(p)
}

// closeItem ends the open item, opening a pending tool call first so a call
// without arguments still reaches the client.
func (s *responsesStream) closeItem() error {
	if err := s.flushPending(); err != nil {
		return err
	}
	item := s.cur
	if item == nil {
		return nil
	}
	s.cur = nil
	acc := s.buf.String()
	s.buf.Reset()
	id, idx := item["id"], s.index()
	var done map[string]any
	switch {
	case s.curText:
		if err := s.event("response.output_text.done", map[string]any{
			"item_id": id, "output_index": idx, "content_index": 0, "text": acc, "logprobs": []any{},
		}); err != nil {
			return err
		}
		if err := s.event("response.content_part.done", map[string]any{
			"item_id": id, "output_index": idx, "content_index": 0, "part": outputTextPart(acc),
		}); err != nil {
			return err
		}
		done = messageItem(id.(string), acc, "completed")
	case item["type"] == "custom_tool_call":
		input := customInput(orEmptyObject(acc))
		if input != "" {
			if err := s.event("response.custom_tool_call_input.delta", map[string]any{
				"item_id": id, "output_index": idx, "call_id": item["call_id"], "delta": input,
			}); err != nil {
				return err
			}
		}
		if err := s.event("response.custom_tool_call_input.done", map[string]any{
			"item_id": id, "output_index": idx, "call_id": item["call_id"], "input": input,
		}); err != nil {
			return err
		}
		done = s.sh.toolItem(id.(string), item["call_id"].(string), item["name"].(string), orEmptyObject(acc), "completed")
	default:
		args := orEmptyObject(acc)
		if err := s.event("response.function_call_arguments.done", map[string]any{
			"item_id": id, "output_index": idx, "name": item["name"], "arguments": args,
		}); err != nil {
			return err
		}
		done = s.sh.toolItem(id.(string), item["call_id"].(string), item["name"].(string), args, "completed")
	}
	s.output = append(s.output, done)
	return s.event("response.output_item.done", map[string]any{"output_index": idx, "item": done})
}

func (s *responsesStream) Send(chunk *pb.GenerateTextChunk) error {
	if chunk.Cached {
		s.cacheHit = true
	}
	if err := s.start(chunk); err != nil {
		return err
	}
	if chunk.Delta != "" {
		if s.cur == nil || !s.curText || s.pending != nil {
			if err := s.openText(); err != nil {
				return err
			}
		}
		s.buf.WriteString(chunk.Delta)
		if err := s.event("response.output_text.delta", map[string]any{
			"item_id": s.cur["id"], "output_index": s.index(), "content_index": 0, "delta": chunk.Delta, "logprobs": []any{},
		}); err != nil {
			return err
		}
	}
	for _, tc := range chunk.ToolCallChunks {
		if err := s.toolChunk(tc); err != nil {
			return err
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

func (s *responsesStream) toolChunk(tc *pb.ToolCallChunk) error {
	current := s.pending != nil || s.cur != nil && !s.curText
	if !current || s.tool != tc.Index {
		if s.toolItems[tc.Index] {
			return status.Errorf(codes.Internal, "tool call %d continued after the next output item began", tc.Index)
		}
		if err := s.closeItem(); err != nil {
			return err
		}
		s.toolItems[tc.Index] = true
		s.tool = tc.Index
		s.pending = &pb.ToolCallChunk{Index: tc.Index}
	}
	if p := s.pending; p != nil {
		// Names arrive as deltas, like arguments.
		p.Name += tc.Name
		if p.Id == "" {
			p.Id = tc.Id
		}
	}
	if tc.ArgumentsDelta == "" || tc.ArgumentsDelta == "null" && s.pending != nil {
		return nil
	}
	if err := s.flushPending(); err != nil {
		return err
	}
	s.buf.WriteString(tc.ArgumentsDelta)
	if s.cur["type"] == "custom_tool_call" {
		// Its text is known once the JSON arguments are complete.
		return nil
	}
	return s.event("response.function_call_arguments.delta", map[string]any{
		"item_id": s.cur["id"], "output_index": s.index(), "delta": tc.ArgumentsDelta,
	})
}

func (s *responsesStream) finish() error {
	if err := s.closeItem(); err != nil {
		return err
	}
	state, incomplete := responseState(s.finishedAs)
	resp := s.sh.object(s.model, state, s.output, responsesUsage(s.usage, s.estimate), incomplete, nil)
	name := "response.completed"
	if state == "incomplete" {
		name = "response.incomplete"
	}
	return s.event(name, map[string]any{"response": resp})
}
