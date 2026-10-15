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
	s := strings.TrimSpace(b.String())
	if len(s) > 4000 {
		return s[:4000]
	}
	return s
}

// judgeEffort asks the configured Judge model to evaluate task complexity
// and recommend an effort level (low, medium, high).
// Fail-open: returns non-nil error so caller can gracefully fall back to default effort.
func (s *ProxyServer) judgeEffort(ctx context.Context, id *apikeys.Identity, text string) (effort string, score float64, judgeModel string, err error) {
	if text == "" {
		return registry.EffortMedium, 0.5, "", nil
	}

	judgeCtx, cancel := context.WithTimeout(ctx, judgeAutoTimeout)
	defer cancel()

	plan, err := s.planMedia(judgeCtx, map[string]string{}, "", modalityJudge, registry.CapabilityJudge)
	if err != nil {
		return "", 0, "", err
	}

	stateJSON, _ := json.Marshal(map[string]string{"prompt": text})
	judgeReq := &llm.JudgeRequest{
		StateJSON: string(stateJSON),
		Questions: map[string]*llm.JudgeQuestion{
			"complexity": {
				Type: llm.JudgeChoice,
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
		log.Warn().Err(err).Msg("judge auto routing failed, falling back to default effort")
		return "", 0, "", err
	}

	if out != nil && out.Answers != nil {
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
			judgeModel = out.Model
			return effort, score, judgeModel, nil
		}
	}

	return registry.EffortMedium, 0.5, out.Model, nil
}

// planRoute resolves the model candidate chain, evaluating placement preferences and auto-judge when applicable.
func (s *ProxyServer) planRoute(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, req *pb.GenerateTextRequest) (*routePlan, error) {
	placement := router.NormalizePlacement(req.GetAttributes()[router.PlacementAttribute])
	isAuto := req.GetAttributes()[router.AutoRouteAttribute] == "true"
	effortSpecified := req.GetAttributes()[router.EffortSpecifiedAttribute] == "true"

	effort := effortToString(req.GetEffort())
	var (
		chain      []string
		ruleName   string
		routeMode  = router.RouteModeDirect
		judgeScore float64
		judgeModel string
	)

	switch {
	case req.GetModelOverride() != "":
		routeMode = router.RouteModeOverride
		if _, ok := snap.Models[req.GetModelOverride()]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: unknown model %q", req.GetModelOverride())
		}
		chain = []string{req.GetModelOverride()}
		if effort == "" {
			effort = snap.Models[req.GetModelOverride()].Efforts[0]
		}
	case isAuto:
		if effortSpecified && effort != "" {
			routeMode = router.RouteModeAutoEffort
		} else {
			jEffort, score, jModel, jErr := s.judgeEffort(ctx, id, promptText(req))
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
		rule := router.Match(snap, effort, req.GetAttributes())
		if rule == nil {
			return nil, router.ToStatus(router.ErrNotConfigured)
		}
		chain = router.ResolveChain(rule)
		ruleName = rule.Name
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

	if placement != "" {
		models, err = router.ApplyPlacement(models, placement)
		if err != nil {
			return nil, router.ToStatus(err)
		}
	}

	return &routePlan{
		effort:     effort,
		models:     models,
		ruleName:   ruleName,
		routeMode:  routeMode,
		placement:  placement,
		judgeScore: judgeScore,
		judgeModel: judgeModel,
	}, nil
}
