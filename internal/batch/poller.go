package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/panicsafe"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
)

const (
	defaultLeaseDuration = 5 * time.Minute
	leaseBatchLimit      = 20
)

// SnapshotProvider provides the current routing snapshot.
type SnapshotProvider interface {
	Snapshot() *registry.Snapshot
}

// PollerDB defines the DB operations needed by the poller.
type PollerDB interface {
	LeaseUnfinishedBatches(ctx context.Context, leaseDuration time.Duration, limit int) ([]*proxydb.Batch, error)
	UpdateBatchRunning(ctx context.Context, id string, total, done, failed int32) error
	CompleteBatch(ctx context.Context, id string, finalState string, doneCount, failedCount int32, completedAt time.Time, items []*proxydb.BatchItem) error
	ExpireBatch(ctx context.Context, id string, expiredAt time.Time) error
	ListAllBatchItems(ctx context.Context, batchID string) ([]*proxydb.BatchItem, error)
	DeleteBatchesOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// Poller polls vendor APIs for batch job status and cleans up expired results.
type Poller struct {
	db              PollerDB
	snapshots       SnapshotProvider
	throttle        router.Throttle
	tracker         llm.UsageTracker
	pollInterval    time.Duration
	retentionPeriod time.Duration
	leaseDuration   time.Duration

	mu     sync.Mutex
	stopCh chan struct{}
}

func NewPoller(
	db PollerDB,
	snapshots SnapshotProvider,
	throttle router.Throttle,
	tracker llm.UsageTracker,
	pollInterval, retentionPeriod time.Duration,
) *Poller {
	if pollInterval <= 0 {
		pollInterval = 60 * time.Second
	}
	if retentionPeriod <= 0 {
		retentionPeriod = 7 * 24 * time.Hour
	}
	return &Poller{
		db:              db,
		snapshots:       snapshots,
		throttle:        throttle,
		tracker:         tracker,
		pollInterval:    pollInterval,
		retentionPeriod: retentionPeriod,
		leaseDuration:   defaultLeaseDuration,
	}
}

// Start launches background polling and cleanup loops until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	p.mu.Lock()
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	go func() {
		defer panicsafe.Guard("batch poller loop")
		pollTicker := time.NewTicker(p.pollInterval)
		defer pollTicker.Stop()

		cleanupTicker := time.NewTicker(1 * time.Hour)
		defer cleanupTicker.Stop()

		p.PollOnce(ctx) // Pick up unleased batches immediately after deploy without waiting for ticker

		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stopCh:
				return
			case <-pollTicker.C:
				p.PollOnce(ctx)
			case <-cleanupTicker.C:
				p.CleanupOnce(ctx)
			}
		}
	}()
}

// Stop stops the poller.
func (p *Poller) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopCh != nil {
		close(p.stopCh)
		p.stopCh = nil
	}
}

// PollOnce executes one round of leased batch polling.
func (p *Poller) PollOnce(ctx context.Context) {
	leaseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	batches, err := p.db.LeaseUnfinishedBatches(leaseCtx, p.leaseDuration, leaseBatchLimit)
	cancel()
	if err != nil {
		log.Error().Err(err).Msg("batch poller: failed to lease batches")
		return
	}

	for _, b := range batches {
		if err := p.processBatch(ctx, b); err != nil {
			log.Warn().Err(err).Str("batch_id", b.ID).Str("vendor_job", b.VendorJobName).Msg("batch poller: process batch error")
		}
	}
}

// CleanupOnce executes one retention cleanup run.
func (p *Poller) CleanupOnce(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-p.retentionPeriod)
	deleted, err := p.db.DeleteBatchesOlderThan(ctx, cutoff)
	if err != nil {
		log.Error().Err(err).Msg("batch poller: failed to delete old batches")
		return 0, err
	}
	if deleted > 0 {
		log.Info().Int64("deleted_batches", deleted).Msg("batch poller: cleaned up old batch results")
	}
	return deleted, nil
}

func (p *Poller) processBatch(ctx context.Context, b *proxydb.Batch) error {
	snap := p.snapshots.Snapshot()
	if snap == nil {
		return fmt.Errorf("no routing snapshot available")
	}

	m, ok := snap.Models[b.Model]
	if !ok {
		return fmt.Errorf("model %q not found in routing snapshot", b.Model)
	}

	adapter, err := snap.Adapter(b.APIKeyID, m)
	if err != nil {
		return fmt.Errorf("resolve adapter for key ID %q model %q: %w", b.APIKeyID, m.ID, err)
	}

	batchAdapter, ok := adapter.(llm.BatchAdapter)
	if !ok {
		return fmt.Errorf("adapter for model %q does not support batch", m.ID)
	}

	batchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	status, err := batchAdapter.GetBatch(batchCtx, b.VendorJobName)
	if err != nil {
		return fmt.Errorf("get batch %s: %w", b.VendorJobName, err)
	}

	switch status.State {
	case "PENDING", "RUNNING":
		return p.db.UpdateBatchRunning(ctx, b.ID, status.TotalCount, status.DoneCount, status.FailedCount)

	case "EXPIRED":
		return p.db.ExpireBatch(ctx, b.ID, time.Now().UTC())

	case "CANCELLED", "SUCCEEDED", "FAILED":
		return p.completeBatch(batchCtx, b, m, batchAdapter, status)

	default:
		return nil
	}
}

func (p *Poller) completeBatch(
	ctx context.Context,
	b *proxydb.Batch,
	m *proxydb.Model,
	adapter llm.BatchAdapter,
	status *llm.BatchJobStatus,
) error {
	dbItems, err := p.db.ListAllBatchItems(ctx, b.ID)
	if err != nil {
		return fmt.Errorf("list batch items for %s: %w", b.ID, err)
	}

	submitItems := make([]*llm.BatchSubmitItem, len(dbItems))
	for i, it := range dbItems {
		var schema *llm.ResponseSchema
		if len(it.ResponseSchema) > 0 {
			_ = json.Unmarshal(it.ResponseSchema, &schema)
		}
		chat := llm.NewChatContext()
		chat.SetResponseSchema(schema)
		submitItems[i] = &llm.BatchSubmitItem{
			CustomID: it.CustomID,
			Chat:     chat,
		}
	}

	results, err := adapter.FetchBatchResults(ctx, b.VendorJobName, submitItems)
	if err != nil {
		return fmt.Errorf("fetch batch results for %s: %w", b.VendorJobName, err)
	}

	var doneCount, failedCount int32
	var totalTokens int64
	itemsToUpdate := make([]*proxydb.BatchItem, len(results))

	type pendingUsage struct {
		customID  string
		inTokens  int
		outTokens int
		status    usagev1.Status
		errReason string
	}
	var usageQueue []pendingUsage

	for i, res := range results {
		itemUpdate := &proxydb.BatchItem{
			BatchID:  b.ID,
			CustomID: res.CustomID,
		}

		if res.Error != nil {
			failedCount++
			itemUpdate.Status = "FAILED"
			itemUpdate.Error = res.Error.JSON()
			usageQueue = append(usageQueue, pendingUsage{
				customID:  res.CustomID,
				status:    usagev1.Status_STATUS_ERROR,
				errReason: res.Error.Reason,
			})
		} else if res.Response != nil {
			doneCount++
			itemUpdate.Status = "SUCCEEDED"
			itemUpdate.InputTokens = int64(res.Response.Usage.Input)
			itemUpdate.OutputTokens = int64(res.Response.Usage.Output)

			var choices []string
			for _, c := range res.Response.Choices {
				choices = append(choices, c.Text)
			}
			respProto := &pb.GenerateTextResponse{
				Choices: choices,
				Usage: &pb.TokensUsage{
					Input:  int64(res.Response.Usage.Input),
					Output: int64(res.Response.Usage.Output),
				},
				ResolvedModel:  m.ID,
				ResolvedVendor: m.Vendor,
				MatchedRule:    b.Attributes["matched_rule"],
			}
			resBytes, _ := protojson.Marshal(respProto)
			itemUpdate.Result = resBytes

			itemTokens := int64(res.Response.Usage.Input + res.Response.Usage.Output)
			totalTokens += itemTokens
			usageQueue = append(usageQueue, pendingUsage{
				customID:  res.CustomID,
				inTokens:  res.Response.Usage.Input,
				outTokens: res.Response.Usage.Output,
				status:    usagev1.Status_STATUS_SUCCESS,
			})
		} else {
			failedCount++
			itemUpdate.Status = "FAILED"
			code := codes.Internal
			reason := "vendor_error"
			msg := "no response received from vendor"
			if status.State == "CANCELLED" {
				code = codes.Canceled
				reason = "batch_cancelled"
				msg = "batch was cancelled"
			}
			itemErr := llm.NewBatchItemError(code, reason, msg)
			itemUpdate.Error = itemErr.JSON()
			usageQueue = append(usageQueue, pendingUsage{
				customID:  res.CustomID,
				status:    usagev1.Status_STATUS_ERROR,
				errReason: itemErr.Reason,
			})
		}

		itemsToUpdate[i] = itemUpdate
	}

	finalState := "SUCCEEDED"
	if failedCount > 0 && doneCount == 0 {
		finalState = "FAILED"
	}
	if status.State == "CANCELLED" {
		finalState = "CANCELLED"
	}
	completedAt := time.Now().UTC()
	if status.CompletedAt != nil {
		completedAt = *status.CompletedAt
	}

	// Commit DB state before external effects so failures don't record usage twice
	if err := p.db.CompleteBatch(ctx, b.ID, finalState, doneCount, failedCount, completedAt, itemsToUpdate); err != nil {
		return err
	}

	for _, u := range usageQueue {
		p.emitUsage(b, m, u.customID, u.inTokens, u.outTokens, u.status, u.errReason)
	}

	if p.throttle != nil && totalTokens > 0 {
		userID := router.UserID(b.Attributes, b.KeyName)
		p.throttle.ChargeDailyBudget(ctx, m, b.KeyName, userID, totalTokens)
	}

	return nil
}

func (p *Poller) emitUsage(
	b *proxydb.Batch,
	m *proxydb.Model,
	customID string,
	inTokens, outTokens int,
	status usagev1.Status,
	errReason string,
) {
	if p.tracker == nil {
		return
	}

	meta := make(map[string]string, len(b.Attributes)+6)
	for k, v := range b.Attributes {
		meta[k] = v
	}
	meta["batch_id"] = b.ID
	meta["custom_id"] = customID
	if errReason != "" {
		meta["error_reason"] = errReason
	}
	priceIn := m.BatchPriceInPerMtok()
	priceOut := m.BatchPriceOutPerMtok()
	meta["price_in_batch_per_mtok"] = strconv.FormatFloat(priceIn, 'f', -1, 64)
	meta["price_out_batch_per_mtok"] = strconv.FormatFloat(priceOut, 'f', -1, 64)
	meta["price_in_per_mtok"] = strconv.FormatFloat(priceIn, 'f', -1, 64)
	meta["price_out_per_mtok"] = strconv.FormatFloat(priceOut, 'f', -1, 64)
	if inTokens > 0 || outTokens > 0 {
		cost := (float64(inTokens)*priceIn + float64(outTokens)*priceOut) / 1_000_000.0
		meta["cost_usd"] = strconv.FormatFloat(cost, 'f', -1, 64)
	}

	userID := router.UserID(b.Attributes, b.KeyName)
	actionID := b.ActionID
	if actionID == "" {
		actionID = "llmproxy.generate_text"
	}

	if err := p.tracker.RecordUsage(
		userID,
		actionID,
		m.Vendor,
		m.ID,
		inTokens,
		outTokens,
		0,
		status,
		meta,
	); err != nil {
		log.Error().Err(err).Str("batch_id", b.ID).Str("custom_id", customID).Msg("failed to record batch item usage")
	}
}
