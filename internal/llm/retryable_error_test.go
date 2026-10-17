package llm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

func TestIsRetryableError_Anthropic_429(t *testing.T) {
	err := &anthropic.Error{StatusCode: 429}
	if !IsRetryableError(err) {
		t.Error("Anthropic 429 should be retryable")
	}
}

func TestIsRetryableError_Anthropic_500(t *testing.T) {
	err := &anthropic.Error{StatusCode: 500}
	if !IsRetryableError(err) {
		t.Error("Anthropic 500 should be retryable")
	}
}

func TestIsRetryableError_Anthropic_503(t *testing.T) {
	err := &anthropic.Error{StatusCode: 503}
	if !IsRetryableError(err) {
		t.Error("Anthropic 503 should be retryable")
	}
}

func TestIsRetryableError_Anthropic_400_NotRetryable(t *testing.T) {
	err := &anthropic.Error{StatusCode: 400}
	if IsRetryableError(err) {
		t.Error("Anthropic 400 should not be retryable")
	}
}

func TestIsRetryableError_OpenAI_429(t *testing.T) {
	err := &openai.Error{StatusCode: 429}
	if !IsRetryableError(err) {
		t.Error("OpenAI 429 should be retryable")
	}
}

func TestIsRetryableError_OpenAI_500(t *testing.T) {
	err := &openai.Error{StatusCode: 500}
	if !IsRetryableError(err) {
		t.Error("OpenAI 500 should be retryable")
	}
}

func TestIsRetryableError_OpenAI_503(t *testing.T) {
	err := &openai.Error{StatusCode: 503}
	if !IsRetryableError(err) {
		t.Error("OpenAI 503 should be retryable")
	}
}

func TestIsRetryableError_OpenAI_400_NotRetryable(t *testing.T) {
	err := &openai.Error{StatusCode: 400}
	if IsRetryableError(err) {
		t.Error("OpenAI 400 should not be retryable")
	}
}

func TestIsRetryableError_Google_429(t *testing.T) {
	err := genai.APIError{Code: 429}
	if !IsRetryableError(err) {
		t.Error("Google 429 should be retryable")
	}
}

func TestIsRetryableError_Google_500(t *testing.T) {
	err := genai.APIError{Code: 500}
	if !IsRetryableError(err) {
		t.Error("Google 500 should be retryable")
	}
}

func TestIsRetryableError_Google_400_NotRetryable(t *testing.T) {
	err := genai.APIError{Code: 400}
	if IsRetryableError(err) {
		t.Error("Google 400 should not be retryable")
	}
}

func TestIsRetryableError_Nil(t *testing.T) {
	if IsRetryableError(nil) {
		t.Error("nil should not be retryable")
	}
}

func TestIsRetryableError_GenericError_NotRetryable(t *testing.T) {
	err := errors.New("generic error")
	if IsRetryableError(err) {
		t.Error("generic error should not be retryable")
	}
}

type mockSmithyAPIError struct {
	code string
}

func (m mockSmithyAPIError) Error() string     { return m.code }
func (m mockSmithyAPIError) ErrorCode() string { return m.code }

type mockHTTPStatusError struct {
	code int
}

func (m mockHTTPStatusError) Error() string       { return fmt.Sprintf("status %d", m.code) }
func (m mockHTTPStatusError) HTTPStatusCode() int { return m.code }

func TestIsRetryableError_Bedrock(t *testing.T) {
	if !IsRetryableError(mockSmithyAPIError{code: "ThrottlingException"}) {
		t.Error("ThrottlingException should be retryable")
	}
	if !IsRetryableError(mockSmithyAPIError{code: "ServiceUnavailableException"}) {
		t.Error("ServiceUnavailableException should be retryable")
	}
	if !IsRetryableError(mockHTTPStatusError{code: 429}) {
		t.Error("HTTP 429 should be retryable")
	}
	if !IsRetryableError(mockHTTPStatusError{code: 503}) {
		t.Error("HTTP 503 should be retryable")
	}
	if IsRetryableError(mockSmithyAPIError{code: "ValidationException"}) {
		t.Error("ValidationException should not be retryable")
	}
	if IsRetryableError(mockHTTPStatusError{code: 400}) {
		t.Error("HTTP 400 should not be retryable")
	}
}

func TestIsContentFilterError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&openai.Error{Code: "content_filter"}, true},
		{&openai.Error{Code: "ResponsibleAIPolicyViolation"}, true},
		{&openai.Error{Message: "The response was filtered due to the prompt triggering Azure OpenAI's content management policy."}, true},
		{&openai.Error{Code: "invalid_request_error"}, false},
		{mockSmithyAPIError{code: "GuardrailIntervened"}, true},
		{errors.New("bedrock content filter: content_filtered"), true},
		{errors.New("guardrail intervened during generation"), true},
		{errors.New("some error"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsContentFilterError(c.err); got != c.want {
			t.Errorf("IsContentFilterError(%v) = %v, want %v", c.err, got, c.want)
		}
		if got := ShouldSkipToNextModel(c.err); got != c.want {
			t.Errorf("ShouldSkipToNextModel(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestExtractHTTPStatusCode(t *testing.T) {
	cases := []struct {
		err      error
		wantCode int
		wantOk   bool
	}{
		{nil, 0, false},
		{errors.New("plain error"), 0, false},
		{mockHTTPStatusError{code: 429}, 429, true},
		{mockHTTPStatusError{code: 503}, 503, true},
		{&openai.Error{StatusCode: 500}, 500, true},
		{&anthropic.Error{StatusCode: 529}, 529, true},
	}
	for _, c := range cases {
		code, ok := ExtractHTTPStatusCode(c.err)
		if ok != c.wantOk || code != c.wantCode {
			t.Errorf("ExtractHTTPStatusCode(%v) = (%d, %v), want (%d, %v)", c.err, code, ok, c.wantCode, c.wantOk)
		}
	}
}
