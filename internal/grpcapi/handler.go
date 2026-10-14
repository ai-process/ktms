package grpcapi

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
)

const defaultActionID = "llmproxy.generate_text"

// SnapshotProvider hands out the active config view (the registry Loader in
// production, a fixed snapshot in tests).
type SnapshotProvider interface {
	Snapshot() *registry.Snapshot
}

// TextCache stores and retrieves cached LLM responses.
type TextCache interface {
	Get(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest) (*pb.GenerateTextResponse, bool, error)
	Set(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest, resp *pb.GenerateTextResponse) error
}

// ProxyServer implements the data-plane LLMProxyService.
type ProxyServer struct {
	pb.UnimplementedLLMProxyServiceServer
	snapshots     SnapshotProvider
	throttle      router.Throttle
	cache         TextCache
	batchStore    BatchStore
	batchMaxItems int
}

// WithCache configures response caching on the server.
func (s *ProxyServer) WithCache(c TextCache) *ProxyServer {
	s.cache = c
	return s
}

// Cache returns the configured response cache.
func (s *ProxyServer) Cache() TextCache {
	return s.cache
}

// Reserved attribute names. Callers may set usage_project when one api key
// fronts many tenants; everything else in attributes is opaque to the proxy.
const (
	APIKeyAttribute       = "api_key"
	UsageProjectAttribute = "usage_project"
)

// UsageProject names the usage-collector project for one event. A caller that
// fronts many tenants sets usage_project itself; the api key alone would
// collapse them into one project.
func UsageProject(meta map[string]string) string {
	if p := meta[UsageProjectAttribute]; p != "" {
		return p
	}
	return meta[APIKeyAttribute]
}

func NewProxyServer(snapshots SnapshotProvider, throttle router.Throttle) *ProxyServer {
	if throttle == nil {
		throttle = router.NoopThrottle{}
	}
	return &ProxyServer{snapshots: snapshots, throttle: throttle}
}

func (s *ProxyServer) GenerateText(ctx context.Context, req *pb.GenerateTextRequest) (*pb.GenerateTextResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	if len(req.GetMessages()) == 0 && req.GetBaseSystemInstruction() == "" && req.GetRequestInstruction() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: no messages or instructions")
	}

	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}

	if req.GetEnableCache() && s.cache != nil {
		cached, hit, err := s.cache.Get(ctx, id.KeyID, snap.Version, req)
		if err != nil {
			log.Warn().Err(err).Str("api_key_id", id.KeyID).Msg("cache lookup failed")
		} else if hit && cached != nil {
			userID := router.UserID(req.GetAttributes(), id.Name)
			if m, ok := snap.Models[cached.ResolvedModel]; ok && m != nil {
				if !s.throttle.AllowRPM(ctx, id.Name, userID, m) {
					return nil, status.Error(codes.ResourceExhausted, "resource_exhausted: rpm limit exceeded")
				}
			}
			if tracker := snap.Tracker(); tracker != nil {
				meta := make(map[string]string, len(req.GetAttributes())+3)
				for k, v := range req.GetAttributes() {
					meta[k] = v
				}
				if cached.MatchedRule != "" {
					meta["rule"] = cached.MatchedRule
				}
				meta["api_key"] = id.Name
				meta["cache"] = "hit"
				if err := tracker.RecordUsage(userID, defaultActionID, cached.ResolvedVendor, cached.ResolvedModel, 0, 0, 0, usagev1.Status_STATUS_SUCCESS, meta); err != nil {
					log.Warn().Err(err).Str("user_id", userID).Str("model", cached.ResolvedModel).Msg("failed to record cache hit usage")
				}
			}
			return cached, nil
		}
	}

	effort := effortToString(req.GetEffort())
	var (
		chain    []string
		ruleName string
	)
	switch {
	case req.GetModelOverride() != "":
		if _, ok := snap.Models[req.GetModelOverride()]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", req.GetModelOverride())
		}
		chain = []string{req.GetModelOverride()}
		if effort == "" {
			// Override may skip effort; filter by what the model serves.
			effort = snap.Models[req.GetModelOverride()].Efforts[0]
		}
	case effort == "":
		return nil, status.Error(codes.InvalidArgument, "bad_request: effort is required")
	default:
		rule := router.Match(snap, effort, req.GetAttributes())
		if rule == nil {
			return nil, router.ToStatus(router.ErrNotConfigured)
		}
		chain = router.ResolveChain(rule)
		ruleName = rule.Name
	}

	reqCaps := requiredCapabilities(req)
	models, err := router.FilterChain(snap, chain, id.KeyID, effort, req.GetEnableGoogleSearch(), reqCaps...)
	if err != nil {
		return nil, router.ToStatus(err)
	}

	chat := buildChat(req, id, ruleName, effort)
	resp, model, err := router.Execute(ctx, s.throttle, router.Request{
		Chain:      models,
		KeyName:    id.Name,
		UserID:     router.UserID(req.GetAttributes(), id.Name),
		Estimate:   estimateTokens(req),
		Chat:       chat,
		Adapter:    func(m *proxydb.Model) (llm.ClientAdapter, error) { return snap.Adapter(id.KeyID, m) },
		SoftSchema: registry.SoftSchema,
	})
	if err != nil {
		return nil, router.ToStatus(err)
	}

	out := &pb.GenerateTextResponse{
		Usage: &pb.TokensUsage{
			Input:  int64(resp.Usage.Input),
			Output: int64(resp.Usage.Output),
		},
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		MatchedRule:    ruleName,
	}
	for i, choice := range resp.Choices {
		out.Choices = append(out.Choices, choice.Text)
		var tcs []*pb.ToolCall
		for _, tc := range choice.ToolCalls {
			tcs = append(tcs, &pb.ToolCall{
				Id:   tc.ID,
				Type: tc.Type,
				Function: &pb.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		finish := choice.FinishReason
		if finish == "" {
			finish = "stop"
		}
		out.Details = append(out.Details, &pb.Choice{
			Index:        int32(i),
			Text:         choice.Text,
			ToolCalls:    tcs,
			FinishReason: finish,
		})
	}
	if req.GetEnableCache() && s.cache != nil {
		if err := s.cache.Set(ctx, id.KeyID, snap.Version, req, out); err != nil {
			log.Warn().Err(err).Str("api_key_id", id.KeyID).Msg("failed to cache response")
		}
	}
	return out, nil
}

// GenerateTextStream handles streaming text generation.
func (s *ProxyServer) GenerateTextStream(req *pb.GenerateTextStreamRequest, stream pb.LLMProxyService_GenerateTextStreamServer) error {
	id := apikeys.IdentityFrom(stream.Context())
	if id == nil {
		return status.Error(codes.Unauthenticated, "no identity")
	}
	inner := req.GetRequest()
	if inner == nil {
		return status.Error(codes.InvalidArgument, "bad_request: request is required")
	}
	if len(inner.GetMessages()) == 0 && inner.GetBaseSystemInstruction() == "" && inner.GetRequestInstruction() == "" {
		return status.Error(codes.InvalidArgument, "bad_request: no messages or instructions")
	}

	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return router.ToStatus(router.ErrNotConfigured)
	}

	if inner.GetEnableCache() && s.cache != nil {
		cached, hit, err := s.cache.Get(stream.Context(), id.KeyID, snap.Version, inner)
		if err != nil {
			log.Warn().Err(err).Str("api_key_id", id.KeyID).Msg("cache lookup failed in stream")
		} else if hit && cached != nil {
			userID := router.UserID(inner.GetAttributes(), id.Name)
			if m, ok := snap.Models[cached.ResolvedModel]; ok && m != nil {
				if !s.throttle.AllowRPM(stream.Context(), id.Name, userID, m) {
					return status.Error(codes.ResourceExhausted, "resource_exhausted: rpm limit exceeded")
				}
			}
			if tracker := snap.Tracker(); tracker != nil {
				meta := make(map[string]string, len(inner.GetAttributes())+3)
				for k, v := range inner.GetAttributes() {
					meta[k] = v
				}
				if cached.MatchedRule != "" {
					meta["rule"] = cached.MatchedRule
				}
				meta["api_key"] = id.Name
				meta["cache"] = "hit"
				if err := tracker.RecordUsage(userID, defaultActionID, cached.ResolvedVendor, cached.ResolvedModel, 0, 0, 0, usagev1.Status_STATUS_SUCCESS, meta); err != nil {
					log.Warn().Err(err).Str("user_id", userID).Str("model", cached.ResolvedModel).Msg("failed to record cache hit usage in stream")
				}
			}
			return sendCachedStream(stream, cached)
		}
	}

	effort := effortToString(inner.GetEffort())
	var (
		chain    []string
		ruleName string
	)
	switch {
	case inner.GetModelOverride() != "":
		if _, ok := snap.Models[inner.GetModelOverride()]; !ok {
			return status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", inner.GetModelOverride())
		}
		chain = []string{inner.GetModelOverride()}
		if effort == "" {
			effort = snap.Models[inner.GetModelOverride()].Efforts[0]
		}
	case effort == "":
		return status.Error(codes.InvalidArgument, "bad_request: effort is required")
	default:
		rule := router.Match(snap, effort, inner.GetAttributes())
		if rule == nil {
			return router.ToStatus(router.ErrNotConfigured)
		}
		chain = router.ResolveChain(rule)
		ruleName = rule.Name
	}

	reqCaps := requiredCapabilities(inner)
	models, err := router.FilterChain(snap, chain, id.KeyID, effort, inner.GetEnableGoogleSearch(), reqCaps...)
	if err != nil {
		return router.ToStatus(err)
	}

	chat := buildChat(inner, id, ruleName, effort)
	var (
		sentFirst       bool
		streamText      strings.Builder
		streamToolCalls = make(map[int32]*pb.ToolCall)
		streamFinish    string
		streamUsage     *pb.TokensUsage
		streamModel     string
		streamVendor    string
	)
	_, err = router.ExecuteStream(stream.Context(), s.throttle, router.Request{
		Chain:      models,
		KeyName:    id.Name,
		UserID:     router.UserID(inner.GetAttributes(), id.Name),
		Estimate:   estimateTokens(inner),
		Chat:       chat,
		Adapter:    func(m *proxydb.Model) (llm.ClientAdapter, error) { return snap.Adapter(id.KeyID, m) },
		SoftSchema: registry.SoftSchema,
	}, func(m *proxydb.Model, chunk llm.StreamChunk) error {
		streamModel = m.ID
		streamVendor = m.Vendor
		if chunk.Delta != "" {
			streamText.WriteString(chunk.Delta)
		}
		if chunk.FinishReason != "" {
			streamFinish = chunk.FinishReason
		}
		outChunk := &pb.GenerateTextChunk{
			Delta:        chunk.Delta,
			FinishReason: chunk.FinishReason,
		}
		if !sentFirst {
			sentFirst = true
			outChunk.ResolvedModel = m.ID
			outChunk.ResolvedVendor = m.Vendor
			outChunk.MatchedRule = ruleName
		}
		if chunk.Usage != nil {
			outChunk.Usage = &pb.TokensUsage{
				Input:  int64(chunk.Usage.Input),
				Output: int64(chunk.Usage.Output),
			}
			streamUsage = outChunk.Usage
		}
		for _, tc := range chunk.ToolCallChunks {
			outChunk.ToolCallChunks = append(outChunk.ToolCallChunks, &pb.ToolCallChunk{
				Index:          tc.Index,
				Id:             tc.ID,
				Type:           tc.Type,
				Name:           tc.Name,
				ArgumentsDelta: tc.ArgumentsDelta,
			})
			t, ok := streamToolCalls[tc.Index]
			if !ok {
				t = &pb.ToolCall{
					Id:   tc.ID,
					Type: tc.Type,
					Function: &pb.FunctionCall{
						Name: tc.Name,
					},
				}
				streamToolCalls[tc.Index] = t
			} else {
				if tc.ID != "" {
					t.Id = tc.ID
				}
				if tc.Type != "" {
					t.Type = tc.Type
				}
				if tc.Name != "" {
					t.Function.Name += tc.Name
				}
			}
			t.Function.Arguments += tc.ArgumentsDelta
		}
		return stream.Send(outChunk)
	})
	if err != nil {
		return router.ToStatus(err)
	}

	if inner.GetEnableCache() && s.cache != nil {
		if streamFinish == "" {
			streamFinish = "stop"
		}
		if streamUsage == nil {
			streamUsage = &pb.TokensUsage{}
		}
		var tcs []*pb.ToolCall
		if len(streamToolCalls) > 0 {
			for i := int32(0); i < int32(len(streamToolCalls)); i++ {
				if tc, ok := streamToolCalls[i]; ok {
					tcs = append(tcs, tc)
				}
			}
		}
		cachedResp := &pb.GenerateTextResponse{
			Choices: []string{streamText.String()},
			Details: []*pb.Choice{
				{
					Index:        0,
					Text:         streamText.String(),
					ToolCalls:    tcs,
					FinishReason: streamFinish,
				},
			},
			Usage:          streamUsage,
			ResolvedModel:  streamModel,
			ResolvedVendor: streamVendor,
			MatchedRule:    ruleName,
		}
		if err := s.cache.Set(stream.Context(), id.KeyID, snap.Version, inner, cachedResp); err != nil {
			log.Warn().Err(err).Str("api_key_id", id.KeyID).Msg("failed to cache stream response")
		}
	}
	return nil
}

func sendCachedStream(stream pb.LLMProxyService_GenerateTextStreamServer, cached *pb.GenerateTextResponse) error {
	var sentFirst bool
	for _, choice := range cached.Details {
		if choice.Text != "" {
			chunk := &pb.GenerateTextChunk{
				Delta:          choice.Text,
				ResolvedModel:  cached.ResolvedModel,
				ResolvedVendor: cached.ResolvedVendor,
				MatchedRule:    cached.MatchedRule,
				Cached:         true,
			}
			sentFirst = true
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		if len(choice.ToolCalls) > 0 {
			var tcChunks []*pb.ToolCallChunk
			for i, tc := range choice.ToolCalls {
				tcChunks = append(tcChunks, &pb.ToolCallChunk{
					Index:          int32(i),
					Id:             tc.Id,
					Type:           tc.Type,
					Name:           tc.Function.GetName(),
					ArgumentsDelta: tc.Function.GetArguments(),
				})
			}
			chunk := &pb.GenerateTextChunk{
				ToolCallChunks: tcChunks,
				Cached:         true,
			}
			if !sentFirst {
				sentFirst = true
				chunk.ResolvedModel = cached.ResolvedModel
				chunk.ResolvedVendor = cached.ResolvedVendor
				chunk.MatchedRule = cached.MatchedRule
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		finish := choice.FinishReason
		if finish == "" {
			finish = "stop"
		}
		finalChunk := &pb.GenerateTextChunk{
			FinishReason: finish,
			Usage:        cached.Usage,
			Cached:       true,
		}
		if !sentFirst {
			sentFirst = true
			finalChunk.ResolvedModel = cached.ResolvedModel
			finalChunk.ResolvedVendor = cached.ResolvedVendor
			finalChunk.MatchedRule = cached.MatchedRule
		}
		if err := stream.Send(finalChunk); err != nil {
			return err
		}
	}
	if len(cached.Details) == 0 && len(cached.Choices) > 0 {
		for _, text := range cached.Choices {
			chunk := &pb.GenerateTextChunk{
				Delta:          text,
				ResolvedModel:  cached.ResolvedModel,
				ResolvedVendor: cached.ResolvedVendor,
				MatchedRule:    cached.MatchedRule,
				Cached:         true,
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		finalChunk := &pb.GenerateTextChunk{
			FinishReason: "stop",
			Usage:        cached.Usage,
			Cached:       true,
		}
		if err := stream.Send(finalChunk); err != nil {
			return err
		}
	}
	return nil
}

// buildChat maps the wire request onto the vendored ChatContext.
func buildChat(req *pb.GenerateTextRequest, id *apikeys.Identity, ruleName, effort string) *llm.ChatContext {
	chat := llm.NewChatContext()
	chat.Version = llm.ChatContextVersion2
	chat.MessagesLimit = 0 // never cut: the client already chose what to send
	chat.BaseSystemInstruction = req.GetBaseSystemInstruction()
	if req.GetRequestInstruction() != "" {
		chat.SetRequestInstruction(req.GetRequestInstruction())
	}
	for _, m := range req.GetMessages() {
		switch m.GetRole() {
		case pb.MessageRole_MESSAGE_ROLE_ASSISTANT:
			if len(m.GetToolCalls()) > 0 {
				var tcs []*llm.ToolCall
				for _, tc := range m.GetToolCalls() {
					tcs = append(tcs, &llm.ToolCall{
						ID:   tc.GetId(),
						Type: tc.GetType(),
						Function: llm.FunctionCall{
							Name:      tc.GetFunction().GetName(),
							Arguments: tc.GetFunction().GetArguments(),
						},
					})
				}
				chat.AddAssistantMessageWithToolCalls(m.GetText(), tcs)
			} else {
				chat.AddAssistantMessage(m.GetText())
			}
		case pb.MessageRole_MESSAGE_ROLE_TOOL:
			chat.AddToolMessage(m.GetToolCallId(), m.GetText())
		default:
			if len(m.GetParts()) > 0 {
				var parts []*llm.ContentPart
				for _, p := range m.GetParts() {
					switch part := p.GetPart().(type) {
					case *pb.ContentPart_Text:
						parts = append(parts, &llm.ContentPart{
							Type: llm.ContentPartTypeText,
							Text: part.Text.GetText(),
						})
					case *pb.ContentPart_Image:
						parts = append(parts, &llm.ContentPart{
							Type:     llm.ContentPartTypeImage,
							MimeType: part.Image.GetMimeType(),
							Data:     part.Image.GetData(),
							URL:      part.Image.GetUrl(),
							Detail:   llm.ImageDetail(part.Image.GetDetail()),
						})
					case *pb.ContentPart_Audio:
						parts = append(parts, &llm.ContentPart{
							Type:     llm.ContentPartTypeAudio,
							MimeType: part.Audio.GetMimeType(),
							Data:     part.Audio.GetData(),
							Format:   part.Audio.GetFormat(),
						})
					case *pb.ContentPart_Document:
						parts = append(parts, &llm.ContentPart{
							Type:     llm.ContentPartTypeDocument,
							MimeType: part.Document.GetMimeType(),
							Data:     part.Document.GetData(),
							FileName: part.Document.GetFileName(),
						})
					}
				}
				chat.AddUserMessageWithParts(m.GetText(), parts)
			} else {
				chat.AddUserMessage(m.GetText())
			}
		}
	}
	if schema := schemaFromProto(req.GetResponseSchema()); schema != nil {
		chat.SetResponseSchema(schema)
	}
	if len(req.GetTools()) > 0 {
		var tools []*llm.Tool
		for _, t := range req.GetTools() {
			tools = append(tools, &llm.Tool{
				Type: t.GetType(),
				Function: llm.FunctionDefinition{
					Name:        t.GetFunction().GetName(),
					Description: t.GetFunction().GetDescription(),
					Parameters:  schemaFromProto(t.GetFunction().GetParameters()),
					Strict:      t.GetFunction().GetStrict(),
				},
			})
		}
		chat.SetTools(tools)
	}
	if tc := req.GetToolChoice(); tc != nil {
		var mode string
		switch tc.GetMode() {
		case pb.ToolChoice_MODE_AUTO:
			mode = "auto"
		case pb.ToolChoice_MODE_NONE:
			mode = "none"
		case pb.ToolChoice_MODE_REQUIRED:
			mode = "required"
		case pb.ToolChoice_MODE_SPECIFIC:
			mode = "specific"
		}
		chat.SetToolChoice(&llm.ToolChoice{
			Mode:                 mode,
			SpecificFunctionName: tc.GetSpecificFunctionName(),
		})
	}
	if req.GetMaxOutputTokens() > 0 {
		chat.SetMaxOutputTokens(int(req.GetMaxOutputTokens()))
	}
	if req.GetEnableGoogleSearch() {
		chat.SetEnableGoogleSearch()
	}

	actionID := req.GetActionId()
	if actionID == "" {
		actionID = defaultActionID
	}
	chat.SetActionID(actionID)

	userID := router.UserID(req.GetAttributes(), id.Name)
	chat.SetInternalUserID(userID)

	// Attributes flow into usage meta, plus what the proxy resolved. A caller's
	// own usage_project survives the copy — it names the usage project, with
	// the api key as the fallback (resolver in cmd/server/main.go).
	meta := make(map[string]string, len(req.GetAttributes())+3)
	for k, v := range req.GetAttributes() {
		meta[k] = v
	}
	meta[APIKeyAttribute] = id.Name
	meta["effort"] = effort
	if ruleName != "" {
		meta["rule"] = ruleName
	}
	chat.SetMeta(meta)
	return chat
}

// estimateTokens is the budget reservation guess: total runes / 4 plus heuristic
// allowances for media parts. Settled against vendor-reported actuals after the call.
func estimateTokens(req *pb.GenerateTextRequest) int64 {
	runes := utf8.RuneCountInString(req.GetBaseSystemInstruction()) +
		utf8.RuneCountInString(req.GetRequestInstruction())
	var extraTokens int64
	for _, m := range req.GetMessages() {
		runes += utf8.RuneCountInString(m.GetText())
		for _, p := range m.GetParts() {
			switch part := p.GetPart().(type) {
			case *pb.ContentPart_Text:
				if m.GetText() == "" {
					runes += utf8.RuneCountInString(part.Text.GetText())
				}
			case *pb.ContentPart_Image:
				if part.Image.GetDetail() == "low" {
					extraTokens += 85
				} else {
					extraTokens += 1000
				}
			case *pb.ContentPart_Audio:
				extraTokens += 200
			case *pb.ContentPart_Document:
				extraTokens += 500
			}
		}
	}
	return int64(runes)/4 + extraTokens + 1
}

// requiredCapabilities checks request parts and returns required model capabilities.
func requiredCapabilities(req *pb.GenerateTextRequest) []string {
	var caps []string
	hasVision := false
	hasAudio := false
	hasDoc := false
	for _, m := range req.GetMessages() {
		for _, p := range m.GetParts() {
			switch p.GetPart().(type) {
			case *pb.ContentPart_Image:
				hasVision = true
			case *pb.ContentPart_Audio:
				hasAudio = true
			case *pb.ContentPart_Document:
				hasDoc = true
			}
		}
	}
	if hasVision {
		caps = append(caps, registry.CapabilityVision)
	}
	if hasAudio {
		caps = append(caps, registry.CapabilityAudioInput)
	}
	if hasDoc {
		caps = append(caps, registry.CapabilityPDFInput)
	}
	return caps
}

// ListModels returns only models the calling client holds a vendor key for.
func (s *ProxyServer) ListModels(ctx context.Context, _ *pb.ListModelsRequest) (*pb.ListModelsResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	snap := s.snapshots.Snapshot()
	resp := &pb.ListModelsResponse{}
	for _, m := range snap.ClientModels(id.KeyID) {
		resp.Models = append(resp.Models, modelInfo(m))
	}
	return resp, nil
}

func modelInfo(m *proxydb.Model) *pb.ModelInfo {
	return &pb.ModelInfo{
		Id:                   m.ID,
		Vendor:               m.Vendor,
		Efforts:              effortsToProto(m.Efforts),
		Capabilities:         m.Capabilities,
		Rpm:                  m.RPM,
		PriceInPerMtok:       m.PriceInPerMtok,
		PriceOutPerMtok:      m.PriceOutPerMtok,
		PriceInBatchPerMtok:  m.BatchPriceInPerMtok(),
		PriceOutBatchPerMtok: m.BatchPriceOutPerMtok(),
	}
}
