package grpcapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
)

// ModalityAttribute is set by the proxy on speech and image calls so a rule can
// pin them without the caller naming a model. Reserved: a caller's own value is
// overwritten.
const ModalityAttribute = "modality"

const (
	modalityTTS   = "tts"
	modalityImage = "image"
)

// mediaEffort is the tier speech and image route at. They have no effort of
// their own — the capability filter decides what can serve them — but rules are
// keyed by effort, so they need one, and the cheapest is the honest default.
const mediaEffort = registry.EffortLow

func (s *ProxyServer) SynthesizeSpeech(ctx context.Context, req *pb.SynthesizeSpeechRequest) (*pb.SynthesizeSpeechResponse, error) {
	if req.GetText() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: text is empty")
	}
	plan, err := s.planMedia(ctx, req.GetAttributes(), req.GetModelOverride(), modalityTTS, registry.CapabilityTTS)
	if err != nil {
		return nil, err
	}

	var out *llm.SpeechResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:   plan.chain,
		KeyName: plan.identity.Name,
		UserID:  router.UserID(plan.attrs, plan.identity.Name),
		// Input-side estimate only; Settle replaces it with what the vendor
		// billed, which for speech is audio tokens and far larger.
		Estimate: int64(len([]rune(req.GetText()))/4 + 1),
		Adapter:  plan.adapter,
		Call: func(m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			synth, ok := adapter.(llm.SpeechSynthesizer)
			if !ok {
				// The capability filter should have excluded this model; if the
				// registry and the adapter disagree, say so rather than panic.
				return llm.TokensUsage{}, status.Errorf(codes.FailedPrecondition,
					"no_capable_model: %s cannot synthesize speech", m.ID)
			}
			res, err := synth.SynthesizeSpeech(req.GetText(), req.GetLanguage(), router.UserID(plan.attrs, plan.identity.Name), plan.meta)
			if err != nil {
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	if err != nil {
		return nil, router.ToStatus(err)
	}

	return &pb.SynthesizeSpeechResponse{
		Audio:          out.Audio,
		MimeType:       out.MimeType,
		DurationMs:     int32(out.DurationMs),
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		Usage:          &pb.TokensUsage{Input: int64(out.Usage.Input), Output: int64(out.Usage.Output)},
		MatchedRule:    plan.ruleName,
	}, nil
}

func (s *ProxyServer) GenerateImage(ctx context.Context, req *pb.GenerateImageRequest) (*pb.GenerateImageResponse, error) {
	if req.GetPrompt() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: prompt is empty")
	}
	plan, err := s.planMedia(ctx, req.GetAttributes(), req.GetModelOverride(), modalityImage, registry.CapabilityImage)
	if err != nil {
		return nil, err
	}

	var out *llm.ImageResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:   plan.chain,
		KeyName: plan.identity.Name,
		UserID:  router.UserID(plan.attrs, plan.identity.Name),
		// Input-side estimate only; image output tokens dwarf it, so Settle is
		// what makes the budget accounting true.
		Estimate: int64(len([]rune(req.GetPrompt()))/4 + 1),
		Adapter:  plan.adapter,
		Call: func(m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			gen, ok := adapter.(llm.ImageGenerator)
			if !ok {
				return llm.TokensUsage{}, status.Errorf(codes.FailedPrecondition,
					"no_capable_model: %s cannot generate images", m.ID)
			}
			res, err := gen.GenerateImage(req.GetPrompt(), router.UserID(plan.attrs, plan.identity.Name), plan.meta)
			if err != nil {
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	if err != nil {
		return nil, router.ToStatus(err)
	}

	return &pb.GenerateImageResponse{
		Image:          out.Data,
		MimeType:       out.MimeType,
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		Usage:          &pb.TokensUsage{Input: int64(out.Usage.Input), Output: int64(out.Usage.Output)},
		MatchedRule:    plan.ruleName,
	}, nil
}

// mediaPlan is everything the two media RPCs resolve identically: who is
// calling, which models may serve it, and the usage attribution to carry.
type mediaPlan struct {
	identity *apikeys.Identity
	chain    []*proxydb.Model
	ruleName string
	attrs    map[string]string
	meta     map[string]string
	adapter  func(m *proxydb.Model) (llm.ClientAdapter, error)
}

func (s *ProxyServer) planMedia(ctx context.Context, attrs map[string]string, override, modality, capability string) (*mediaPlan, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}

	// The modality is the proxy's to state, not the caller's: rules match on it
	// and a caller could otherwise route image work to a speech chain.
	routing := make(map[string]string, len(attrs)+1)
	for k, v := range attrs {
		routing[k] = v
	}
	routing[ModalityAttribute] = modality

	var chain []string
	var ruleName string
	if override != "" {
		if _, ok := snap.Models[override]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", override)
		}
		chain = []string{override}
	} else {
		// Only rules that name the modality: a text rule matching on subject
		// alone would otherwise win and yield a chain with nothing capable.
		rule := router.MatchModality(snap, mediaEffort, ModalityAttribute, modality, routing)
		if rule == nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"no_capable_model: no %s rule configured", modality)
		}
		chain = rule.Use
		ruleName = rule.Name
	}

	models, err := router.FilterChain(snap, chain, id.KeyID, mediaEffort, false, capability)
	if err != nil {
		return nil, router.ToStatus(err)
	}

	meta := make(map[string]string, len(routing)+2)
	for k, v := range routing {
		meta[k] = v
	}
	meta[APIKeyAttribute] = id.Name
	if ruleName != "" {
		meta["rule"] = ruleName
	}

	return &mediaPlan{
		identity: id,
		chain:    models,
		ruleName: ruleName,
		attrs:    routing,
		meta:     meta,
		adapter:  func(m *proxydb.Model) (llm.ClientAdapter, error) { return snap.Adapter(id.KeyID, m) },
	}, nil
}
