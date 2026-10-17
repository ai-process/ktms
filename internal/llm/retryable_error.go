package llm

import (
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

// ErrNoAudioContent marks a TTS reply that arrived intact but carried no audio —
// the preview models answer 200 with text instead. Retryable on purpose: it is
// transient per call, and treating it as terminal stops the chain from ever
// reaching another vendor.
var ErrNoAudioContent = errors.New("no audio in TTS response")

// HTTPStatusError is any vendor error carrying a raw HTTP status — the hand
// -rolled adapters implement it so retryability stays in one place.
type HTTPStatusError interface {
	error
	HTTPStatus() int
}

// ExtractHTTPStatusCode resolves HTTP status codes across supported provider SDKs.
func ExtractHTTPStatusCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var httpErr HTTPStatusError
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

// IsRetryableError returns true if the error is from an LLM API and has a retryable
// HTTP status code (429 Too Many Requests or 5xx server errors).
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoAudioContent) || errors.Is(err, ErrJudgmentUnsupported) || errors.Is(err, ErrEmbeddingUnsupported) {
		return true
	}
	if code, ok := ExtractHTTPStatusCode(err); ok {
		return code == 429 || (code >= 500 && code < 600)
	}
	type smithyAPIError interface {
		ErrorCode() string
	}
	var smithyErr smithyAPIError
	if errors.As(err, &smithyErr) {
		switch smithyErr.ErrorCode() {
		case "ThrottlingException", "ServiceUnavailableException", "InternalServerException", "ModelTimeoutException", "ModelErrorException":
			return true
		}
	}
	return false
}

// SkipToNextModel is implemented by errors that should bypass model-level retries
// and immediately advance to the next model in the fallback chain (e.g. pool capacity exhausted).
type SkipToNextModel interface {
	error
	SkipToNextModel() bool
}

// IsContentFilterError reports whether err indicates an Azure or vendor content filter block.
func IsContentFilterError(err error) bool {
	if err == nil {
		return false
	}
	var openaiErr *openai.Error
	if errors.As(err, &openaiErr) && openaiErr != nil {
		code := strings.ToLower(openaiErr.Code)
		if code == "content_filter" || code == "responsibleaipolicyviolation" {
			return true
		}
		msg := strings.ToLower(openaiErr.Message)
		return strings.Contains(msg, "content filter") || strings.Contains(msg, "content management policy")
	}
	type smithyAPIError interface {
		ErrorCode() string
	}
	var smithyErr smithyAPIError
	if errors.As(err, &smithyErr) {
		code := strings.ToLower(smithyErr.ErrorCode())
		if strings.Contains(code, "guardrail") || strings.Contains(code, "contentfilter") {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "content filter") || strings.Contains(msg, "guardrail intervened") {
		return true
	}
	return false
}

// ShouldSkipToNextModel reports whether err or any wrapped error indicates that
// the chain should advance to the next candidate model immediately.
func ShouldSkipToNextModel(err error) bool {
	if err == nil {
		return false
	}
	var s SkipToNextModel
	if errors.As(err, &s) {
		return s.SkipToNextModel()
	}
	return IsContentFilterError(err)
}
