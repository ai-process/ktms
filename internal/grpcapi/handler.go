package grpcapi

import (
	"context"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
)

const defaultActionID = "llmproxy.generate_text"

// SnapshotProvider hands out the active config view (the registry Loader in
// production, a fixed snapshot in tests).
type SnapshotProvider interface {
	Snapshot() *registry.Snapshot
}

// ProxyServer implements the data-plane LLMProxyService.
type ProxyServer struct {
	pb.UnimplementedLLMProxyServiceServer
	snapshots     SnapshotProvider
	throttle      router.Throttle
	batchStore    BatchStore
	batchMaxItems int
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
		chain = rule.Use
		ruleName = rule.Name
	}

	models, err := router.FilterChain(snap, chain, id.KeyID, effort, req.GetEnableGoogleSearch(), "")
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
	for _, choice := range resp.Choices {
		out.Choices = append(out.Choices, choice.Text)
	}
	return out, nil
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
			chat.AddAssistantMessage(m.GetText())
		default:
			chat.AddUserMessage(m.GetText())
		}
	}
	if schema := schemaFromProto(req.GetResponseSchema()); schema != nil {
		chat.SetResponseSchema(schema)
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

// estimateTokens is the budget reservation guess: total runes / 4. Settled
// against vendor-reported actuals after the call, so precision is not needed.
func estimateTokens(req *pb.GenerateTextRequest) int64 {
	runes := utf8.RuneCountInString(req.GetBaseSystemInstruction()) +
		utf8.RuneCountInString(req.GetRequestInstruction())
	for _, m := range req.GetMessages() {
		runes += utf8.RuneCountInString(m.GetText())
	}
	return int64(runes)/4 + 1
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
