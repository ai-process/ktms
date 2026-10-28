package grpcapi

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/cooldown"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

const defaultActionID = "ktms.generate_text"

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

// ProxyServer implements the data-plane GatewayService.
type ProxyServer struct {
	pb.UnimplementedGatewayServiceServer
	snapshots       SnapshotProvider
	throttle        router.Throttle
	circuitBreaker  router.CircuitBreaker
	cooldown        *cooldown.Store
	upstreamTimeout time.Duration
	cache           TextCache
	semantic        SemanticCache
	guardrails      *guardrails.Guardrails
	batchStore      BatchStore
	batchMaxItems   int
	keyLimiter      KeyLimiter
}

// WithCircuitBreaker configures upstream circuit breaking on the server.
func (s *ProxyServer) WithCircuitBreaker(cb router.CircuitBreaker) *ProxyServer {
	s.circuitBreaker = cb
	return s
}

// WithUpstreamTimeout bounds each attempt against an upstream vendor.
func (s *ProxyServer) WithUpstreamTimeout(d time.Duration) *ProxyServer {
	s.upstreamTimeout = d
	return s
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

// WithGuardrails configures guardrails on the server.
func (s *ProxyServer) WithGuardrails(g *guardrails.Guardrails) *ProxyServer {
	s.guardrails = g
	return s
}

// Guardrails returns the configured guardrails.
func (s *ProxyServer) Guardrails() *guardrails.Guardrails {
	return s.guardrails
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
	return &ProxyServer{
		snapshots: snapshots,
		throttle:  throttle,
	}
}

func (s *ProxyServer) GenerateText(ctx context.Context, req *pb.GenerateTextRequest) (*pb.GenerateTextResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	if len(req.GetMessages()) == 0 && req.GetBaseSystemInstruction() == "" && req.GetRequestInstruction() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: no messages or instructions")
	}

	if req.GetAttributes() != nil {
		req.Attributes = router.SanitizeAttributes(req.GetAttributes())
	}
	if err := classify(ctx, id, req); err != nil {
		return nil, err
	}

	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}

	if hit := s.lookupCache(ctx, snap, id, req); hit != nil {
		if err := s.admitCacheHit(ctx, snap, id, req, hit); err != nil {
			return nil, err
		}
		return hit.resp, nil
	}

	plan, err := s.planRoute(ctx, snap, id, req, nil)
	if err != nil {
		return nil, err
	}

	chat := buildChat(req, id, plan.ruleName, plan.effort)
	var vault *guardrails.TokenVault
	if s.guardrails != nil && s.guardrails.Enabled {
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
		}
		if _, err := s.guardrails.InspectChatContext(ctx, chat, vault); err != nil {
			return nil, guardrailStatus(err)
		}
	}
	var shadowEvaluator func(context.Context, *proxydb.Model, *proxydb.Model, *llm.Response, *llm.Response)
	if plan.shadowEval {
		shadowEvaluator = s.makeShadowEvaluator(id, chat, req.GetAttributes(), plan.restricted)
	}
	estimate := estimateTokens(req)
	settleKey, err := s.admitKey(ctx, id, estimate)
	if err != nil {
		return nil, err
	}
	resp, model, err := router.Execute(ctx, s.throttle, router.Request{
		Chain:            plan.models,
		KeyName:          id.Name,
		UserID:           router.UserID(req.GetAttributes(), id.Name),
		TeamID:           id.TeamID,
		Estimate:         estimate,
		Chat:             chat,
		CircuitBreaker:   s.circuitBreaker,
		Cooldown:         s.cooldownFor(snap, id),
		UpstreamTimeout:  s.upstreamTimeout,
		Adapter:          func(m *proxydb.Model) (llm.ClientAdapter, error) { return snap.Adapter(id.KeyID, m) },
		SoftSchema:       registry.SoftSchema,
		ShadowModel:      plan.shadowModel,
		ShadowPercentage: plan.shadowPercentage,
		ShadowEvaluator:  shadowEvaluator,
	})
	if resp != nil {
		settleKey(&resp.Usage)
	} else {
		settleKey(nil)
	}
	if err != nil {
		return nil, router.ToStatus(err)
	}

	out := &pb.GenerateTextResponse{
		Usage: &pb.TokensUsage{
			Input:      int64(resp.Usage.Input),
			Output:     int64(resp.Usage.Output),
			CacheRead:  int64(resp.Usage.CacheRead),
			CacheWrite: int64(resp.Usage.CacheWrite),
		},
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		MatchedRule:    plan.ruleName,
		RouteMode:      plan.routeMode,
		Placement:      plan.placement,
		JudgeScore:     plan.judgeScore,
		JudgeModel:     plan.judgeModel,
	}
	for i, choice := range resp.Choices {
		choiceText := choice.Text
		if vault != nil {
			choiceText = vault.Desanitize(choiceText)
		}
		out.Choices = append(out.Choices, choiceText)
		var tcs []*pb.ToolCall
		for _, tc := range choice.ToolCalls {
			args := tc.Function.Arguments
			if vault != nil && args != "" {
				args = vault.DesanitizeJSON(args)
			}
			tcs = append(tcs, &pb.ToolCall{
				Id:   tc.ID,
				Type: tc.Type,
				Function: &pb.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: args,
				},
			})
		}
		finish := choice.FinishReason
		if finish == "" {
			finish = "stop"
		}
		out.Details = append(out.Details, &pb.Choice{
			Index:        int32(i),
			Text:         choiceText,
			ToolCalls:    tcs,
			FinishReason: finish,
		})
	}
	s.storeCache(ctx, snap, id, req, out)
	return out, nil
}

// GenerateTextStream handles streaming text generation.
func (s *ProxyServer) GenerateTextStream(req *pb.GenerateTextStreamRequest, stream pb.GatewayService_GenerateTextStreamServer) error {
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

	if inner.GetAttributes() != nil {
		inner.Attributes = router.SanitizeAttributes(inner.GetAttributes())
	}
	if err := classify(stream.Context(), id, inner); err != nil {
		return err
	}

	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return router.ToStatus(router.ErrNotConfigured)
	}

	if hit := s.lookupCache(stream.Context(), snap, id, inner); hit != nil {
		if err := s.admitCacheHit(stream.Context(), snap, id, inner, hit); err != nil {
			return err
		}
		return sendCachedStream(stream, hit.resp)
	}

	plan, err := s.planRoute(stream.Context(), snap, id, inner, nil)
	if err != nil {
		return err
	}

	chat := buildChat(inner, id, plan.ruleName, plan.effort)
	var (
		vault    *guardrails.TokenVault
		streamer *guardrails.StreamDesanitizer
	)
	if s.guardrails != nil && s.guardrails.Enabled {
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
			streamer = guardrails.NewStreamDesanitizer(vault)
		}
		if _, err := s.guardrails.InspectChatContext(stream.Context(), chat, vault); err != nil {
			return guardrailStatus(err)
		}
	}
	var (
		sentFirst       bool
		streamText      strings.Builder
		streamToolCalls = make(map[int32]*pb.ToolCall)
		streamFinish    string
		streamUsage     *pb.TokensUsage
		streamModel     string
		streamVendor    string
	)
	var shadowEvaluator func(context.Context, *proxydb.Model, *proxydb.Model, *llm.Response, *llm.Response)
	if plan.shadowEval {
		shadowEvaluator = s.makeShadowEvaluator(id, chat, inner.GetAttributes(), plan.restricted)
	}
	estimate := estimateTokens(inner)
	settleKey, err := s.admitKey(stream.Context(), id, estimate)
	if err != nil {
		return err
	}
	defer func() {
		// A stream cut before its usage chunk still produced what was sent.
		u := llm.TokensUsage{Input: int(estimate), Output: llm.EstimateOutputTokens(streamText.Len())}
		if streamUsage != nil {
			u = llm.TokensUsage{Input: int(streamUsage.GetInput()), Output: int(streamUsage.GetOutput())}
		}
		settleKey(&u)
	}()
	_, err = router.ExecuteStream(stream.Context(), s.throttle, router.Request{
		Chain:            plan.models,
		KeyName:          id.Name,
		UserID:           router.UserID(inner.GetAttributes(), id.Name),
		TeamID:           id.TeamID,
		Estimate:         estimate,
		Chat:             chat,
		CircuitBreaker:   s.circuitBreaker,
		Cooldown:         s.cooldownFor(snap, id),
		UpstreamTimeout:  s.upstreamTimeout,
		Adapter:          func(m *proxydb.Model) (llm.ClientAdapter, error) { return snap.Adapter(id.KeyID, m) },
		SoftSchema:       registry.SoftSchema,
		ShadowModel:      plan.shadowModel,
		ShadowPercentage: plan.shadowPercentage,
		ShadowEvaluator:  shadowEvaluator,
	}, func(m *proxydb.Model, chunk llm.StreamChunk) error {
		streamModel = m.ID
		streamVendor = m.Vendor
		delta := chunk.Delta
		if streamer != nil && delta != "" {
			delta = streamer.ProcessChunk(delta)
		}
		if streamer != nil && chunk.FinishReason != "" {
			delta += streamer.Flush()
		}
		if delta != "" {
			streamText.WriteString(delta)
		}
		if chunk.FinishReason != "" {
			streamFinish = chunk.FinishReason
		}
		outChunk := &pb.GenerateTextChunk{
			Delta:        delta,
			FinishReason: chunk.FinishReason,
		}
		if !sentFirst {
			sentFirst = true
			outChunk.ResolvedModel = m.ID
			outChunk.ResolvedVendor = m.Vendor
			outChunk.MatchedRule = plan.ruleName
			outChunk.RouteMode = plan.routeMode
			outChunk.Placement = plan.placement
			outChunk.JudgeScore = plan.judgeScore
			outChunk.JudgeModel = plan.judgeModel
		}
		if chunk.Usage != nil {
			outChunk.Usage = &pb.TokensUsage{
				Input:      int64(chunk.Usage.Input),
				Output:     int64(chunk.Usage.Output),
				CacheRead:  int64(chunk.Usage.CacheRead),
				CacheWrite: int64(chunk.Usage.CacheWrite),
			}
			streamUsage = outChunk.Usage
		}
		for _, tc := range chunk.ToolCallChunks {
			argDelta := tc.ArgumentsDelta
			if vault != nil && argDelta != "" {
				argDelta = vault.Desanitize(argDelta)
			}
			outChunk.ToolCallChunks = append(outChunk.ToolCallChunks, &pb.ToolCallChunk{
				Index:          tc.Index,
				Id:             tc.ID,
				Type:           tc.Type,
				Name:           tc.Name,
				ArgumentsDelta: argDelta,
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
			t.Function.Arguments += argDelta
		}
		return stream.Send(outChunk)
	})
	if err != nil {
		return router.ToStatus(err)
	}

	if inner.GetEnableCache() && (s.cache != nil || s.semantic != nil) {
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
			MatchedRule:    plan.ruleName,
			RouteMode:      plan.routeMode,
			Placement:      plan.placement,
			JudgeScore:     plan.judgeScore,
			JudgeModel:     plan.judgeModel,
		}
		s.storeCache(stream.Context(), snap, id, inner, cachedResp)
	}
	return nil
}

func sendCachedStream(stream pb.GatewayService_GenerateTextStreamServer, cached *pb.GenerateTextResponse) error {
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
	meta := usageMeta(req.GetAttributes(), id)
	meta["effort"] = effort
	if ruleName != "" {
		meta["rule"] = ruleName
	}
	if isRestricted(req) {
		meta[DataClassificationAttribute] = apikeys.DataRestricted
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

// ListModels returns only models the calling client may call: it holds their
// vendor key, and its team (if any) allows them.
func (s *ProxyServer) ListModels(ctx context.Context, _ *pb.ListModelsRequest) (*pb.ListModelsResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	snap := s.snapshots.Snapshot()
	resp := &pb.ListModelsResponse{}
	for _, m := range snap.ClientModels(id.KeyID) {
		if !id.CanAccessModel(m.ID) {
			continue
		}
		resp.Models = append(resp.Models, modelInfo(m))
	}
	return resp, nil
}

func modelInfo(m *proxydb.Model) *pb.ModelInfo {
	return &pb.ModelInfo{
		Id:                     m.ID,
		Vendor:                 m.Vendor,
		Efforts:                effortsToProto(m.Efforts),
		Capabilities:           m.Capabilities,
		Rpm:                    m.RPM,
		PriceInPerMtok:         m.PriceInPerMtok,
		PriceOutPerMtok:        m.PriceOutPerMtok,
		PriceInBatchPerMtok:    m.BatchPriceInPerMtok(),
		PriceOutBatchPerMtok:   m.BatchPriceOutPerMtok(),
		PriceCacheReadPerMtok:  m.CacheReadPerMtok(),
		PriceCacheWritePerMtok: m.CacheWritePerMtok(),
	}
}
