package llm

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
	"google.golang.org/genai"
)

// maxRetryAfter bounds parsed hints before they become a Duration, so absurd
// headers cannot overflow; callers apply their own, much lower, cap.
const maxRetryAfter = 24 * time.Hour

// RetryAfter returns how long the vendor asked callers to wait, read from a 429
// or 503 reply: the Retry-After headers of OpenAI-compatible and Anthropic APIs,
// or the google.rpc.RetryInfo detail of Gemini. Other statuses carry no promise.
func RetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var genaiErr genai.APIError
	if errors.As(err, &genaiErr) {
		if genaiErr.Code != http.StatusTooManyRequests && genaiErr.Code != http.StatusServiceUnavailable {
			return 0, false
		}
		return retryInfoDelay(genaiErr.Details)
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
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			if d, ok := fromSeconds(ms / 1000); ok {
				return d, true
			}
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return fromSeconds(secs)
	}
	if at, err := http.ParseTime(v); err == nil {
		return fromSeconds(at.Sub(now).Seconds())
	}
	return 0, false
}

// retryInfoDelay reads google.rpc.RetryInfo.retryDelay ("17s") from the
// error details Gemini attaches to RESOURCE_EXHAUSTED and UNAVAILABLE.
func retryInfoDelay(details []map[string]any) (time.Duration, bool) {
	for _, d := range details {
		if typ, _ := d["@type"].(string); !strings.HasSuffix(typ, "google.rpc.RetryInfo") {
			continue
		}
		raw, _ := d["retryDelay"].(string)
		delay, err := time.ParseDuration(raw)
		if err != nil {
			return 0, false
		}
		return fromSeconds(delay.Seconds())
	}
	return 0, false
}

func fromSeconds(secs float64) (time.Duration, bool) {
	if math.IsNaN(secs) || secs <= 0 {
		return 0, false
	}
	if secs >= maxRetryAfter.Seconds() {
		return maxRetryAfter, true
	}
	return time.Duration(secs * float64(time.Second)), true
}
