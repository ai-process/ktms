package grpcapi

import (
	"context"
	"strings"

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

const modalityTranslate = "translate"

// Batch ceilings: they bound the budget reservation and one engine call, and
// keep a request-path RPC from carrying a bulk job that belongs in many calls.
const (
	maxTranslateTexts = 128
	maxTranslateBytes = 256 << 10
)

// Translate translates a batch of texts, in order, through whichever engine
// the translate rule's chain reaches first: a node MT model, any chat model by
// prompt, or a cloud MT vendor.
func (s *ProxyServer) Translate(ctx context.Context, req *pb.TranslateRequest) (*pb.TranslateResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}
	tr, err := translateRequestFromProto(req)
	if err != nil {
		return nil, err
	}
	var vault *guardrails.TokenVault
	if s.guardrails != nil && s.guardrails.Enabled {
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
		}
		for i, t := range tr.Texts {
			clean, _, err := s.guardrails.InspectText(ctx, t, vault)
			if err != nil {
				return nil, guardrailStatus(err)
			}
			tr.Texts[i] = clean
		}
	}
	// Blank texts and a pair with nothing to translate never reach an engine:
	// a model asked to translate "" or en to en answers it unreliably.
	all := tr.Texts
	var sent []int
	for i, t := range all {
		if strings.TrimSpace(t) != "" {
			sent = append(sent, i)
		}
	}
	if len(sent) == 0 || (tr.SourceLanguage != "" && strings.EqualFold(tr.SourceLanguage, tr.TargetLanguage)) {
		return passthrough(req.GetTexts(), tr.SourceLanguage), nil
	}
	tr.Texts = make([]string, len(sent))
	for j, i := range sent {
		tr.Texts[j] = all[i]
	}

	plan, err := s.planMediaFor(ctx, id, mediaQuery{
		attrs:    req.GetAttributes(),
		override: req.GetModelOverride(),
		modality: modalityTranslate,
		caps:     []string{registry.CapabilityTranslate},
		pair:     &languagePair{source: tr.SourceLanguage, target: tr.TargetLanguage},
	}, nil)
	if err != nil {
		return nil, err
	}

	// Input plus about as much output: a translation is the length of its source.
	estimate := 2*int64(llm.EstimateOutputTokens(llm.SourceCharacters(tr.Texts))) + 16
	settleKey, err := s.admitKey(ctx, plan.identity, estimate)
	if err != nil {
		return nil, err
	}
	userID := router.UserID(plan.attrs, plan.identity.Name)
	var out *llm.TranslateResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:           plan.chain,
		KeyName:         plan.identity.Name,
		UserID:          userID,
		TeamID:          plan.identity.TeamID,
		Estimate:        estimate,
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Cooldown:        plan.cooldown,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			// Without a usage tracker the adapter is the bare vendor one; a chat
			// model still translates, by prompt.
			var res *llm.TranslateResult
			var err error
			if translator, ok := adapter.(llm.Translator); ok {
				res, err = translator.Translate(callCtx, tr, userID, plan.meta)
			} else {
				res, err = llm.TranslateByPrompt(callCtx, adapter, tr, userID, plan.meta)
			}
			if err != nil {
				if res != nil {
					return res.Usage, err
				}
				return llm.TokensUsage{}, err
			}
			if len(res.Translations) != len(tr.Texts) {
				return res.Usage, &llm.TranslationMismatchError{Reason: "engine returned a different number of translations"}
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

	resp := passthrough(req.GetTexts(), tr.SourceLanguage)
	*resp = pb.TranslateResponse{
		Translations:   resp.Translations,
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		MatchedRule:    plan.ruleName,
		Usage: &pb.TokensUsage{
			Input:      int64(out.Usage.Input),
			Output:     int64(out.Usage.Output),
			Characters: int64(out.Usage.Characters),
		},
	}
	for j, t := range out.Translations {
		text := t.Text
		if vault != nil {
			text = vault.Desanitize(text)
		}
		resp.Translations[sent[j]] = &pb.Translation{Text: text, DetectedSourceLanguage: t.DetectedSourceLanguage}
	}
	return resp, nil
}

// passthrough returns the caller's texts as sent (never redacted), for those
// no engine is asked about.
func passthrough(texts []string, source string) *pb.TranslateResponse {
	out := &pb.TranslateResponse{Translations: make([]*pb.Translation, len(texts)), Usage: &pb.TokensUsage{}}
	for i, t := range texts {
		out.Translations[i] = &pb.Translation{Text: t, DetectedSourceLanguage: source}
	}
	return out
}

// translateRequestFromProto validates the wire request. A bad tag fails here
// rather than as a pair no model serves, which would read as a config gap.
func translateRequestFromProto(req *pb.TranslateRequest) (*llm.TranslateRequest, error) {
	texts := req.GetTexts()
	if len(texts) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: texts is empty")
	}
	if len(texts) > maxTranslateTexts {
		return nil, status.Errorf(codes.InvalidArgument, "bad_request: %d texts, at most %d per call", len(texts), maxTranslateTexts)
	}
	size := 0
	for _, t := range texts {
		size += len(t)
	}
	if size > maxTranslateBytes {
		return nil, status.Errorf(codes.InvalidArgument, "bad_request: texts total %d bytes, at most %d per call", size, maxTranslateBytes)
	}
	target := strings.TrimSpace(req.GetTargetLanguage())
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: target_language is required")
	}
	source := strings.TrimSpace(req.GetSourceLanguage())
	for _, tag := range []string{source, target} {
		if tag != "" && (tag == registry.AnyLanguage || !registry.ValidLanguageTag(tag)) {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: language %q: want a BCP-47 tag such as de or pt-BR", tag)
		}
	}
	format := strings.ToLower(strings.TrimSpace(req.GetFormat()))
	switch format {
	case "":
		format = llm.TranslateFormatText
	case llm.TranslateFormatText, llm.TranslateFormatHTML:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "bad_request: format %q: want text or html", req.GetFormat())
	}
	return &llm.TranslateRequest{
		Texts:          append([]string(nil), texts...),
		SourceLanguage: source,
		TargetLanguage: target,
		Format:         format,
		ActionID:       req.GetActionId(),
	}, nil
}
