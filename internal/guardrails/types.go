package guardrails

import "errors"

// ErrGuardrailViolation indicates an input violated security policies.
var ErrGuardrailViolation = errors.New("guardrail_violation")

// Action defines how guardrails handle violations.
type Action string

const (
	ActionBlock  Action = "block"
	ActionRedact Action = "redact"
	ActionLog    Action = "log"
)

// ViolationCategory categorizes the detected violation.
type ViolationCategory string

const (
	CategoryCreditCard      ViolationCategory = "credit_card"
	CategoryEmail           ViolationCategory = "email"
	CategoryPhone           ViolationCategory = "phone"
	CategorySSN             ViolationCategory = "ssn"
	CategoryIPAddress       ViolationCategory = "ip_address"
	CategoryPromptInjection ViolationCategory = "prompt_injection"
	CategoryLatentThreat    ViolationCategory = "latent_threat"
)

// Violation records a detected security rule hit.
type Violation struct {
	Category ViolationCategory
	Detail   string
	Value    string
}
