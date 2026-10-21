package router

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/panicsafe"
	"github.com/ai-process/ktms/internal/pricing"
	"github.com/ai-process/ktms/internal/proxydb"
)

const (
	perModelAttempts = 2
	attemptBackoff   = 500 * time.Millisecond
	// maxInPlaceWait: a vendor Retry-After up to this long is waited out on the
	// same model; longer ones pause the model and move the chain on.
	maxInPlaceWait = time.Second
	// schemaAttempts: one retry when a soft-schema model answers in the wrong
	// shape. Models that cannot hold a shape rarely find it on a third go.
	schemaAttempts = 2
)

// Throttle is what Execute needs from the limiter. The Redis implementation
// lives in internal/throttle; NoopThrottle allows everything.
type Throttle interface {
	// AllowRPM takes an RPM slot, or reports the model saturated this minute.
	AllowRPM(ctx context.Context, keyName, userID string, m *proxydb.Model) bool
	// Reserve books the token estimate against daily budgets and the priced worst
	// case (estimate in, maxOutput out) against spend limits. tripped names the
	// refusing limit (budget_*, spend_*); empty means reserved, res may be nil.
	Reserve(ctx context.Context, m *proxydb.Model, keyName, userID string, estimate, maxOutput int64) (res *Reservation, tripped string)
	// Settle adjusts reserved budgets to actual usage; usage zero = release.
	Settle(res *Reservation, usage llm.TokensUsage)
	// CheckDailyBudget reports if daily budget is already exhausted ("budget_key" | "budget_user"), or "" if OK.
	CheckDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string) string
	// ChargeDailyBudget books actual batch usage against daily token budgets
	// and, priced at the batch rate, against spend limits.
	ChargeDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string, usage llm.TokensUsage)
}

// CircuitBreaker fails fast on degraded upstream models without network IO.
type CircuitBreaker interface {
	Allow(vendor, model string) bool
	Report(ctx context.Context, vendor, model string, err error)
	Release(vendor, model string)
	IsClosed(vendor, model string) bool
}

// Reservation is the throttle's bookkeeping handle for one admitted call.
type Reservation struct {
	Keys     []string
	Estimate int64
	Spend    *SpendHold
}

// SpendHold is the monetary side of a reservation: counters booked with
// Reserved nano-USD, settled to the actual usage priced in Mode.
type SpendHold struct {
	Keys     []string
	TTLs     []int64 // seconds, per key
	Reserved int64
	Model    *proxydb.Model
	Mode     pricing.Mode
}

// NoopThrottle admits everything; used when Redis is not configured.
type NoopThrottle struct{}

func (NoopThrottle) AllowRPM(context.Context, string, string, *proxydb.Model) bool { return true }
func (NoopThrottle) Reserve(context.Context, *proxydb.Model, string, string, int64, int64) (*Reservation, string) {
	return nil, ""
}
func (NoopThrottle) Settle(*Reservation, llm.TokensUsage) {}
func (NoopThrottle) CheckDailyBudget(context.Context, *proxydb.Model, string, string) string {
	return ""
}
func (NoopThrottle) ChargeDailyBudget(context.Context, *proxydb.Model, string, string, llm.TokensUsage) {
}

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
	return fmt.Sprintf("%s replied outside the requested schema: %v", e.Model, e.Err)
}

func (e *SchemaFailure) Unwrap() error { return e.Err }

// Request is one admission-controlled generation attempt over a model chain.
type Request struct {
	Chain          []*proxydb.Model // pre-filtered candidates, in fallback order
	KeyName        string           // api_key name: throttle keys, logs
	UserID         string           // attributes["user_id"] or svc:<keyName> fallback
	Estimate       int64            // estimated input tokens to reserve
	Chat           *llm.ChatContext
	CircuitBreaker CircuitBreaker
	// Cooldown skips models paused by a vendor Retry-After; nil disables it.
	Cooldown Cooldown
	// UpstreamTimeout bounds each attempt against an upstream vendor.
	UpstreamTimeout time.Duration
	// Adapter resolves the vendor adapter for a chain model — a closure over
	// the snapshot and the calling client, so the router stays testable.
	Adapter func(m *proxydb.Model) (llm.ClientAdapter, error)
	// SoftSchema reports models whose reply has to be checked here because
	// their API cannot enforce a schema.
	SoftSchema func(m *proxydb.Model) bool
	// ShadowModel is an optional candidate for asynchronous request mirroring.
	ShadowModel *proxydb.Model
	// ShadowPercentage is the traffic percentage (0.0 - 100.0) to mirror.
	ShadowPercentage float64
	// ShadowEvaluator is called asynchronously when both primary and shadow responses are ready.
	ShadowEvaluator func(ctx context.Context, primaryModel, shadowModel *proxydb.Model, primaryResp, shadowResp *llm.Response)
}

type primaryResult struct {
	Model    *proxydb.Model
	Response *llm.Response
}

const (
	defaultShadowTimeout          = 30 * time.Second
	maxConcurrentShadows          = 512
	maxConcurrentShadowEvaluation = 256
)

var (
	shadowSem     = make(chan struct{}, maxConcurrentShadows)
	shadowEvalSem = make(chan struct{}, maxConcurrentShadowEvaluation)
)

func samplePercentage(pct float64) bool {
	if pct <= 0 {
		return false
	}
	if pct >= 100 {
		return true
	}
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return false
	}
	return float64(n.Int64()) < pct*100
}

func (r *Request) shouldShadow() bool {
	return r.ShadowModel != nil && r.ShadowPercentage > 0 && samplePercentage(r.ShadowPercentage)
}

func (r *Request) dispatchShadow(ctx context.Context, th Throttle, primaryCh <-chan *primaryResult) {
	shadowModel := r.ShadowModel

	select {
	case shadowSem <- struct{}{}:
	default:
		observability.ShadowRequestsTotal.WithLabelValues(shadowModel.Vendor, shadowModel.ID, "dropped").Inc()
		log.Warn().Str("shadow_model", shadowModel.ID).Msg("shadow capacity exceeded, dropping mirror request")
		return
	}

	var chat *llm.ChatContext
	if r.Chat != nil {
		chat = r.Chat.Clone()
	}
	cb := r.CircuitBreaker
	cooldown := r.Cooldown
	adapterFn := r.Adapter
	keyName := r.KeyName
	userID := r.UserID

	// Mirror traffic asynchronously without adding caller latency or failing user requests.
	go func() {
		defer func() { <-shadowSem }()
		defer panicsafe.Guard("router: shadow dispatch")

		shadowCtx, cancel := context.WithTimeout(llm.WithShadowTraffic(context.WithoutCancel(ctx)), defaultShadowTimeout)
		defer cancel()

		if cb != nil && !cb.IsClosed(shadowModel.Vendor, shadowModel.ID) {
			observability.ShadowRequestsTotal.WithLabelValues(shadowModel.Vendor, shadowModel.ID, "circuit_open").Inc()
			return
		}
		if _, paused := coolingDown(cooldown, shadowModel); paused {
			observability.ShadowRequestsTotal.WithLabelValues(shadowModel.Vendor, shadowModel.ID, "cooldown").Inc()
			return
		}

		if th != nil && !th.AllowRPM(shadowCtx, keyName, userID, shadowModel) {
			observability.ShadowRequestsTotal.WithLabelValues(shadowModel.Vendor, shadowModel.ID, "throttled").Inc()
			return
		}

		adapter, err := adapterFn(shadowModel)
		if err != nil {
			observability.ShadowRequestsTotal.WithLabelValues(shadowModel.Vendor, shadowModel.ID, "error").Inc()
			log.Warn().Err(err).Str("shadow_model", shadowModel.ID).Msg("shadow adapter creation failed")
			return
		}

		if chat != nil {
			chat.WithContext(shadowCtx)
			meta := chat.GetMeta()
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["is_shadow"] = "true"
			meta["shadow"] = "true"
			chat.SetMeta(meta)
		}

		// Mirror load is optional, so its 429s never pause the model for real traffic.
		resp, callErr := adapter.GenerateText(shadowCtx, chat)

		status := "success"
		if callErr != nil {
			status = "error"
			log.Warn().Err(callErr).Str("shadow_model", shadowModel.ID).Str("vendor", shadowModel.Vendor).
				Msg("shadow execution failed")
		}
		observability.ShadowRequestsTotal.WithLabelValues(shadowModel.Vendor, shadowModel.ID, status).Inc()

		if callErr == nil && resp != nil && r.ShadowEvaluator != nil && primaryCh != nil {
			r.dispatchShadowEval(ctx, shadowModel, resp, primaryCh)
		}
	}()
}

func (r *Request) dispatchShadowEval(ctx context.Context, shadowModel *proxydb.Model, shadowResp *llm.Response, primaryCh <-chan *primaryResult) {
	select {
	case shadowEvalSem <- struct{}{}:
	default:
		log.Warn().Str("shadow_model", shadowModel.ID).Msg("shadow evaluation capacity exceeded, skipping evaluation")
		return
	}

	evaluator := r.ShadowEvaluator
	go func() {
		defer func() { <-shadowEvalSem }()
		defer panicsafe.Guard("router: shadow eval dispatch")

		select {
		case pres, ok := <-primaryCh:
			if ok && pres != nil && pres.Response != nil {
				evalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultShadowTimeout)
				defer cancel()
				evaluator(evalCtx, pres.Model, shadowModel, pres.Response, shadowResp)
			}
		case <-ctx.Done():
			// Primary request cancelled or timed out before completion.
		case <-time.After(5 * time.Minute):
			log.Warn().Str("shadow_model", shadowModel.ID).Msg("shadow evaluation timed out waiting for primary response")
		}
	}()
}

// maxOutput is the caller's output ceiling, 0 when none was set.
func (r Request) maxOutput() int64 {
	if r.Chat == nil {
		return 0
	}
	return int64(r.Chat.GetMaxOutputTokens())
}

// Execute walks the chain: throttle-admit, call with per-model retries on
// retryable errors, settle budgets. Terminal vendor errors stop the chain —
// a schema rejection would fail identically on every model.
func Execute(ctx context.Context, th Throttle, r Request) (*llm.Response, *proxydb.Model, error) {
	if r.Chat != nil {
		r.Chat.WithContext(ctx)
	}
	var (
		lastErr          error
		allThrottled     = true
		allCircuitBroken = true
		anyCircuitBroken = false
		throttledCount   = 0
		throttleKind     = "rpm"
		shadowDispatched bool
		paused           pauseTally
	)

	var primaryCh chan *primaryResult
	doShadow := r.shouldShadow()
	if doShadow && r.ShadowEvaluator != nil {
		primaryCh = make(chan *primaryResult, 1)
		defer close(primaryCh)
	}

	for i, m := range r.Chain {
		if cd, ok := coolingDown(r.Cooldown, m); ok {
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModelID(r.Chain, i), "cooldown").Inc()
			allThrottled, allCircuitBroken = false, false
			lastErr = cd
			paused.note(cd)
			continue
		}
		if r.CircuitBreaker != nil && !r.CircuitBreaker.Allow(m.Vendor, m.ID) {
			nextModel := nextModelID(r.Chain, i)
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModel, "circuit_broken").Inc()
			observability.CircuitBreakerRejectionsTotal.WithLabelValues(m.Vendor, m.ID).Inc()
			log.Warn().Str("model", m.ID).Str("vendor", m.Vendor).
				Msg("circuit breaker is OPEN, skipping to next model")
			allThrottled = false
			anyCircuitBroken = true
			continue
		}
		allCircuitBroken = false

		if !th.AllowRPM(ctx, r.KeyName, r.UserID, m) {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			throttledCount++
			observability.ThrottledTotal.WithLabelValues("rpm").Inc()
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("user", r.UserID).Msg("model rpm-saturated, trying next in chain")
			continue
		}
		res, tripped := th.Reserve(ctx, m, r.KeyName, r.UserID, r.Estimate, r.maxOutput())
		if tripped != "" {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			throttledCount++
			throttleKind = tripped
			observability.ThrottledTotal.WithLabelValues(tripped).Inc()
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("budget", tripped).
				Msg("budget exhausted, trying next in chain")
			continue
		}
		allThrottled = false

		adapter, err := r.Adapter(m)
		if err != nil {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			th.Settle(res, llm.TokensUsage{})
			lastErr = err
			continue
		}

		if doShadow && !shadowDispatched {
			shadowDispatched = true
			r.dispatchShadow(ctx, th, primaryCh)
		}

		resp, err := r.callAndCheck(ctx, adapter, m, th, res)
		if r.CircuitBreaker != nil {
			var schemaErr *SchemaFailure
			if errors.As(err, &schemaErr) {
				r.CircuitBreaker.Report(ctx, m.Vendor, m.ID, nil)
			} else {
				r.CircuitBreaker.Report(ctx, m.Vendor, m.ID, err)
			}
		}
		err = pauseOnRetryAfter(r.Cooldown, m, err)
		paused.note(err)
		if err == nil {
			if primaryCh != nil {
				primaryCh <- &primaryResult{Model: m, Response: resp}
			}
			return resp, m, nil
		}
		lastErr = err
		if llm.ShouldSkipToNextModel(err) {
			reason := skipReason(err)
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModelID(r.Chain, i), reason).Inc()
			log.Warn().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).Str("reason", reason).
				Msg("model skipped, trying next in chain")
			continue
		}
		var schemaErr *SchemaFailure
		if errors.As(err, &schemaErr) {
			nextModel := nextModelID(r.Chain, i)
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModel, "schema_failure").Inc()
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

	return nil, nil, exhaustedError(len(r.Chain), allCircuitBroken, allThrottled, anyCircuitBroken, throttledCount, throttleKind, lastErr, paused.retryAfter(len(r.Chain)))
}

func exhaustedError(chainLen int, allCircuitBroken, allThrottled, anyCircuitBroken bool, throttledCount int, throttleKind string, lastErr error, retryAfter time.Duration) *ChainExhausted {
	if chainLen > 0 && allCircuitBroken {
		return &ChainExhausted{AllCircuitBroken: true}
	}
	if allThrottled {
		return &ChainExhausted{AllThrottled: true, ThrottleKind: throttleKind}
	}
	if lastErr == nil && (anyCircuitBroken || throttledCount > 0) {
		if throttledCount > 0 {
			return &ChainExhausted{
				AllThrottled:     true,
				ThrottleKind:     throttleKind,
				AnyCircuitBroken: anyCircuitBroken,
			}
		}
		return &ChainExhausted{AllCircuitBroken: true}
	}
	return &ChainExhausted{Last: lastErr, AnyCircuitBroken: anyCircuitBroken, RetryAfter: retryAfter}
}

// ExecuteStream walks the model chain for streaming requests. Pre-stream failures
// fallback to the next model; once the first chunk is emitted, the stream locks.
func ExecuteStream(ctx context.Context, th Throttle, r Request, onChunk func(*proxydb.Model, llm.StreamChunk) error) (*proxydb.Model, error) {
	if r.Chat != nil {
		r.Chat.WithContext(ctx)
	}
	var (
		lastErr          error
		allThrottled     = true
		allCircuitBroken = true
		anyCircuitBroken = false
		throttledCount   = 0
		throttleKind     = "rpm"
		shadowDispatched bool
		paused           pauseTally
	)

	var primaryCh chan *primaryResult
	doShadow := r.shouldShadow()
	if doShadow && r.ShadowEvaluator != nil {
		primaryCh = make(chan *primaryResult, 1)
		defer close(primaryCh)
	}

	for i, m := range r.Chain {
		if cd, ok := coolingDown(r.Cooldown, m); ok {
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModelID(r.Chain, i), "cooldown").Inc()
			allThrottled, allCircuitBroken = false, false
			lastErr = cd
			paused.note(cd)
			continue
		}
		if r.CircuitBreaker != nil && !r.CircuitBreaker.Allow(m.Vendor, m.ID) {
			nextModel := nextModelID(r.Chain, i)
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModel, "circuit_broken").Inc()
			observability.CircuitBreakerRejectionsTotal.WithLabelValues(m.Vendor, m.ID).Inc()
			log.Warn().Str("model", m.ID).Str("vendor", m.Vendor).
				Msg("circuit breaker is OPEN, skipping in stream chain")
			allThrottled = false
			anyCircuitBroken = true
			continue
		}
		allCircuitBroken = false

		if !th.AllowRPM(ctx, r.KeyName, r.UserID, m) {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			throttledCount++
			observability.ThrottledTotal.WithLabelValues("rpm").Inc()
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("user", r.UserID).Msg("model rpm-saturated, trying next in stream chain")
			continue
		}
		res, tripped := th.Reserve(ctx, m, r.KeyName, r.UserID, r.Estimate, r.maxOutput())
		if tripped != "" {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			throttledCount++
			throttleKind = tripped
			observability.ThrottledTotal.WithLabelValues(tripped).Inc()
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("budget", tripped).
				Msg("budget exhausted, trying next in chain")
			continue
		}
		allThrottled = false

		adapter, err := r.Adapter(m)
		if err != nil {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			th.Settle(res, llm.TokensUsage{})
			lastErr = err
			continue
		}

		type streamChecker interface {
			SupportsStreaming() bool
		}
		if sc, ok := adapter.(streamChecker); ok && !sc.SupportsStreaming() {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			th.Settle(res, llm.TokensUsage{})
			log.Warn().Str("model", m.ID).Msg("model does not support streaming, trying next in chain")
			continue
		}

		streamAdapter, ok := adapter.(llm.StreamAdapter)
		if !ok {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			th.Settle(res, llm.TokensUsage{})
			log.Warn().Str("model", m.ID).Msg("model does not support streaming, trying next in chain")
			continue
		}

		if doShadow && !shadowDispatched {
			shadowDispatched = true
			r.dispatchShadow(ctx, th, primaryCh)
		}

		schema := r.Chat.GetResponseSchema()
		soft := schema != nil && r.SoftSchema != nil && r.SoftSchema(m)

		var (
			firstChunkSent bool
			spent          llm.TokensUsage
			outChars       int
			bufferedChunks []llm.StreamChunk
			fullText       strings.Builder
			streamFullText strings.Builder
		)

		attemptStream := func() error {
			streamCtx := ctx
			var stopWatchdog func()
			var pauseWatchdog func()
			var resumeWatchdog func()
			if r.UpstreamTimeout > 0 {
				var cancel context.CancelCauseFunc
				streamCtx, cancel = context.WithCancelCause(ctx)
				var mu sync.Mutex
				timer := time.AfterFunc(r.UpstreamTimeout, func() {
					cancel(circuitbreaker.ErrUpstreamTimeout)
				})
				pauseWatchdog = func() {
					mu.Lock()
					defer mu.Unlock()
					if timer != nil {
						timer.Stop()
					}
				}
				resumeWatchdog = func() {
					mu.Lock()
					defer mu.Unlock()
					if timer != nil {
						timer.Reset(r.UpstreamTimeout)
					}
				}
				stopWatchdog = func() {
					mu.Lock()
					defer mu.Unlock()
					if timer != nil {
						timer.Stop()
						timer = nil
					}
					cancel(nil)
				}
				defer stopWatchdog()
			}

			if r.Chat != nil {
				r.Chat.WithContext(streamCtx)
				defer r.Chat.WithContext(ctx)
			}

			streamStart := time.Now()
			callErr := streamAdapter.GenerateTextStream(streamCtx, r.Chat, func(chunk llm.StreamChunk) error {
				if primaryCh != nil && chunk.Delta != "" {
					streamFullText.WriteString(chunk.Delta)
				}
				if !firstChunkSent {
					firstChunkSent = true
					observability.TimeToFirstToken.WithLabelValues(m.Vendor, m.ID).Observe(time.Since(streamStart).Seconds())
				}
				if soft {
					bufferedChunks = append(bufferedChunks, chunk)
					fullText.WriteString(chunk.Delta)
					if chunk.Usage != nil {
						spent.Input = chunk.Usage.Input
						spent.Output = chunk.Usage.Output
					}
					if resumeWatchdog != nil {
						resumeWatchdog()
					}
					return nil
				}
				outChars += len([]rune(chunk.Delta))
				for _, tc := range chunk.ToolCallChunks {
					outChars += len([]rune(tc.ArgumentsDelta)) + len([]rune(tc.Name))
				}
				if chunk.Usage != nil {
					spent.Input = chunk.Usage.Input
					spent.Output = chunk.Usage.Output
				}
				if pauseWatchdog != nil {
					pauseWatchdog()
				}
				chunkErr := onChunk(m, chunk)
				if chunkErr == nil && resumeWatchdog != nil {
					resumeWatchdog()
				}
				return chunkErr
			})
			if callErr != nil && r.UpstreamTimeout > 0 && errors.Is(context.Cause(streamCtx), circuitbreaker.ErrUpstreamTimeout) {
				if !llm.IsCapacityExhausted(callErr) {
					callErr = circuitbreaker.NewUpstreamTimeoutError(r.UpstreamTimeout, callErr)
				}
			}
			return callErr
		}
		// Only a stream that has sent nothing yet can be retried on the same model.
		for attempt := 1; ; attempt++ {
			err = attemptStream()
			if err == nil || firstChunkSent || !retrySameModel(ctx, err, attempt, m.ID, r.Cooldown != nil) {
				break
			}
		}
		err = callerGone(ctx, err)

		if soft && err == nil {
			if checkErr := llm.CheckResponseSchema(fullText.String(), schema); checkErr != nil {
				nextModel := nextModelID(r.Chain, i)
				observability.FallbacksTotal.WithLabelValues(m.ID, nextModel, "schema_failure").Inc()
				log.Error().Err(checkErr).Str("model", m.ID).Str("vendor", m.Vendor).
					Msg("stream reply did not match requested schema, trying next in chain")
				if spent.Input == 0 {
					if r.Chat != nil {
						spent.Input = r.Chat.EstimateInputTokens()
					}
					if spent.Input == 0 && r.Estimate > 0 {
						spent.Input = int(r.Estimate)
					}
				}
				if spent.Output == 0 {
					spent.Output = llm.EstimateOutputTokens(len([]rune(fullText.String())))
				}
				th.Settle(res, spent)
				if r.CircuitBreaker != nil {
					r.CircuitBreaker.Report(ctx, m.Vendor, m.ID, nil)
				}
				lastErr = &SchemaFailure{Model: m.ID, Err: checkErr}
				continue
			}
			firstChunkSent = true
			for _, c := range bufferedChunks {
				outChars += len([]rune(c.Delta))
				for _, tc := range c.ToolCallChunks {
					outChars += len([]rune(tc.ArgumentsDelta)) + len([]rune(tc.Name))
				}
				if c.Usage != nil {
					spent.Input = c.Usage.Input
					spent.Output = c.Usage.Output
				}
				if emitErr := onChunk(m, c); emitErr != nil {
					th.Settle(res, spent)
					if r.CircuitBreaker != nil {
						r.CircuitBreaker.Report(ctx, m.Vendor, m.ID, nil)
					}
					return m, emitErr
				}
			}
		}

		disconnected := ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if (firstChunkSent || disconnected || (soft && len(bufferedChunks) > 0)) && spent.Input == 0 {
			if r.Chat != nil {
				spent.Input = r.Chat.EstimateInputTokens()
			}
			if spent.Input == 0 && r.Estimate > 0 {
				spent.Input = int(r.Estimate)
			}
		}
		if spent.Output == 0 {
			if outChars > 0 {
				spent.Output = llm.EstimateOutputTokens(outChars)
			} else if soft && fullText.Len() > 0 {
				spent.Output = llm.EstimateOutputTokens(len([]rune(fullText.String())))
			}
		}

		th.Settle(res, spent)

		if r.CircuitBreaker != nil {
			r.CircuitBreaker.Report(ctx, m.Vendor, m.ID, err)
		}
		err = pauseOnRetryAfter(r.Cooldown, m, err)
		paused.note(err)

		if err == nil {
			if primaryCh != nil {
				primaryCh <- &primaryResult{
					Model: m,
					Response: &llm.Response{
						Choices: []*llm.Message{
							{
								Type: llm.MessageTypeBot,
								Text: streamFullText.String(),
							},
						},
						Usage: spent,
					},
				}
			}
			return m, nil
		}

		lastErr = err
		if firstChunkSent {
			return m, err
		}

		if errors.Is(err, llm.ErrStreamingNotSupported) {
			log.Warn().Str("model", m.ID).Msg("streaming not supported, trying next in chain")
			continue
		}

		if llm.ShouldSkipToNextModel(err) {
			reason := skipReason(err)
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModelID(r.Chain, i), reason).Inc()
			log.Warn().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).Str("reason", reason).
				Msg("model skipped in stream, trying next in chain")
			continue
		}

		if errors.Is(err, llm.ErrOutputTruncated) || !llm.IsRetryableError(err) {
			log.Error().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
				Str("key", r.KeyName).Msg("terminal vendor error, ending stream chain")
			return m, err
		}
	}

	return nil, exhaustedError(len(r.Chain), allCircuitBroken, allThrottled, anyCircuitBroken, throttledCount, throttleKind, lastErr, paused.retryAfter(len(r.Chain)))
}

// callAndCheck checks what the vendor may not have enforced (soft-schema text,
// strict tool arguments). A wrong shape gets one more attempt on the same
// model before the chain moves on; the reservation is settled on every path.
func (r Request) callAndCheck(ctx context.Context, adapter llm.ClientAdapter, m *proxydb.Model,
	th Throttle, res *Reservation) (*llm.Response, error) {
	soft := r.Chat.GetResponseSchema() != nil && r.SoftSchema != nil && r.SoftSchema(m)
	var spent llm.TokensUsage
	var lastCheck error
	for attempt := 0; attempt < schemaAttempts; attempt++ {
		resp, err := callWithRetries(ctx, adapter, r.Chat, m.ID, r.UpstreamTimeout, r.Cooldown != nil)
		if err != nil {
			th.Settle(res, spent)
			return nil, err
		}
		// Tokens are billed per attempt, so they accumulate even when the
		// shape is wrong and the answer is thrown away.
		spent.Input += resp.Usage.Input
		spent.Output += resp.Usage.Output

		if err := checkReply(resp, r.Chat, soft); err == nil {
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

// checkReply validates strict tool calls when the model called tools, and a
// soft-schema model's text against the response schema otherwise.
func checkReply(resp *llm.Response, chat *llm.ChatContext, soft bool) error {
	if resp == nil || len(resp.Choices) == 0 || chat == nil {
		return nil
	}
	msg := resp.Choices[0]
	if len(msg.ToolCalls) > 0 {
		return llm.CheckToolCalls(msg.ToolCalls, strictTools(chat.GetTools()))
	}
	if !soft {
		return nil
	}
	return llm.CheckResponseSchema(msg.Text, chat.GetResponseSchema())
}

// strictTools are the tools whose arguments the caller asked to be exact;
// the others are best effort by contract and are passed through as given.
func strictTools(tools []*llm.Tool) []*llm.Tool {
	var out []*llm.Tool
	for _, t := range tools {
		if t != nil && t.Function.Strict {
			out = append(out, t)
		}
	}
	return out
}

// callWithRetries retries retryable vendor errors (429/5xx, transport failures)
// on the same model before Execute moves down the chain.
func callWithRetries(ctx context.Context, adapter llm.ClientAdapter, chat *llm.ChatContext, modelID string, upstreamTimeout time.Duration, pausable bool) (*llm.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := func() (*llm.Response, error) {
			attemptCtx := ctx
			if upstreamTimeout > 0 {
				var cancel context.CancelFunc
				attemptCtx, cancel = context.WithTimeoutCause(ctx, upstreamTimeout, circuitbreaker.ErrUpstreamTimeout)
				defer cancel()
			}
			if chat != nil {
				chat.WithContext(attemptCtx)
				defer chat.WithContext(ctx)
			}

			r, callErr := adapter.GenerateText(attemptCtx, chat)
			if callErr != nil && upstreamTimeout > 0 && errors.Is(context.Cause(attemptCtx), circuitbreaker.ErrUpstreamTimeout) {
				if !llm.IsCapacityExhausted(callErr) {
					callErr = circuitbreaker.NewUpstreamTimeoutError(upstreamTimeout, callErr)
				}
			}
			return r, callErr
		}()
		if err == nil {
			return resp, nil
		}
		if !retrySameModel(ctx, err, attempt, modelID, pausable) {
			return nil, callerGone(ctx, err)
		}
	}
}

// retrySameModel decides whether a failed attempt gets another go on the same
// model and waits for it: the vendor's short Retry-After, else the fixed
// backoff. A longer Retry-After returns false when the chain can pause the model.
func retrySameModel(ctx context.Context, err error, attempt int, modelID string, pausable bool) bool {
	if attempt >= perModelAttempts || ctx.Err() != nil || llm.ShouldSkipToNextModel(err) || !llm.IsRetryableError(err) {
		return false
	}
	// These are "retryable" only so the chain advances; the same model answers alike.
	if errors.Is(err, llm.ErrNoAudioContent) || errors.Is(err, llm.ErrJudgmentUnsupported) || errors.Is(err, llm.ErrEmbeddingUnsupported) {
		return false
	}
	wait := attemptBackoff * time.Duration(attempt)
	if hint, ok := llm.RetryAfter(err); ok {
		switch {
		case hint <= maxInPlaceWait:
			wait = hint
		case pausable:
			return false
		}
	}
	log.Warn().Err(err).Str("model", modelID).Int("attempt", attempt).Dur("wait", wait).Msg("retryable vendor error")
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// MediaRequest is the chain walk for the speech and image RPCs. They share the
// text path's throttling and chain semantics but not its schema handling: there
// is no shape to verify, only bytes to get back.
type MediaRequest struct {
	Chain          []*proxydb.Model
	KeyName        string
	UserID         string
	Estimate       int64
	CircuitBreaker CircuitBreaker
	// Cooldown skips models paused by a vendor Retry-After; nil disables it.
	Cooldown Cooldown
	// UpstreamTimeout bounds each attempt against an upstream vendor.
	UpstreamTimeout time.Duration
	// Call runs one model and reports what the vendor billed. The adapter is
	// resolved for the calling client, exactly as on the text path.
	Call func(ctx context.Context, m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error)
	// Adapter resolves the vendor adapter for a chain model.
	Adapter func(m *proxydb.Model) (llm.ClientAdapter, error)
}

// ExecuteMedia walks the chain for a non-text call. A retryable failure — which
// includes a TTS reply that carried no audio — moves to the next model, so a
// vendor that answers without speech degrades to another rather than failing.
func ExecuteMedia(ctx context.Context, th Throttle, r MediaRequest) (*proxydb.Model, error) {
	var lastErr error
	allThrottled := true
	allCircuitBroken := true
	anyCircuitBroken := false
	throttledCount := 0
	throttleKind := "rpm"
	var paused pauseTally

	for i, m := range r.Chain {
		if cd, ok := coolingDown(r.Cooldown, m); ok {
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModelID(r.Chain, i), "cooldown").Inc()
			allThrottled, allCircuitBroken = false, false
			lastErr = cd
			paused.note(cd)
			continue
		}
		if r.CircuitBreaker != nil && !r.CircuitBreaker.Allow(m.Vendor, m.ID) {
			nextModel := nextModelID(r.Chain, i)
			observability.FallbacksTotal.WithLabelValues(m.ID, nextModel, "circuit_broken").Inc()
			observability.CircuitBreakerRejectionsTotal.WithLabelValues(m.Vendor, m.ID).Inc()
			log.Warn().Str("model", m.ID).Str("vendor", m.Vendor).
				Msg("circuit breaker is OPEN, skipping in media chain")
			allThrottled = false
			anyCircuitBroken = true
			continue
		}
		allCircuitBroken = false

		if !th.AllowRPM(ctx, r.KeyName, r.UserID, m) {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			throttledCount++
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("user", r.UserID).Msg("model rpm-saturated, trying next in chain")
			continue
		}
		res, tripped := th.Reserve(ctx, m, r.KeyName, r.UserID, r.Estimate, 0)
		if tripped != "" {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			throttledCount++
			throttleKind = tripped
			log.Warn().Str("model", m.ID).Str("key", r.KeyName).Str("budget", tripped).
				Msg("budget exhausted, trying next in chain")
			continue
		}
		allThrottled = false

		adapter, err := r.Adapter(m)
		if err != nil {
			if r.CircuitBreaker != nil {
				r.CircuitBreaker.Release(m.Vendor, m.ID)
			}
			th.Settle(res, llm.TokensUsage{})
			lastErr = err
			continue
		}

		attemptCall := func() (llm.TokensUsage, error) {
			attemptCtx := ctx
			if r.UpstreamTimeout > 0 {
				var cancel context.CancelFunc
				attemptCtx, cancel = context.WithTimeoutCause(ctx, r.UpstreamTimeout, circuitbreaker.ErrUpstreamTimeout)
				defer cancel()
			}
			sp, callErr := r.Call(attemptCtx, m, adapter)
			if callErr != nil && r.UpstreamTimeout > 0 && errors.Is(context.Cause(attemptCtx), circuitbreaker.ErrUpstreamTimeout) {
				if !llm.IsCapacityExhausted(callErr) {
					callErr = circuitbreaker.NewUpstreamTimeoutError(r.UpstreamTimeout, callErr)
				}
			}
			return sp, callErr
		}
		var spent llm.TokensUsage
		for attempt := 1; ; attempt++ {
			sp, callErr := attemptCall()
			// Failed attempts can still be billed (partial media), so usage adds up.
			spent.Input += sp.Input
			spent.Output += sp.Output
			err = callErr
			if err == nil || !retrySameModel(ctx, err, attempt, m.ID, r.Cooldown != nil) {
				break
			}
		}
		err = callerGone(ctx, err)
		th.Settle(res, spent)
		if r.CircuitBreaker != nil {
			r.CircuitBreaker.Report(ctx, m.Vendor, m.ID, err)
		}
		err = pauseOnRetryAfter(r.Cooldown, m, err)
		paused.note(err)
		if err == nil {
			return m, nil
		}
		lastErr = err
		if llm.ShouldSkipToNextModel(err) {
			log.Warn().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
				Msg("skipping to next model in chain")
			continue
		}
		if !llm.IsRetryableError(err) {
			log.Error().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
				Str("key", r.KeyName).Msg("terminal vendor error, ending chain")
			return m, err
		}
		log.Warn().Err(err).Str("model", m.ID).Str("vendor", m.Vendor).
			Msg("media call failed, trying next in chain")
	}

	return nil, exhaustedError(len(r.Chain), allCircuitBroken, allThrottled, anyCircuitBroken, throttledCount, throttleKind, lastErr, paused.retryAfter(len(r.Chain)))
}
