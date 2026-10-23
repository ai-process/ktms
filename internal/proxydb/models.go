// Package proxydb is the pgx data layer: api keys, the model registry, routing
// rules, encrypted vendor keys, and the config version counter.
package proxydb

import (
	"errors"
	"time"
)

var (
	ErrNotFound      = errors.New("proxydb: not found")
	ErrAlreadyExists = errors.New("proxydb: already exists")
	ErrTenantInUse   = errors.New("proxydb: tenant still has active api keys")
)

type Organization struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

type Team struct {
	ID               string
	OrgID            string
	Name             string
	MonthlyBudgetUSD float64
	AllowedModels    []string
	CreatedAt        time.Time
}

type APIKey struct {
	ID                string
	Name              string
	KeyLookup         string
	KeyHash           string
	Scopes            []string
	DisabledAt        *time.Time
	CreatedAt         time.Time
	OrgID             *string
	TeamID            *string
	TeamAllowedModels []string
}

func (k *APIKey) Active() bool { return k.DisabledAt == nil }

type NodeKey struct {
	ID             int64
	NodeID         string
	Name           string
	KeyLookup      string
	KeyHash        string
	AllowedCIDRs   []string
	AllowedModels  []string
	MaxBaseWeight  float64
	MaxConcurrency int32
	DisabledAt     *time.Time
	CreatedAt      time.Time
}

func (k *NodeKey) Active() bool { return k.DisabledAt == nil }

type Model struct {
	ID              string
	Vendor          string
	Endpoint        string
	Efforts         []string
	Capabilities    []string
	RPM             int32
	PriceInPerMtok  float64
	PriceOutPerMtok float64
	// Peak rates, 0 when the vendor bills one price around the clock. DeepSeek
	// V4 doubles for two windows a day, so one price understates its cost.
	PriceInPeakPerMtok  float64
	PriceOutPeakPerMtok float64
	// Batch rates, 0 means half the standard (off-peak) rate.
	PriceInBatchPerMtok  float64
	PriceOutBatchPerMtok float64
	// Prompt-cache rates at the standard rate, 0 = the vendor default.
	PriceCacheReadPerMtok  float64
	PriceCacheWritePerMtok float64
	DailyTokensPerKey      int64
	DailyTokensPerUser     int64
	Burst                  int32
	QueueTimeoutMs         int32
	BudgetGraceMargin      float64
	Enabled                bool
}

func (m *Model) EffectiveBurst() int32 {
	if m.Burst > 0 {
		return m.Burst
	}
	if m.Burst < 0 {
		return 0
	}
	if m.RPM > 1 {
		return m.RPM - 1
	}
	return 0
}

type UserRateLimit struct {
	UserID         string
	ModelID        string
	RPM            int32
	Burst          int32
	QueueTimeoutMs int32
	DailyTokens    int64
}

func (u *UserRateLimit) EffectiveBurst() int32 {
	if u.Burst > 0 {
		return u.Burst
	}
	if u.Burst < 0 {
		return 0
	}
	if u.RPM > 1 {
		return u.RPM - 1
	}
	return 0
}

// SpendLimit caps spend for one subject over one UTC calendar period.
type SpendLimit struct {
	SubjectType  string // "key" | "user" | "team"
	SubjectID    string // key name / user id, or "*" for the subject type's default
	Period       string // "day" | "month"
	LimitNanoUSD int64
}

func (m *Model) BatchPriceInPerMtok() float64 {
	if m.PriceInBatchPerMtok > 0 {
		return m.PriceInBatchPerMtok
	}
	return m.PriceInPerMtok / 2.0
}

// CacheReadPerMtok is the standard rate for input read from the prompt cache.
// Unset, only Anthropic's published ratio is assumed (it holds for every Claude
// model); elsewhere input price, which may overcharge but never undercharges.
func (m *Model) CacheReadPerMtok() float64 {
	if m.PriceCacheReadPerMtok > 0 {
		return m.PriceCacheReadPerMtok
	}
	if m.Vendor == "anthropic" {
		return m.PriceInPerMtok * 0.1
	}
	return m.PriceInPerMtok
}

// CacheWritePerMtok is the standard rate for input written to the prompt cache:
// Anthropic and Bedrock charge 1.25x input for the default 5-minute cache.
func (m *Model) CacheWritePerMtok() float64 {
	if m.PriceCacheWritePerMtok > 0 {
		return m.PriceCacheWritePerMtok
	}
	if m.Vendor == "anthropic" || m.Vendor == "bedrock" {
		return m.PriceInPerMtok * 1.25
	}
	return m.PriceInPerMtok
}

func (m *Model) BatchPriceOutPerMtok() float64 {
	if m.PriceOutBatchPerMtok > 0 {
		return m.PriceOutBatchPerMtok
	}
	return m.PriceOutPerMtok / 2.0
}

type Batch struct {
	ID            string
	ClientBatchID string
	APIKeyID      string
	KeyName       string
	Model         string
	VendorJobName string
	State         string // PENDING, RUNNING, SUCCEEDED, FAILED, CANCELLED, EXPIRED
	TotalCount    int32
	DoneCount     int32
	FailedCount   int32
	Attributes    map[string]string
	ActionID      string
	CreatedAt     time.Time
	CompletedAt   *time.Time
	LeasedUntil   *time.Time
	// The submitting key's tenant, read on lease: stored attributes predate
	// the reserved team keys and may carry a caller's own team_id.
	OrgID  string
	TeamID string
}

type BatchItem struct {
	BatchID        string
	CustomID       string
	Position       int32
	Status         string // PENDING, SUCCEEDED, FAILED
	ResponseSchema []byte // optional JSON representation of ResponseSchema
	Result         []byte // raw JSON of GenerateTextResponse
	Error          []byte // raw JSON of BatchItemError
	InputTokens    int64
	OutputTokens   int64
}

type Rule struct {
	ID               string
	Name             string
	Priority         int32
	Effort           string // "" = any
	Attributes       map[string]string
	Enabled          bool
	Use              []string // ordered model ids
	Weights          []int32  // optional traffic weights per model in Use
	ShadowModel      string   // optional shadow model for async traffic mirroring
	ShadowPercentage float64  // 0.0 to 100.0 percent of traffic mirrored to ShadowModel
	ShadowEval       bool     // optional automated quality evaluation using Judge
}

type VendorKey struct {
	ID            string
	APIKeyID      string
	APIKeyName    string // joined for admin listings
	Vendor        string
	KeyCiphertext []byte
	KeyVersion    int32
	CreatedAt     time.Time
}
