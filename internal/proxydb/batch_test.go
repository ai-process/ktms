package proxydb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ai-process/llm-proxy/migrations"
)

const testAPIKeyID = "11111111-1111-1111-1111-111111111111"

func setupTestDB(t *testing.T) *DB {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://gen:gen@localhost:5442/gen_test?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Skipf("cannot parse database config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Skipf("cannot connect to test postgres: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		t.Skipf("cannot ping test postgres: %v", err)
	}

	// Apply migrations
	if err := Migrate(ctx, pool, migrations.FS()); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Insert test API key if not exists
	_, err = pool.Exec(ctx, `
		INSERT INTO api_key (id, name, key_lookup, key_hash, scopes, created_at)
		VALUES ($1, 'client-test', 'lookup-123', 'hash-123', ARRAY['generate'], NOW())
		ON CONFLICT (id) DO NOTHING
	`, testAPIKeyID)
	if err != nil {
		t.Fatalf("failed to insert test api_key: %v", err)
	}

	t.Cleanup(func() {
		// Clean up batches created during tests
		_, _ = pool.Exec(context.Background(), `DELETE FROM batch WHERE api_key_id = $1`, testAPIKeyID)
		pool.Close()
	})

	return New(pool)
}

func TestBatchStorageAndIdempotency(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	batchID := uuid.NewString()
	clientBatchID := "client_test_" + time.Now().Format("20060102150405.000000")

	b := &Batch{
		ID:            batchID,
		ClientBatchID: clientBatchID,
		APIKeyID:      testAPIKeyID,
		KeyName:       "client-test",
		Model:         "gemini-flash",
		VendorJobName: "vendor-123",
		State:         "PENDING",
		TotalCount:    2,
		Attributes:    map[string]string{"env": "test"},
		ActionID:      "action-1",
		CreatedAt:     time.Now().UTC(),
	}

	items := []*BatchItem{
		{
			BatchID:        batchID,
			CustomID:       "cust-1",
			Position:       0,
			Status:         "PENDING",
			ResponseSchema: []byte(`{"type":"object"}`),
		},
		{
			BatchID:        batchID,
			CustomID:       "cust-2",
			Position:       1,
			Status:         "PENDING",
		},
	}

	// 1. Create batch
	created, err := db.CreateBatch(ctx, b, items)
	if err != nil {
		t.Fatalf("CreateBatch failed: %v", err)
	}
	if created.ID != batchID {
		t.Fatalf("expected ID %s, got %s", batchID, created.ID)
	}

	// 2. Idempotent create with same client_batch_id returns existing batch
	duplicateBatch := &Batch{
		ID:            uuid.NewString(),
		ClientBatchID: clientBatchID,
		APIKeyID:      testAPIKeyID,
		KeyName:       "client-test",
		Model:         "gemini-flash",
		State:         "PENDING",
		CreatedAt:     time.Now().UTC(),
	}
	idempotentRes, err := db.CreateBatch(ctx, duplicateBatch, nil)
	if err != nil {
		t.Fatalf("idempotent CreateBatch failed: %v", err)
	}
	if idempotentRes.ID != batchID {
		t.Fatalf("expected original batch ID %s, got %s", batchID, idempotentRes.ID)
	}

	// 3. Get batch by ID and API key
	fetched, err := db.GetBatch(ctx, batchID, testAPIKeyID)
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}
	if fetched.State != "PENDING" || fetched.TotalCount != 2 {
		t.Fatalf("unexpected fetched batch: %+v", fetched)
	}

	// 4. UpdateBatchRunning
	if err := db.UpdateBatchRunning(ctx, batchID, 2, 1, 0); err != nil {
		t.Fatalf("UpdateBatchRunning failed: %v", err)
	}
	runningBatch, err := db.GetBatchByID(ctx, batchID)
	if err != nil {
		t.Fatalf("GetBatchByID failed: %v", err)
	}
	if runningBatch.State != "RUNNING" || runningBatch.DoneCount != 1 {
		t.Fatalf("unexpected running state: %+v", runningBatch)
	}

	// Calling UpdateBatchRunning again on RUNNING batch should also succeed and update counts
	if err := db.UpdateBatchRunning(ctx, batchID, 2, 2, 0); err != nil {
		t.Fatalf("second UpdateBatchRunning on RUNNING batch failed: %v", err)
	}
	runningBatch2, err := db.GetBatchByID(ctx, batchID)
	if err != nil || runningBatch2.DoneCount != 2 {
		t.Fatalf("expected done count 2 after second UpdateBatchRunning, got %+v", runningBatch2)
	}

	// 5. Complete batch
	resultBytes := []byte(`{"choices":["output text"]}`)
	errBytes, _ := json.Marshal(map[string]any{"code": 9, "reason": "output_truncated"})
	completedItems := []*BatchItem{
		{
			BatchID:      batchID,
			CustomID:     "cust-1",
			Status:       "SUCCEEDED",
			Result:       resultBytes,
			InputTokens:  10,
			OutputTokens: 20,
		},
		{
			BatchID:  batchID,
			CustomID: "cust-2",
			Status:   "FAILED",
			Error:    errBytes,
		},
	}
	now := time.Now().UTC()
	if err := db.CompleteBatch(ctx, batchID, "SUCCEEDED", 1, 1, now, completedItems); err != nil {
		t.Fatalf("CompleteBatch failed: %v", err)
	}

	// 6. ListBatchItems verification
	storedItems, err := db.ListBatchItems(ctx, batchID, 10, 0)
	if err != nil {
		t.Fatalf("ListBatchItems failed: %v", err)
	}
	if len(storedItems) != 2 {
		t.Fatalf("expected 2 items, got %d", len(storedItems))
	}
	if storedItems[0].Status != "SUCCEEDED" || storedItems[0].InputTokens != 10 {
		t.Fatalf("unexpected item 0 status or tokens: %+v", storedItems[0])
	}
	var resObj struct {
		Choices []string `json:"choices"`
	}
	if err := json.Unmarshal(storedItems[0].Result, &resObj); err != nil || len(resObj.Choices) != 1 || resObj.Choices[0] != "output text" {
		t.Fatalf("unexpected item 0 result: %s", string(storedItems[0].Result))
	}

	if storedItems[1].Status != "FAILED" {
		t.Fatalf("unexpected item 1 status: %+v", storedItems[1])
	}
	var errObj struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(storedItems[1].Error, &errObj); err != nil || errObj.Reason != "output_truncated" {
		t.Fatalf("unexpected item 1 error: %s", string(storedItems[1].Error))
	}

	// 7. Verify terminal state guards: CompleteBatch, ExpireBatch cannot mutate SUCCEEDED batch
	if err := db.CompleteBatch(ctx, batchID, "FAILED", 0, 2, now, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound when trying to complete already SUCCEEDED batch, got %v", err)
	}
	if err := db.ExpireBatch(ctx, batchID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound when trying to expire already SUCCEEDED batch, got %v", err)
	}
	finalBatch, _ := db.GetBatchByID(ctx, batchID)
	if finalBatch.State != "SUCCEEDED" {
		t.Fatalf("batch state should have remained SUCCEEDED, got %s", finalBatch.State)
	}
}

func TestBatchLeasingSkipLocked(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	b1ID := uuid.NewString()
	b2ID := uuid.NewString()

	_, err := db.CreateBatch(ctx, &Batch{
		ID:        b1ID,
		APIKeyID:  testAPIKeyID,
		KeyName:   "client-test",
		Model:     "gemini-flash",
		State:     "PENDING",
		CreatedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatalf("create b1 failed: %v", err)
	}

	_, err = db.CreateBatch(ctx, &Batch{
		ID:        b2ID,
		APIKeyID:  testAPIKeyID,
		KeyName:   "client-test",
		Model:     "gemini-flash",
		State:     "RUNNING",
		CreatedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatalf("create b2 failed: %v", err)
	}

	// Lease both batches for 10 seconds
	leased, err := db.LeaseUnfinishedBatches(ctx, 10*time.Second, 10)
	if err != nil {
		t.Fatalf("LeaseUnfinishedBatches failed: %v", err)
	}
	if len(leased) < 2 {
		t.Fatalf("expected at least 2 leased batches, got %d", len(leased))
	}

	// Verify that immediately trying to lease again returns NO batches (since leased_until is in future)
	leasedAgain, err := db.LeaseUnfinishedBatches(ctx, 10*time.Second, 10)
	if err != nil {
		t.Fatalf("second lease failed: %v", err)
	}
	for _, b := range leasedAgain {
		if b.ID == b1ID || b.ID == b2ID {
			t.Fatalf("batch %s should not have been leased again while leased_until is active", b.ID)
		}
	}

	// Concurrent leasing test: run multiple goroutines calling LeaseUnfinishedBatches
	// Ensure no batch is leased twice across workers
	var wg sync.WaitGroup
	seenLeases := sync.Map{}
	duplicateFound := false

	for worker := 0; worker < 5; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wLeased, _ := db.LeaseUnfinishedBatches(ctx, 10*time.Second, 5)
			for _, b := range wLeased {
				if _, loaded := seenLeases.LoadOrStore(b.ID, true); loaded {
					duplicateFound = true
				}
			}
		}()
	}
	wg.Wait()

	if duplicateFound {
		t.Fatal("concurrent lease allocated the same batch to multiple workers")
	}
}

func TestBatchExpire(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	expireID := uuid.NewString()
	_, err := db.CreateBatch(ctx, &Batch{
		ID:        expireID,
		APIKeyID:  testAPIKeyID,
		KeyName:   "client-test",
		Model:     "gemini-flash",
		State:     "RUNNING",
		CreatedAt: time.Now().UTC(),
	}, []*BatchItem{
		{BatchID: expireID, CustomID: "c1", Status: "PENDING"},
	})
	if err != nil {
		t.Fatalf("create expire batch failed: %v", err)
	}

	if err := db.ExpireBatch(ctx, expireID, time.Now().UTC()); err != nil {
		t.Fatalf("ExpireBatch failed: %v", err)
	}
	expBatch, _ := db.GetBatchByID(ctx, expireID)
	if expBatch.State != "EXPIRED" {
		t.Fatalf("expected EXPIRED, got %s", expBatch.State)
	}
	expItems, _ := db.ListBatchItems(ctx, expireID, 10, 0)
	if expItems[0].Status != "FAILED" {
		t.Fatalf("expected pending item to be marked FAILED on expire, got %s", expItems[0].Status)
	}
	var expErrObj struct {
		Code    int    `json:"code"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(expItems[0].Error, &expErrObj); err != nil || expErrObj.Code != 4 || expErrObj.Reason != "batch_expired" {
		t.Fatalf("unexpected expire error payload: %s", string(expItems[0].Error))
	}
}

func TestBatchRetentionCleanup(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	oldID := uuid.NewString()
	past := time.Now().UTC().Add(-10 * 24 * time.Hour) // 10 days ago
	_, err := db.CreateBatch(ctx, &Batch{
		ID:          oldID,
		APIKeyID:    testAPIKeyID,
		KeyName:     "client-test",
		Model:       "gemini-flash",
		State:       "SUCCEEDED",
		CreatedAt:   past,
		CompletedAt: &past,
	}, nil)
	if err != nil {
		t.Fatalf("create old batch failed: %v", err)
	}

	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	deleted, err := db.DeleteBatchesOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteBatchesOlderThan failed: %v", err)
	}
	if deleted == 0 {
		t.Fatal("expected at least 1 deleted batch")
	}

	_, err = db.GetBatchByID(ctx, oldID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for deleted batch, got %v", err)
	}
}

func TestBatchCreate_ConcurrentIdempotentInsert(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	clientBatchID := "concurrent-race-" + uuid.NewString()

	const concurrency = 10
	var wg sync.WaitGroup
	results := make([]*Batch, concurrency)
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b := &Batch{
				ID:            uuid.NewString(),
				ClientBatchID: clientBatchID,
				APIKeyID:      testAPIKeyID,
				KeyName:       "test-key",
				Model:         "gemini-flash",
				VendorJobName: "vendor-job-" + uuid.NewString(),
				State:         "PENDING",
				TotalCount:    1,
				CreatedAt:     time.Now().UTC(),
			}
			res, err := db.CreateBatch(ctx, b, []*BatchItem{{
				BatchID:  b.ID,
				CustomID: "c1",
				Status:   "PENDING",
			}})
			results[idx] = res
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	var firstID string
	for i := 0; i < concurrency; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d failed to create or fetch idempotent batch: %v", i, errs[i])
		}
		if results[i] == nil {
			t.Fatalf("goroutine %d returned nil batch", i)
		}
		if firstID == "" {
			firstID = results[i].ID
		} else if results[i].ID != firstID {
			t.Fatalf("goroutine %d returned different batch ID %s, expected %s", i, results[i].ID, firstID)
		}
	}
}
