package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts completed requests partitioned by vendor, model, effort, and status.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "requests_total",
			Help:      "Total number of completed requests by vendor, model, effort, and status.",
		},
		[]string{"vendor", "model", "effort", "status"},
	)

	// RequestDuration measures request latency end-to-end.
	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "llmproxy",
			Name:      "request_duration_seconds",
			Help:      "Latency of requests in seconds.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"vendor", "model", "effort"},
	)

	// TimeToFirstToken tracks latency to first chunk in streaming requests.
	TimeToFirstToken = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "llmproxy",
			Name:      "time_to_first_token_seconds",
			Help:      "Latency to the first streaming chunk in seconds.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20},
		},
		[]string{"vendor", "model"},
	)

	// TokensTotal tracks billed input and output tokens.
	TokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "tokens_total",
			Help:      "Total tokens consumed by vendor, model, and direction (input/output).",
		},
		[]string{"vendor", "model", "type"},
	)

	// ActiveRequests gauges current in-flight calls to upstream vendors.
	ActiveRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "llmproxy",
			Name:      "active_requests",
			Help:      "Number of currently active upstream requests.",
		},
		[]string{"vendor", "model"},
	)

	// ThrottledTotal counts requests rejected by rate limits or budgets.
	ThrottledTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "throttled_total",
			Help:      "Total number of requests rejected by throttling or budget limits.",
		},
		[]string{"reason"},
	)

	// FallbacksTotal tracks transparent fallback events across models.
	FallbacksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "fallbacks_total",
			Help:      "Total number of fallback transitions between models.",
		},
		[]string{"from_model", "to_model", "reason"},
	)

	// BatchJobsTotal tracks batch jobs by vendor and lifecycle state.
	BatchJobsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "batch_jobs_total",
			Help:      "Total batch jobs processed by vendor and state.",
		},
		[]string{"vendor", "state"},
	)

	// BatchItemsTotal tracks individual batch item outcomes.
	BatchItemsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "batch_items_total",
			Help:      "Total batch items processed by vendor and state.",
		},
		[]string{"vendor", "state"},
	)

	// BatchPollDuration tracks duration of background batch polling runs.
	BatchPollDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "llmproxy",
			Name:      "batch_poll_duration_seconds",
			Help:      "Duration of vendor batch polling operations in seconds.",
			Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30},
		},
		[]string{"vendor"},
	)

	// CircuitBreakerState exports the state machine value (0=Closed, 1=Open, 2=HalfOpen)
	// so monitoring dashboards can alert when upstream vendors trip into outages.
	CircuitBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "llmproxy",
			Name:      "circuit_breaker_state",
			Help:      "Current upstream circuit breaker state (0=closed, 1=open, 2=half_open).",
		},
		[]string{"vendor", "model"},
	)

	// CircuitBreakerTransitionsTotal tracks tripped and recovered circuits to observe
	// upstream vendor stability trends over time.
	CircuitBreakerTransitionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "circuit_breaker_transitions_total",
			Help:      "Total number of circuit breaker state transitions.",
		},
		[]string{"vendor", "model", "from", "to"},
	)

	// CircuitBreakerRejectionsTotal tracks requests skipped immediately without network
	// latency while a model's upstream circuit is open.
	CircuitBreakerRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "circuit_breaker_rejections_total",
			Help:      "Total number of requests rejected by an open circuit breaker.",
		},
		[]string{"vendor", "model"},
	)

	// ShadowRequestsTotal tracks mirrored requests sent to shadow models.
	ShadowRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "llmproxy",
			Name:      "shadow_requests_total",
			Help:      "Total number of mirrored shadow requests by vendor, model, and status.",
		},
		[]string{"vendor", "model", "status"},
	)
)
