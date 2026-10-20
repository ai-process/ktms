package router

import (
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/proxydb"
)

// Cooldown holds back models whose vendor answered with Retry-After. It is
// bound to the calling client, so one client's quota does not stall others.
type Cooldown interface {
	Remaining(m *proxydb.Model) time.Duration
	Pause(m *proxydb.Model, d time.Duration) time.Duration
}

// CoolingDown is a model skipped, or just answered, inside its vendor's
// Retry-After window. It advances the chain rather than ending it.
type CoolingDown struct {
	Model     string
	Remaining time.Duration
	Err       error // the vendor reply that started the pause; nil on a skip
}

func (e *CoolingDown) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("model %s paused for %s by vendor Retry-After: %v", e.Model, e.Remaining.Round(time.Millisecond), e.Err)
	}
	return fmt.Sprintf("model %s paused for %s by vendor Retry-After", e.Model, e.Remaining.Round(time.Millisecond))
}

func (e *CoolingDown) Unwrap() error { return e.Err }

func (e *CoolingDown) SkipToNextModel() bool { return true }

// coolingDown reports a model still inside its pause, before any breaker probe
// or budget is spent on it.
func coolingDown(cd Cooldown, m *proxydb.Model) (*CoolingDown, bool) {
	if cd == nil {
		return nil, false
	}
	if d := cd.Remaining(m); d > 0 {
		return &CoolingDown{Model: m.ID, Remaining: d}, true
	}
	return nil, false
}

// pauseOnRetryAfter starts a pause when the vendor named one and returns err
// wrapped so the chain moves on instead of retrying the same model.
func pauseOnRetryAfter(cd Cooldown, m *proxydb.Model, err error) error {
	if cd == nil || err == nil {
		return err
	}
	wait, ok := llm.RetryAfter(err)
	if !ok {
		return err
	}
	d := cd.Pause(m, wait)
	if d <= 0 {
		return err
	}
	observability.VendorCooldownsTotal.WithLabelValues(m.Vendor, m.ID).Inc()
	log.Warn().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).Dur("retry_after", wait).Dur("paused", d).
		Msg("vendor asked to back off, pausing model")
	return &CoolingDown{Model: m.ID, Remaining: d, Err: err}
}

// shortestPause keeps the soonest a paused model in the chain frees up, which
// is what the caller is told to wait when the whole chain fails.
func shortestPause(cur time.Duration, err error) time.Duration {
	var cd *CoolingDown
	if !errors.As(err, &cd) || cd.Remaining <= 0 {
		return cur
	}
	if cur == 0 || cd.Remaining < cur {
		return cd.Remaining
	}
	return cur
}

func nextModelID(chain []*proxydb.Model, i int) string {
	if i+1 < len(chain) {
		return chain[i+1].ID
	}
	return ""
}

// skipReason labels a skip-to-next error for the fallbacks metric.
func skipReason(err error) string {
	var cd *CoolingDown
	switch {
	case errors.As(err, &cd):
		return "cooldown"
	case errors.Is(err, circuitbreaker.ErrUpstreamTimeout):
		return "upstream_timeout"
	case llm.IsContentFilterError(err):
		return "content_filter"
	default:
		return "capacity_exhausted"
	}
}
