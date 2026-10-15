package llm

import (
	"errors"
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

func TestIsContentFilterError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&openai.Error{Code: "content_filter"}, true},
		{&openai.Error{Code: "ResponsibleAIPolicyViolation"}, true},
		{&openai.Error{Message: "The response was filtered due to the prompt triggering Azure OpenAI's content management policy."}, true},
		{&openai.Error{Code: "invalid_request_error"}, false},
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

