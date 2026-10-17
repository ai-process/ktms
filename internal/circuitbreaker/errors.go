package circuitbreaker

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"

	"github.com/ai-process/llm-proxy/internal/llm"
)

// IsClientCancellation distinguishes client-side aborts from vendor failures
// so caller disconnects do not erroneously trip upstream circuit breakers.
func IsClientCancellation(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	return false
}

// ExtractHTTPStatusCode resolves HTTP status codes across supported provider SDKs
// and custom adapter error envelopes.
func ExtractHTTPStatusCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var httpErr llm.HTTPStatusError
	if errors.As(err, &httpErr) {
		return httpErr.HTTPStatus(), true
	}
	var openaiErr *openai.Error
	if errors.As(err, &openaiErr) && openaiErr != nil {
		return openaiErr.StatusCode, true
	}
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) && anthropicErr != nil {
		return anthropicErr.StatusCode, true
	}
	var genaiErr genai.APIError
	if errors.As(err, &genaiErr) {
		return genaiErr.Code, true
	}
	type httpStatusCodeError interface {
		HTTPStatusCode() int
	}
	var httpStatusErr httpStatusCodeError
	if errors.As(err, &httpStatusErr) {
		return httpStatusErr.HTTPStatusCode(), true
	}
	return 0, false
}

// IsTrippingError classifies whether an error indicates vendor failure (5xx, timeouts,
// rate exhaustion) rather than caller errors (400, auth, client cancellations).
func IsTrippingError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if IsClientCancellation(ctx, err) {
		return false
	}

	if code, ok := ExtractHTTPStatusCode(err); ok {
		return code == 429 || (code >= 500 && code < 600)
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
		case "ThrottlingException", "ServiceUnavailableException", "InternalServerException", "ModelTimeoutException", "ModelErrorException":
			return true
		case "ValidationException", "AccessDeniedException", "ResourceNotFoundException":
			return false
		}
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

	errMsg := safeErrorString(err)
	if strings.Contains(errMsg, "could not honour the response schema") {
		return false
	}

	msg := strings.ToLower(errMsg)
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "no such host") ||
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

