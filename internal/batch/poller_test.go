package batch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
)

type fakePollerDB struct {
	batches       map[string]*proxydb.Batch
	items         map[string][]*proxydb.BatchItem
	leased        []*proxydb.Batch
	completed     map[string]string // id -> finalState
	completedDone map[string]int32
	completedFail map[string]int32
	expired       map[string]bool
	deletedOlder  time.Time
	completeErr   error
}

func newFakePollerDB() *fakePollerDB {
	return &fakePollerDB{
		batches:       make(map[string]*proxydb.Batch),
		items:         make(map[string][]*proxydb.BatchItem),
		completed:     make(map[string]string),
		completedDone: make(map[string]int32),
		completedFail: make(map[string]int32),
		expired:       make(map[string]bool),
	}
}

func (f *fakePollerDB) LeaseUnfinishedBatches(ctx context.Context, leaseDuration time.Duration, limit int) ([]*proxydb.Batch, error) {
	return f.leased, nil
}

func (f *fakePollerDB) UpdateBatchRunning(ctx context.Context, id string, total, done, failed int32) error {
	if b, ok := f.batches[id]; ok {
		b.State = "RUNNING"
		b.TotalCount = total
		b.DoneCount = done
		b.FailedCount = failed
	}
	return nil
}

func (f *fakePollerDB) CompleteBatch(ctx context.Context, id string, finalState string, doneCount, failedCount int32, completedAt time.Time, items []*proxydb.BatchItem) error {
	if f.completeErr != nil {
		return f.completeErr
	}
	f.completed[id] = finalState
	f.completedDone[id] = doneCount
	f.completedFail[id] = failedCount
	if b, ok := f.batches[id]; ok {
		b.State = finalState
		b.DoneCount = doneCount
		b.FailedCount = failedCount
		b.CompletedAt = &completedAt
	}
	f.items[id] = items
	return nil
}

func (f *fakePollerDB) ExpireBatch(ctx context.Context, id string, expiredAt time.Time) error {
	f.expired[id] = true
	if b, ok := f.batches[id]; ok {
		b.State = "EXPIRED"
		b.CompletedAt = &expiredAt
	}
	return nil
}

func (f *fakePollerDB) ListAllBatchItems(ctx context.Context, batchID string) ([]*proxydb.BatchItem, error) {
	return f.items[batchID], nil
}

func (f *fakePollerDB) DeleteBatchesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	f.deletedOlder = cutoff
	return 3, nil
}

type fakeUsageTracker struct {
	recordedEvents []recordedUsage
}

type recordedUsage struct {
	userID       string
	actionID     string
	vendor       string
	model        string
	inputTokens  int
	outputTokens int
	status       usagev1.Status
	meta         map[string]string
}

func (t *fakeUsageTracker) RecordUsage(userID, actionID, vendor, model string, inputTokens, outputTokens int, durationMs int64, status usagev1.Status, meta map[string]string) error {
	t.recordedEvents = append(t.recordedEvents, recordedUsage{
		userID:       userID,
		actionID:     actionID,
		vendor:       vendor,
		model:        model,
		inputTokens:  inputTokens,
		outputTokens: outputTokens,
		status:       status,
		meta:         meta,
	})
	return nil
}

type fakeThrottle struct {
	router.NoopThrottle
	chargedTokens map[string]int64
}

func (th *fakeThrottle) ChargeDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string, tokens int64) {
	if th.chargedTokens == nil {
		th.chargedTokens = make(map[string]int64)
	}
	th.chargedTokens[m.ID+":"+keyName+":"+userID] += tokens
}

type fakeBatchAdapter struct {
	status  *llm.BatchJobStatus
	results []*llm.BatchItemResult
}

func (a *fakeBatchAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	return nil, nil
}
func (a *fakeBatchAdapter) SubmitBatch(ctx context.Context, batchID string, items []*llm.BatchSubmitItem) (string, error) {
	return "job-1", nil
}
func (a *fakeBatchAdapter) GetBatch(ctx context.Context, vendorJobID string) (*llm.BatchJobStatus, error) {
	return a.status, nil
}
func (a *fakeBatchAdapter) CancelBatch(ctx context.Context, vendorJobID string) error {
	return nil
}
func (a *fakeBatchAdapter) FetchBatchResults(ctx context.Context, vendorJobID string, items []*llm.BatchSubmitItem) ([]*llm.BatchItemResult, error) {
	return a.results, nil
}

type fixedSnapshot struct {
	snap *registry.Snapshot
}

func (f fixedSnapshot) Snapshot() *registry.Snapshot { return f.snap }

func TestPoller_ProcessSucceededBatch(t *testing.T) {
	adapter := &fakeBatchAdapter{
		status: &llm.BatchJobStatus{
			State:     "SUCCEEDED",
			DoneCount: 2,
		},
		results: []*llm.BatchItemResult{
			{
				CustomID: "c1",
				Response: &llm.Response{
					Choices: []*llm.Message{{Text: "choice 1"}},
					Usage:   llm.TokensUsage{Input: 100, Output: 50},
				},
			},
			{
				CustomID: "c2",
				Error: &llm.BatchItemError{
					Code:    9,
					Reason:  "output_truncated",
					Message: "max tokens hit",
				},
			},
		},
	}

	model := &proxydb.Model{
		ID:                  "gemini-flash",
		Vendor:              registry.VendorGoogle,
		Capabilities:        []string{registry.CapabilityBatch},
		PriceInPerMtok:      1.0,
		PriceOutPerMtok:     4.0,
		PriceInBatchPerMtok: 0.5, // 50% discount
		PriceOutBatchPerMtok: 2.0,
		Enabled:             true,
	}

	snap := registry.NewSnapshotForTest([]*proxydb.Model{model}, nil, nil, nil)
	snap.SetAdapter("key-id-1", "gemini-flash", adapter)

	db := newFakePollerDB()
	batchRecord := &proxydb.Batch{
		ID:            "b-100",
		KeyName:       "client-app-label",
		APIKeyID:      "key-id-1",
		Model:         "gemini-flash",
		VendorJobName: "vendor-job-100",
		State:         "RUNNING",
		Attributes:    map[string]string{"env": "prod", "user_id": "u-42"},
		ActionID:      "test.batch_action",
	}
	db.batches["b-100"] = batchRecord
	db.leased = []*proxydb.Batch{batchRecord}
	db.items["b-100"] = []*proxydb.BatchItem{
		{BatchID: "b-100", CustomID: "c1"},
		{BatchID: "b-100", CustomID: "c2"},
	}

	tracker := &fakeUsageTracker{}
	th := &fakeThrottle{}

	poller := NewPoller(db, fixedSnapshot{snap}, th, tracker, 1*time.Second, 7*24*time.Hour)
	poller.PollOnce(context.Background())

	// 1. Verify batch completed in DB
	if db.completed["b-100"] != "SUCCEEDED" {
		t.Fatalf("expected batch state SUCCEEDED, got %s", db.completed["b-100"])
	}
	if db.completedDone["b-100"] != 1 || db.completedFail["b-100"] != 1 {
		t.Fatalf("expected 1 done 1 fail, got %d done, %d fail", db.completedDone["b-100"], db.completedFail["b-100"])
	}

	// 2. Verify usage events emitted
	if len(tracker.recordedEvents) != 2 {
		t.Fatalf("expected 2 usage events, got %d", len(tracker.recordedEvents))
	}
	ev1 := tracker.recordedEvents[0]
	if ev1.userID != "u-42" || ev1.actionID != "test.batch_action" || ev1.inputTokens != 100 || ev1.outputTokens != 50 {
		t.Fatalf("unexpected event 1: %+v", ev1)
	}
	if ev1.meta["batch_id"] != "b-100" || ev1.meta["custom_id"] != "c1" || ev1.meta["env"] != "prod" {
		t.Fatalf("unexpected event 1 meta: %+v", ev1.meta)
	}
	if ev1.meta["price_in_batch_per_mtok"] != "0.5" || ev1.meta["price_out_batch_per_mtok"] != "2" {
		t.Fatalf("unexpected batch pricing in meta: %+v", ev1.meta)
	}

	ev2 := tracker.recordedEvents[1]
	if ev2.status != usagev1.Status_STATUS_ERROR || ev2.meta["custom_id"] != "c2" || ev2.meta["error_reason"] != "output_truncated" {
		t.Fatalf("unexpected event 2: %+v", ev2)
	}

	// 3. Verify tokens charged to daily budget
	charged := th.chargedTokens["gemini-flash:client-app-label:u-42"]
	if charged != 150 { // 100 in + 50 out
		t.Fatalf("expected 150 tokens charged to daily budget, got %d", charged)
	}
}

func TestPoller_ProcessExpiredBatch(t *testing.T) {
	adapter := &fakeBatchAdapter{
		status: &llm.BatchJobStatus{
			State: "EXPIRED",
		},
	}
	model := &proxydb.Model{
		ID:           "gemini-flash",
		Vendor:       registry.VendorGoogle,
		Capabilities: []string{registry.CapabilityBatch},
		Enabled:      true,
	}

	snap := registry.NewSnapshotForTest([]*proxydb.Model{model}, nil, nil, nil)
	snap.SetAdapter("key-id-2", "gemini-flash", adapter)

	db := newFakePollerDB()
	batchRecord := &proxydb.Batch{
		ID:            "b-exp",
		KeyName:       "client-app-label-2",
		APIKeyID:      "key-id-2",
		Model:         "gemini-flash",
		VendorJobName: "vendor-job-exp",
		State:         "RUNNING",
	}
	db.batches["b-exp"] = batchRecord
	db.leased = []*proxydb.Batch{batchRecord}

	poller := NewPoller(db, fixedSnapshot{snap}, nil, nil, 1*time.Second, 7*24*time.Hour)
	poller.PollOnce(context.Background())

	if !db.expired["b-exp"] {
		t.Fatal("expected batch to be marked EXPIRED in DB")
	}
}

func TestPoller_ProcessCancelledBatch_FetchesLateResults(t *testing.T) {
	adapter := &fakeBatchAdapter{
		status: &llm.BatchJobStatus{
			State: "CANCELLED",
		},
		results: []*llm.BatchItemResult{
			{
				CustomID: "c1",
				Response: &llm.Response{
					Choices: []*llm.Message{{Text: "late finished item"}},
					Usage:   llm.TokensUsage{Input: 100, Output: 50},
				},
			},
			{
				CustomID: "c2", // unfinished when cancelled
			},
		},
	}
	model := &proxydb.Model{
		ID:                  "gemini-flash",
		Vendor:              registry.VendorGoogle,
		Capabilities:        []string{registry.CapabilityBatch},
		PriceInBatchPerMtok: 0.5,
		PriceOutBatchPerMtok: 2.0,
		Enabled:             true,
	}

	snap := registry.NewSnapshotForTest([]*proxydb.Model{model}, nil, nil, nil)
	snap.SetAdapter("key-id-cancel", "gemini-flash", adapter)

	db := newFakePollerDB()
	batchRecord := &proxydb.Batch{
		ID:            "b-cancel-late",
		KeyName:       "client-app",
		APIKeyID:      "key-id-cancel",
		Model:         "gemini-flash",
		VendorJobName: "vendor-job-cancel",
		State:         "RUNNING",
		Attributes:    map[string]string{"user_id": "u-99"},
	}
	db.batches["b-cancel-late"] = batchRecord
	db.leased = []*proxydb.Batch{batchRecord}
	db.items["b-cancel-late"] = []*proxydb.BatchItem{
		{BatchID: "b-cancel-late", CustomID: "c1"},
		{BatchID: "b-cancel-late", CustomID: "c2"},
	}

	tracker := &fakeUsageTracker{}
	th := &fakeThrottle{}

	poller := NewPoller(db, fixedSnapshot{snap}, th, tracker, 1*time.Second, 7*24*time.Hour)
	poller.PollOnce(context.Background())

	if db.completed["b-cancel-late"] != "CANCELLED" {
		t.Fatalf("expected batch final state CANCELLED, got %s", db.completed["b-cancel-late"])
	}
	if db.completedDone["b-cancel-late"] != 1 || db.completedFail["b-cancel-late"] != 1 {
		t.Fatalf("expected 1 done 1 failed, got %d done, %d fail", db.completedDone["b-cancel-late"], db.completedFail["b-cancel-late"])
	}

	// Verify finished item usage recorded and unfinished item recorded as error
	if len(tracker.recordedEvents) != 2 {
		t.Fatalf("expected 2 usage events, got %d", len(tracker.recordedEvents))
	}
	if tracker.recordedEvents[0].status != usagev1.Status_STATUS_SUCCESS || tracker.recordedEvents[0].inputTokens != 100 {
		t.Fatalf("unexpected event 0: %+v", tracker.recordedEvents[0])
	}
	if tracker.recordedEvents[1].status != usagev1.Status_STATUS_ERROR || tracker.recordedEvents[1].meta["error_reason"] != "batch_cancelled" {
		t.Fatalf("unexpected event 1: %+v", tracker.recordedEvents[1])
	}

	// Verify budget charged for finished item
	if th.chargedTokens["gemini-flash:client-app:u-99"] != 150 {
		t.Fatalf("expected 150 tokens charged, got %d", th.chargedTokens["gemini-flash:client-app:u-99"])
	}
}

func TestPoller_DBCommitFailureDoesNotEmitUsage(t *testing.T) {
	adapter := &fakeBatchAdapter{
		status: &llm.BatchJobStatus{
			State:     "SUCCEEDED",
			DoneCount: 1,
		},
		results: []*llm.BatchItemResult{
			{
				CustomID: "c1",
				Response: &llm.Response{
					Choices: []*llm.Message{{Text: "choice 1"}},
					Usage:   llm.TokensUsage{Input: 100, Output: 50},
				},
			},
		},
	}
	model := &proxydb.Model{
		ID:           "gemini-flash",
		Vendor:       registry.VendorGoogle,
		Capabilities: []string{registry.CapabilityBatch},
		Enabled:      true,
	}
	snap := registry.NewSnapshotForTest([]*proxydb.Model{model}, nil, nil, nil)
	snap.SetAdapter("key-id-fail", "gemini-flash", adapter)

	db := newFakePollerDB()
	db.completeErr = errors.New("simulated db commit error")

	batchRecord := &proxydb.Batch{
		ID:            "b-fail",
		KeyName:       "client-app",
		APIKeyID:      "key-id-fail",
		Model:         "gemini-flash",
		VendorJobName: "vendor-job-fail",
		State:         "RUNNING",
	}
	db.batches["b-fail"] = batchRecord
	db.leased = []*proxydb.Batch{batchRecord}
	db.items["b-fail"] = []*proxydb.BatchItem{{BatchID: "b-fail", CustomID: "c1"}}

	tracker := &fakeUsageTracker{}
	th := &fakeThrottle{}

	poller := NewPoller(db, fixedSnapshot{snap}, th, tracker, 1*time.Second, 7*24*time.Hour)
	poller.PollOnce(context.Background())

	if len(tracker.recordedEvents) != 0 {
		t.Fatalf("expected 0 usage events on db commit failure, got %d", len(tracker.recordedEvents))
	}
	if len(th.chargedTokens) != 0 {
		t.Fatalf("expected 0 tokens charged on db commit failure, got %d", len(th.chargedTokens))
	}
}

func TestPoller_Cleanup(t *testing.T) {
	db := newFakePollerDB()
	poller := NewPoller(db, fixedSnapshot{nil}, nil, nil, 1*time.Second, 7*24*time.Hour)

	deleted, err := poller.CleanupOnce(context.Background())
	if err != nil {
		t.Fatalf("CleanupOnce failed: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("expected 3 deleted, got %d", deleted)
	}
	if db.deletedOlder.IsZero() {
		t.Fatal("expected cutoff time passed to DeleteBatchesOlderThan")
	}
}
