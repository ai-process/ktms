package llm

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
)

// RetryAfterHinter is implemented by hand-rolled adapter errors that carry the
// vendor's back-off hint without exposing an *http.Response.
type RetryAfterHinter interface {
	RetryAfterHint() (time.Duration, bool)
}

// RetryAfter returns how long the vendor asked callers to wait, read from a 429
// or 503 reply. Other statuses carry no back-off promise worth honouring.
func RetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var hinter RetryAfterHinter
	if errors.As(err, &hinter) {
		return hinter.RetryAfterHint()
	}
	var resp *http.Response
	var openaiErr *openai.Error
	var anthropicErr *anthropic.Error
	switch {
	case errors.As(err, &openaiErr) && openaiErr != nil:
		resp = openaiErr.Response
	case errors.As(err, &anthropicErr) && anthropicErr != nil:
		resp = anthropicErr.Response
	}
	if resp == nil || (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) {
		return 0, false
	}
	return ParseRetryAfter(resp.Header, time.Now())
}

// ParseRetryAfter reads retry-after-ms (OpenAI, Azure) before the standard
// Retry-After header, which is either delay-seconds or an HTTP date.
func ParseRetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if at, err := http.ParseTime(v); err == nil {
		if d := at.Sub(now); d > 0 {
			return d, true
		}
	}
	return 0, false
}
