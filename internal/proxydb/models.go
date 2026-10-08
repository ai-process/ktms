// Package proxydb is the pgx data layer: api keys, the model registry, routing
// rules, encrypted vendor keys, and the config version counter.
package proxydb

import (
	"errors"
	"time"
)

var ErrNotFound = errors.New("proxydb: not found")

type APIKey struct {
	ID         string
	Name       string
	KeyLookup  string
	KeyHash    string
	Scopes     []string
	DisabledAt *time.Time
	CreatedAt  time.Time
}

func (k *APIKey) Active() bool { return k.DisabledAt == nil }

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
	DailyTokensPerKey    int64
	DailyTokensPerUser   int64
	Enabled              bool
}

func (m *Model) BatchPriceInPerMtok() float64 {
	if m.PriceInBatchPerMtok > 0 {
		return m.PriceInBatchPerMtok
	}
	return m.PriceInPerMtok / 2.0
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
	ID         string
	Name       string
	Priority   int32
	Effort     string // "" = any
	Attributes map[string]string
	Enabled    bool
	Use        []string // ordered model ids
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
