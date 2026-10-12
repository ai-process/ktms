package router

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-process/llm-proxy/internal/llm"
)

// Sentinels for the documented error contract (see llmproxy.proto).
var (
	ErrNotConfigured  = errors.New("not_configured")
	ErrNoCapableModel = errors.New("no_capable_model")
	ErrNoVendorKey    = errors.New("no_vendor_key")
)

// ChainExhausted reports a chain walked to the end without a response.
type ChainExhausted struct {
	Last         error  // last vendor error, nil when every model was skipped
	AllThrottled bool   // every model skipped by a throttle
	ThrottleKind string // "rpm" | "budget_key" | "budget_user"
}

func (e *ChainExhausted) Error() string {
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
	case errors.Is(err, ErrNoCapableModel), errors.Is(err, llm.ErrJudgmentUnsupported):
		return status.Error(codes.FailedPrecondition, "no_capable_model")
	case errors.Is(err, ErrNoVendorKey):
		return status.Error(codes.FailedPrecondition, "no_vendor_key")
	case errors.Is(err, llm.ErrOutputTruncated):
		return status.Error(codes.FailedPrecondition, "output_truncated")
	}
	var exhausted *ChainExhausted
	if errors.As(err, &exhausted) {
		if exhausted.AllThrottled {
			return status.Error(codes.ResourceExhausted, "throttled:"+exhausted.ThrottleKind)
		}
		if llm.IsRetryableError(exhausted.Last) {
			return status.Error(codes.Unavailable, "vendors_unavailable")
		}
		return status.Error(codes.Internal, "vendor_error")
	}
	return status.Error(codes.Internal, "vendor_error")
}
