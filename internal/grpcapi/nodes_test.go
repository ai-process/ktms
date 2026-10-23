package grpcapi

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/nodes"
	"github.com/ai-process/ktms/internal/rediskeys"
)

func specs(ids ...string) []*pb.NodeModelSpec {
	var out []*pb.NodeModelSpec
	for _, id := range ids {
		out = append(out, &pb.NodeModelSpec{ModelId: id})
	}
	return out
}

func TestScreenModels(t *testing.T) {
	for _, c := range []struct {
		name     string
		allowed  []string
		offered  []string
		kept     []string
		dropped  []string
		wantCode codes.Code
	}{
		{"no restriction keeps all", nil, []string{"a", "b"}, []string{"a", "b"}, nil, codes.OK},
		{"all allowed", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}, nil, codes.OK},
		{"extra model is dropped, node still registers", []string{"a"}, []string{"a", "gemma4:latest"}, []string{"a"}, []string{"gemma4:latest"}, codes.OK},
		{"nothing allowed is refused", []string{"a"}, []string{"x", "y"}, nil, []string{"x", "y"}, codes.PermissionDenied},
		{"no models offered is fine", []string{"a"}, nil, nil, nil, codes.OK},
	} {
		kept, dropped, err := (&NodeRegistryServer{}).screenModels(&apikeys.Identity{AllowedModels: c.allowed}, "n1", specs(c.offered...))
		if got := status.Code(err); got != c.wantCode {
			t.Errorf("%s: code = %v, want %v (%v)", c.name, got, c.wantCode, err)
		}
		var keptIDs []string
		for _, m := range kept {
			keptIDs = append(keptIDs, m.GetModelId())
		}
		if !reflect.DeepEqual(keptIDs, c.kept) || !reflect.DeepEqual(dropped, c.dropped) {
			t.Errorf("%s: kept %v dropped %v, want kept %v dropped %v", c.name, keptIDs, dropped, c.kept, c.dropped)
		}
	}
}

func TestValidateNode(t *testing.T) {
	s := &NodeRegistryServer{}
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{NodeID: "node-1", AllowedModels: []string{"tiny:1b"}})

	resp, err := s.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: "node-1", Models: specs("tiny:1b", "gemma4:latest")})
	if err != nil {
		t.Fatalf("ValidateNode: %v", err)
	}
	if !reflect.DeepEqual(resp.GetAcceptedModels(), []string{"tiny:1b"}) || !reflect.DeepEqual(resp.GetDroppedModels(), []string{"gemma4:latest"}) {
		t.Errorf("accepted %v dropped %v", resp.GetAcceptedModels(), resp.GetDroppedModels())
	}

	if _, err := s.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: "node-1", Models: specs("gemma4:latest")}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("only forbidden models: code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := s.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: "other"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("wrong node id: code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := s.ValidateNode(context.Background(), &pb.ValidateNodeRequest{NodeId: "node-1"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no identity: code = %v, want Unauthenticated", status.Code(err))
	}
}

func newRegistry(t *testing.T) (*NodeRegistryServer, *nodes.Manager) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := nodes.NewStore(rdb, rediskeys.New("test"))
	mgr := nodes.NewManager(store, nodes.NewLeaseManager(rdb, store),
		nodes.NewCircuitBreaker(rdb, store, 3, time.Second),
		nodes.NewTelemetryTracker(rdb, store, 0.2, time.Hour),
		nodes.Config{HeartbeatInterval: time.Second, HeartbeatTTL: 5 * time.Second, RetentionTTL: time.Minute, AllowLocalhost: true})
	return NewNodeRegistryServer(mgr, nil), mgr
}

func snapshotOf(t *testing.T, mgr *nodes.Manager, id string) *nodes.NodeSnapshot {
	t.Helper()
	all, err := mgr.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range all {
		if n.Meta.NodeID == id {
			return n
		}
	}
	t.Fatalf("node %q not listed", id)
	return nil
}

func modelIDs(ms []*pb.NodeModelSpec) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.GetModelId())
	}
	return out
}

func TestRegisterAndHeartbeatDropDisallowedModels(t *testing.T) {
	s, mgr := newRegistry(t)
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{NodeID: "node-1", AllowedModels: []string{"tiny:1b"}})

	_, err := s.RegisterNode(ctx, &pb.RegisterNodeRequest{NodeId: "node-1", Endpoint: "http://127.0.0.1:9", Models: specs("tiny:1b", "gemma4:latest")})
	if err != nil {
		t.Fatalf("RegisterNode must keep the node and drop the extra model: %v", err)
	}
	if got := modelIDs(snapshotOf(t, mgr, "node-1").Meta.Models); !reflect.DeepEqual(got, []string{"tiny:1b"}) {
		t.Errorf("registered models = %v, want only tiny:1b", got)
	}

	if _, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", Models: []*pb.NodeModelSpec{{ModelId: "tiny:1b", IsWarm: true}, {ModelId: "gemma4:latest", IsWarm: true}}}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	hb := snapshotOf(t, mgr, "node-1").Heartbeat
	if !reflect.DeepEqual(hb.WarmModels, []string{"tiny:1b"}) || !reflect.DeepEqual(modelIDs(hb.ModelSpecs), []string{"tiny:1b"}) {
		t.Errorf("heartbeat stored warm %v specs %v, want only tiny:1b", hb.WarmModels, modelIDs(hb.ModelSpecs))
	}

	if _, err := s.RegisterNode(ctx, &pb.RegisterNodeRequest{NodeId: "node-1", Endpoint: "http://127.0.0.1:9", Models: specs("gemma4:latest")}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("only disallowed models: code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", Models: specs("gemma4:latest")}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("heartbeat with only disallowed models: code = %v, want PermissionDenied", status.Code(err))
	}
}

// Node auth tokens live in Redis under the same keyring; rotation must move
// them to the active version so the old one can be dropped.
func TestRotateNodeTokens(t *testing.T) {
	_, mgr := newRegistry(t)
	ctx := context.Background()
	old := keyring(t, 1)
	ct, v, err := old.Encrypt("node-secret")
	if err != nil {
		t.Fatal(err)
	}
	meta := &nodes.NodeMeta{NodeID: "n1", Endpoint: "http://127.0.0.1:1", AuthToken: crypto.EncodeVersioned(ct, v)}
	if err := mgr.Store().SaveMeta(ctx, meta, time.Minute); err != nil {
		t.Fatal(err)
	}
	s := &AdminServer{keyring: keyring(t, 1, 2), nodeMgr: mgr}
	n, err := s.rotateNodeTokens(ctx, 2)
	if err != nil || n != 1 {
		t.Fatalf("rotateNodeTokens = %d, %v", n, err)
	}
	got, err := mgr.Store().GetMeta(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := keyring(t, 2).DecodeVersioned(got.AuthToken); err != nil || plain != "node-secret" {
		t.Fatalf("token after rotation decodes to %q, %v with version 2 alone", plain, err)
	}
	if n, _ := s.rotateNodeTokens(ctx, 2); n != 0 {
		t.Fatalf("second rotation changed %d tokens", n)
	}
}

func TestHeartbeatCapacityPercent(t *testing.T) {
	s, mgr := newRegistry(t)
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{NodeID: "node-1"})
	if _, err := s.RegisterNode(ctx, &pb.RegisterNodeRequest{NodeId: "node-1", Endpoint: "http://127.0.0.1:9", Models: specs("tiny:1b")}); err != nil {
		t.Fatal(err)
	}

	resp, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1"})
	if err != nil || !resp.GetCapacityApplied() {
		t.Fatalf("Heartbeat: resp=%v err=%v, want capacity_applied", resp, err)
	}
	if sn := snapshotOf(t, mgr, "node-1"); sn.Heartbeat.CapacityPercent != nil || sn.Capacity() != 100 {
		t.Errorf("unset capacity stored as %v, want nil (100%%)", sn.Heartbeat.CapacityPercent)
	}

	zero := uint32(0)
	if _, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", CapacityPercent: &zero}); err != nil {
		t.Fatal(err)
	}
	if st := snapshotOf(t, mgr, "node-1").State; st != pb.NodeState_NODE_STATE_PAUSED {
		t.Errorf("state at 0%% = %v, want PAUSED", st)
	}

	over := uint32(101)
	if _, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", CapacityPercent: &over}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("101%%: code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestListNodesReportsLevel(t *testing.T) {
	s, mgr := newRegistry(t)
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{NodeID: "node-1"})
	if _, err := s.RegisterNode(ctx, &pb.RegisterNodeRequest{NodeId: "node-1", Endpoint: "http://127.0.0.1:9", Models: specs("tiny:1b"),
		Resources: &pb.NodeResources{MaxConcurrent: 3}}); err != nil {
		t.Fatal(err)
	}
	half := uint32(50)
	if _, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", CapacityPercent: &half}); err != nil {
		t.Fatal(err)
	}
	// One running request and one cooldown hold: two slots taken, one request.
	if ok, err := mgr.Slots().AcquireLease(ctx, "node-1", "r1", 2, time.Minute); err != nil || !ok {
		t.Fatalf("acquire r1: %v %v", ok, err)
	}
	if ok, err := mgr.Slots().AcquireLease(ctx, "node-1", "r2", 2, time.Minute); err != nil || !ok {
		t.Fatalf("acquire r2: %v %v", ok, err)
	}
	if err := mgr.Slots().ReleaseLease(ctx, "node-1", "r2", time.Minute); err != nil {
		t.Fatal(err)
	}

	admin := &AdminServer{nodeMgr: mgr}
	resp, err := admin.ListNodes(context.Background(), &pb.ListNodesRequest{})
	if err != nil || len(resp.GetNodes()) != 1 {
		t.Fatalf("ListNodes: %v %v", resp, err)
	}
	n := resp.GetNodes()[0]
	if n.GetCapacityPercent() != 50 || n.GetEffectiveSlots() != 2 || n.GetActiveRequests() != 1 {
		t.Errorf("capacity=%d slots=%d active=%d, want 50, 2, 1", n.GetCapacityPercent(), n.GetEffectiveSlots(), n.GetActiveRequests())
	}
	hb, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", CapacityPercent: &half})
	if err != nil || hb.GetActiveProxyRequests() != 1 {
		t.Errorf("heartbeat active_proxy_requests = %d (%v), want 1", hb.GetActiveProxyRequests(), err)
	}
}
