package router

import (
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/ai-process/llm-proxy/internal/audio"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/spendlimit"
)

// InsufficientQuota is the OpenAI-compatible code for an exhausted spend limit.
const InsufficientQuota = "insufficient_quota"

// Sentinels for the documented error contract (see llmproxy.proto).
var (
	ErrNotConfigured  = errors.New("not_configured")
	ErrNoCapableModel = errors.New("no_capable_model")
	ErrNoVendorKey    = errors.New("no_vendor_key")
)

// ChainExhausted reports a chain walked to the end without a response.
type ChainExhausted struct {
	Last             error  // last vendor error, nil when every model was skipped
	AllThrottled     bool   // every model skipped by a throttle
	ThrottleKind     string // "rpm" | "budget_key" | "budget_user" | spendlimit reasons
	AllCircuitBroken bool   // every model skipped because circuit breaker is open
	AnyCircuitBroken bool   // observed at least one open circuit in chain; retained for telemetry and tests
}

func (e *ChainExhausted) Error() string {
	if e.AllCircuitBroken {
		return "chain exhausted: circuit_broken: all upstream models temporarily unavailable"
	}
	if e.AllThrottled {
		return "chain exhausted: throttled:" + e.ThrottleKind
	}
	return fmt.Sprintf("chain exhausted: %v", e.Last)
}

func (e *ChainExhausted) Unwrap() error { return e.Last }

// ToStatus maps router errors onto the gRPC contract. Clients retry only on
// Unavailable; everything else is theirs to handle.
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrNotConfigured):
		return status.Error(codes.FailedPrecondition, "not_configured")
	case errors.Is(err, ErrNoCapableModel), errors.Is(err, llm.ErrJudgmentUnsupported), errors.Is(err, llm.ErrEmbeddingUnsupported):
		return status.Error(codes.FailedPrecondition, "no_capable_model")
	case errors.Is(err, ErrNoVendorKey):
		return status.Error(codes.FailedPrecondition, "no_vendor_key")
	case errors.Is(err, llm.ErrOutputTruncated):
		return status.Error(codes.FailedPrecondition, "output_truncated")
	case errors.Is(err, audio.ErrFFmpegMissing):
		return status.Error(codes.Unimplemented, "ffmpeg is required for Gemini speech synthesis: please use the 'coyl/llm-proxy:audio' container image")
	}
	var exhausted *ChainExhausted
	if errors.As(err, &exhausted) {
		if exhausted.AllCircuitBroken {
			return status.Error(codes.Unavailable, "vendors_unavailable")
		}
		if exhausted.AllThrottled {
			return throttledStatus(exhausted.ThrottleKind, time.Now())
		}
		if llm.IsRetryableError(exhausted.Last) || llm.ShouldSkipToNextModel(exhausted.Last) {
			return status.Error(codes.Unavailable, "vendors_unavailable")
		}
		return status.Error(codes.Internal, "vendor_error")
	}
	return status.Error(codes.Internal, "vendor_error")
}

// throttledStatus carries a RetryInfo until the period resets for spend
// refusals, so gRPC clients and the HTTP layer can tell when to come back.
func throttledStatus(kind string, now time.Time) error {
	if kind == spendlimit.ReasonUnavailable {
		return status.Error(codes.Unavailable, "throttled:"+kind)
	}
	st := status.New(codes.ResourceExhausted, "throttled:"+kind)
	retry, ok := spendlimit.RetryAfter(kind, now)
	if !ok {
		return st.Err()
	}
	withInfo, err := st.WithDetails(
		&errdetails.ErrorInfo{Reason: InsufficientQuota, Domain: "llmproxy"},
		&errdetails.RetryInfo{RetryDelay: durationpb.New(retry)},
	)
	if err != nil {
		log.Error().Err(err).Str("kind", kind).Msg("attach retry info to spend refusal")
		return st.Err()
	}
	return withInfo.Err()
}
