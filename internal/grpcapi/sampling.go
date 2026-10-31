package grpcapi

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

const maxStopSequences = 16

// validateSampling rejects values outside every vendor's range, so a bad
// value fails the call rather than being dropped by whichever model serves it.
func validateSampling(req *pb.GenerateTextRequest) error {
	if req.Temperature != nil && !(req.GetTemperature() >= 0 && req.GetTemperature() <= 2) {
		return status.Error(codes.InvalidArgument, "bad_request: temperature must be between 0 and 2")
	}
	if req.TopP != nil && !(req.GetTopP() >= 0 && req.GetTopP() <= 1) {
		return status.Error(codes.InvalidArgument, "bad_request: top_p must be between 0 and 1")
	}
	if len(req.GetStop()) > maxStopSequences {
		return status.Errorf(codes.InvalidArgument, "bad_request: at most %d stop sequences", maxStopSequences)
	}
	for _, s := range req.GetStop() {
		if s == "" {
			return status.Error(codes.InvalidArgument, "bad_request: stop sequences must not be empty")
		}
	}
	return nil
}

func samplingOf(req *pb.GenerateTextRequest) llm.Sampling {
	return llm.Sampling{Temperature: req.Temperature, TopP: req.TopP, Stop: req.GetStop(), Seed: req.Seed}
}

// ignoredParameters names the caller's sampling the model did not get, by
// the same table its adapter filters with.
func ignoredParameters(m *proxydb.Model, s llm.Sampling) []string {
	if s.Temperature == nil && s.TopP == nil && len(s.Stop) == 0 && s.Seed == nil {
		return nil
	}
	_, ignored := registry.SamplingSupport(m).Apply(s)
	return ignored
}
