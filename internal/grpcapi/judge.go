package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
	"github.com/ai-process/ktms/internal/safefetch"
)

const modalityJudge = "judge"

// State media ceilings. A judgment is a request-path decision: these bound the
// budget reservation and vendor payload before any vendor sees the request.
const (
	maxJudgeMediaParts    = 32
	maxJudgeMediaBytes    = 16 << 20
	maxJudgeEmbeddingDims = 16384
)

// Judge answers typed questions about one state. It is a decision RPC: no text
// is generated, and the answers carry the probabilities the caller routes on.
func (s *ProxyServer) Judge(ctx context.Context, req *pb.JudgeRequest) (*pb.JudgeResponse, error) {
	judgeReq, err := judgeRequestFromProto(req)
	if err != nil {
		return nil, err
	}
	var vault *guardrails.TokenVault
	if s.guardrails != nil && s.guardrails.Enabled {
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
		}
		sanitizedState, _, err := s.guardrails.InspectText(ctx, judgeReq.StateJSON, vault)
		if err != nil {
			return nil, guardrailStatus(err)
		}
		judgeReq.StateJSON = sanitizedState
		// Images, audio and vectors are opaque to text guardrails; only the
		// text parts can be redacted or blocked here.
		for _, p := range judgeReq.StateMedia {
			if p.Type != llm.JudgeMediaText {
				continue
			}
			sanitizedText, _, err := s.guardrails.InspectText(ctx, p.Text, vault)
			if err != nil {
				return nil, guardrailStatus(err)
			}
			p.Text = sanitizedText
		}
		for _, q := range judgeReq.Questions {
			sanitizedInst, _, err := s.guardrails.InspectText(ctx, q.Instructions, vault)
			if err != nil {
				return nil, guardrailStatus(err)
			}
			q.Instructions = sanitizedInst
			for i, opt := range q.Options {
				sanitizedOpt, _, err := s.guardrails.InspectText(ctx, opt, vault)
				if err != nil {
					return nil, guardrailStatus(err)
				}
				q.Options[i] = sanitizedOpt
			}
		}
	}
	plan, err := s.planMedia(ctx, req.GetAttributes(), req.GetModelOverride(), modalityJudge, judgeCapabilities(judgeReq)...)
	if err != nil {
		return nil, err
	}

	var out *llm.JudgeResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:           plan.chain,
		KeyName:         plan.identity.Name,
		UserID:          router.UserID(plan.attrs, plan.identity.Name),
		TeamID:          plan.identity.TeamID,
		Estimate:        judgeEstimate(judgeReq),
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Cooldown:        plan.cooldown,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			judge, ok := adapter.(llm.Judge)
			if !ok {
				// Models misconfigured with the judge capability should yield to
				// capable fallback models in the chain instead of failing immediately.
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
		return nil, router.ToStatus(err)
	}

	resp := &pb.JudgeResponse{
		Answers:        make(map[string]*pb.JudgeAnswer, len(out.Answers)),
		Usage:          &pb.TokensUsage{Input: int64(out.Usage.Input), Output: int64(out.Usage.Output)},
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		MatchedRule:    plan.ruleName,
	}
	for id, ans := range out.Answers {
		legend := ans.Legend
		if vault != nil && len(legend) > 0 {
			desan := make(map[string]string, len(legend))
			for k, v := range legend {
				desan[k] = vault.Desanitize(v)
			}
			legend = desan
		}
		resp.Answers[id] = &pb.JudgeAnswer{
			Type:          judgeTypeToProto(ans.Type),
			Choice:        ans.Choice,
			Noul:          ans.Noul,
			Score:         ans.Score,
			Probabilities: ans.Probabilities,
			Confidence:    ans.Confidence,
			Legend:        legend,
		}
	}
	return resp, nil
}

// judgeRequestFromProto validates the wire request and converts it. Validation
// is strict on purpose: a malformed question costs a vendor round trip and
// comes back as an answer nobody can interpret.
func judgeRequestFromProto(req *pb.JudgeRequest) (*llm.JudgeRequest, error) {
	state := req.GetStateJson()
	if state == "" && len(req.GetStateMedia()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: state_json and state_media are both empty")
	}
	if state != "" && !json.Valid([]byte(state)) {
		return nil, status.Error(codes.InvalidArgument, "bad_request: state_json is not valid JSON")
	}
	if len(req.GetQuestions()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: no questions")
	}
	media, err := judgeMediaFromProto(req.GetStateMedia())
	if err != nil {
		return nil, err
	}

	out := &llm.JudgeRequest{
		StateJSON:  state,
		StateMedia: media,
		Questions:  make(map[string]*llm.JudgeQuestion, len(req.GetQuestions())),
		ActionID:   req.GetActionId(),
	}
	for id, q := range req.GetQuestions() {
		if q.GetInstructions() == "" {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: question %q has no instructions", id)
		}
		kind := judgeTypeFromProto(q.GetType())
		switch kind {
		case llm.JudgeChoice:
			if len(q.GetOptions()) < 2 {
				return nil, status.Errorf(codes.InvalidArgument, "bad_request: choice %q needs at least two options", id)
			}
		case llm.JudgeScore:
			if len(q.GetLevels()) < 2 {
				return nil, status.Errorf(codes.InvalidArgument, "bad_request: score %q needs at least two levels", id)
			}
		case llm.JudgeNoul:
		default:
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: question %q has no type", id)
		}
		out.Questions[id] = &llm.JudgeQuestion{
			Type:          kind,
			Instructions:  q.GetInstructions(),
			Options:       q.GetOptions(),
			Levels:        q.GetLevels(),
			TrueCriteria:  q.GetTrueCriteria(),
			FalseCriteria: q.GetFalseCriteria(),
		}
	}
	return out, nil
}

// judgeMediaFromProto validates and converts state media. Ceilings are checked
// here rather than left to the vendor so an oversized state never reserves budget.
func judgeMediaFromProto(in []*pb.MediaPart) ([]*llm.JudgeMediaPart, error) {
	if len(in) > maxJudgeMediaParts {
		return nil, status.Errorf(codes.InvalidArgument, "bad_request: state_media has %d parts, max %d", len(in), maxJudgeMediaParts)
	}
	bad := func(i int, format string, args ...any) error {
		return status.Errorf(codes.InvalidArgument, "bad_request: state_media[%d]: "+format, append([]any{i}, args...)...)
	}
	out := make([]*llm.JudgeMediaPart, 0, len(in))
	total := 0
	for i, p := range in {
		var part *llm.JudgeMediaPart
		switch v := p.GetPart().(type) {
		case *pb.MediaPart_Text:
			if v.Text.GetText() == "" {
				return nil, bad(i, "text is empty")
			}
			part = &llm.JudgeMediaPart{Type: llm.JudgeMediaText, Text: v.Text.GetText()}
			total += len(part.Text)
		case *pb.MediaPart_Image:
			img := v.Image
			part = &llm.JudgeMediaPart{
				Type:     llm.JudgeMediaImage,
				MimeType: img.GetMimeType(),
				Data:     img.GetData(),
				URL:      img.GetUrl(),
				Detail:   img.GetDetail(),
			}
			switch part.Detail {
			case "", string(llm.ImageDetailAuto), string(llm.ImageDetailLow), string(llm.ImageDetailHigh):
			default:
				return nil, bad(i, "unsupported image detail %q", part.Detail)
			}
			if len(part.Data) > 0 {
				if err := validateJudgeMime(part.MimeType, "image/"); err != nil {
					return nil, bad(i, "%v", err)
				}
				// Inline bytes win; a data URI echoed in url would double the payload.
				part.URL = ""
			} else if !strings.HasPrefix(part.URL, "https://") {
				return nil, bad(i, "image needs data or an https url")
			}
			total += len(part.Data) + len(part.URL)
		case *pb.MediaPart_Audio:
			aud := v.Audio
			part = &llm.JudgeMediaPart{
				Type:     llm.JudgeMediaAudio,
				MimeType: aud.GetMimeType(),
				Data:     aud.GetData(),
				Format:   aud.GetFormat(),
			}
			if len(part.Data) == 0 {
				return nil, bad(i, "audio has no data")
			}
			if part.Format == "" {
				part.Format = "wav"
			}
			if part.MimeType == "" {
				part.MimeType = "audio/" + part.Format
			}
			if err := validateJudgeMime(part.MimeType, "audio/"); err != nil {
				return nil, bad(i, "%v", err)
			}
			total += len(part.Data)
		case *pb.MediaPart_Embedding:
			vals := v.Embedding.GetValues()
			if len(vals) == 0 {
				return nil, bad(i, "embedding has no values")
			}
			if len(vals) > maxJudgeEmbeddingDims {
				return nil, bad(i, "embedding has %d dimensions, max %d", len(vals), maxJudgeEmbeddingDims)
			}
			// NaN and Inf have no JSON encoding; they would fail at the vendor
			// call after the budget is already reserved.
			for _, f := range vals {
				if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
					return nil, bad(i, "embedding has a non-finite value")
				}
			}
			part = &llm.JudgeMediaPart{
				Type:           llm.JudgeMediaEmbedding,
				Embedding:      vals,
				EmbeddingModel: v.Embedding.GetModel(),
			}
			total += 4 * len(vals)
		default:
			return nil, bad(i, "part is empty")
		}
		if total > maxJudgeMediaBytes {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: state_media exceeds %d bytes", maxJudgeMediaBytes)
		}
		out = append(out, part)
	}
	return out, nil
}

func validateJudgeMime(mime, family string) error {
	if err := safefetch.ValidateMediaMimeType(mime); err != nil {
		return err
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), family) {
		return fmt.Errorf("mime type %q is not %s*", mime, family)
	}
	return nil
}

// judgeCapabilities is what a chain model must declare to serve the request:
// a judge that silently drops an image would answer about a different state.
func judgeCapabilities(req *llm.JudgeRequest) []string {
	caps := []string{registry.CapabilityJudge}
	seen := make(map[string]bool, 3)
	for _, p := range req.StateMedia {
		var c string
		switch p.Type {
		case llm.JudgeMediaImage:
			c = registry.CapabilityVision
		case llm.JudgeMediaAudio:
			c = registry.CapabilityAudioInput
		case llm.JudgeMediaEmbedding:
			c = registry.CapabilityEmbeddingInput
		}
		if c != "" && !seen[c] {
			seen[c] = true
			caps = append(caps, c)
		}
	}
	return caps
}

// judgeEstimate is the budget reservation guess: the state plus every question
// is what the vendor bills, and it bills input only. Media allowances mirror
// estimateTokens for chat parts.
func judgeEstimate(req *llm.JudgeRequest) int64 {
	runes := utf8.RuneCountInString(req.StateJSON)
	var extraTokens int64
	for _, p := range req.StateMedia {
		switch p.Type {
		case llm.JudgeMediaText:
			runes += utf8.RuneCountInString(p.Text)
		case llm.JudgeMediaImage:
			if p.Detail == string(llm.ImageDetailLow) {
				extraTokens += 85
			} else {
				extraTokens += 1000
			}
		case llm.JudgeMediaAudio:
			extraTokens += 200
		case llm.JudgeMediaEmbedding:
			extraTokens += int64(len(p.Embedding))/4 + 1
		}
	}
	for _, q := range req.Questions {
		runes += utf8.RuneCountInString(q.Instructions)
		for _, opt := range q.Options {
			runes += utf8.RuneCountInString(opt)
		}
		for _, level := range q.Levels {
			runes += utf8.RuneCountInString(level)
		}
		runes += utf8.RuneCountInString(q.TrueCriteria) + utf8.RuneCountInString(q.FalseCriteria)
	}
	return int64(runes)/4 + extraTokens + 1
}

func judgeTypeFromProto(t pb.JudgeType) string {
	switch t {
	case pb.JudgeType_JUDGE_TYPE_NOUL:
		return llm.JudgeNoul
	case pb.JudgeType_JUDGE_TYPE_CHOICE:
		return llm.JudgeChoice
	case pb.JudgeType_JUDGE_TYPE_SCORE:
		return llm.JudgeScore
	default:
		return ""
	}
}

func judgeTypeToProto(t string) pb.JudgeType {
	switch t {
	case llm.JudgeNoul:
		return pb.JudgeType_JUDGE_TYPE_NOUL
	case llm.JudgeChoice:
		return pb.JudgeType_JUDGE_TYPE_CHOICE
	case llm.JudgeScore:
		return pb.JudgeType_JUDGE_TYPE_SCORE
	default:
		return pb.JudgeType_JUDGE_TYPE_UNSPECIFIED
	}
}

// JudgeStream provides bidirectional streaming decisions for real-time
// agent evaluation and robotics control loops.
func (s *ProxyServer) JudgeStream(stream pb.GatewayService_JudgeStreamServer) error {
	ctx := stream.Context()
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		resp, err := s.Judge(ctx, req)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

const maxRolloutCandidates = 64

// EvaluateRollouts scores multiple candidate trajectories against state and criteria
// in parallel for speculative decoding and Best-of-N selection.
func (s *ProxyServer) EvaluateRollouts(ctx context.Context, req *pb.EvaluateRolloutsRequest) (*pb.EvaluateRolloutsResponse, error) {
	rolloutReq, err := evaluateRolloutsRequestFromProto(req)
	if err != nil {
		return nil, err
	}
	var vault *guardrails.TokenVault
	if s.guardrails != nil && s.guardrails.Enabled {
		if s.guardrails.PIIAction == guardrails.ActionRedact {
			vault = guardrails.NewTokenVault()
		}
		if rolloutReq.StateJSON != "" {
			sanitizedState, _, err := s.guardrails.InspectText(ctx, rolloutReq.StateJSON, vault)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
			rolloutReq.StateJSON = sanitizedState
		}
		for _, p := range rolloutReq.StateMedia {
			if p.Type != llm.JudgeMediaText {
				continue
			}
			sanitizedText, _, err := s.guardrails.InspectText(ctx, p.Text, vault)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
			p.Text = sanitizedText
		}
		for _, q := range rolloutReq.Criteria {
			sanitizedInst, _, err := s.guardrails.InspectText(ctx, q.Instructions, vault)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
			q.Instructions = sanitizedInst
			for i, opt := range q.Options {
				sanitizedOpt, _, err := s.guardrails.InspectText(ctx, opt, vault)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
				}
				q.Options[i] = sanitizedOpt
			}
		}
		for _, c := range rolloutReq.Candidates {
			if c.ActionJSON != "" {
				sanitizedAct, _, err := s.guardrails.InspectText(ctx, c.ActionJSON, vault)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
				}
				c.ActionJSON = sanitizedAct
			}
			if c.ActionText != "" {
				sanitizedTxt, _, err := s.guardrails.InspectText(ctx, c.ActionText, vault)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
				}
				c.ActionText = sanitizedTxt
			}
			for _, p := range c.ActionMedia {
				if p.Type != llm.JudgeMediaText {
					continue
				}
				sanitizedText, _, err := s.guardrails.InspectText(ctx, p.Text, vault)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
				}
				p.Text = sanitizedText
			}
		}
	}

	plan, err := s.planMedia(ctx, req.GetAttributes(), req.GetModelOverride(), modalityJudge, rolloutCapabilities(rolloutReq)...)
	if err != nil {
		return nil, err
	}

	userID := router.UserID(plan.attrs, plan.identity.Name)
	var out *llm.EvaluateRolloutsResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:           plan.chain,
		KeyName:         plan.identity.Name,
		UserID:          userID,
		TeamID:          plan.identity.TeamID,
		Estimate:        rolloutEstimate(rolloutReq),
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Cooldown:        plan.cooldown,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			if rj, ok := adapter.(llm.RolloutJudge); ok {
				res, err := rj.EvaluateRollouts(callCtx, rolloutReq, userID, plan.meta)
				if err == nil {
					out = res
					return res.Usage, nil
				}
				if !errors.Is(err, llm.ErrJudgmentUnsupported) {
					if res != nil {
						return res.Usage, err
					}
					return llm.TokensUsage{}, err
				}
			}
			judge, ok := adapter.(llm.Judge)
			if !ok {
				return llm.TokensUsage{}, llm.ErrJudgmentUnsupported
			}
			res, err := executeParallelCandidateRollouts(callCtx, judge, rolloutReq, userID, plan.meta)
			if err != nil {
				if res != nil {
					return res.Usage, err
				}
				return llm.TokensUsage{}, err
			}
			out = res
			return res.Usage, nil
		},
	})
	if err != nil {
		return nil, router.ToStatus(err)
	}

	evals := make([]*pb.CandidateEvaluation, 0, len(out.Evaluations))
	for _, e := range out.Evaluations {
		ce := &pb.CandidateEvaluation{
			Id:             e.ID,
			CompositeScore: e.CompositeScore,
			Rank:           int32(e.Rank),
			Answers:        make(map[string]*pb.JudgeAnswer, len(e.Answers)),
		}
		for qID, ans := range e.Answers {
			legend := ans.Legend
			if vault != nil && len(legend) > 0 {
				desan := make(map[string]string, len(legend))
				for k, v := range legend {
					desan[k] = vault.Desanitize(v)
				}
				legend = desan
			}
			ce.Answers[qID] = &pb.JudgeAnswer{
				Type:          judgeTypeToProto(ans.Type),
				Choice:        ans.Choice,
				Noul:          ans.Noul,
				Score:         ans.Score,
				Probabilities: ans.Probabilities,
				Confidence:    ans.Confidence,
				Legend:        legend,
			}
		}
		evals = append(evals, ce)
	}

	return &pb.EvaluateRolloutsResponse{
		Evaluations:     evals,
		BestCandidateId: out.BestCandidateID,
		Usage:           &pb.TokensUsage{Input: int64(out.Usage.Input), Output: int64(out.Usage.Output)},
		ResolvedModel:   model.ID,
		ResolvedVendor:  model.Vendor,
		MatchedRule:     plan.ruleName,
	}, nil
}

// maxRolloutConcurrency bounds parallel rollout evaluations to avoid socket starvation.
const maxRolloutConcurrency = 16

func executeParallelCandidateRollouts(ctx context.Context, judge llm.Judge, rolloutReq *llm.EvaluateRolloutsRequest, userID string, meta map[string]string) (*llm.EvaluateRolloutsResult, error) {
	evals := make([]*llm.CandidateEvaluation, len(rolloutReq.Candidates))
	var (
		mu         sync.Mutex
		totalUsage llm.TokensUsage
	)
	g, gCtx := errgroup.WithContext(ctx)
	concurrency := maxRolloutConcurrency
	if len(rolloutReq.Candidates) < concurrency {
		concurrency = len(rolloutReq.Candidates)
	}
	g.SetLimit(concurrency)

	for i, cand := range rolloutReq.Candidates {
		idx := i
		c := cand
		g.Go(func() error {
			candState := composeCandidateState(rolloutReq.StateJSON, c.ActionJSON, c.ActionText)
			var candMedia []*llm.JudgeMediaPart
			if len(rolloutReq.StateMedia) > 0 || len(c.ActionMedia) > 0 {
				candMedia = make([]*llm.JudgeMediaPart, 0, len(rolloutReq.StateMedia)+len(c.ActionMedia))
				candMedia = append(candMedia, rolloutReq.StateMedia...)
				candMedia = append(candMedia, c.ActionMedia...)
			}
			if len(candMedia) > maxJudgeMediaParts {
				return status.Errorf(codes.InvalidArgument, "candidate[%d] combined media parts %d exceed ceiling %d", idx, len(candMedia), maxJudgeMediaParts)
			}
			var candBytes int
			for _, p := range candMedia {
				candBytes += len(p.Data)
			}
			if candBytes > maxJudgeMediaBytes {
				return status.Errorf(codes.InvalidArgument, "candidate[%d] combined media bytes %d exceed ceiling %d", idx, candBytes, maxJudgeMediaBytes)
			}
			jReq := &llm.JudgeRequest{
				StateJSON:  candState,
				StateMedia: candMedia,
				Questions:  rolloutReq.Criteria,
				ActionID:   rolloutReq.ActionID,
			}
			// Copy meta for each candidate goroutine to prevent concurrent map write races.
			candMeta := make(map[string]string, len(meta))
			for k, v := range meta {
				candMeta[k] = v
			}
			res, err := judge.Judge(gCtx, jReq, userID, candMeta)
			if err != nil {
				return err
			}
			mu.Lock()
			totalUsage.Input += res.Usage.Input
			totalUsage.Output += res.Usage.Output
			mu.Unlock()

			score := calculateCompositeScore(res.Answers, rolloutReq.Criteria)
			evals[idx] = &llm.CandidateEvaluation{
				ID:             c.ID,
				Answers:        res.Answers,
				CompositeScore: score,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return &llm.EvaluateRolloutsResult{Usage: totalUsage}, err
	}

	rankEvaluations(evals)
	bestID := ""
	if len(evals) > 0 {
		bestID = evals[0].ID
	}
	return &llm.EvaluateRolloutsResult{
		Evaluations:     evals,
		BestCandidateID: bestID,
		Usage:           totalUsage,
	}, nil
}

type rankedCandidate struct {
	eval    *llm.CandidateEvaluation
	origIdx int
}

func rankEvaluations(evals []*llm.CandidateEvaluation) {
	ranked := make([]rankedCandidate, len(evals))
	for i, e := range evals {
		ranked[i] = rankedCandidate{eval: e, origIdx: i}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].eval.CompositeScore != ranked[j].eval.CompositeScore {
			return ranked[i].eval.CompositeScore > ranked[j].eval.CompositeScore
		}
		return ranked[i].origIdx < ranked[j].origIdx
	})
	for r, rc := range ranked {
		rc.eval.Rank = r + 1
		evals[r] = rc.eval
	}
}

func calculateCompositeScore(answers map[string]*llm.JudgeAnswer, criteria map[string]*llm.JudgeQuestion) float64 {
	if len(criteria) == 0 {
		return 0.0
	}
	var total float64
	for id, q := range criteria {
		ans, ok := answers[id]
		if !ok || ans == nil {
			continue
		}
		var s float64
		switch q.Type {
		case llm.JudgeNoul:
			s = ans.Noul
		case llm.JudgeScore:
			numLevels := len(q.Levels)
			if numLevels <= 1 {
				numLevels = len(ans.Legend)
			}
			if numLevels > 1 {
				s = ans.Score / float64(numLevels-1)
			} else {
				s = ans.Score
			}
		case llm.JudgeChoice:
			s = 0.5
			if ans.Confidence > 0 {
				lower := strings.ToLower(ans.Choice)
				if lower == "yes" || lower == "true" || lower == "good" {
					s = ans.Confidence
				} else if lower == "no" || lower == "false" || lower == "bad" {
					s = 1.0 - ans.Confidence
				} else {
					s = ans.Confidence
				}
			}
		}
		if s < 0.0 {
			s = 0.0
		}
		if s > 1.0 {
			s = 1.0
		}
		total += s
	}
	return total / float64(len(criteria))
}

func composeCandidateState(stateJSON, actionJSON, actionText string) string {
	stateObj := parseOrWrapJSON(stateJSON)
	actionObj := parseOrWrapJSON(actionJSON)

	combined := make(map[string]any)
	if stateObj != nil {
		combined["state"] = stateObj
	}
	if actionObj != nil {
		combined["action"] = actionObj
	}
	if actionText != "" {
		combined["action_text"] = actionText
	}
	if len(combined) == 0 {
		return "{}"
	}
	b, err := json.Marshal(combined)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func parseOrWrapJSON(raw string) any {
	if raw == "" {
		return nil
	}
	var val any
	if err := json.Unmarshal([]byte(raw), &val); err == nil {
		return val
	}
	return raw
}

func evaluateRolloutsRequestFromProto(req *pb.EvaluateRolloutsRequest) (*llm.EvaluateRolloutsRequest, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "bad_request: request is nil")
	}
	cands := req.GetCandidates()
	if len(cands) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: no candidates")
	}
	if len(cands) > maxRolloutCandidates {
		return nil, status.Errorf(codes.InvalidArgument, "bad_request: candidates count %d exceeds max %d", len(cands), maxRolloutCandidates)
	}

	state := req.GetStateJson()
	if state != "" && !json.Valid([]byte(state)) {
		return nil, status.Error(codes.InvalidArgument, "bad_request: state_json is not valid JSON")
	}
	stateMedia, err := judgeMediaFromProto(req.GetStateMedia())
	if err != nil {
		return nil, err
	}

	seenIDs := make(map[string]bool, len(cands))
	parsedCands := make([]*llm.RolloutCandidate, 0, len(cands))
	for i, c := range cands {
		id := c.GetId()
		if id == "" {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: candidate[%d] has empty id", i)
		}
		if seenIDs[id] {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: duplicate candidate id %q", id)
		}
		seenIDs[id] = true

		actJSON := c.GetActionJson()
		if actJSON != "" && !json.Valid([]byte(actJSON)) {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: candidate %q action_json is not valid JSON", id)
		}
		actMedia, err := judgeMediaFromProto(c.GetActionMedia())
		if err != nil {
			return nil, fmt.Errorf("candidate %q: %w", id, err)
		}
		if actJSON == "" && len(actMedia) == 0 && c.GetActionText() == "" && state == "" && len(stateMedia) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: candidate %q has no action or state", id)
		}
		parsedCands = append(parsedCands, &llm.RolloutCandidate{
			ID:          id,
			ActionJSON:  actJSON,
			ActionMedia: actMedia,
			ActionText:  c.GetActionText(),
		})
	}

	criteriaMap := req.GetCriteria()
	parsedCriteria := make(map[string]*llm.JudgeQuestion, len(criteriaMap))
	if len(criteriaMap) == 0 {
		parsedCriteria["reward"] = &llm.JudgeQuestion{
			Type:         llm.JudgeScore,
			Instructions: "Evaluate the expected reward and rollout quality of this candidate action.",
			Levels:       []string{"poor", "suboptimal", "acceptable", "good", "optimal"},
		}
	} else {
		for id, q := range criteriaMap {
			if q.GetInstructions() == "" {
				return nil, status.Errorf(codes.InvalidArgument, "bad_request: criteria %q has no instructions", id)
			}
			kind := judgeTypeFromProto(q.GetType())
			switch kind {
			case llm.JudgeChoice:
				if len(q.GetOptions()) < 2 {
					return nil, status.Errorf(codes.InvalidArgument, "bad_request: choice %q needs at least two options", id)
				}
			case llm.JudgeScore:
				if len(q.GetLevels()) < 2 {
					return nil, status.Errorf(codes.InvalidArgument, "bad_request: score %q needs at least two levels", id)
				}
			case llm.JudgeNoul:
			default:
				return nil, status.Errorf(codes.InvalidArgument, "bad_request: criteria %q has no type", id)
			}
			parsedCriteria[id] = &llm.JudgeQuestion{
				Type:          kind,
				Instructions:  q.GetInstructions(),
				Options:       q.GetOptions(),
				Levels:        q.GetLevels(),
				TrueCriteria:  q.GetTrueCriteria(),
				FalseCriteria: q.GetFalseCriteria(),
			}
		}
	}

	return &llm.EvaluateRolloutsRequest{
		StateJSON:  state,
		StateMedia: stateMedia,
		Candidates: parsedCands,
		Criteria:   parsedCriteria,
		ActionID:   req.GetActionId(),
	}, nil
}

func rolloutCapabilities(req *llm.EvaluateRolloutsRequest) []string {
	caps := []string{registry.CapabilityJudge}
	seen := make(map[string]bool, 3)
	checkMedia := func(parts []*llm.JudgeMediaPart) {
		for _, p := range parts {
			var c string
			switch p.Type {
			case llm.JudgeMediaImage:
				c = registry.CapabilityVision
			case llm.JudgeMediaAudio:
				c = registry.CapabilityAudioInput
			case llm.JudgeMediaEmbedding:
				c = registry.CapabilityEmbeddingInput
			}
			if c != "" && !seen[c] {
				seen[c] = true
				caps = append(caps, c)
			}
		}
	}
	checkMedia(req.StateMedia)
	for _, c := range req.Candidates {
		checkMedia(c.ActionMedia)
	}
	return caps
}

func mediaPartTokens(p *llm.JudgeMediaPart) int64 {
	switch p.Type {
	case llm.JudgeMediaText:
		return int64(utf8.RuneCountInString(p.Text)) / 4
	case llm.JudgeMediaImage:
		if p.Detail == string(llm.ImageDetailLow) {
			return 85
		}
		return 1000
	case llm.JudgeMediaAudio:
		return 200
	case llm.JudgeMediaEmbedding:
		return int64(len(p.Embedding))/4 + 1
	default:
		return 0
	}
}

func rolloutEstimate(req *llm.EvaluateRolloutsRequest) int64 {
	baseRunes := utf8.RuneCountInString(req.StateJSON)
	var baseMediaTokens int64
	for _, p := range req.StateMedia {
		baseMediaTokens += mediaPartTokens(p)
	}
	criteriaRunes := 0
	for _, q := range req.Criteria {
		criteriaRunes += utf8.RuneCountInString(q.Instructions)
		for _, opt := range q.Options {
			criteriaRunes += utf8.RuneCountInString(opt)
		}
		for _, lvl := range q.Levels {
			criteriaRunes += utf8.RuneCountInString(lvl)
		}
		criteriaRunes += utf8.RuneCountInString(q.TrueCriteria) + utf8.RuneCountInString(q.FalseCriteria)
	}
	var totalTokens int64
	for _, c := range req.Candidates {
		candRunes := baseRunes + utf8.RuneCountInString(c.ActionJSON) + utf8.RuneCountInString(c.ActionText) + criteriaRunes
		var candMediaTokens int64
		for _, p := range c.ActionMedia {
			candMediaTokens += mediaPartTokens(p)
		}
		totalTokens += int64(candRunes)/4 + baseMediaTokens + candMediaTokens + 1
	}
	return totalTokens + 1
}
