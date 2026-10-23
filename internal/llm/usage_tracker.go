package llm

import (
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// Caller attributes an LLM request for usage tracking: the internal user id
// plus optional metadata forwarded to usage events (e.g. batch job and
// task ids, so token expense can be aggregated per job).
type Caller struct {
	UserID string
	Meta   map[string]string
}

// UsageTracker is an interface for tracking LLM usage events
type UsageTracker interface {
	// RecordUsage records a usage event asynchronously
	// Returns an error if validation fails or if the event cannot be queued for sending
	// userID is the UUID of the user making the request
	// actionID identifies the LLM action (e.g., "generate_question", "evaluate_solution")
	// vendor is the LLM vendor (e.g., "openai", "google")
	// model is the model name (e.g., "gpt-5", "gemini-2.5-pro")
	// usage is the tokens billed, input including its prompt-cache part
	// durationMs is the request duration in milliseconds
	// costNanoUSD is the call's price in nano-USD (1e-9 USD), 0 when unpriced
	// status is the request status (success, error, etc.)
	// meta is optional metadata stored on the event (keep small)
	RecordUsage(
		userID string,
		actionID string,
		vendor string,
		model string,
		usage TokensUsage,
		durationMs int64,
		costNanoUSD int64,
		status usagev1.Status,
		meta map[string]string,
	) error
}

// NoOpUsageTracker is a usage tracker that does nothing (for when usage collection is disabled)
type NoOpUsageTracker struct{}

func (n *NoOpUsageTracker) RecordUsage(userID, actionID, vendor, model string, usage TokensUsage, durationMs, costNanoUSD int64, status usagev1.Status, meta map[string]string) error {
	// No-op
	return nil
}
