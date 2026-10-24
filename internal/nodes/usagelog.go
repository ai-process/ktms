package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/llm/adapters"
)

const (
	usageField        = "r"
	defaultUsageLimit = 500
	maxUsageLimit     = 5000
)

// UsageRecord is one request on a node as its owner sees it: what the machine
// did, never for whom.
type UsageRecord struct {
	ID              string    `json:"-"`
	RequestID       string    `json:"req"`
	StartedAt       time.Time `json:"at"`
	DurationMs      int64     `json:"dur"`
	TTFTMs          int64     `json:"ttft,omitempty"`
	ModelID         string    `json:"model"`
	Kind            string    `json:"kind"`
	InputTokens     int64     `json:"in,omitempty"`
	OutputTokens    int64     `json:"out,omitempty"`
	CacheReadTokens int64     `json:"cache,omitempty"`
	Outcome         string    `json:"outcome"`
	ErrorCode       string    `json:"err,omitempty"`
	CapacityPercent uint32    `json:"level"`
	CooldownMs      int64     `json:"cd,omitempty"`
}

// UsagePage is one pull from a node's log.
type UsagePage struct {
	Records  []UsageRecord
	NextID   string
	More     bool
	GapUntil time.Time // zero unless the cursor is older than the log keeps
}

// UsageLog keeps each node's requests in a Redis stream that every proxy
// replica appends to, trimmed by age on each write.
type UsageLog struct {
	rdb       redis.UniversalClient
	store     *Store
	retention time.Duration // 0 disables the log
	now       func() time.Time
}

func NewUsageLog(rdb redis.UniversalClient, store *Store, retention time.Duration) *UsageLog {
	return &UsageLog{rdb: rdb, store: store, retention: retention, now: time.Now}
}

func (l *UsageLog) Enabled() bool { return l != nil && l.retention > 0 }

// Append adds a record and drops those older than the retention.
func (l *UsageLog) Append(ctx context.Context, nodeID string, r UsageRecord) error {
	if !l.Enabled() {
		return nil
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal usage record: %w", err)
	}
	return l.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: l.store.keys.NodeUsage(nodeID),
		MinID:  strconv.FormatInt(l.now().Add(-l.retention).UnixMilli(), 10),
		Approx: true,
		Values: []any{usageField, data},
	}).Err()
}

// Pull returns up to limit records after the cursor afterID ("" = the oldest kept).
func (l *UsageLog) Pull(ctx context.Context, nodeID, afterID string, limit int) (*UsagePage, error) {
	if limit <= 0 {
		limit = defaultUsageLimit
	}
	limit = min(limit, maxUsageLimit)
	key := l.store.keys.NodeUsage(nodeID)
	start := "-"
	if afterID != "" {
		if _, _, err := parseStreamID(afterID); err != nil {
			return nil, fmt.Errorf("%w: after_id %q: %w", ErrBadCursor, afterID, err)
		}
		start = "(" + afterID
	}
	msgs, err := l.rdb.XRangeN(ctx, key, start, "+", int64(limit)+1).Result()
	if err != nil {
		return nil, err
	}
	page := &UsagePage{NextID: afterID}
	if len(msgs) > limit {
		msgs, page.More = msgs[:limit], true
	}
	for _, m := range msgs {
		page.NextID = m.ID
		raw, _ := m.Values[usageField].(string)
		var r UsageRecord
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			log.Warn().Err(err).Str("node_id", nodeID).Str("entry_id", m.ID).Msg("skipping unreadable node usage record")
			continue
		}
		r.ID = m.ID
		page.Records = append(page.Records, r)
	}
	if afterID != "" {
		gap, err := l.gapAfter(ctx, key, afterID)
		if err != nil {
			return nil, err
		}
		page.GapUntil = gap
	}
	return page, nil
}

// gapAfter tells how far records after the cursor may have been trimmed.
// Trimming only drops the oldest records and nothing newer than the
// retention, so a gap needs an old cursor and a log that starts after it.
func (l *UsageLog) gapAfter(ctx context.Context, key, afterID string) (time.Time, error) {
	ms, seq, _ := parseStreamID(afterID)
	cutoff := l.now().Add(-l.retention)
	if !time.UnixMilli(ms).Before(cutoff) {
		return time.Time{}, nil
	}
	oldest, err := l.rdb.XRangeN(ctx, key, "-", "+", 1).Result()
	if err != nil {
		return time.Time{}, err
	}
	if len(oldest) == 0 {
		return cutoff, nil
	}
	fms, fseq, _ := parseStreamID(oldest[0].ID)
	if fms < ms || fms == ms && fseq <= seq {
		return time.Time{}, nil
	}
	return time.UnixMilli(fms), nil
}

func parseStreamID(id string) (ms, seq int64, err error) {
	a, b, ok := strings.Cut(id, "-")
	if ms, err = strconv.ParseInt(a, 10, 64); err != nil || ms < 0 {
		return 0, 0, fmt.Errorf("not a stream id")
	}
	if ok {
		if seq, err = strconv.ParseInt(b, 10, 64); err != nil || seq < 0 {
			return 0, 0, fmt.Errorf("not a stream id")
		}
	}
	return ms, seq, nil
}

const (
	usageKindChat     = "chat"
	usageKindStream   = "stream"
	usageKindJudge    = "judge"
	usageKindRollouts = "rollouts"
)

// nodeCall gathers what one call reports to the usage log; finish runs once
// the engine has answered, so a call that never reached it reads as an error.
type nodeCall struct {
	kind     string
	usage    llm.TokensUsage
	ttft     time.Duration
	err      error
	finished bool
}

func (c *nodeCall) finish(u llm.TokensUsage, ttft time.Duration, err error) {
	c.usage, c.ttft, c.err, c.finished = u, ttft, err, true
}

func (c *nodeCall) record(ctx context.Context, reqID, modelID string, started time.Time, took time.Duration, level uint32, cooldown time.Duration) UsageRecord {
	outcome, code := callOutcome(ctx, c.err)
	if !c.finished {
		outcome, code = usageOutcomeError, ""
	}
	return UsageRecord{
		RequestID:       reqID,
		StartedAt:       started.UTC(),
		DurationMs:      took.Milliseconds(),
		TTFTMs:          c.ttft.Milliseconds(),
		ModelID:         modelID,
		Kind:            c.kind,
		InputTokens:     int64(c.usage.Input),
		OutputTokens:    int64(c.usage.Output),
		CacheReadTokens: int64(c.usage.CacheRead),
		Outcome:         outcome,
		ErrorCode:       code,
		CapacityPercent: level,
		CooldownMs:      cooldown.Milliseconds(),
	}
}

const (
	usageOutcomeOK        = "ok"
	usageOutcomeError     = "error"
	usageOutcomeCancelled = "cancelled"
	usageOutcomeTimeout   = "timeout"
)

// callOutcome sorts a call's end the way the circuit breaker does: the
// caller leaving is not the node's fault, an upstream timeout is.
func callOutcome(ctx context.Context, err error) (outcome, code string) {
	switch {
	case err == nil:
		return usageOutcomeOK, ""
	case errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout) || errors.Is(err, context.DeadlineExceeded):
		return usageOutcomeTimeout, ""
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return usageOutcomeCancelled, ""
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode > 0 {
		return usageOutcomeError, fmt.Sprintf("http_%d", apiErr.StatusCode)
	}
	var decisionErr *adapters.OpenAIDecisionError
	if errors.As(err, &decisionErr) && decisionErr.Status > 0 {
		return usageOutcomeError, fmt.Sprintf("http_%d", decisionErr.Status)
	}
	return usageOutcomeError, ""
}
