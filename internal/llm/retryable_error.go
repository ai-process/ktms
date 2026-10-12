package llm

import (
	"errors"

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

// IsRetryableError returns true if the error is from an LLM API and has a retryable
// HTTP status code (429 Too Many Requests or 5xx server errors).
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoAudioContent) {
		return true
	}
	var httpErr HTTPStatusError
	if errors.As(err, &httpErr) {
		code := httpErr.HTTPStatus()
		return code == 429 || (code >= 500 && code < 600)
	}
	// OpenAI SDK: Error (alias for apierror.Error) with StatusCode
	var openaiErr *openai.Error
	if errors.As(err, &openaiErr) && openaiErr != nil {
		code := openaiErr.StatusCode
		return code == 429 || (code >= 500 && code < 600)
	}
	// Anthropic SDK: Error (alias for apierror.Error) with StatusCode
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) && anthropicErr != nil {
		code := anthropicErr.StatusCode
		return code == 429 || (code >= 500 && code < 600)
	}
	// Google genai: APIError (value type) with Code
	var genaiErr genai.APIError
	if errors.As(err, &genaiErr) {
		code := genaiErr.Code
		return code == 429 || (code >= 500 && code < 600)
	}
	return false
}
