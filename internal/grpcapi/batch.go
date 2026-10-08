package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gofrs/uuid"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/router"
)

// BatchStore abstracts batch persistence operations.
type BatchStore interface {
	CreateBatch(ctx context.Context, b *proxydb.Batch, items []*proxydb.BatchItem) (*proxydb.Batch, error)
	GetBatch(ctx context.Context, id, apiKeyID string) (*proxydb.Batch, error)
	GetBatchByClientBatchID(ctx context.Context, apiKeyID, clientBatchID string) (*proxydb.Batch, error)
	ListBatchItems(ctx context.Context, batchID string, limit, offset int) ([]*proxydb.BatchItem, error)
}

// WithBatch configures the batch store and max item limit on the server.
func (s *ProxyServer) WithBatch(store BatchStore, maxItems int) *ProxyServer {
	s.batchStore = store
	s.batchMaxItems = maxItems
	return s
}

func (s *ProxyServer) SubmitBatch(ctx context.Context, req *pb.SubmitBatchRequest) (*pb.SubmitBatchResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}

	if s.batchStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "batch not configured")
	}

	items := req.GetItems()
	maxItems := s.batchMaxItems
	if maxItems <= 0 {
		maxItems = 10000
	}
	if len(items) == 0 {
		return nil, status.Error(codes.InvalidArgument, "bad_request: empty batch")
	}
	if len(items) > maxItems {
		return nil, status.Errorf(codes.InvalidArgument, "bad_request: batch exceeds max items of %d", maxItems)
	}

	// Validate custom_id uniqueness and item contents
	seenCustomIDs := make(map[string]struct{}, len(items))
	for _, it := range items {
		cid := it.GetCustomId()
		if cid == "" {
			return nil, status.Error(codes.InvalidArgument, "bad_request: custom_id cannot be empty")
		}
		if _, exists := seenCustomIDs[cid]; exists {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: duplicate custom_id %q", cid)
		}
		seenCustomIDs[cid] = struct{}{}

		itemReq := it.GetRequest()
		if itemReq == nil || (len(itemReq.GetMessages()) == 0 && itemReq.GetBaseSystemInstruction() == "" && itemReq.GetRequestInstruction() == "") {
			return nil, status.Errorf(codes.InvalidArgument, "bad_request: item %q has no messages or instructions", cid)
		}
	}

	// Idempotency: check if client_batch_id already exists for this client key
	if req.GetClientBatchId() != "" {
		existing, err := s.batchStore.GetBatchByClientBatchID(ctx, id.KeyID, req.GetClientBatchId())
		if err == nil && existing != nil {
			return &pb.SubmitBatchResponse{Batch: batchToProto(existing)}, nil
		}
	}

	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}

	effort := effortToString(req.GetEffort())
	if effort == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: effort is required")
	}

	rule := router.Match(snap, effort, req.GetAttributes())
	if rule == nil {
		return nil, router.ToStatus(router.ErrNotConfigured)
	}

	userID := router.UserID(req.GetAttributes(), id.Name)
	model, err := router.ResolveBatchModel(ctx, snap, s.throttle, rule.Use, id.KeyID, id.Name, userID)
	if err != nil {
		return nil, router.ToStatus(err)
	}

	adapter, err := snap.Adapter(id.KeyID, model)
	if err != nil {
		return nil, router.ToStatus(err)
	}

	batchAdapter, ok := adapter.(llm.BatchAdapter)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "no_capable_model")
	}

	batchID := uuid.Must(uuid.NewV7()).String()
	adapterItems := make([]*llm.BatchSubmitItem, len(items))
	dbItems := make([]*proxydb.BatchItem, len(items))

	actionID := req.GetAttributes()["action_id"]
	if actionID == "" && len(items) > 0 && items[0].GetRequest() != nil {
		actionID = items[0].GetRequest().GetActionId()
	}
	if actionID == "" {
		actionID = defaultActionID
	}

	for i, it := range items {
		chat := buildChat(it.GetRequest(), id, rule.Name, effort)
		adapterItems[i] = &llm.BatchSubmitItem{
			CustomID: it.GetCustomId(),
			Chat:     chat,
		}

		var schemaBytes []byte
		if it.GetRequest().GetResponseSchema() != nil {
			if schema := schemaFromProto(it.GetRequest().GetResponseSchema()); schema != nil {
				schemaBytes, _ = json.Marshal(schema)
			}
		}

		dbItems[i] = &proxydb.BatchItem{
			BatchID:        batchID,
			CustomID:       it.GetCustomId(),
			Position:       int32(i),
			Status:         "PENDING",
			ResponseSchema: schemaBytes,
		}
	}

	vendorJobID, err := batchAdapter.SubmitBatch(ctx, batchID, adapterItems)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "vendor_error: %v", err)
	}

	batchRecord := &proxydb.Batch{
		ID:            batchID,
		ClientBatchID: req.GetClientBatchId(),
		APIKeyID:      id.KeyID,
		KeyName:       id.Name,
		Model:         model.ID,
		VendorJobName: vendorJobID,
		State:         "PENDING",
		TotalCount:    int32(len(items)),
		DoneCount:     0,
		FailedCount:   0,
		Attributes:    req.GetAttributes(),
		ActionID:      actionID,
		CreatedAt:     time.Now().UTC(),
	}

	created, err := s.batchStore.CreateBatch(ctx, batchRecord, dbItems)
	if err != nil {
		if cancelErr := batchAdapter.CancelBatch(ctx, vendorJobID); cancelErr != nil {
			log.Error().Err(cancelErr).Str("batch_id", batchID).Str("vendor_job", vendorJobID).Msg("failed to cancel vendor batch on db store failure")
		}
		return nil, status.Errorf(codes.Internal, "failed to store batch: %v", err)
	}
	if created.ID != batchID {
		if cancelErr := batchAdapter.CancelBatch(ctx, vendorJobID); cancelErr != nil {
			log.Error().Err(cancelErr).Str("batch_id", batchID).Str("vendor_job", vendorJobID).Msg("failed to cancel duplicate vendor batch on idempotency collision")
		}
	}

	return &pb.SubmitBatchResponse{Batch: batchToProto(created)}, nil
}

func (s *ProxyServer) GetBatch(ctx context.Context, req *pb.GetBatchRequest) (*pb.Batch, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}

	if req.GetBatchId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: batch_id is required")
	}

	if s.batchStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "batch not configured")
	}

	b, err := s.batchStore.GetBatch(ctx, req.GetBatchId(), id.KeyID)
	if err != nil {
		if errors.Is(err, proxydb.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "unknown batch")
		}
		return nil, status.Errorf(codes.Internal, "get batch: %v", err)
	}

	return batchToProto(b), nil
}

func (s *ProxyServer) ListBatchResults(ctx context.Context, req *pb.ListBatchResultsRequest) (*pb.ListBatchResultsResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}

	if req.GetBatchId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: batch_id is required")
	}

	if s.batchStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "batch not configured")
	}

	b, err := s.batchStore.GetBatch(ctx, req.GetBatchId(), id.KeyID)
	if err != nil {
		if errors.Is(err, proxydb.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "unknown batch")
		}
		return nil, status.Errorf(codes.Internal, "get batch: %v", err)
	}

	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = 100
	} else if pageSize > 1000 {
		pageSize = 1000
	}

	offset := 0
	if req.GetPageToken() != "" {
		parsedOffset, err := strconv.Atoi(req.GetPageToken())
		if err != nil || parsedOffset < 0 {
			return nil, status.Error(codes.InvalidArgument, "bad_request: invalid page_token")
		}
		offset = parsedOffset
	}

	items, err := s.batchStore.ListBatchItems(ctx, b.ID, pageSize+1, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list batch items: %v", err)
	}

	var nextPageToken string
	if len(items) > pageSize {
		nextPageToken = strconv.Itoa(offset + pageSize)
		items = items[:pageSize]
	}

	results := make([]*pb.BatchItemResult, 0, len(items))
	for _, it := range items {
		itemRes := &pb.BatchItemResult{
			CustomId: it.CustomID,
		}
		if len(it.Result) > 0 {
			var resp pb.GenerateTextResponse
			if err := protojson.Unmarshal(it.Result, &resp); err == nil {
				itemRes.Result = &pb.BatchItemResult_Response{
					Response: &resp,
				}
			}
		} else if len(it.Error) > 0 {
			var errObj struct {
				Code    int32  `json:"code"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(it.Error, &errObj); err == nil {
				itemRes.Result = &pb.BatchItemResult_Error{
					Error: &pb.BatchItemError{
						Code:    errObj.Code,
						Reason:  errObj.Reason,
						Message: errObj.Message,
					},
				}
			}
		}
		results = append(results, itemRes)
	}

	return &pb.ListBatchResultsResponse{
		Items:         results,
		NextPageToken: nextPageToken,
	}, nil
}

func (s *ProxyServer) CancelBatch(ctx context.Context, req *pb.CancelBatchRequest) (*pb.CancelBatchResponse, error) {
	id := apikeys.IdentityFrom(ctx)
	if id == nil {
		return nil, status.Error(codes.Unauthenticated, "no identity")
	}

	if req.GetBatchId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bad_request: batch_id is required")
	}

	if s.batchStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "batch not configured")
	}

	b, err := s.batchStore.GetBatch(ctx, req.GetBatchId(), id.KeyID)
	if err != nil {
		if errors.Is(err, proxydb.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "unknown batch")
		}
		return nil, status.Errorf(codes.Internal, "get batch: %v", err)
	}

	if b.State == "SUCCEEDED" || b.State == "FAILED" || b.State == "CANCELLED" || b.State == "EXPIRED" {
		return &pb.CancelBatchResponse{}, nil
	}

	snap := s.snapshots.Snapshot()
	if snap == nil {
		return nil, status.Error(codes.Unavailable, "proxy configuration not ready")
	}
	m, ok := snap.Models[b.Model]
	if !ok {
		return nil, status.Errorf(codes.Internal, "batch model %q not found", b.Model)
	}
	adapter, err := snap.Adapter(id.KeyID, m)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve adapter for key ID %q: %v", id.KeyID, err)
	}
	ba, ok := adapter.(llm.BatchAdapter)
	if !ok {
		return nil, status.Errorf(codes.Internal, "model %q does not support batch", b.Model)
	}
	if err := ba.CancelBatch(ctx, b.VendorJobName); err != nil {
		log.Error().Err(err).Str("batch_id", b.ID).Str("vendor_job", b.VendorJobName).Msg("vendor cancel batch failed")
		return nil, status.Errorf(codes.Internal, "vendor cancel batch failed: %v", err)
	}

	// Cancelling vendor job is a request; row stays running until vendor confirms and poller fetches results
	return &pb.CancelBatchResponse{}, nil
}

func batchToProto(b *proxydb.Batch) *pb.Batch {
	if b == nil {
		return nil
	}
	out := &pb.Batch{
		Id:            b.ID,
		ClientBatchId: b.ClientBatchID,
		State:         mapBatchStateToProto(b.State),
		Counts: &pb.BatchCounts{
			Total:  b.TotalCount,
			Done:   b.DoneCount,
			Failed: b.FailedCount,
		},
		Model:     b.Model,
		CreatedAt: timestamppb.New(b.CreatedAt),
	}
	if b.CompletedAt != nil {
		out.CompletedAt = timestamppb.New(*b.CompletedAt)
	}
	return out
}

func mapBatchStateToProto(state string) pb.BatchState {
	switch state {
	case "PENDING":
		return pb.BatchState_BATCH_STATE_PENDING
	case "RUNNING":
		return pb.BatchState_BATCH_STATE_RUNNING
	case "SUCCEEDED":
		return pb.BatchState_BATCH_STATE_SUCCEEDED
	case "FAILED":
		return pb.BatchState_BATCH_STATE_FAILED
	case "CANCELLED":
		return pb.BatchState_BATCH_STATE_CANCELLED
	case "EXPIRED":
		return pb.BatchState_BATCH_STATE_EXPIRED
	default:
		return pb.BatchState_BATCH_STATE_UNSPECIFIED
	}
}
