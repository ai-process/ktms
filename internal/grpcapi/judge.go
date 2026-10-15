package grpcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/guardrails"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
	"github.com/ai-process/llm-proxy/internal/safefetch"
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
		sanitizedState, _, err := s.guardrails.InspectText(judgeReq.StateJSON, vault)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
		}
		judgeReq.StateJSON = sanitizedState
		// Images, audio and vectors are opaque to text guardrails; only the
		// text parts can be redacted or blocked here.
		for _, p := range judgeReq.StateMedia {
			if p.Type != llm.JudgeMediaText {
				continue
			}
			sanitizedText, _, err := s.guardrails.InspectText(p.Text, vault)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
			p.Text = sanitizedText
		}
		for _, q := range judgeReq.Questions {
			sanitizedInst, _, err := s.guardrails.InspectText(q.Instructions, vault)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
			q.Instructions = sanitizedInst
			for i, opt := range q.Options {
				sanitizedOpt, _, err := s.guardrails.InspectText(opt, vault)
				if err != nil {
					return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
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
		Chain:    plan.chain,
		KeyName:  plan.identity.Name,
		UserID:   router.UserID(plan.attrs, plan.identity.Name),
		Estimate: judgeEstimate(judgeReq),
		Adapter:  plan.adapter,
		Call: func(m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			judge, ok := adapter.(llm.Judge)
			if !ok {
				// Models misconfigured with the judge capability should yield to
				// capable fallback models in the chain instead of failing immediately.
				return llm.TokensUsage{}, llm.ErrJudgmentUnsupported
			}
			res, err := judge.Judge(ctx, judgeReq, router.UserID(plan.attrs, plan.identity.Name), plan.meta)
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
