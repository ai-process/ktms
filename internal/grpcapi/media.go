package grpcapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

// ModalityAttribute is set by the proxy on speech and image calls so a rule can
// pin them without the caller naming a model. Reserved: a caller's own value is
// overwritten.
const ModalityAttribute = "modality"

const (
	modalityTTS   = "tts"
	modalityImage = "image"
)

func (s *ProxyServer) SynthesizeSpeech(ctx context.Context, req *pb.SynthesizeSpeechRequest) (*pb.SynthesizeSpeechResponse, error) {
	if req.GetText() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: text is empty")
	}
	speechText := req.GetText()
	if s.guardrails != nil && s.guardrails.Enabled {
		var vault *guardrails.TokenVault
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
		}
		redacted, _, err := s.guardrails.InspectText(ctx, speechText, vault)
		if err != nil {
			return nil, guardrailStatus(err)
		}
		speechText = redacted
	}
	plan, err := s.planMediaNeeds(ctx, req.GetAttributes(), req.GetNeeds(), req.GetModelOverride(), modalityTTS, registry.CapabilityTTS)
	if err != nil {
		return nil, err
	}

	estimate := int64(len([]rune(speechText))/4 + 1)
	settleKey, err := s.admitKey(ctx, plan.identity, estimate)
	if err != nil {
		return nil, err
	}
	var out *llm.SpeechResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:   plan.chain,
		KeyName: plan.identity.Name,
		UserID:  router.UserID(plan.attrs, plan.identity.Name),
		TeamID:  plan.identity.TeamID,
		// Input-side estimate only; Settle replaces it with what the vendor
		// billed, which for speech is audio tokens and far larger.
		Estimate:        estimate,
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Cooldown:        plan.cooldown,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			synth, ok := adapter.(llm.SpeechSynthesizer)
			if !ok {
				// The capability filter should have excluded this model; if the
				// registry and the adapter disagree, say so rather than panic.
				return llm.TokensUsage{}, status.Errorf(codes.FailedPrecondition,
					"no_capable_model: %s cannot synthesize speech", m.ID)
			}
			res, err := synth.SynthesizeSpeech(callCtx, speechText, req.GetLanguage(), router.UserID(plan.attrs, plan.identity.Name), plan.meta)
			if err != nil {
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	var used *llm.TokensUsage
	if out != nil {
		used = &out.Usage
	}
	settleKey(used)
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
	prompt := req.GetPrompt()
	if s.guardrails != nil && s.guardrails.Enabled {
		var vault *guardrails.TokenVault
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
		}
		redacted, _, err := s.guardrails.InspectText(ctx, prompt, vault)
		if err != nil {
			return nil, guardrailStatus(err)
		}
		prompt = redacted
	}
	plan, err := s.planMediaNeeds(ctx, req.GetAttributes(), req.GetNeeds(), req.GetModelOverride(), modalityImage, registry.CapabilityImage)
	if err != nil {
		return nil, err
	}

	estimate := int64(len([]rune(prompt))/4 + 1)
	settleKey, err := s.admitKey(ctx, plan.identity, estimate)
	if err != nil {
		return nil, err
	}
	var out *llm.ImageResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:   plan.chain,
		KeyName: plan.identity.Name,
		UserID:  router.UserID(plan.attrs, plan.identity.Name),
		TeamID:  plan.identity.TeamID,
		// Input-side estimate only; image output tokens dwarf it, so Settle is
		// what makes the budget accounting true.
		Estimate:        estimate,
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Cooldown:        plan.cooldown,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			gen, ok := adapter.(llm.ImageGenerator)
			if !ok {
				return llm.TokensUsage{}, status.Errorf(codes.FailedPrecondition,
					"no_capable_model: %s cannot generate images", m.ID)
			}
			res, err := gen.GenerateImage(callCtx, prompt, router.UserID(plan.attrs, plan.identity.Name), plan.meta)
			if err != nil {
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	var used *llm.TokensUsage
	if out != nil {
		used = &out.Usage
	}
	settleKey(used)
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
	cooldown router.Cooldown
}

func (s *ProxyServer) planMedia(ctx context.Context, attrs map[string]string, override, modality string, capabilities ...string) (*mediaPlan, error) {
	return s.planMediaNeeds(ctx, attrs, nil, override, modality, capabilities...)
}

// planMediaNeeds is planMedia for a call that states needs.
func (s *ProxyServer) planMediaNeeds(ctx context.Context, attrs, needs map[string]string, override, modality string, capabilities ...string) (*mediaPlan, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	return s.planMediaFor(ctx, id, mediaQuery{attrs: attrs, needs: needs, override: override, modality: modality, caps: capabilities}, nil)
}

// mediaQuery is what a non-text call routes by. pair narrows a translate
// chain to the models declaring the language pair.
type mediaQuery struct {
	attrs    map[string]string
	needs    map[string]string // empty: the modality alone decides
	override string
	modality string
	caps     []string
	pair     *languagePair
}

type languagePair struct{ source, target string }

// planMediaFor is planMedia for a known identity, recording into tr when
// ExplainRoute asks; a nil trace costs a real call nothing.
func (s *ProxyServer) planMediaFor(ctx context.Context, id *apikeys.Identity, q mediaQuery, tr *routeTrace) (*mediaPlan, error) {
	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}

	// The modality is the proxy's to state, not the caller's: rules match on it
	// and a caller could otherwise route image work to a speech chain.
	routing := routingAttributes(q.attrs, id)
	routing[ModalityAttribute] = q.modality

	needs, err := registry.RequestNeeds(q.needs, "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "bad_request: needs: "+err.Error())
	}
	restricted := restrictedCall(ctx, id)
	var chain []string
	var ruleName string
	var models []*proxydb.Model
	selected := false
	if q.override != "" {
		chain = []string{q.override}
		tr.start(router.RouteModeOverride, chain, nil)
		if !id.CanAccessModel(q.override) {
			tr.add(router.Candidate{ID: q.override, Model: snap.Models[q.override], Dropped: id.DeniedBy(q.override) + "_policy"})
			return nil, status.Errorf(codes.PermissionDenied, "model %q not allowed for %s", q.override, id.DeniedBy(q.override))
		}
		if _, ok := snap.Models[q.override]; !ok {
			tr.add(router.Candidate{ID: q.override, Dropped: router.DropNotFound})
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", q.override)
		}
	} else {
		// Only rules that name the modality: a text rule matching on subject
		// alone would otherwise win and yield a chain with nothing capable.
		rule := router.MatchModality(snap, needs, ModalityAttribute, q.modality, routing)
		switch {
		case rule != nil:
			canaryKey := router.CanaryKey(routing)
			chain = router.ResolveChain(rule, canaryKey)
			ruleName = rule.Name
			tr.start(router.RouteModeDirect, chain, rule)
		case snap.Selects():
			selected = true
			tr.start(router.RouteModeDirect, nil, nil)
		default:
			return nil, status.Errorf(codes.FailedPrecondition,
				"no_capable_model: no %s rule configured", q.modality)
		}
	}
	tr.matched(needs)

	if selected {
		placement := ""
		if restricted {
			placement = router.PlacementNodesOnly
		}
		if models, err = s.selectChain(ctx, snap, id, needs, false, false, q.caps, placement, q.pair, tr); err != nil {
			return nil, err
		}
	} else {
		if models, err = tr.filter(snap, chain, id.KeyID, needs, false, q.caps); err != nil {
			return nil, router.ToStatus(err)
		}
		if q.pair != nil {
			if models, err = tr.languagePair(models, q.pair); err != nil {
				return nil, err
			}
		}
		tr.policy(ctx, id, models)
		if models, err = teamModels(ctx, id, models); err != nil {
			return nil, err
		}
	}
	if restricted {
		before := models
		if models, err = nodeModels(models, q.modality); err != nil {
			return nil, err
		}
		tr.placed(before, models)
	}

	meta := usageMeta(routing, id)
	if restricted {
		meta[DataClassificationAttribute] = apikeys.DataRestricted
	}
	if ruleName != "" {
		meta["rule"] = ruleName
	}
	if len(needs) > 0 {
		meta["needs"] = registry.FormatNeeds(needs)
	}

	return &mediaPlan{
		identity: id,
		chain:    models,
		ruleName: ruleName,
		attrs:    routing,
		meta:     meta,
		adapter:  func(m *proxydb.Model) (llm.ClientAdapter, error) { return snap.Adapter(id.KeyID, m) },
		cooldown: s.cooldownFor(snap, id),
	}, nil
}
