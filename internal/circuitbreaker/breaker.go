package circuitbreaker

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/observability"
)

// State represents the lifecycle state of a circuit breaker.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "UNKNOWN"
	}
}

// Config controls failure threshold and cooldown recovery durations.
type Config struct {
	Enabled       bool
	FailThreshold int
	Cooldown      time.Duration
}

// Breaker tracks consecutive upstream failures and guards access during outages.
type Breaker struct {
	mu               sync.Mutex
	vendor           string
	model            string
	state            State
	consecutiveFails int
	failThreshold    int
	cooldown         time.Duration
	openedAt         time.Time
	probeInProgress  bool
	probeStartedAt   time.Time
	nowFunc          func() time.Time
	onStateChange    func(vendor, model string, from, to State)
}

func (b *Breaker) now() time.Time {
	if b.nowFunc != nil {
		return b.nowFunc()
	}
	return time.Now()
}

// Allow determines if an execution attempt is permitted or failed fast without network IO.
func (b *Breaker) Allow() bool {
	var from State
	var changed bool

	b.mu.Lock()
	now := b.now()
	allowed := false

	switch b.state {
	case StateClosed:
		allowed = true

	case StateOpen:
		if now.Sub(b.openedAt) >= b.cooldown {
			from, changed = b.transitionTo(StateHalfOpen)
			b.probeInProgress = true
			b.probeStartedAt = now
			allowed = true
		}

	case StateHalfOpen:
		if b.probeInProgress && now.Sub(b.probeStartedAt) > b.cooldown {
			b.probeInProgress = false
		}
		if !b.probeInProgress {
			b.probeInProgress = true
			b.probeStartedAt = now
			allowed = true
		}

	default:
		allowed = true
	}
	b.mu.Unlock()

	if changed && b.onStateChange != nil {
		b.onStateChange(b.vendor, b.model, from, StateHalfOpen)
	}
	return allowed
}

// Release frees any active probe lock when a request was admitted by Allow but skipped
// before calling the upstream vendor (e.g. rate limits or pre-call errors).
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateHalfOpen && b.probeInProgress {
		b.probeInProgress = false
	}
}

// Report records execution outcome, updating error counts and state machine transitions.
func (b *Breaker) Report(ctx context.Context, err error) {
	var from, to State
	var changed bool

	b.mu.Lock()
	if IsClientCancellation(ctx, err) {
		if b.state == StateHalfOpen && b.probeInProgress {
			b.probeInProgress = false
		}
		b.mu.Unlock()
		return
	}

	if err == nil {
		b.consecutiveFails = 0
		if b.state == StateHalfOpen {
			b.probeInProgress = false
			from, changed = b.transitionTo(StateClosed)
			to = StateClosed
		}
		b.mu.Unlock()
		if changed && b.onStateChange != nil {
			b.onStateChange(b.vendor, b.model, from, to)
		}
		return
	}

	if !IsTrippingError(ctx, err) {
		if b.state == StateHalfOpen && b.probeInProgress {
			b.probeInProgress = false
		}
		b.mu.Unlock()
		return
	}

	switch b.state {
	case StateClosed:
		b.consecutiveFails++
		if b.consecutiveFails >= b.failThreshold {
			from, changed = b.transitionTo(StateOpen)
			to = StateOpen
		}
	case StateHalfOpen:
		b.probeInProgress = false
		from, changed = b.transitionTo(StateOpen)
		to = StateOpen
	case StateOpen:
	}
	b.mu.Unlock()

	if changed && b.onStateChange != nil {
		b.onStateChange(b.vendor, b.model, from, to)
	}
}

// State returns current lifecycle state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) transitionTo(to State) (State, bool) {
	if b.state == to {
		return b.state, false
	}
	from := b.state
	b.state = to
	if to == StateOpen {
		b.openedAt = b.now()
	}
	observability.CircuitBreakerState.WithLabelValues(b.vendor, b.model).Set(float64(to))
	return from, true
}

// Manager maintains distinct breakers for each upstream (vendor, model) pair.
type Manager struct {
	mu            sync.RWMutex
	breakers      map[string]*Breaker
	cfg           Config
	nowFunc       func() time.Time
	onStateChange func(vendor, model string, from, to State)
}

// NewManager initializes a thread-safe registry of model circuit breakers.
func NewManager(cfg Config) *Manager {
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	return &Manager{
		breakers:      make(map[string]*Breaker),
		cfg:           cfg,
		nowFunc:       time.Now,
		onStateChange: defaultStateChangeLoggerAndMetrics,
	}
}

// Allow delegates admission check to the matching breaker, or admits all if disabled.
func (m *Manager) Allow(vendor, model string) bool {
	if !m.cfg.Enabled {
		return true
	}
	b := m.getOrCreateBreaker(vendor, model)
	return b.Allow()
}

// Release releases a probe slot if an admitted request was aborted before calling the vendor.
func (m *Manager) Release(vendor, model string) {
	if !m.cfg.Enabled {
		return
	}
	key := vendor + "/" + model
	m.mu.RLock()
	b, ok := m.breakers[key]
	m.mu.RUnlock()
	if ok {
		b.Release()
	}
}

// Report forwards call results to the model's breaker.
func (m *Manager) Report(ctx context.Context, vendor, model string, err error) {
	if !m.cfg.Enabled {
		return
	}
	b := m.getOrCreateBreaker(vendor, model)
	b.Report(ctx, err)
}

// State inspects breaker state without mutating registry or emitting metrics.
func (m *Manager) State(vendor, model string) State {
	if !m.cfg.Enabled {
		return StateClosed
	}
	key := vendor + "/" + model
	m.mu.RLock()
	b, ok := m.breakers[key]
	m.mu.RUnlock()
	if !ok {
		return StateClosed
	}
	return b.State()
}

// IsClosed reports whether the model breaker is in CLOSED state.
func (m *Manager) IsClosed(vendor, model string) bool {
	return m.State(vendor, model) == StateClosed
}

// SetNowFunc overrides the clock source for deterministic testing.
func (m *Manager) SetNowFunc(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowFunc = fn
	for _, b := range m.breakers {
		b.mu.Lock()
		b.nowFunc = fn
		b.mu.Unlock()
	}
}

func (m *Manager) getOrCreateBreaker(vendor, model string) *Breaker {
	key := vendor + "/" + model
	m.mu.RLock()
	b, ok := m.breakers[key]
	m.mu.RUnlock()
	if ok {
		return b
	}

	m.mu.Lock()
	if b, ok = m.breakers[key]; ok {
		m.mu.Unlock()
		return b
	}

	b = &Breaker{
		vendor:        vendor,
		model:         model,
		state:         StateClosed,
		failThreshold: m.cfg.FailThreshold,
		cooldown:      m.cfg.Cooldown,
		nowFunc:       m.nowFunc,
		onStateChange: m.onStateChange,
	}
	m.breakers[key] = b
	callback := m.onStateChange
	m.mu.Unlock()

	if callback != nil {
		callback(vendor, model, StateClosed, StateClosed)
	}
	return b
}

func defaultStateChangeLoggerAndMetrics(vendor, model string, from, to State) {
	if from != to {
		observability.CircuitBreakerTransitionsTotal.WithLabelValues(vendor, model, from.String(), to.String()).Inc()
		if to == StateClosed {
			log.Info().
				Str("vendor", vendor).
				Str("model", model).
				Str("from", from.String()).
				Str("to", to.String()).
				Msg("circuit breaker recovered to CLOSED")
		} else {
			log.Warn().
				Str("vendor", vendor).
				Str("model", model).
				Str("from", from.String()).
				Str("to", to.String()).
				Msg("circuit breaker state transition")
		}
	}
}
