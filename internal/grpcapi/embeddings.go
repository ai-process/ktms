package grpcapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/guardrails"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
)

const (
	modalityEmbedding  = "embedding"
	maxEmbeddingInputs = 2048
)

// GetEmbeddings computes vector embeddings for inputs.
// Invariant: Vector space isolation is strictly preserved. Cross-model fallbacks are disallowed.
func (s *ProxyServer) GetEmbeddings(ctx context.Context, req *pb.GetEmbeddingsRequest) (*pb.GetEmbeddingsResponse, error) {
	if req.GetModel() == "" || req.GetModel() == "auto" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: model is required and auto routing is not supported for embeddings")
	}
	if len(req.GetInputs()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: empty_input")
	}
	if len(req.GetInputs()) > maxEmbeddingInputs {
		return nil, status.Error(codes.InvalidArgument, "bad_request: too_many_inputs")
	}

	sanitizedInputs := make([]string, len(req.GetInputs()))
	var totalRunes int
	for i, inp := range req.GetInputs() {
		if inp == "" {
			return nil, status.Error(codes.InvalidArgument, "bad_request: empty_input")
		}
		text := inp
		if s.guardrails != nil && s.guardrails.Enabled {
			var vault *guardrails.TokenVault
			if s.guardrails.PIIAction == guardrails.ActionRedact {
				vault = guardrails.NewTokenVault()
			}
			inspected, _, err := s.guardrails.InspectPII(text, vault)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "%s", err.Error())
			}
			text = inspected
		}
		sanitizedInputs[i] = text
		totalRunes += len([]rune(text))
	}

	plan, err := s.planMedia(ctx, req.GetAttributes(), req.GetModel(), modalityEmbedding, registry.CapabilityEmbedding)
	if err != nil {
		return nil, err
	}

	embReq := &llm.EmbeddingRequest{
		Inputs:     sanitizedInputs,
		Model:      req.GetModel(),
		Dimensions: int(req.GetDimensions()),
		InputType:  req.GetInputType(),
		ActionID:   req.GetActionId(),
	}

	var out *llm.EmbeddingResult
	model, err := router.ExecuteMedia(ctx, s.throttle, router.MediaRequest{
		Chain:           plan.chain,
		KeyName:         plan.identity.Name,
		UserID:          router.UserID(plan.attrs, plan.identity.Name),
		Estimate:        int64(totalRunes/4 + 1),
		CircuitBreaker:  s.circuitBreaker,
		UpstreamTimeout: s.upstreamTimeout,
		Adapter:         plan.adapter,
		Call: func(callCtx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			embAdapter, ok := adapter.(llm.EmbeddingAdapter)
			if !ok {
				return llm.TokensUsage{}, llm.ErrEmbeddingUnsupported
			}
			res, err := embAdapter.CreateEmbeddings(callCtx, embReq, router.UserID(plan.attrs, plan.identity.Name), plan.meta)
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

	pbData := make([]*pb.Embedding, len(out.Data))
	for i, d := range out.Data {
		pbData[i] = &pb.Embedding{
			Index:  int32(d.Index),
			Values: d.Values,
		}
	}

	return &pb.GetEmbeddingsResponse{
		Data: pbData,
		Usage: &pb.TokensUsage{
			Input:  int64(out.Usage.Input),
			Output: int64(out.Usage.Output),
		},
		ResolvedModel:  model.ID,
		ResolvedVendor: model.Vendor,
		MatchedRule:    plan.ruleName,
	}, nil
}
