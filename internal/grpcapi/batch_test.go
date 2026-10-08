package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
)

type fakeBatchStore struct {
	batches        map[string]*proxydb.Batch
	byClient       map[string]*proxydb.Batch
	items          map[string][]*proxydb.BatchItem
	createBatchErr error
}

func newFakeBatchStore() *fakeBatchStore {
	return &fakeBatchStore{
		batches:  make(map[string]*proxydb.Batch),
		byClient: make(map[string]*proxydb.Batch),
		items:    make(map[string][]*proxydb.BatchItem),
	}
}

func (s *fakeBatchStore) CreateBatch(ctx context.Context, b *proxydb.Batch, items []*proxydb.BatchItem) (*proxydb.Batch, error) {
	if s.createBatchErr != nil {
		return nil, s.createBatchErr
	}
	if b.ClientBatchID != "" {
		k := b.APIKeyID + ":" + b.ClientBatchID
		if existing, ok := s.byClient[k]; ok {
			return existing, nil
		}
		s.byClient[k] = b
	}
	s.batches[b.ID] = b
	s.items[b.ID] = items
	return b, nil
}

func (s *fakeBatchStore) GetBatch(ctx context.Context, id, apiKeyID string) (*proxydb.Batch, error) {
	b, ok := s.batches[id]
	if !ok || b.APIKeyID != apiKeyID {
		return nil, proxydb.ErrNotFound
	}
	return b, nil
}

func (s *fakeBatchStore) GetBatchByClientBatchID(ctx context.Context, apiKeyID, clientBatchID string) (*proxydb.Batch, error) {
	k := apiKeyID + ":" + clientBatchID
	b, ok := s.byClient[k]
	if !ok {
		return nil, proxydb.ErrNotFound
	}
	return b, nil
}

func (s *fakeBatchStore) ListBatchItems(ctx context.Context, batchID string, limit, offset int) ([]*proxydb.BatchItem, error) {
	all, ok := s.items[batchID]
	if !ok {
		return nil, nil
	}
	if offset >= len(all) {
		return nil, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

type fakeBatchAdapter struct {
	submittedID    string
	submitted      []*llm.BatchSubmitItem
	cancelledJobID string
	cancelErr      error
}

func (f *fakeBatchAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeBatchAdapter) SubmitBatch(ctx context.Context, batchID string, items []*llm.BatchSubmitItem) (string, error) {
	f.submittedID = batchID
	f.submitted = items
	return "vendor-job-" + batchID, nil
}

func (f *fakeBatchAdapter) GetBatch(ctx context.Context, vendorJobID string) (*llm.BatchJobStatus, error) {
	return &llm.BatchJobStatus{State: "RUNNING"}, nil
}

func (f *fakeBatchAdapter) CancelBatch(ctx context.Context, vendorJobID string) error {
	f.cancelledJobID = vendorJobID
	return f.cancelErr
}

func (f *fakeBatchAdapter) FetchBatchResults(ctx context.Context, vendorJobID string, items []*llm.BatchSubmitItem) ([]*llm.BatchItemResult, error) {
	return nil, nil
}

func authCtx(keyID, name string) context.Context {
	return apikeys.WithIdentity(context.Background(), &apikeys.Identity{
		KeyID: keyID,
		Name:  name,
	})
}

func batchServerFixture() (*ProxyServer, *fakeBatchStore, *fakeBatchAdapter) {
	adapter := &fakeBatchAdapter{}
	models := []*proxydb.Model{
		{
			ID:           "gemini-flash",
			Vendor:       registry.VendorGoogle,
			Capabilities: []string{registry.CapabilityBatch},
			Efforts:      []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Enabled:      true,
		},
	}
	rules := []*proxydb.Rule{
		{
			Name:     "default-batch",
			Priority: 1,
			Effort:   registry.EffortMedium,
			Use:      []string{"gemini-flash"},
			Enabled:  true,
		},
	}
	keys := map[string]map[string]string{
		"key-1": {registry.VendorGoogle: "g-secret"},
	}

	snap := registry.NewSnapshotForTest(models, rules, keys, nil)
	snap.SetAdapter("key-1", "gemini-flash", adapter)

	store := newFakeBatchStore()
	server := NewProxyServer(fixedSnapshot{snap}, nil).WithBatch(store, 100)
	return server, store, adapter
}

func TestSubmitBatch_Validations(t *testing.T) {
	server, _, _ := batchServerFixture()
	ctx := authCtx("key-1", "test-client")

	// 1. Empty items
	_, err := server.SubmitBatch(ctx, &pb.SubmitBatchRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty items, got %v", err)
	}

	// 2. Duplicate custom_id
	_, err = server.SubmitBatch(ctx, &pb.SubmitBatchRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{
			{CustomId: "dup-1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "hi"}}}},
			{CustomId: "dup-1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "hi"}}}},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on duplicate custom_id, got %v", err)
	}

	// 3. Exceeds max items
	tooMany := make([]*pb.BatchItemRequest, 101)
	for i := range tooMany {
		tooMany[i] = &pb.BatchItemRequest{CustomId: "id", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "hi"}}}}
	}
	_, err = server.SubmitBatch(ctx, &pb.SubmitBatchRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Items:  tooMany,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument when items exceed max, got %v", err)
	}

	// 4. Missing effort
	_, err = server.SubmitBatch(ctx, &pb.SubmitBatchRequest{
		Items: []*pb.BatchItemRequest{
			{CustomId: "id-1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "hi"}}}},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on missing effort, got %v", err)
	}
}

func TestSubmitBatch_SuccessAndIdempotency(t *testing.T) {
	server, store, adapter := batchServerFixture()
	ctx := authCtx("key-1", "test-client")

	req := &pb.SubmitBatchRequest{
		ClientBatchId: "client-batch-abc",
		Effort:        pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{
			{CustomId: "c1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "first"}}}},
			{CustomId: "c2", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "second"}}}},
		},
		Attributes: map[string]string{"env": "test"},
	}

	res1, err := server.SubmitBatch(ctx, req)
	if err != nil {
		t.Fatalf("SubmitBatch failed: %v", err)
	}
	if res1.GetBatch() == nil || res1.GetBatch().GetId() == "" {
		t.Fatalf("expected batch in response, got %+v", res1)
	}
	if res1.GetBatch().GetState() != pb.BatchState_BATCH_STATE_PENDING {
		t.Fatalf("expected PENDING state, got %v", res1.GetBatch().GetState())
	}
	// Verify ClientBatchId and counts are preserved
	if res1.GetBatch().GetClientBatchId() != "client-batch-abc" {
		t.Fatalf("expected client_batch_id 'client-batch-abc', got %q", res1.GetBatch().GetClientBatchId())
	}
	if res1.GetBatch().GetCounts().GetTotal() != 2 {
		t.Fatalf("expected total 2, got %d", res1.GetBatch().GetCounts().GetTotal())
	}

	batchID := res1.GetBatch().GetId()

	// Verify adapter received submission
	if len(adapter.submitted) != 2 {
		t.Fatalf("expected adapter to receive 2 items, got %d", len(adapter.submitted))
	}

	// Verify items stored in DB
	items, err := store.ListBatchItems(ctx, batchID, 10, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("expected 2 items in store, got %d, err: %v", len(items), err)
	}

	// Resubmit with same client_batch_id (idempotency)
	res2, err := server.SubmitBatch(ctx, req)
	if err != nil {
		t.Fatalf("idempotent resubmit failed: %v", err)
	}
	if res2.GetBatch().GetId() != batchID {
		t.Fatalf("expected same batch ID %s on idempotent resubmit, got %s", batchID, res2.GetBatch().GetId())
	}
}

func TestGetBatch(t *testing.T) {
	server, store, _ := batchServerFixture()
	ctx := authCtx("key-1", "test-client")

	now := time.Now().UTC()
	store.batches["b-1"] = &proxydb.Batch{
		ID:         "b-1",
		APIKeyID:   "key-1",
		Model:      "gemini-flash",
		State:      "RUNNING",
		TotalCount: 5,
		DoneCount:  2,
		CreatedAt:  now,
	}

	// 1. Success
	b, err := server.GetBatch(ctx, &pb.GetBatchRequest{BatchId: "b-1"})
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}
	if b.GetId() != "b-1" || b.GetState() != pb.BatchState_BATCH_STATE_RUNNING || b.GetCounts().GetDone() != 2 {
		t.Fatalf("unexpected batch: %+v", b)
	}

	// 2. Not found
	_, err = server.GetBatch(ctx, &pb.GetBatchRequest{BatchId: "nonexistent"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}

	// 3. Different client key
	otherCtx := authCtx("key-2", "other-client")
	_, err = server.GetBatch(otherCtx, &pb.GetBatchRequest{BatchId: "b-1"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for other key, got %v", err)
	}
}

func TestListBatchResults_PaginationAndOutcomes(t *testing.T) {
	server, store, _ := batchServerFixture()
	ctx := authCtx("key-1", "test-client")

	store.batches["b-results"] = &proxydb.Batch{
		ID:        "b-results",
		APIKeyID:  "key-1",
		Model:     "gemini-flash",
		State:     "SUCCEEDED",
		CreatedAt: time.Now().UTC(),
	}

	respProto := &pb.GenerateTextResponse{
		Choices: []string{"successful choice"},
		Usage:   &pb.TokensUsage{Input: 10, Output: 20},
	}
	respBytes, _ := protojson.Marshal(respProto)

	errBytes, _ := json.Marshal(map[string]any{
		"code":    9,
		"reason":  "output_truncated",
		"message": "hit max output tokens",
	})

	store.items["b-results"] = []*proxydb.BatchItem{
		{
			BatchID:  "b-results",
			CustomID: "c-1",
			Position: 0,
			Status:   "SUCCEEDED",
			Result:   respBytes,
		},
		{
			BatchID:  "b-results",
			CustomID: "c-2",
			Position: 1,
			Status:   "FAILED",
			Error:    errBytes,
		},
		{
			BatchID:  "b-results",
			CustomID: "c-3",
			Position: 2,
			Status:   "SUCCEEDED",
			Result:   respBytes,
		},
	}

	// Page 1: pageSize = 2
	resPage1, err := server.ListBatchResults(ctx, &pb.ListBatchResultsRequest{
		BatchId:  "b-results",
		PageSize: 2,
	})
	if err != nil {
		t.Fatalf("ListBatchResults page 1 failed: %v", err)
	}
	if len(resPage1.GetItems()) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resPage1.GetItems()))
	}
	if resPage1.GetNextPageToken() != "2" {
		t.Fatalf("expected next page token 2, got %q", resPage1.GetNextPageToken())
	}

	// Verify Item 1: response outcome
	it1 := resPage1.GetItems()[0]
	if it1.GetCustomId() != "c-1" || it1.GetResponse() == nil || it1.GetResponse().GetChoices()[0] != "successful choice" {
		t.Fatalf("unexpected item 1: %+v", it1)
	}

	// Verify Item 2: error outcome
	it2 := resPage1.GetItems()[1]
	if it2.GetCustomId() != "c-2" || it2.GetError() == nil || it2.GetError().GetReason() != "output_truncated" {
		t.Fatalf("unexpected item 2 error: %+v", it2)
	}

	// Page 2: with token = "2"
	resPage2, err := server.ListBatchResults(ctx, &pb.ListBatchResultsRequest{
		BatchId:   "b-results",
		PageSize:  2,
		PageToken: resPage1.GetNextPageToken(),
	})
	if err != nil {
		t.Fatalf("ListBatchResults page 2 failed: %v", err)
	}
	if len(resPage2.GetItems()) != 1 {
		t.Fatalf("expected 1 item on page 2, got %d", len(resPage2.GetItems()))
	}
	if resPage2.GetNextPageToken() != "" {
		t.Fatalf("expected no next page token, got %q", resPage2.GetNextPageToken())
	}
}

func TestCancelBatch(t *testing.T) {
	server, store, adapter := batchServerFixture()
	ctx := authCtx("key-1", "test-client")

	store.batches["b-cancel"] = &proxydb.Batch{
		ID:            "b-cancel",
		APIKeyID:      "key-1",
		KeyName:       "test-client",
		Model:         "gemini-flash",
		VendorJobName: "vendor-job-cancel-123",
		State:         "RUNNING",
		CreatedAt:     time.Now().UTC(),
	}

	_, err := server.CancelBatch(ctx, &pb.CancelBatchRequest{BatchId: "b-cancel"})
	if err != nil {
		t.Fatalf("CancelBatch failed: %v", err)
	}

	if adapter.cancelledJobID != "vendor-job-cancel-123" {
		t.Fatalf("expected vendor cancel for vendor-job-cancel-123, got %q", adapter.cancelledJobID)
	}

	b := store.batches["b-cancel"]
	if b.State != "RUNNING" {
		t.Fatalf("expected state to remain RUNNING until poller confirms and fetches results, got %s", b.State)
	}

	// Vendor cancel error must propagate and not mark batch cancelled
	store.batches["b-cancel-fail"] = &proxydb.Batch{
		ID:            "b-cancel-fail",
		APIKeyID:      "key-1",
		KeyName:       "test-client",
		Model:         "gemini-flash",
		VendorJobName: "vendor-job-fail",
		State:         "RUNNING",
		CreatedAt:     time.Now().UTC(),
	}
	adapter.cancelErr = errors.New("vendor unavailable")
	_, err = server.CancelBatch(ctx, &pb.CancelBatchRequest{BatchId: "b-cancel-fail"})
	if err == nil {
		t.Fatal("expected error on vendor cancel failure, got nil")
	}
	if store.batches["b-cancel-fail"].State != "RUNNING" {
		t.Fatalf("expected batch state to remain RUNNING on vendor cancel error, got %s", store.batches["b-cancel-fail"].State)
	}
}

func TestSubmitBatch_CancelVendorOnDBFailure(t *testing.T) {
	server, store, adapter := batchServerFixture()
	ctx := authCtx("key-1", "test-client")

	store.createBatchErr = errors.New("database connection refused")

	req := &pb.SubmitBatchRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{
			{CustomId: "c1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "hi"}}}},
		},
	}

	_, err := server.SubmitBatch(ctx, req)
	if err == nil {
		t.Fatal("expected error when DB write fails, got nil")
	}

	if adapter.cancelledJobID == "" {
		t.Fatal("expected vendor batch to be cancelled when DB write failed")
	}
}

type fakeBatchThrottle struct {
	router.NoopThrottle
	checkedKeyName string
	checkedUserID  string
	trippedReason  string
}

func (f *fakeBatchThrottle) CheckDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string) string {
	f.checkedKeyName = keyName
	f.checkedUserID = userID
	return f.trippedReason
}

func TestSubmitBatch_ServiceCallerBudgetCheck(t *testing.T) {
	adapter := &fakeBatchAdapter{}
	models := []*proxydb.Model{
		{
			ID:                 "gemini-flash",
			Vendor:             registry.VendorGoogle,
			Capabilities:       []string{registry.CapabilityBatch},
			Efforts:            []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			DailyTokensPerUser: 1000,
			Enabled:            true,
		},
	}
	rules := []*proxydb.Rule{
		{
			Name:     "default-batch",
			Priority: 1,
			Effort:   registry.EffortMedium,
			Use:      []string{"gemini-flash"},
			Enabled:  true,
		},
	}
	keys := map[string]map[string]string{
		"key-1": {registry.VendorGoogle: "g-secret"},
	}

	snap := registry.NewSnapshotForTest(models, rules, keys, nil)
	snap.SetAdapter("key-1", "gemini-flash", adapter)

	store := newFakeBatchStore()
	th := &fakeBatchThrottle{}
	server := NewProxyServer(fixedSnapshot{snap}, th).WithBatch(store, 100)
	ctx := authCtx("key-1", "service-client")

	// 1. Submit with no user_id attribute -> must check budget with svc:service-client
	req := &pb.SubmitBatchRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{
			{CustomId: "c1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "hi"}}}},
		},
	}
	_, err := server.SubmitBatch(ctx, req)
	if err != nil {
		t.Fatalf("SubmitBatch failed: %v", err)
	}
	if th.checkedUserID != "svc:service-client" {
		t.Fatalf("expected checked user ID 'svc:service-client', got %q", th.checkedUserID)
	}

	// 2. When budget is exhausted -> must return ResourceExhausted
	th.trippedReason = "budget_user"
	_, err = server.SubmitBatch(ctx, req)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted when budget tripped, got %v", err)
	}
}
