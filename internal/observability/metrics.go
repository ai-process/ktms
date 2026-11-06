package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts completed requests partitioned by vendor, model, effort, and status.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "requests_total",
			Help:      "Total number of completed requests by vendor, model, effort, and status.",
		},
		[]string{"vendor", "model", "effort", "status"},
	)

	// RequestDuration measures request latency end-to-end.
	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ktms",
			Name:      "request_duration_seconds",
			Help:      "Latency of requests in seconds.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"vendor", "model", "effort"},
	)

	// TimeToFirstToken tracks latency to first chunk in streaming requests.
	TimeToFirstToken = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ktms",
			Name:      "time_to_first_token_seconds",
			Help:      "Latency to the first streaming chunk in seconds.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20},
		},
		[]string{"vendor", "model"},
	)

	// MCPToolCallsTotal counts tool calls through the MCP gateway by outcome:
	// success | tool_error | denied | throttled | timeout | too_large | upstream_error.
	MCPToolCallsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "mcp_tool_calls_total",
			Help:      "Tool calls through the MCP gateway by server, tool and status.",
		},
		[]string{"server", "tool", "status"},
	)

	// MCPToolCallDuration measures a tool call from the gateway to the server and back.
	MCPToolCallDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ktms",
			Name:      "mcp_tool_call_duration_seconds",
			Help:      "Latency of tool calls through the MCP gateway in seconds.",
			Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"server"},
	)

	// MCPResponseBytes is the size of tool results, as JSON.
	MCPResponseBytes = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ktms",
			Name:      "mcp_response_bytes",
			Help:      "Size of tool results through the MCP gateway in bytes.",
			Buckets:   prometheus.ExponentialBuckets(256, 4, 9),
		},
		[]string{"server"},
	)

	// TokensTotal tracks billed input and output tokens.
	TokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "tokens_total",
			Help:      "Total tokens consumed by vendor, model, and direction (input/output).",
		},
		[]string{"vendor", "model", "type"},
	)

	// CacheTokensTotal counts the part of input tokens that hit the prompt
	// cache; apart from TokensTotal so summing that one never counts them twice.
	CacheTokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "cache_tokens_total",
			Help:      "Input tokens read from (read) or written to (write) the vendor's prompt cache, by vendor and model.",
		},
		[]string{"vendor", "model", "type"},
	)

	// ActiveRequests gauges current in-flight calls to upstream vendors.
	ActiveRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "ktms",
			Name:      "active_requests",
			Help:      "Number of currently active upstream requests.",
		},
		[]string{"vendor", "model"},
	)

	// ThrottledTotal counts requests rejected by rate limits or budgets.
	ThrottledTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "throttled_total",
			Help:      "Total number of requests rejected by throttling or budget limits.",
		},
		[]string{"reason"},
	)

	// FallbacksTotal tracks transparent fallback events across models.
	FallbacksTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "fallbacks_total",
			Help:      "Total number of fallback transitions between models.",
		},
		[]string{"from_model", "to_model", "reason"},
	)

	// BatchJobsTotal tracks batch jobs by vendor and lifecycle state.
	BatchJobsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "batch_jobs_total",
			Help:      "Total batch jobs processed by vendor and state.",
		},
		[]string{"vendor", "state"},
	)

	// BatchItemsTotal tracks individual batch item outcomes.
	BatchItemsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "batch_items_total",
			Help:      "Total batch items processed by vendor and state.",
		},
		[]string{"vendor", "state"},
	)

	// BatchPollDuration tracks duration of background batch polling runs.
	BatchPollDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ktms",
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
			Namespace: "ktms",
			Name:      "circuit_breaker_state",
			Help:      "Current upstream circuit breaker state (0=closed, 1=open, 2=half_open).",
		},
		[]string{"vendor", "model"},
	)

	// CircuitBreakerTransitionsTotal tracks tripped and recovered circuits to observe
	// upstream vendor stability trends over time.
	CircuitBreakerTransitionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "circuit_breaker_transitions_total",
			Help:      "Total number of circuit breaker state transitions.",
		},
		[]string{"vendor", "model", "from", "to"},
	)

	// DependencyErrorsTotal counts failed calls to Redis, Postgres and the usage
	// collector, labelled by what the call was for; each one means a limit was
	// skipped, a feature degraded or a request refused, as the README table says.
	DependencyErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "dependency_errors_total",
			Help:      "Total number of failed calls to a dependency (redis, postgres, usage_collector) by operation.",
		},
		[]string{"dependency", "operation"},
	)

	// UsageEventsDroppedTotal counts usage events lost for good: the queue was
	// full while the collector was down, or shutdown came before delivery.
	UsageEventsDroppedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "usage_events_dropped_total",
			Help:      "Total number of usage events dropped before reaching the collector, by reason.",
		},
		[]string{"reason"},
	)

	// VendorCooldownsTotal counts pauses started because a vendor answered 429
	// or 503 with Retry-After; skipped calls show up as fallbacks with reason "cooldown".
	VendorCooldownsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "vendor_cooldowns_total",
			Help:      "Total number of model pauses started from a vendor Retry-After.",
		},
		[]string{"vendor", "model"},
	)

	// CircuitBreakerRejectionsTotal tracks requests skipped immediately without network
	// latency while a model's upstream circuit is open.
	CircuitBreakerRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "circuit_breaker_rejections_total",
			Help:      "Total number of requests rejected by an open circuit breaker.",
		},
		[]string{"vendor", "model"},
	)

	// ShadowRequestsTotal tracks mirrored requests sent to shadow models.
	ShadowRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "shadow_requests_total",
			Help:      "Total number of mirrored shadow requests by vendor, model, and status.",
		},
		[]string{"vendor", "model", "status"},
	)

	// ShadowJudgeEvaluationsTotal tracks automated quality evaluations comparing primary vs shadow.
	ShadowJudgeEvaluationsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "shadow_judge_evaluations_total",
			Help:      "Total automated quality evaluations by primary model, shadow model, and preference outcome.",
		},
		[]string{"primary_model", "shadow_model", "preference"},
	)

	// ShadowJudgeScores records quality scores assigned by Judge to candidates.
	ShadowJudgeScores = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ktms",
			Name:      "shadow_judge_scores",
			Help:      "Quality scores assigned by Judge to primary and shadow outputs.",
			Buckets:   []float64{0.0, 0.25, 0.5, 0.75, 1.0},
		},
		[]string{"model", "role"},
	)

	// LegacyHeadersRequestsTotal tracks requests using deprecated X-LLM-Proxy-* headers.
	LegacyHeadersRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ktms",
			Name:      "legacy_headers_requests_total",
			Help:      "Total requests using deprecated X-LLM-Proxy-* headers.",
		},
		[]string{"header"},
	)
)
