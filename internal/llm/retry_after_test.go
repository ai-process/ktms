package llm

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
		ok      bool
	}{
		{"seconds", map[string]string{"Retry-After": "5"}, 5 * time.Second, true},
		{"fractional seconds", map[string]string{"Retry-After": "1.5"}, 1500 * time.Millisecond, true},
		{"milliseconds win", map[string]string{"Retry-After-Ms": "250", "Retry-After": "5"}, 250 * time.Millisecond, true},
		{"http date", map[string]string{"Retry-After": now.Add(30 * time.Second).Format(http.TimeFormat)}, 30 * time.Second, true},
		{"date in the past", map[string]string{"Retry-After": now.Add(-time.Minute).Format(http.TimeFormat)}, 0, false},
		{"zero", map[string]string{"Retry-After": "0"}, 0, false},
		{"garbage", map[string]string{"Retry-After": "soon"}, 0, false},
		{"absent", nil, 0, false},
		{"huge seconds are clamped", map[string]string{"Retry-After": "1e12"}, maxRetryAfter, true},
		{"infinite ms are clamped", map[string]string{"Retry-After-Ms": "Inf"}, maxRetryAfter, true},
		{"NaN is ignored", map[string]string{"Retry-After": "NaN"}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got, ok := ParseRetryAfter(h, now)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseRetryAfter = %v, %v; want %v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRetryAfterOnlyFor429And503(t *testing.T) {
	for _, tc := range []struct {
		status int
		ok     bool
	}{{429, true}, {503, true}, {500, false}, {400, false}} {
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"7"}}}
		err := fmt.Errorf("wrapped: %w", &openai.Error{StatusCode: tc.status, Response: resp})
		d, ok := RetryAfter(err)
		if ok != tc.ok || (ok && d != 7*time.Second) {
			t.Fatalf("status %d: RetryAfter = %v, %v; want ok=%v", tc.status, d, ok, tc.ok)
		}
	}
}

func TestRetryAfterFromGeminiRetryInfo(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Details: []map[string]any{
		{"@type": "type.googleapis.com/google.rpc.QuotaFailure"},
		{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "17s"},
	}})
	if d, ok := RetryAfter(err); !ok || d != 17*time.Second {
		t.Fatalf("RetryAfter = %v, %v; want 17s", d, ok)
	}
	if _, ok := RetryAfter(genai.APIError{Code: 500, Details: []map[string]any{
		{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "17s"},
	}}); ok {
		t.Fatal("RetryInfo on a 500 must be ignored")
	}
}

func TestTransportFailuresAreRetryable(t *testing.T) {
	if !IsRetryableError(fmt.Errorf("post: %w", io.ErrUnexpectedEOF)) {
		t.Fatal("unexpected EOF on a reused connection must be retryable")
	}
	if !IsRetryableError(&net.OpError{Op: "read", Err: syscall.ECONNRESET}) {
		t.Fatal("connection reset must be retryable")
	}
	if IsRetryableError(fmt.Errorf("post: %w", context.Canceled)) {
		t.Fatal("a cancelled caller must not be retried")
	}
	if IsRetryableError(&url.Error{Op: "Post", URL: "https://typo.invalid", Err: &net.DNSError{Err: "no such host", Name: "typo.invalid", IsNotFound: true}}) {
		t.Fatal("an unknown host is a misconfiguration, not worth a retry")
	}
	if IsRetryableError(&url.Error{Op: "Post", URL: "https://self-signed.example", Err: x509.UnknownAuthorityError{}}) {
		t.Fatal("a TLS verification failure is not worth a retry")
	}
	if !IsRetryableError(&url.Error{Op: "Post", URL: "https://api.example.com", Err: &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}}) {
		t.Fatal("a dial timeout must be retryable")
	}
}
