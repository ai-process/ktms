package grpcapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
	"github.com/rs/zerolog/log"
)

const judgeAutoTimeout = 250 * time.Millisecond

type routePlan struct {
	effort           string
	models           []*proxydb.Model
	ruleName         string
	routeMode        string
	placement        string
	judgeScore       float64
	judgeModel       string
	shadowModel      *proxydb.Model
	shadowPercentage float64
	shadowEval       bool
	restricted       bool // data_classification restricted: nodes only, judge calls included
}

// promptText extracts representative text from instructions and user messages for judge evaluation.
// Truncates on rune boundaries up to 4000 characters.
func promptText(req *pb.GenerateTextRequest) string {
	var b strings.Builder
	if req.GetRequestInstruction() != "" {
		b.WriteString(req.GetRequestInstruction())
		b.WriteString("\n")
	}
	for _, m := range req.GetMessages() {
		if m.GetRole() == pb.MessageRole_MESSAGE_ROLE_USER {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(m.GetText())
			for _, p := range m.GetParts() {
				if txt := p.GetText().GetText(); txt != "" {
					b.WriteString("\n")
					b.WriteString(txt)
				}
			}
		}
	}
	runes := []rune(strings.TrimSpace(b.String()))
	if len(runes) > 4000 {
		return string(runes[:4000])
	}
	return string(runes)
}

// promptTextFromChat extracts representative text from ChatContext.
// It ensures any sensitive data masked by guardrails remains masked for the judge.
func promptTextFromChat(chat *llm.ChatContext) string {
	if chat == nil {
		return ""
	}
	var b strings.Builder
	if ins := chat.PeekRequestInstruction(); ins != "" {
		b.WriteString(ins)
		b.WriteString("\n")
	}
	for _, m := range chat.GetMessages() {
		if m.Type == llm.MessageTypeUser {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(m.Text)
			for _, p := range m.Parts {
				if p.Type == llm.ContentPartTypeText && p.Text != "" {
					b.WriteString("\n")
					b.WriteString(p.Text)
				}
			}
		}
	}
	runes := []rune(strings.TrimSpace(b.String()))
	if len(runes) > 4000 {
		return string(runes[:4000])
	}
	return string(runes)
}

// judgeEffort asks the configured Judge model to evaluate task complexity
// and recommend an effort level (low, medium, high).
// Fail-open: returns non-nil error so caller can gracefully fall back to default effort.
func (s *ProxyServer) judgeEffort(ctx context.Context, attrs map[string]string, text string) (effort string, score float64, judgeModel string, err error) {
	if text == "" {
		return registry.EffortMedium, 0.5, "", nil
	}

	judgeCtx, cancel := context.WithTimeout(withInternalCall(ctx), judgeAutoTimeout)
	defer cancel()

	plan, err := s.planMedia(judgeCtx, attrs, "", modalityJudge, registry.CapabilityJudge)
	if err != nil {
		return "", 0, "", err
	}

	stateJSON, err := json.Marshal(map[string]string{"prompt": text})
	if err != nil {
		return "", 0, "", err
	}

	judgeReq := &llm.JudgeRequest{
		StateJSON: string(stateJSON),
		Questions: map[string]*llm.JudgeQuestion{
			"complexity": {
				Type:         llm.JudgeChoice,
				Instructions: "Choose the computational complexity and model tier required to accurately answer this user prompt.",
				Options: map[string]string{
					"low":    "Simple, short, greeting, basic formatting, translation, or straightforward retrieval",
					"medium": "Standard instruction following, summary, general conversational chat, creative writing",
					"high":   "Complex multi-step reasoning, mathematical proof or calculation, advanced programming/coding, deep technical analysis",
				},
			},
		},
	}

	var out *llm.JudgeResult
	_, err = router.ExecuteMedia(judgeCtx, s.throttle, router.MediaRequest{
		Chain:           plan.chain,
		KeyName:         plan.identity.Name,
		UserID:          router.UserID(plan.attrs, plan.identity.Name),
		TeamID:          plan.identity.TeamID,
		Estimate:        50,
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Cooldown:        plan.cooldown,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			judge, ok := adapter.(llm.Judge)
			if !ok {
				return llm.TokensUsage{}, llm.ErrJudgmentUnsupported
			}
			res, err := judge.Judge(callCtx, judgeReq, router.UserID(plan.attrs, plan.identity.Name), plan.meta)
			if err != nil {
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	if err != nil {
		ev := log.Warn().Err(err)
		if plan.identity != nil {
			ev = ev.Str("key_id", plan.identity.KeyID)
		}
		if len(plan.chain) > 0 {
			ids := make([]string, len(plan.chain))
			for i, m := range plan.chain {
				ids[i] = m.ID
			}
			ev = ev.Str("judge_chain", strings.Join(ids, ","))
		}
		ev.Msg("judge auto routing failed, falling back to default effort")
		return "", 0, "", err
	}

	var modelName string
	if out != nil {
		modelName = out.Model
		if out.Answers != nil {
			if ans, ok := out.Answers["complexity"]; ok && ans != nil {
				switch strings.ToLower(ans.Choice) {
				case "low":
					effort = registry.EffortLow
				case "high":
					effort = registry.EffortHigh
				default:
					effort = registry.EffortMedium
				}
				score = ans.Confidence
				if score == 0 && ans.Score > 0 {
					score = ans.Score
				}
				return effort, score, modelName, nil
			}
		}
	}

	return registry.EffortMedium, 0.5, modelName, nil
}

// planRoute resolves the model candidate chain, evaluating placement preferences and auto-judge when applicable.
// With a trace it also records why each model was dropped, and never calls the judge.
func (s *ProxyServer) planRoute(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, req *pb.GenerateTextRequest, tr *routeTrace) (*routePlan, error) {
	placement := router.NormalizePlacement(req.GetPlacement())
	restricted := isRestricted(req)
	if restricted {
		var err error
		if placement, err = restrictedPlacement(placement); err != nil {
			return nil, err
		}
		ctx = withRestricted(ctx)
	}
	isAuto := req.GetAutoRoute()
	effortSpecified := req.GetEffortSpecified()

	effort := effortToString(req.GetEffort())
	var (
		routeMode  = router.RouteModeDirect
		judgeScore float64
		judgeModel string
	)

	switch {
	case req.GetModelOverride() != "":
		routeMode = router.RouteModeOverride
		tr.start(routeMode, []string{req.GetModelOverride()}, nil)
		if !id.CanAccessModel(req.GetModelOverride()) {
			tr.add(router.Candidate{ID: req.GetModelOverride(), Model: snap.Models[req.GetModelOverride()], Dropped: id.DeniedBy(req.GetModelOverride()) + "_policy"})
			return nil, status.Errorf(codes.PermissionDenied, "model %q not allowed for %s", req.GetModelOverride(), id.DeniedBy(req.GetModelOverride()))
		}
		m, ok := snap.Models[req.GetModelOverride()]
		if !ok {
			tr.add(router.Candidate{ID: req.GetModelOverride(), Dropped: router.DropNotFound})
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", req.GetModelOverride())
		}
		if effort == "" && len(m.Efforts) > 0 {
			effort = m.Efforts[0]
		}
		tr.matched(effort)
		models, err := tr.filter(snap, []string{req.GetModelOverride()}, id.KeyID, effort, req.GetEnableGoogleSearch(), tr.capabilities(req))
		if err != nil {
			return nil, router.ToStatus(err)
		}
		if placement != "" {
			before := models
			models, err = router.ApplyPlacement(models, placement)
			tr.placed(before, models)
			if err != nil {
				return nil, router.ToStatus(err)
			}
		}
		return &routePlan{
			effort:     effort,
			models:     models,
			routeMode:  routeMode,
			placement:  placement,
			restricted: restricted,
		}, nil

	case isAuto:
		if effortSpecified && effort != "" {
			routeMode = router.RouteModeAutoEffort
		} else if tr != nil {
			// A dry run must not call a model: route as the judge's fallback does.
			effort, routeMode, tr.judgeSkipped = registry.EffortMedium, router.RouteModeAutoDefault, true
		} else {
			jEffort, score, jModel, jErr := s.judgeEffort(ctx, req.GetAttributes(), promptText(req))
			if jErr == nil && jEffort != "" {
				effort = jEffort
				routeMode = router.RouteModeAutoJudge
				judgeScore = score
				judgeModel = jModel
			} else {
				effort = registry.EffortMedium
				routeMode = router.RouteModeAutoDefault
			}
		}

	case effort == "":
		return nil, status.Error(codes.InvalidArgument, "bad_request: effort is required")
	}

	tr.matched(effort)
	rule := router.Match(snap, effort, routingAttributes(req.GetAttributes(), id))
	if rule == nil {
		tr.start(routeMode, nil, nil)
		return nil, router.ToStatus(router.ErrNotConfigured)
	}
	canaryKey := router.CanaryKey(req.GetAttributes())
	chain := router.ResolveChain(rule, canaryKey)
	tr.start(routeMode, chain, rule)

	reqCaps := tr.capabilities(req)
	models, err := tr.filter(snap, chain, id.KeyID, effort, req.GetEnableGoogleSearch(), reqCaps)
	if err != nil {
		return nil, router.ToStatus(err)
	}

	tr.policy(ctx, id, models)
	if models, err = teamModels(ctx, id, models); err != nil {
		return nil, err
	}

	if placement != "" {
		before := models
		models, err = router.ApplyPlacement(models, placement)
		tr.placed(before, models)
		if err != nil {
			return nil, router.ToStatus(err)
		}
	}

	var (
		shadowModel *proxydb.Model
		shadowPct   float64
		shadowEval  bool
	)
	if rule.ShadowModel != "" && rule.ShadowPercentage > 0 && id.CanAccessModel(rule.ShadowModel) {
		shadowChain, sErr := router.FilterChain(snap, []string{rule.ShadowModel}, id.KeyID, effort, req.GetEnableGoogleSearch(), reqCaps...)
		if sErr == nil && len(shadowChain) > 0 {
			if placement != "" {
				shadowChain, sErr = router.ApplyPlacement(shadowChain, placement)
			}
			if sErr == nil && len(shadowChain) > 0 {
				shadowModel = shadowChain[0]
				shadowPct = rule.ShadowPercentage
				shadowEval = rule.ShadowEval
			}
		}
	}

	return &routePlan{
		effort:           effort,
		models:           models,
		ruleName:         rule.Name,
		routeMode:        routeMode,
		placement:        placement,
		judgeScore:       judgeScore,
		judgeModel:       judgeModel,
		shadowModel:      shadowModel,
		shadowPercentage: shadowPct,
		shadowEval:       shadowEval,
		restricted:       restricted,
	}, nil
}
