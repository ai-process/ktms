package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

const (
	perModelAttempts = 2
	attemptBackoff   = 500 * time.Millisecond
	// schemaAttempts: one retry when a soft-schema model answers in the wrong
	// shape. Models that cannot hold a shape rarely find it on a third go.
	schemaAttempts = 2
)

// Throttle is what Execute needs from the limiter. The Redis implementation
// lives in internal/throttle; NoopThrottle allows everything.
type Throttle interface {
	// AllowRPM takes an RPM slot, or reports the model saturated this minute.
	AllowRPM(ctx context.Context, keyName string, m *proxydb.Model) bool
	// Reserve books the token estimate against the applicable daily budgets.
	// tripped names the budget that refused ("budget_key" | "budget_user");
	// empty tripped means reserved (res may still be nil when nothing applies).
	Reserve(ctx context.Context, m *proxydb.Model, keyName, userID string, estimate int64) (res *Reservation, tripped string)
	// Settle adjusts reserved budgets to actual usage; usage zero = release.
	Settle(res *Reservation, usage llm.TokensUsage)
	// CheckDailyBudget reports if daily budget is already exhausted ("budget_key" | "budget_user"), or "" if OK.
	CheckDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string) string
	// ChargeDailyBudget increments the daily budgets with actual batch tokens.
	ChargeDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string, tokens int64)
}

// Reservation is the throttle's bookkeeping handle for one admitted call.
type Reservation struct {
	Keys     []string
	Estimate int64
}

// NoopThrottle admits everything; used when Redis is not configured.
type NoopThrottle struct{}

func (NoopThrottle) AllowRPM(context.Context, string, *proxydb.Model) bool { return true }
func (NoopThrottle) Reserve(context.Context, *proxydb.Model, string, string, int64) (*Reservation, string) {
	return nil, ""
}
func (NoopThrottle) Settle(*Reservation, llm.TokensUsage) {}
func (NoopThrottle) CheckDailyBudget(context.Context, *proxydb.Model, string, string) string { return "" }
func (NoopThrottle) ChargeDailyBudget(context.Context, *proxydb.Model, string, string, int64) {}

// UserID resolves user identity from attributes with an svc:<keyName> fallback.
func UserID(attributes map[string]string, keyName string) string {
	if u := attributes["user_id"]; u != "" {
		return u
	}
	return "svc:" + keyName
}

// SchemaFailure reports a model that answered but could not produce the
// requested shape, even given a second try. It advances the chain rather than
// ending it: another model may well comply.
type SchemaFailure struct {
	Model string
	Err   error
}

func (e *SchemaFailure) Error() string {
	return fmt.Sprintf("%s could not honour the response schema: %v", e.Model, e.Err)
}

func (e *SchemaFailure) Unwrap() error { return e.Err }

// Request is one admission-controlled generation attempt over a model chain.
type Request struct {
	Chain    []*proxydb.Model // pre-filtered candidates, in fallback order
	KeyName  string           // api_key name: throttle keys, logs
	UserID   string           // attributes["user_id"] or svc:<keyName> fallback
	Estimate int64            // estimated input tokens to reserve
	Chat     *llm.ChatContext
	// Adapter resolves the vendor adapter for a chain model — a closure over
	// the snapshot and the calling client, so the router stays testable.
	Adapter func(m *proxydb.Model) (llm.ClientAdapter, error)
	// SoftSchema reports models whose reply has to be checked here because
	// their API cannot enforce a schema.
	SoftSchema func(m *proxydb.Model) bool
}

// Execute walks the chain: throttle-admit, call with per-model retries on
// retryable errors, settle budgets. Terminal vendor errors stop the chain —
// a schema rejection would fail identically on every model.
func Execute(ctx context.Context, th Throttle, r Request) (*llm.Response, *proxydb.Model, error) {
	var lastErr error
	allThrottled := true
	throttleKind := "rpm"

	for _, m := range r.Chain {
		if !th.AllowRPM(ctx, r.KeyName, m) {
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Msg("model rpm-saturated, trying next in chain")
			continue
		}
		res, tripped := th.Reserve(ctx, m, r.KeyName, r.UserID, r.Estimate)
		if tripped != "" {
			throttleKind = tripped
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("budget", tripped).
				Msg("budget exhausted, trying next in chain")
			continue
		}
		allThrottled = false

		adapter, err := r.Adapter(m)
		if err != nil {
			th.Settle(res, llm.TokensUsage{})
			lastErr = err
			continue
		}

		resp, err := r.callAndCheck(ctx, adapter, m, th, res)
		if err == nil {
			return resp, m, nil
		}
		lastErr = err
		var schemaErr *SchemaFailure
		if errors.As(err, &schemaErr) {
			// The vendor answered; it just could not hold the shape. Another
			// model may, so this advances the chain instead of ending it.
			continue
		}
		if errors.Is(err, llm.ErrOutputTruncated) || !llm.IsRetryableError(err) {
			// Terminal for this request, but log what the vendor actually said:
			// the status the caller sees carries none of it.
			log.Error().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
				Str("key", r.KeyName).Msg("terminal vendor error, ending chain")
			return nil, m, err
		}
	}

	if allThrottled {
		return nil, nil, &ChainExhausted{AllThrottled: true, ThrottleKind: throttleKind}
	}
	return nil, nil, &ChainExhausted{Last: lastErr}
}

// callAndCheck runs one model and, when that model cannot be handed a schema
// through its API, verifies the reply here. A reply of the wrong shape earns
// one more attempt on the same model before the chain moves on. Settles the
// throttle reservation on every path.
func (r Request) callAndCheck(ctx context.Context, adapter llm.ClientAdapter, m *proxydb.Model,
	th Throttle, res *Reservation) (*llm.Response, error) {
	schema := r.Chat.GetResponseSchema()
	soft := schema != nil && r.SoftSchema != nil && r.SoftSchema(m)

	var spent llm.TokensUsage
	var lastCheck error
	for attempt := 0; attempt < schemaAttempts; attempt++ {
		resp, err := callWithRetries(ctx, adapter, r.Chat, m.ID)
		if err != nil {
			th.Settle(res, spent)
			return nil, err
		}
		// Tokens are billed per attempt, so they accumulate even when the
		// shape is wrong and the answer is thrown away.
		spent.Input += resp.Usage.Input
		spent.Output += resp.Usage.Output

		if !soft {
			th.Settle(res, spent)
			return resp, nil
		}
		if err := llm.CheckResponseSchema(resp.Choices[0].Text, schema); err == nil {
			th.Settle(res, spent)
			return resp, nil
		} else {
			lastCheck = err
			log.Error().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
				Int("attempt", attempt+1).Msg("reply did not match the requested schema")
		}
	}
	th.Settle(res, spent)
	return nil, &SchemaFailure{Model: m.ID, Err: lastCheck}
}

// callWithRetries retries retryable vendor errors (429/5xx) on the same
// model before Execute moves down the chain.
func callWithRetries(ctx context.Context, adapter llm.ClientAdapter, chat *llm.ChatContext, modelID string) (*llm.Response, error) {
	var lastErr error
	for attempt := 0; attempt < perModelAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(attemptBackoff * time.Duration(attempt)):
			}
		}
		resp, err := adapter.GenerateText(chat)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !llm.IsRetryableError(err) {
			return nil, err
		}
		log.Warn().Err(err).Str("model", modelID).Int("attempt", attempt+1).Msg("retryable vendor error")
	}
	return nil, lastErr
}

// MediaRequest is the chain walk for the speech and image RPCs. They share the
// text path's throttling and chain semantics but not its schema handling: there
// is no shape to verify, only bytes to get back.
type MediaRequest struct {
	Chain    []*proxydb.Model
	KeyName  string
	UserID   string
	Estimate int64
	// Call runs one model and reports what the vendor billed. The adapter is
	// resolved for the calling client, exactly as on the text path.
	Call func(m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error)
	// Adapter resolves the vendor adapter for a chain model.
	Adapter func(m *proxydb.Model) (llm.ClientAdapter, error)
}

// ExecuteMedia walks the chain for a non-text call. A retryable failure — which
// includes a TTS reply that carried no audio — moves to the next model, so a
// vendor that answers without speech degrades to another rather than failing.
func ExecuteMedia(ctx context.Context, th Throttle, r MediaRequest) (*proxydb.Model, error) {
	var lastErr error
	allThrottled := true
	throttleKind := "rpm"

	for _, m := range r.Chain {
		if !th.AllowRPM(ctx, r.KeyName, m) {
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Msg("model rpm-saturated, trying next in chain")
			continue
		}
		res, tripped := th.Reserve(ctx, m, r.KeyName, r.UserID, r.Estimate)
		if tripped != "" {
			throttleKind = tripped
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("budget", tripped).
				Msg("budget exhausted, trying next in chain")
			continue
		}
		allThrottled = false

		adapter, err := r.Adapter(m)
		if err != nil {
			th.Settle(res, llm.TokensUsage{})
			lastErr = err
			continue
		}

		spent, err := r.Call(m, adapter)
		th.Settle(res, spent)
		if err == nil {
			return m, nil
		}
		lastErr = err
		if !llm.IsRetryableError(err) {
			log.Error().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
				Str("key", r.KeyName).Msg("terminal vendor error, ending chain")
			return m, err
		}
		log.Warn().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
			Msg("media call failed, trying next in chain")
	}

	if allThrottled {
		return nil, &ChainExhausted{AllThrottled: true, ThrottleKind: throttleKind}
	}
	return nil, &ChainExhausted{Last: lastErr}
}
