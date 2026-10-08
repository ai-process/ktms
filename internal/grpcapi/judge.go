package grpcapi

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
)

const modalityJudge = "judge"

// Judge answers typed questions about one state. It is a decision RPC: no text
// is generated, and the answers carry the probabilities the caller routes on.
func (s *ProxyServer) Judge(ctx context.Context, req *pb.JudgeRequest) (*pb.JudgeResponse, error) {
	judgeReq, err := judgeRequestFromProto(req)
	if err != nil {
		return nil, err
	}
	plan, err := s.planMedia(ctx, req.GetAttributes(), req.GetModelOverride(), modalityJudge, registry.CapabilityJudge)
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
				// The capability filter should have excluded this model; if the
				// registry and the adapter disagree, say so rather than panic.
				return llm.TokensUsage{}, status.Errorf(codes.FailedPrecondition,
					"no_capable_model: %s cannot judge", m.ID)
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
		resp.Answers[id] = &pb.JudgeAnswer{
			Type:          judgeTypeToProto(ans.Type),
			Choice:        ans.Choice,
			Noul:          ans.Noul,
			Score:         ans.Score,
			Probabilities: ans.Probabilities,
			Confidence:    ans.Confidence,
			Legend:        ans.Legend,
		}
	}
	return resp, nil
}

// judgeRequestFromProto validates the wire request and converts it. Validation
// is strict on purpose: a malformed question costs a vendor round trip and
// comes back as an answer nobody can interpret.
func judgeRequestFromProto(req *pb.JudgeRequest) (*llm.JudgeRequest, error) {
	state := req.GetStateJson()
	if state == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: state_json is empty")
	}
	if !json.Valid([]byte(state)) {
		return nil, status.Error(codes.InvalidArgument, "bad_request: state_json is not valid JSON")
	}
	if len(req.GetQuestions()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: no questions")
	}

	out := &llm.JudgeRequest{
		StateJSON: state,
		Questions: make(map[string]*llm.JudgeQuestion, len(req.GetQuestions())),
		ActionID:  req.GetActionId(),
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

// judgeEstimate is the budget reservation guess: the state plus every question
// is what the vendor bills, and it bills input only.
func judgeEstimate(req *llm.JudgeRequest) int64 {
	runes := utf8.RuneCountInString(req.StateJSON)
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
	return int64(runes)/4 + 1
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
