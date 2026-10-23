package grpcapi

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/nodes"
	"github.com/ai-process/ktms/internal/proxydb"
)

// NodeRegistryServer implements pb.NodeRegistryServiceServer.
type NodeRegistryServer struct {
	pb.UnimplementedNodeRegistryServiceServer
	mgr     *nodes.Manager
	keyring *crypto.Keyring
	warned  sync.Map // node id -> dropped-model list already logged, so heartbeats do not repeat it
}

func NewNodeRegistryServer(mgr *nodes.Manager, keyring *crypto.Keyring) *NodeRegistryServer {
	return &NodeRegistryServer{
		mgr:     mgr,
		keyring: keyring,
	}
}

// authorize applies the node key's restrictions to a registration.
func (s *NodeRegistryServer) authorize(ctx context.Context, caller *apikeys.Identity, nodeID string, models []*pb.NodeModelSpec) (kept []*pb.NodeModelSpec, dropped []string, err error) {
	if caller.NodeID != "" && caller.NodeID != nodeID {
		return nil, nil, status.Errorf(codes.PermissionDenied, "key is bound to node_id %q, cannot register as %q", caller.NodeID, nodeID)
	}
	if len(caller.AllowedCIDRs) > 0 {
		var remoteAddr string
		if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
			remoteAddr = p.Addr.String()
		}
		if err := nodes.ValidateCIDRs(remoteAddr, caller.AllowedCIDRs); err != nil {
			return nil, nil, status.Errorf(codes.PermissionDenied, "remote IP not allowed: %v", err)
		}
	}
	return s.screenModels(caller, nodeID, models)
}

// screenModels drops models the key does not allow, with a warning; it fails only
// when models were offered and none is left.
func (s *NodeRegistryServer) screenModels(caller *apikeys.Identity, nodeID string, models []*pb.NodeModelSpec) (kept []*pb.NodeModelSpec, dropped []string, err error) {
	kept, dropped = filterAllowedModels(models, caller.AllowedModels)
	if len(dropped) > 0 {
		if prev, _ := s.warned.Swap(nodeID, strings.Join(dropped, ",")); prev != strings.Join(dropped, ",") {
			log.Warn().Str("node_id", nodeID).Strs("dropped_models", dropped).Msg("node offered models its key does not allow; ignoring them")
		}
	} else {
		s.warned.Delete(nodeID)
	}
	if len(models) > 0 && len(kept) == 0 {
		return nil, dropped, status.Errorf(codes.PermissionDenied, "none of the models %q is permitted for this node key (allowed: %q)", dropped, caller.AllowedModels)
	}
	return kept, dropped, nil
}

// filterAllowedModels splits models by the key's allow list; an empty list allows all.
func filterAllowedModels(models []*pb.NodeModelSpec, allowed []string) (kept []*pb.NodeModelSpec, dropped []string) {
	if len(allowed) == 0 {
		return models, nil
	}
	ok := make(map[string]bool, len(allowed))
	for _, m := range allowed {
		ok[m] = true
	}
	for _, m := range models {
		if ok[m.GetModelId()] {
			kept = append(kept, m)
		} else {
			dropped = append(dropped, m.GetModelId())
		}
	}
	return kept, dropped
}

func (s *NodeRegistryServer) ValidateNode(ctx context.Context, req *pb.ValidateNodeRequest) (*pb.ValidateNodeResponse, error) {
	caller := apikeys.IdentityFrom(ctx)
	if caller == nil {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	kept, dropped, err := s.authorize(ctx, caller, req.GetNodeId(), req.GetModels())
	if err != nil {
		return nil, err
	}
	resp := &pb.ValidateNodeResponse{DroppedModels: dropped}
	for _, m := range kept {
		resp.AcceptedModels = append(resp.AcceptedModels, m.GetModelId())
	}
	return resp, nil
}

func (s *NodeRegistryServer) RegisterNode(ctx context.Context, req *pb.RegisterNodeRequest) (*pb.RegisterNodeResponse, error) {
	caller := apikeys.IdentityFrom(ctx)
	if caller == nil {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	models, _, err := s.authorize(ctx, caller, req.GetNodeId(), req.GetModels())
	if err != nil {
		return nil, err
	}

	baseWeight := float64(100.0)
	if req.GetResources() != nil && req.GetResources().GetBaseWeight() > 0 {
		baseWeight = req.GetResources().GetBaseWeight()
	}
	if caller.MaxBaseWeight > 0 && baseWeight > caller.MaxBaseWeight {
		baseWeight = caller.MaxBaseWeight
	}

	maxConc := int32(16)
	if req.GetResources() != nil && req.GetResources().GetMaxConcurrent() > 0 {
		maxConc = int32(req.GetResources().GetMaxConcurrent())
	}
	if caller.MaxConcurrent > 0 && maxConc > caller.MaxConcurrent {
		maxConc = caller.MaxConcurrent
	}

	// Encrypt node auth token if provided
	tokenCiphertext := ""
	if req.GetNodeAuthToken() != "" && s.keyring != nil {
		ct, v, err := s.keyring.Encrypt(req.GetNodeAuthToken())
		if err != nil {
			return nil, status.Error(codes.Internal, "failed to encrypt auth token")
		}
		tokenCiphertext = crypto.EncodeVersioned(ct, v)
	}

	meta := &nodes.NodeMeta{
		NodeID:        req.GetNodeId(),
		Name:          req.GetName(),
		Endpoint:      req.GetEndpoint(),
		Engine:        req.GetEngine(),
		AuthToken:     tokenCiphertext,
		BaseWeight:    baseWeight,
		MaxConcurrent: maxConc,
		Models:        models,
		Tags:          req.GetTags(),
	}

	if err := s.mgr.Register(ctx, meta); err != nil {
		if errors.Is(err, nodes.ErrInvalidNode) {
			return nil, status.Errorf(codes.InvalidArgument, "failed to register: %v", err)
		}
		// Pool storage is down: the agent retries, and should not read it as a rejection.
		log.Error().Err(err).Str("node_id", meta.NodeID).Msg("node registration could not be stored")
		return nil, status.Error(codes.Unavailable, "node registry unavailable")
	}

	cfg := s.mgr.Config()
	return &pb.RegisterNodeResponse{
		AssignedNodeId:           meta.NodeID,
		HeartbeatIntervalSeconds: int32(cfg.HeartbeatInterval.Seconds()),
		HeartbeatTtlSeconds:      int32(cfg.HeartbeatTTL.Seconds()),
	}, nil
}

func (s *NodeRegistryServer) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	caller := apikeys.IdentityFrom(ctx)
	if caller == nil {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if caller.NodeID != "" && caller.NodeID != req.GetNodeId() {
		return nil, status.Errorf(codes.PermissionDenied, "key is bound to node_id %q", caller.NodeID)
	}
	if req.CapacityPercent != nil && req.GetCapacityPercent() > 100 {
		return nil, status.Errorf(codes.InvalidArgument, "capacity_percent %d is above 100", req.GetCapacityPercent())
	}
	models, _, err := s.screenModels(caller, req.GetNodeId(), req.GetModels())
	if err != nil {
		return nil, err
	}

	var warmModels []string
	for _, m := range models {
		if m.GetIsWarm() {
			warmModels = append(warmModels, m.GetModelId())
		}
	}

	var totalVRAM, freeVRAM uint64
	var gpuUtil float64
	var gpuTemp uint32
	var gpuModel string
	if req.GetResources() != nil {
		totalVRAM = req.GetResources().GetVramTotalMb()
		freeVRAM = req.GetResources().GetVramFreeMb()
		gpuUtil = req.GetResources().GetGpuUtilPercent()
		gpuTemp = req.GetResources().GetGpuTempC()
		gpuModel = req.GetResources().GetGpuModel()
	}

	hb := &nodes.NodeHeartbeatData{
		NodeID:          req.GetNodeId(),
		Draining:        req.GetDraining(),
		ActiveRequests:  clampInt32(req.GetActiveRequests()),
		QueueDepth:      clampInt32(req.GetQueueDepth()),
		VRAMTotalMB:     totalVRAM,
		VRAMFreeMB:      freeVRAM,
		GPUUtilPercent:  gpuUtil,
		GPUTempC:        gpuTemp,
		GPUModel:        gpuModel,
		WarmModels:      warmModels,
		ModelSpecs:      models,
		CapacityPercent: req.CapacityPercent,
	}

	drainReq, activeProxy, err := s.mgr.Heartbeat(ctx, hb)
	if errors.Is(err, nodes.ErrNodeNotFound) {
		return &pb.HeartbeatResponse{
			Ok:                 false,
			ReRegisterRequired: true,
		}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "heartbeat failed: %v", err)
	}

	return &pb.HeartbeatResponse{
		Ok:                  true,
		DrainRequested:      drainReq,
		ActiveProxyRequests: activeProxy,
		CapacityApplied:     true,
	}, nil
}

func (s *NodeRegistryServer) UnregisterNode(ctx context.Context, req *pb.UnregisterNodeRequest) (*pb.UnregisterNodeResponse, error) {
	caller := apikeys.IdentityFrom(ctx)
	if caller == nil {
		return nil, status.Error(codes.Unauthenticated, "unauthenticated")
	}
	if caller.NodeID != "" && caller.NodeID != req.GetNodeId() {
		return nil, status.Errorf(codes.PermissionDenied, "key is bound to node_id %q", caller.NodeID)
	}

	if err := s.mgr.Unregister(ctx, req.GetNodeId()); err != nil {
		return nil, status.Errorf(codes.Internal, "unregister failed: %v", err)
	}
	return &pb.UnregisterNodeResponse{Ok: true}, nil
}

// Extend AdminServer with Node Management RPCs

func (s *AdminServer) MintNodeKey(ctx context.Context, req *pb.MintNodeKeyRequest) (*pb.MintNodeKeyResponse, error) {
	if req.GetName() == "" || req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "name and node_id are required")
	}

	plain, row, err := apikeys.GenerateNodeKey(
		req.GetName(),
		req.GetNodeId(),
		req.GetAllowedCidrs(),
		req.GetAllowedModels(),
		req.GetMaxBaseWeight(),
		req.GetMaxConcurrency(),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to mint node key")
	}

	if err := s.db.InsertNodeKey(ctx, row); err != nil {
		if errors.Is(err, proxydb.ErrAlreadyExists) {
			return nil, status.Error(codes.AlreadyExists, "active node key with same name or node_id exists")
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, status.Error(codes.AlreadyExists, "active node key already exists")
		}
		return nil, dbStatus(err)
	}

	return &pb.MintNodeKeyResponse{
		Id:       row.ID,
		PlainKey: plain,
	}, nil
}

func (s *AdminServer) RevokeNodeKey(ctx context.Context, req *pb.RevokeNodeKeyRequest) (*pb.RevokeNodeKeyResponse, error) {
	nodeID, err := s.db.RevokeNodeKey(ctx, req.GetId())
	if err != nil {
		return nil, dbStatus(err)
	}
	// Evict from node pool if nodeMgr is available. The key is already revoked,
	// so a failed eviction only delays it: the node's next heartbeat is refused.
	if s.nodeMgr != nil && nodeID != "" {
		if err := s.nodeMgr.Unregister(ctx, nodeID); err != nil {
			log.Warn().Err(err).Str("node_id", nodeID).Msg("revoked node key, but evicting the node failed")
		}
	}
	return &pb.RevokeNodeKeyResponse{}, nil
}

func (s *AdminServer) ListNodeKeys(ctx context.Context, _ *pb.ListNodeKeysRequest) (*pb.ListNodeKeysResponse, error) {
	keys, err := s.db.ListNodeKeys(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	var out []*pb.NodeKeyInfo
	for _, k := range keys {
		out = append(out, &pb.NodeKeyInfo{
			Id:             k.ID,
			Name:           k.Name,
			NodeId:         k.NodeID,
			CreatedAt:      timestamppb.New(k.CreatedAt),
			Disabled:       !k.Active(),
			AllowedCidrs:   k.AllowedCIDRs,
			AllowedModels:  k.AllowedModels,
			MaxBaseWeight:  k.MaxBaseWeight,
			MaxConcurrency: k.MaxConcurrency,
		})
	}
	return &pb.ListNodeKeysResponse{Keys: out}, nil
}

func (s *AdminServer) ListNodes(ctx context.Context, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	if s.nodeMgr == nil {
		return &pb.ListNodesResponse{}, nil
	}
	snapshots, err := s.nodeMgr.ListNodes(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list nodes: %v", err)
	}
	var out []*pb.NodeInfo
	for _, sn := range snapshots {
		var models []string
		for _, m := range sn.Meta.Models {
			models = append(models, m.ModelId)
		}
		var warmModels []string
		var gpuUtil float64
		var gpuTemp uint32
		var vramFree, vramTotal uint64
		var lastHB *timestamppb.Timestamp
		if sn.Heartbeat != nil {
			warmModels = sn.Heartbeat.WarmModels
			gpuUtil = sn.Heartbeat.GPUUtilPercent
			gpuTemp = sn.Heartbeat.GPUTempC
			vramFree = sn.Heartbeat.VRAMFreeMB
			vramTotal = sn.Heartbeat.VRAMTotalMB
			lastHB = timestamppb.New(sn.Heartbeat.LastHeartbeat)
		}

		var tpsEma, ttftEma float64
		for _, p := range sn.Perf {
			if p.TPSEMA > 0 {
				tpsEma = p.TPSEMA
			}
			if p.TTFTEMAMs > 0 {
				ttftEma = p.TTFTEMAMs
			}
		}

		out = append(out, &pb.NodeInfo{
			NodeId:          sn.Meta.NodeID,
			Name:            sn.Meta.Name,
			Endpoint:        sn.Meta.Endpoint,
			State:           sn.State.String(),
			Models:          models,
			EffectiveWeight: sn.Meta.BaseWeight,
			ActiveRequests:  sn.Active, // cooldown holds would show an idle node busy
			MaxConcurrent:   sn.Meta.MaxConcurrent,
			VramFreeMb:      vramFree,
			VramTotalMb:     vramTotal,
			TtftEmaMs:       ttftEma,
			TpsEma:          tpsEma,
			LastHeartbeat:   lastHB,
			GpuUtilPercent:  gpuUtil,
			GpuTempC:        gpuTemp,
			BaseWeight:      sn.Meta.BaseWeight,
			Engine:          sn.Meta.Engine,
			WarmModels:      warmModels,
			CapacityPercent: sn.Capacity(),
			EffectiveSlots:  sn.Slots(),
		})
	}
	return &pb.ListNodesResponse{Nodes: out}, nil
}

func (s *AdminServer) DrainNode(ctx context.Context, req *pb.DrainNodeRequest) (*pb.DrainNodeResponse, error) {
	if s.nodeMgr == nil {
		return nil, status.Error(codes.FailedPrecondition, "node manager disabled")
	}
	if err := s.nodeMgr.Drain(ctx, req.GetNodeId()); err != nil {
		log.Error().Err(err).Str("node_id", req.GetNodeId()).Msg("drain node failed")
		return nil, status.Errorf(codes.Internal, "drain node failed: %v", err)
	}
	return &pb.DrainNodeResponse{}, nil
}

// clampInt32 keeps a reported count from wrapping negative, which would make a
// saturated node look idle to admission.
func clampInt32(v uint32) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}
