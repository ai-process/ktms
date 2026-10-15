package grpcapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
	"github.com/rs/zerolog/log"
)

const judgeAutoTimeout = 250 * time.Millisecond

type routePlan struct {
	effort     string
	models     []*proxydb.Model
	ruleName   string
	routeMode  string
	placement  string
	judgeScore float64
	judgeModel string
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

// judgeEffort asks the configured Judge model to evaluate task complexity
// and recommend an effort level (low, medium, high).
// Fail-open: returns non-nil error so caller can gracefully fall back to default effort.
func (s *ProxyServer) judgeEffort(ctx context.Context, attrs map[string]string, text string) (effort string, score float64, judgeModel string, err error) {
	if text == "" {
		return registry.EffortMedium, 0.5, "", nil
	}

	judgeCtx, cancel := context.WithTimeout(ctx, judgeAutoTimeout)
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
		Chain:    plan.chain,
		KeyName:  plan.identity.Name,
		UserID:   router.UserID(plan.attrs, plan.identity.Name),
		Estimate: 50,
		Adapter:  plan.adapter,
		Call: func(m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			judge, ok := adapter.(llm.Judge)
			if !ok {
				return llm.TokensUsage{}, llm.ErrJudgmentUnsupported
			}
			res, err := judge.Judge(judgeCtx, judgeReq, router.UserID(plan.attrs, plan.identity.Name), plan.meta)
			if err != nil {
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	if err != nil {
		ev := log.Warn().Err(err)
		if plan != nil && plan.identity != nil {
			ev = ev.Str("key_id", plan.identity.KeyID)
		}
		if plan != nil && len(plan.chain) > 0 {
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
func (s *ProxyServer) planRoute(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, req *pb.GenerateTextRequest) (*routePlan, error) {
	placement := router.NormalizePlacement(req.GetPlacement())
	if placement == "" {
		placement = router.NormalizePlacement(req.GetAttributes()[router.PlacementAttribute])
	}
	isAuto := req.GetAutoRoute() || req.GetAttributes()[router.AutoRouteAttribute] == "true"
	effortSpecified := req.GetEffortSpecified() || req.GetAttributes()[router.EffortSpecifiedAttribute] == "true"

	effort := effortToString(req.GetEffort())
	var (
		routeMode  = router.RouteModeDirect
		judgeScore float64
		judgeModel string
	)

	switch {
	case req.GetModelOverride() != "":
		routeMode = router.RouteModeOverride
		m, ok := snap.Models[req.GetModelOverride()]
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", req.GetModelOverride())
		}
		if effort == "" && len(m.Efforts) > 0 {
			effort = m.Efforts[0]
		}
		models, err := router.FilterChain(snap, []string{req.GetModelOverride()}, id.KeyID, effort, req.GetEnableGoogleSearch(), requiredCapabilities(req)...)
		if err != nil {
			return nil, router.ToStatus(err)
		}
		if placement != "" {
			models, err = router.ApplyPlacement(models, placement)
			if err != nil {
				return nil, router.ToStatus(err)
			}
		}
		return &routePlan{
			effort:    effort,
			models:    models,
			routeMode: routeMode,
			placement: placement,
		}, nil

	case isAuto:
		if effortSpecified && effort != "" {
			routeMode = router.RouteModeAutoEffort
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

	rule := router.Match(snap, effort, req.GetAttributes())
	if rule == nil {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}
	chain := router.ResolveChain(rule)

	reqCaps := requiredCapabilities(req)
	models, err := router.FilterChain(snap, chain, id.KeyID, effort, req.GetEnableGoogleSearch(), reqCaps...)
	if err != nil {
		return nil, router.ToStatus(err)
	}

	if placement != "" {
		models, err = router.ApplyPlacement(models, placement)
		if err != nil {
			return nil, router.ToStatus(err)
		}
	}

	return &routePlan{
		effort:     effort,
		models:     models,
		ruleName:   rule.Name,
		routeMode:  routeMode,
		placement:  placement,
		judgeScore: judgeScore,
		judgeModel: judgeModel,
	}, nil
}
