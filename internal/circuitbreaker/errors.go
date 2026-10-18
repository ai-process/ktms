package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/ai-process/ktms/internal/llm"
)

// ErrUpstreamTimeout marks an attempt aborted by the per-attempt upstream watchdog.
// It skips to fallback models and trips circuit breakers rather than masking as client drops.
var ErrUpstreamTimeout error = errors.New("upstream request timeout")

// UpstreamTimeoutError wraps a vendor failure caused by an upstream request watchdog timeout.
type UpstreamTimeoutError struct {
	Duration time.Duration
	Err      error
}

func (e *UpstreamTimeoutError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("upstream request timeout after %v: %v", e.Duration, e.Err)
	}
	return fmt.Sprintf("upstream request timeout after %v", e.Duration)
}

func (e *UpstreamTimeoutError) Unwrap() error { return e.Err }

func (e *UpstreamTimeoutError) Is(target error) bool {
	return target == ErrUpstreamTimeout
}

func (e *UpstreamTimeoutError) SkipToNextModel() bool { return true }

func (e *UpstreamTimeoutError) Timeout() bool { return true }

func (e *UpstreamTimeoutError) Temporary() bool { return true }

// NewUpstreamTimeoutError creates an error signaling that an upstream vendor call exceeded its attempt budget.
func NewUpstreamTimeoutError(d time.Duration, cause error) error {
	return &UpstreamTimeoutError{
		Duration: d,
		Err:      cause,
	}
}

// IsClientCancellation distinguishes client-side aborts and deadlines from vendor failures
// so caller disconnects or short deadlines do not erroneously trip upstream circuit breakers.
func IsClientCancellation(ctx context.Context, err error) bool {
	if errors.Is(err, ErrUpstreamTimeout) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return false
}

// IsTrippingError classifies whether an error indicates vendor failure (5xx, network drops,
// connection refusal) rather than caller errors (400, auth, client cancellations, quota 429).
func IsTrippingError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if IsClientCancellation(ctx, err) {
		return false
	}
	if llm.IsCapacityExhausted(err) {
		return false
	}
	if errors.Is(err, ErrUpstreamTimeout) {
		return true
	}

	if errors.Is(err, llm.ErrOutputTruncated) ||
		errors.Is(err, llm.ErrStreamingNotSupported) ||
		errors.Is(err, llm.ErrEmbeddingUnsupported) ||
		errors.Is(err, llm.ErrImageGenerationUnsupported) ||
		errors.Is(err, llm.ErrJudgmentUnsupported) ||
		errors.Is(err, llm.ErrSchemaViolation) {
		return false
	}
	if llm.IsContentFilterError(err) {
		return false
	}

	type smithyAPIError interface {
		ErrorCode() string
	}
	var smithyErr smithyAPIError
	if errors.As(err, &smithyErr) {
		switch smithyErr.ErrorCode() {
		case "ServiceUnavailableException", "InternalServerException", "ModelTimeoutException", "ModelErrorException":
			return true
		case "ThrottlingException", "ValidationException", "AccessDeniedException", "ResourceNotFoundException":
			return false
		}
	}

	if code, ok := llm.ExtractHTTPStatusCode(err); ok {
		return code >= 500 && code < 600
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		if syscallErr == syscall.ECONNREFUSED || syscallErr == syscall.ECONNRESET || syscallErr == syscall.ETIMEDOUT {
			return true
		}
	}

	msg := strings.ToLower(safeErrorString(err))
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "tls handshake timeout") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "broken pipe")
}

func safeErrorString(err error) (s string) {
	if err == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			s = ""
		}
	}()
	return err.Error()
}
