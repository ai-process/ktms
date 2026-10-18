package grpcapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/panicsafe"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

type judgeThrottle struct {
	router.Throttle
}

func (j judgeThrottle) Reserve(context.Context, *proxydb.Model, string, string, int64, int64) (*router.Reservation, string) {
	return nil, ""
}

func (j judgeThrottle) Settle(*router.Reservation, llm.TokensUsage) {}

func (s *ProxyServer) makeShadowEvaluator(id *apikeys.Identity, chat *llm.ChatContext, attrs map[string]string) func(context.Context, *proxydb.Model, *proxydb.Model, *llm.Response, *llm.Response) {
	return func(ctx context.Context, primaryModel, shadowModel *proxydb.Model, primaryResp, shadowResp *llm.Response) {
		prompt := promptTextFromChat(chat)
		s.evaluateShadowJudge(ctx, id, prompt, attrs, primaryModel, shadowModel, primaryResp, shadowResp)
	}
}

func truncateRunes(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return s
}

func (s *ProxyServer) evaluateShadowJudge(ctx context.Context, id *apikeys.Identity, prompt string, attrs map[string]string, primaryModel, shadowModel *proxydb.Model, primaryResp, shadowResp *llm.Response) {
	if id == nil || primaryModel == nil || shadowModel == nil || primaryResp == nil || shadowResp == nil {
		return
	}
	if primaryModel.ID == shadowModel.ID {
		return
	}
	defer panicsafe.Guard("grpcapi: evaluateShadowJudge")

	var primaryText string
	if len(primaryResp.Choices) > 0 && primaryResp.Choices[0] != nil {
		primaryText = strings.TrimSpace(primaryResp.Choices[0].Text)
	}
	var shadowText string
	if len(shadowResp.Choices) > 0 && shadowResp.Choices[0] != nil {
		shadowText = strings.TrimSpace(shadowResp.Choices[0].Text)
	}
	if primaryText == "" || shadowText == "" {
		return
	}

	primaryText = truncateRunes(primaryText, 4000)
	shadowText = truncateRunes(shadowText, 4000)
	prompt = truncateRunes(prompt, 4000)

	judgeCtx, cancel := context.WithTimeout(llm.WithShadowTraffic(apikeys.WithIdentity(ctx, id)), 15*time.Second)
	defer cancel()

	plan, err := s.planMedia(judgeCtx, attrs, "", modalityJudge, registry.CapabilityJudge)
	if err != nil {
		log.Warn().Err(err).Msg("shadow judge evaluation skipped: no judge model available")
		observability.ShadowJudgeEvaluationsTotal.WithLabelValues("unconfigured", "unconfigured", "no_judge").Inc()
		return
	}
	plan.meta["is_shadow"] = "true"
	plan.meta["shadow"] = "true"

	stateJSON, err := json.Marshal(map[string]string{
		"prompt":           prompt,
		"primary_model":    primaryModel.ID,
		"primary_response": primaryText,
		"shadow_model":     shadowModel.ID,
		"shadow_response":  shadowText,
	})
	if err != nil {
		log.Warn().Err(err).Msg("shadow judge: failed to marshal state json")
		return
	}

	judgeReq := &llm.JudgeRequest{
		StateJSON: string(stateJSON),
		Questions: map[string]*llm.JudgeQuestion{
			"preference": {
				Type:         llm.JudgeChoice,
				Instructions: "Compare the primary and shadow model responses to the user prompt. Which one is better in quality, accuracy, and adherence to instructions?",
				Options: map[string]string{
					"primary": "The primary response is distinctly better.",
					"shadow":  "The shadow response is distinctly better.",
					"tie":     "Both responses are of comparable quality or equally good/bad.",
				},
			},
			"primary_quality": {
				Type:         llm.JudgeScore,
				Instructions: "Rate the overall quality of the primary model response.",
				Levels:       []string{"poor", "fair", "good", "excellent"},
			},
			"shadow_quality": {
				Type:         llm.JudgeScore,
				Instructions: "Rate the overall quality of the shadow model response.",
				Levels:       []string{"poor", "fair", "good", "excellent"},
			},
		},
	}

	estTokens := int64((len([]rune(prompt)) + len([]rune(primaryText)) + len([]rune(shadowText)))/4 + 100)
	var th router.Throttle = router.NoopThrottle{}
	if s.throttle != nil {
		th = judgeThrottle{s.throttle}
	}
	var out *llm.JudgeResult
	_, err = router.ExecuteMedia(judgeCtx, th, router.MediaRequest{
		Chain:          plan.chain,
		KeyName:        plan.identity.Name,
		UserID:         router.UserID(plan.attrs, plan.identity.Name),
		Estimate:       estTokens,
		CircuitBreaker: s.circuitBreaker,
		Adapter:        plan.adapter,
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
		log.Warn().Err(err).Msg("shadow judge evaluation failed")
		return
	}

	if out != nil && out.Answers != nil {
		preference := "tie"
		if ans, ok := out.Answers["preference"]; ok && ans != nil && ans.Choice != "" {
			switch strings.ToLower(ans.Choice) {
			case "primary":
				preference = "primary"
			case "shadow":
				preference = "shadow"
			default:
				preference = "tie"
			}
		}
		observability.ShadowJudgeEvaluationsTotal.WithLabelValues(primaryModel.ID, shadowModel.ID, preference).Inc()

		if pScore, ok := out.Answers["primary_quality"]; ok && pScore != nil {
			observability.ShadowJudgeScores.WithLabelValues(primaryModel.ID, "primary").Observe(pScore.Score)
		}
		if sScore, ok := out.Answers["shadow_quality"]; ok && sScore != nil {
			observability.ShadowJudgeScores.WithLabelValues(shadowModel.ID, "shadow").Observe(sScore.Score)
		}

		log.Info().
			Str("primary_model", primaryModel.ID).
			Str("shadow_model", shadowModel.ID).
			Str("preference", preference).
			Msg("shadow judge evaluation finished")
	}
}
