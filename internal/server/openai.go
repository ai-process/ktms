package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
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
	"github.com/ai-process/llm-proxy/internal/router"
	"github.com/ai-process/llm-proxy/internal/safefetch"
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
	embeddings := method(pb.LLMProxyService_GetEmbeddings_FullMethodName)
	models := method(pb.LLMProxyService_ListModels_FullMethodName)

	mux.HandleFunc("POST /v1/chat/completions", a.wrap(chat, a.chatCompletions))
	mux.HandleFunc("POST /v1/audio/speech", a.wrap(speech, a.audioSpeech))
	mux.HandleFunc("POST /v1/images/generations", a.wrap(image, a.imageGenerations))
	mux.HandleFunc("POST /v1/judge", a.wrap(judge, a.judge))
	mux.HandleFunc("POST /v1/decisions", a.wrap(judge, a.decisions))
	mux.HandleFunc("POST /v1/embeddings", a.wrap(embeddings, a.embeddings))
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
	// Proxy extension: enables exact-match prompt/response caching.
	EnableCache *bool `json:"enable_cache,omitempty"`
	// Proxy extension: infrastructure locality preference (nodes_first, nodes_only, cloud_first, cloud_only).
	Placement string `json:"placement,omitempty"`
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

type rawPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio,omitempty"`
	File *struct {
		FileData string `json:"file_data,omitempty"`
		Filename string `json:"filename,omitempty"`
		MimeType string `json:"mime_type,omitempty"`
	} `json:"file,omitempty"`
	Document *struct {
		Data     string `json:"data,omitempty"`
		MimeType string `json:"mime_type,omitempty"`
		FileName string `json:"file_name,omitempty"`
	} `json:"document,omitempty"`
}

// parseContent extracts text and multimodal parts from OpenAI content field.
func (m chatMessage) parseContent(ctx context.Context) (string, []*pb.ContentPart, error) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return "", nil, nil
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, nil, nil
	}
	var rawParts []rawPart
	if err := json.Unmarshal(m.Content, &rawParts); err != nil {
		return "", nil, invalid("message content must be a string or a list of parts")
	}

	var b strings.Builder
	var parts []*pb.ContentPart
	for _, p := range rawParts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
			parts = append(parts, &pb.ContentPart{
				Part: &pb.ContentPart_Text{
					Text: &pb.TextPart{Text: p.Text},
				},
			})
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return "", nil, invalid("image_url part must contain a non-empty url")
			}
			rawURL := p.ImageURL.URL
			detail := p.ImageURL.Detail
			if detail == "" {
				detail = "auto"
			}
			var imgData []byte
			var mimeType string
			if strings.HasPrefix(rawURL, "data:") {
				var err error
				mimeType, imgData, err = safefetch.ParseDataURI(rawURL)
				if err != nil {
					return "", nil, invalid("invalid data URI: %v", err)
				}
				if err := safefetch.ValidateMediaMimeType(mimeType); err != nil {
					return "", nil, invalid("%v", err)
				}
			} else if strings.HasPrefix(rawURL, "https://") {
				var err error
				imgData, mimeType, err = safefetch.FetchURL(ctx, rawURL, maxRequestBytes)
				if err != nil {
					return "", nil, invalid("failed to fetch image: %v", err)
				}
			} else {
				return "", nil, invalid("invalid image URL %q: only https and data URI schemes are allowed", rawURL)
			}
			parts = append(parts, &pb.ContentPart{
				Part: &pb.ContentPart_Image{
					Image: &pb.ImagePart{
						MimeType: mimeType,
						Data:     imgData,
						Url:      rawURL,
						Detail:   detail,
					},
				},
			})
		case "input_audio":
			if p.InputAudio == nil || p.InputAudio.Data == "" {
				return "", nil, invalid("input_audio part must contain base64 data")
			}
			data, err := base64.StdEncoding.DecodeString(p.InputAudio.Data)
			if err != nil {
				return "", nil, invalid("invalid base64 in input_audio: %v", err)
			}
			format := p.InputAudio.Format
			if format == "" {
				format = "wav"
			}
			mimeType := "audio/" + format
			if err := safefetch.ValidateMediaMimeType(mimeType); err != nil {
				return "", nil, invalid("%v", err)
			}
			parts = append(parts, &pb.ContentPart{
				Part: &pb.ContentPart_Audio{
					Audio: &pb.AudioPart{
						MimeType: mimeType,
						Data:     data,
						Format:   format,
					},
				},
			})
		case "file":
			if p.File == nil || p.File.FileData == "" {
				return "", nil, invalid("file part must contain file_data")
			}
			data, err := base64.StdEncoding.DecodeString(p.File.FileData)
			if err != nil {
				return "", nil, invalid("invalid base64 in file data: %v", err)
			}
			mimeType := p.File.MimeType
			if mimeType == "" {
				mimeType = "application/pdf"
			}
			if err := safefetch.ValidateMediaMimeType(mimeType); err != nil {
				return "", nil, invalid("%v", err)
			}
			parts = append(parts, &pb.ContentPart{
				Part: &pb.ContentPart_Document{
					Document: &pb.DocumentPart{
						MimeType: mimeType,
						Data:     data,
						FileName: p.File.Filename,
					},
				},
			})
		case "document":
			if p.Document == nil || p.Document.Data == "" {
				return "", nil, invalid("document part must contain data")
			}
			data, err := base64.StdEncoding.DecodeString(p.Document.Data)
			if err != nil {
				return "", nil, invalid("invalid base64 in document data: %v", err)
			}
			mimeType := p.Document.MimeType
			if mimeType == "" {
				mimeType = "application/pdf"
			}
			if err := safefetch.ValidateMediaMimeType(mimeType); err != nil {
				return "", nil, invalid("%v", err)
			}
			parts = append(parts, &pb.ContentPart{
				Part: &pb.ContentPart_Document{
					Document: &pb.DocumentPart{
						MimeType: mimeType,
						Data:     data,
						FileName: p.Document.FileName,
					},
				},
			})
		default:
			return "", nil, invalid("content part type %q is not supported", p.Type)
		}
	}
	return b.String(), parts, nil
}

func hasMediaParts(parts []*pb.ContentPart) bool {
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch p.Part.(type) {
		case *pb.ContentPart_Image, *pb.ContentPart_Audio, *pb.ContentPart_Document:
			return true
		}
	}
	return false
}

// text flattens string or [{"type":"text","text":...}] content for backward compatibility.
func (m chatMessage) text() (string, error) {
	text, _, err := m.parseContent(context.Background())
	return text, err
}

type routeDecision struct {
	Effort          pb.Effort
	ModelOverride   string
	IsAuto          bool
	EffortSpecified bool
	Placement       string
	HasPlacement    bool
}

// route resolves the OpenAI "model" field.
// Supports "auto", "auto:nodes-first", "auto:nodes-only", "auto:cloud-only", "auto:low", etc.
func route(model, reasoningEffort, explicitPlacement string) (routeDecision, error) {
	dec := routeDecision{
		Effort: pb.Effort_EFFORT_MEDIUM,
	}

	if explicitPlacement != "" {
		norm := router.NormalizePlacement(explicitPlacement)
		if norm == "" {
			return dec, invalid("unknown placement %q", explicitPlacement)
		}
		dec.Placement = norm
		dec.HasPlacement = true
	}

	switch {
	case model == "" || model == "auto":
		dec.IsAuto = true
		if dec.Placement == "" {
			dec.Placement = router.PlacementNodesFirst
		}
		if reasoningEffort != "" {
			eff, err := parseEffort(reasoningEffort)
			if err != nil {
				return dec, err
			}
			dec.Effort = eff
			dec.EffortSpecified = true
		}

	case strings.HasPrefix(model, "auto:"):
		dec.IsAuto = true
		suffix := strings.TrimPrefix(model, "auto:")
		if suffix == "" {
			return dec, invalid("auto route specifier cannot be empty")
		}
		parts := strings.Split(suffix, ":")
		var (
			seenEffort     bool
			seenPlacement  bool
			modelPlacement string
		)
		for _, part := range parts {
			if part == "" {
				return dec, invalid("empty specifier in auto route %q", model)
			}
			if eff, effErr := parseEffort(part); effErr == nil {
				if seenEffort {
					return dec, invalid("duplicate effort specifier %q in model", part)
				}
				seenEffort = true
				dec.Effort = eff
				dec.EffortSpecified = true
				continue
			}
			if p := router.NormalizePlacement(part); p != "" {
				if seenPlacement {
					return dec, invalid("duplicate placement specifier %q in model", part)
				}
				seenPlacement = true
				modelPlacement = p
				continue
			}
			return dec, invalid("unknown auto route specifier %q in model", part)
		}

		if modelPlacement != "" {
			dec.HasPlacement = true
			if dec.Placement == "" {
				dec.Placement = modelPlacement
			}
		}
		if dec.Placement == "" {
			dec.Placement = router.PlacementNodesFirst
		}
		// Model string effort takes precedence; reasoning_effort serves as fallback effort.
		if !dec.EffortSpecified && reasoningEffort != "" {
			eff, err := parseEffort(reasoningEffort)
			if err != nil {
				return dec, err
			}
			dec.Effort = eff
			dec.EffortSpecified = true
		}

	default:
		dec.ModelOverride = model
		if reasoningEffort != "" {
			eff, err := parseEffort(reasoningEffort)
			if err != nil {
				return dec, err
			}
			dec.Effort = eff
		}
	}

	return dec, nil
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

func (req *chatRequest) toProto(ctx context.Context, headerPlacement string) (*pb.GenerateTextRequest, error) {
	if len(req.Functions) > 0 && string(req.Functions) != "null" {
		return nil, invalid("functions is deprecated and not supported; use tools instead")
	}
	if req.N != nil && *req.N != 1 {
		return nil, invalid("n must be 1")
	}
	if len(req.Messages) == 0 {
		return nil, invalid("messages is required")
	}
	chosenPlacement := req.Placement
	if headerPlacement != "" {
		chosenPlacement = headerPlacement
	}
	dec, err := route(req.Model, req.ReasoningEffort, chosenPlacement)
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
		Effort:             dec.Effort,
		ModelOverride:      dec.ModelOverride,
		MaxOutputTokens:    req.MaxCompletionTokens,
		EnableGoogleSearch: req.EnableGoogleSearch,
		Tools:              tools,
		ToolChoice:         toolChoice,
		Placement:          dec.Placement,
		AutoRoute:          dec.IsAuto,
		EffortSpecified:    dec.EffortSpecified,
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
		text, parts, err := m.parseContent(ctx)
		if err != nil {
			return nil, err
		}
		if hasMediaParts(parts) && m.Role != "user" {
			return nil, invalid("messages[%d]: multimodal content is only supported for user messages", i)
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, text)
		case "user":
			out.Messages = append(out.Messages, &pb.ChatMessage{
				Role:  pb.MessageRole_MESSAGE_ROLE_USER,
				Text:  text,
				Parts: parts,
			})
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
	preq, err := req.toProto(ctx, r.Header.Get("X-LLM-Proxy-Placement"))
	if err != nil {
		return err
	}
	var enableCache bool
	if req.EnableCache != nil {
		enableCache = *req.EnableCache
	} else if h := r.Header.Get("X-LLM-Proxy-Cache"); h != "" {
		enableCache = h == "true" || h == "1"
	}
	preq.EnableCache = enableCache

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
	if resp.Placement != "" {
		w.Header().Set("X-LLM-Proxy-Placement", resp.Placement)
	}
	if resp.RouteMode != "" {
		w.Header().Set("X-LLM-Proxy-Route-Mode", resp.RouteMode)
	}
	if resp.JudgeScore > 0 {
		w.Header().Set("X-LLM-Proxy-Judge-Score", fmt.Sprintf("%.2f", resp.JudgeScore))
	}
	if resp.JudgeModel != "" {
		w.Header().Set("X-LLM-Proxy-Judge-Model", resp.JudgeModel)
	}
	if preq.EnableCache {
		if resp.Cached {
			w.Header().Set("X-LLM-Proxy-Cache", "HIT")
		} else {
			w.Header().Set("X-LLM-Proxy-Cache", "MISS")
		}
	}

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

	s := &httpStreamAdapter{
		ctx:          ctx,
		w:            w,
		flusher:      flusher,
		id:           id,
		created:      created,
		includeUsage: includeUsage,
		enableCache:  preq.EnableCache,
	}

	err := a.proxy.GenerateTextStream(&pb.GenerateTextStreamRequest{Request: preq}, s)
	if err != nil {
		if !s.headersSent {
			return err
		}
		errData, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "api_error",
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errData)
	}

	if err == nil && !s.headersSent {
		s.ensureHeaders(nil)
	}

	if s.headersSent {
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
	}
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
	headersSent   bool
	enableCache   bool
	cacheHit      bool
}

func (s *httpStreamAdapter) Context() context.Context {
	return s.ctx
}

func (s *httpStreamAdapter) ensureHeaders(chunk *pb.GenerateTextChunk) {
	if s.headersSent {
		return
	}
	if chunk != nil {
		setResolvedHeaders(s.w, chunk.ResolvedVendor, chunk.MatchedRule)
		if chunk.Placement != "" {
			s.w.Header().Set("X-LLM-Proxy-Placement", chunk.Placement)
		}
		if chunk.RouteMode != "" {
			s.w.Header().Set("X-LLM-Proxy-Route-Mode", chunk.RouteMode)
		}
		if chunk.JudgeScore > 0 {
			s.w.Header().Set("X-LLM-Proxy-Judge-Score", fmt.Sprintf("%.2f", chunk.JudgeScore))
		}
		if chunk.JudgeModel != "" {
			s.w.Header().Set("X-LLM-Proxy-Judge-Model", chunk.JudgeModel)
		}
	}
	if s.enableCache {
		if s.cacheHit {
			s.w.Header().Set("X-LLM-Proxy-Cache", "HIT")
		} else {
			s.w.Header().Set("X-LLM-Proxy-Cache", "MISS")
		}
	}
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flusher.Flush()
	s.headersSent = true
}

func (s *httpStreamAdapter) Send(chunk *pb.GenerateTextChunk) error {
	if chunk.Cached {
		s.cacheHit = true
	}
	s.ensureHeaders(chunk)

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
	dec, err := route(req.Model, "", "")
	if err != nil {
		return err
	}
	if dec.HasPlacement {
		return invalid("placement is only supported for chat completions")
	}
	override := dec.ModelOverride
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
	dec, err := route(req.Model, "", "")
	if err != nil {
		return err
	}
	if dec.HasPlacement {
		return invalid("placement is only supported for chat completions")
	}
	override := dec.ModelOverride
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

// ---- decisions (OpenAI Decisions API wire format) ----

type decisionsChoiceReq struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

type decisionsLevelReq struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type decisionsQuestionReq struct {
	Name         string               `json:"name"`
	Type         string               `json:"type"` // "predicate" | "choice" | "score"
	Instructions string               `json:"instructions"`
	Choices      []decisionsChoiceReq `json:"choices"`
	Levels       json.RawMessage      `json:"levels"`
}

type decisionsRequest struct {
	Model     string                 `json:"model"`
	Input     json.RawMessage        `json:"input"`
	Questions []decisionsQuestionReq `json:"questions"`
	Metadata  map[string]string      `json:"metadata,omitempty"`
	User      string                 `json:"user,omitempty"`
}

type decisionsAnswerOut struct {
	Name          string             `json:"name"`
	Type          string             `json:"type"`
	Probability   *float64           `json:"probability,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

type decisionsResponseOut struct {
	Model   string               `json:"model"`
	Answers []decisionsAnswerOut `json:"answers"`
	Usage   map[string]int       `json:"usage"`
}

func (a *OpenAIAPI) decisions(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var decReq decisionsRequest
	if err := decodeBody(r, &decReq); err != nil {
		return err
	}
	if len(decReq.Input) == 0 {
		return invalid("input is required")
	}
	if !json.Valid(decReq.Input) {
		return invalid("input must be valid JSON")
	}
	if len(decReq.Questions) == 0 {
		return invalid("questions are required")
	}

	scoreLabels := make(map[string][]string)
	pbQuestions := make(map[string]*pb.JudgeQuestion, len(decReq.Questions))
	for _, q := range decReq.Questions {
		if q.Name == "" {
			return invalid("question name is required")
		}
		if _, exists := pbQuestions[q.Name]; exists {
			return invalid("duplicate question name %q", q.Name)
		}
		if q.Instructions == "" {
			return invalid("question %q has no instructions", q.Name)
		}
		jq := &pb.JudgeQuestion{
			Instructions: q.Instructions,
		}
		switch q.Type {
		case "predicate":
			jq.Type = pb.JudgeType_JUDGE_TYPE_NOUL
		case "choice":
			jq.Type = pb.JudgeType_JUDGE_TYPE_CHOICE
			if len(q.Choices) < 2 {
				return invalid("choice %q needs at least two choices", q.Name)
			}
			jq.Options = make(map[string]string, len(q.Choices))
			for _, c := range q.Choices {
				if _, exists := jq.Options[c.Value]; exists {
					return invalid("choice %q has duplicate value %q", q.Name, c.Value)
				}
				jq.Options[c.Value] = c.Description
			}
		case "score":
			jq.Type = pb.JudgeType_JUDGE_TYPE_SCORE
			if len(q.Levels) == 0 {
				return invalid("score %q needs levels", q.Name)
			}
			var objLevels []decisionsLevelReq
			var labels []string
			if err := json.Unmarshal(q.Levels, &objLevels); err == nil && len(objLevels) > 0 {
				jq.Levels = make([]string, len(objLevels))
				labels = make([]string, len(objLevels))
				seenLabels := make(map[string]bool, len(objLevels))
				for i, l := range objLevels {
					lbl := l.Label
					if lbl == "" {
						lbl = l.Description
					}
					if lbl == "" {
						lbl = strconv.Itoa(i)
					}
					if seenLabels[lbl] {
						return invalid("score %q has duplicate level %q", q.Name, lbl)
					}
					seenLabels[lbl] = true
					labels[i] = lbl
					if l.Description != "" {
						jq.Levels[i] = l.Description
					} else {
						jq.Levels[i] = l.Label
					}
				}
			} else {
				var strLevels []string
				if err := json.Unmarshal(q.Levels, &strLevels); err == nil {
					jq.Levels = strLevels
					labels = make([]string, len(strLevels))
					seenLabels := make(map[string]bool, len(strLevels))
					for i, s := range strLevels {
						if seenLabels[s] {
							return invalid("score %q has duplicate level %q", q.Name, s)
						}
						seenLabels[s] = true
						labels[i] = s
					}
				} else {
					return invalid("invalid levels format for %q", q.Name)
				}
			}
			if len(jq.Levels) < 2 {
				return invalid("score %q needs at least two levels", q.Name)
			}
			scoreLabels[q.Name] = labels
		default:
			return invalid("unsupported question type %q", q.Type)
		}
		pbQuestions[q.Name] = jq
	}

	dec, err := route(decReq.Model, "", "")
	if err != nil {
		return err
	}
	if dec.HasPlacement {
		return invalid("placement is only supported for chat completions")
	}
	override := dec.ModelOverride
	attrs, actionID := attributes(decReq.Metadata, decReq.User)
	req := &pb.JudgeRequest{
		StateJson:     string(decReq.Input),
		Questions:     pbQuestions,
		ModelOverride: override,
		Attributes:    attrs,
		ActionId:      actionID,
	}

	resp, err := a.proxy.Judge(ctx, req)
	if err != nil {
		return err
	}

	answers := make([]decisionsAnswerOut, 0, len(decReq.Questions))
	for _, q := range decReq.Questions {
		ans, ok := resp.Answers[q.Name]
		if !ok {
			continue
		}
		outAns := decisionsAnswerOut{Name: q.Name}
		switch ans.Type {
		case pb.JudgeType_JUDGE_TYPE_NOUL:
			outAns.Type = "predicate"
			prob := ans.Noul
			outAns.Probability = &prob
		case pb.JudgeType_JUDGE_TYPE_CHOICE:
			outAns.Type = "choice"
			outAns.Choice = ans.Choice
			outAns.Probabilities = ans.Probabilities
			conf := ans.Confidence
			outAns.Confidence = &conf
		case pb.JudgeType_JUDGE_TYPE_SCORE:
			outAns.Type = "score"
			sc := ans.Score
			outAns.Score = &sc
			conf := ans.Confidence
			outAns.Confidence = &conf
			if labels, ok := scoreLabels[q.Name]; ok && len(ans.Probabilities) > 0 {
				mappedProbs := make(map[string]float64, len(ans.Probabilities))
				for k, v := range ans.Probabilities {
					idx, err := strconv.Atoi(k)
					if err == nil && idx >= 0 && idx < len(labels) {
						mappedProbs[labels[idx]] = v
					} else {
						mappedProbs[k] = v
					}
				}
				outAns.Probabilities = mappedProbs
			} else {
				outAns.Probabilities = ans.Probabilities
			}
		}
		answers = append(answers, outAns)
	}

	usage := map[string]int{
		"input_tokens":  int(resp.Usage.Input),
		"output_tokens": int(resp.Usage.Output),
		"total_tokens":  int(resp.Usage.Input + resp.Usage.Output),
	}

	setResolvedHeaders(w, resp.ResolvedVendor, resp.MatchedRule)
	w.Header().Set("X-LLM-Proxy-Model", resp.ResolvedModel)
	writeJSON(w, http.StatusOK, decisionsResponseOut{
		Model:   resp.ResolvedModel,
		Answers: answers,
		Usage:   usage,
	})
	return nil
}

// ---- embeddings ----

type embeddingRequest struct {
	Input          json.RawMessage   `json:"input"`
	Model          string            `json:"model"`
	Dimensions     *int32            `json:"dimensions,omitempty"`
	EncodingFormat string            `json:"encoding_format,omitempty"`
	User           string            `json:"user,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

func (a *OpenAIAPI) embeddings(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var req embeddingRequest
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if req.Model == "" {
		return invalid("model is required")
	}
	if req.Model == "auto" {
		return invalid("auto routing is not supported for embeddings")
	}
	format := req.EncodingFormat
	if format == "" {
		format = "float"
	}
	if format != "float" && format != "base64" {
		return invalid("encoding_format must be 'float' or 'base64'")
	}

	var inputs []string
	if len(req.Input) == 0 {
		return invalid("input is required")
	}

	var singleStr string
	if err := json.Unmarshal(req.Input, &singleStr); err == nil {
		if singleStr == "" {
			return invalid("input is required")
		}
		inputs = []string{singleStr}
	} else {
		var strArr []string
		if err := json.Unmarshal(req.Input, &strArr); err == nil {
			if len(strArr) == 0 {
				return invalid("input is required")
			}
			for _, s := range strArr {
				if s == "" {
					return invalid("input items must not be empty")
				}
			}
			inputs = strArr
		} else {
			var intArr []int
			if err := json.Unmarshal(req.Input, &intArr); err == nil {
				return invalid("token input is not supported; provide text strings")
			}
			return invalid("input must be a string or array of strings")
		}
	}

	var dims int32
	if req.Dimensions != nil {
		dims = *req.Dimensions
	}
	attrs, actionID := attributes(req.Metadata, req.User)
	preq := &pb.GetEmbeddingsRequest{
		Inputs:     inputs,
		Model:      req.Model,
		Dimensions: dims,
		ActionId:   actionID,
		Attributes: attrs,
	}

	resp, err := a.proxy.GetEmbeddings(ctx, preq)
	if err != nil {
		return err
	}

	if resp.ResolvedModel != "" {
		w.Header().Set("X-LLM-Proxy-Model", resp.ResolvedModel)
	}
	setResolvedHeaders(w, resp.ResolvedVendor, resp.MatchedRule)

	type openAIEmbeddingItem struct {
		Object    string `json:"object"`
		Index     int    `json:"index"`
		Embedding any    `json:"embedding"`
	}

	data := make([]openAIEmbeddingItem, len(resp.Data))
	for i, d := range resp.Data {
		var embVal any
		if format == "base64" {
			embVal = float32sToBase64(d.Values)
		} else {
			embVal = d.Values
		}
		data[i] = openAIEmbeddingItem{
			Object:    "embedding",
			Index:     int(d.Index),
			Embedding: embVal,
		}
	}

	out := map[string]any{
		"object": "list",
		"data":   data,
		"model":  resp.ResolvedModel,
		"usage": map[string]any{
			"prompt_tokens": resp.Usage.GetInput(),
			"total_tokens":  resp.Usage.GetInput() + resp.Usage.GetOutput(),
		},
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func float32sToBase64(vals []float32) string {
	buf := make([]byte, len(vals)*4)
	for i, v := range vals {
		bits := math.Float32bits(v)
		binary.LittleEndian.PutUint32(buf[i*4:], bits)
	}
	return base64.StdEncoding.EncodeToString(buf)
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
	} else if strings.HasPrefix(msg, "guardrail_violation") {
		errCode = "guardrail_violation"
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
