package nodes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/rediskeys"
)

func setupTestManager(t *testing.T) (*Manager, *miniredis.Miniredis) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rkeys := rediskeys.New("test")
	store := NewStore(rdb, rkeys)
	slots := NewLeaseManager(rdb, store)
	circuit := NewCircuitBreaker(rdb, store, 3, 10*time.Second)
	telemetry := NewTelemetryTracker(rdb, store, 0.2, 1*time.Hour)

	mgr := NewManager(store, slots, circuit, telemetry, Config{
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTTL:      500 * time.Millisecond,
		RetentionTTL:      2 * time.Second,
		AllowLocalhost:    true,
	})
	return mgr, mr
}

func TestRegisterAndHeartbeatLifecycle(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	meta := &NodeMeta{
		NodeID:        "node-1",
		Name:          "GPU Node 1",
		Endpoint:      "http://127.0.0.1:11434",
		Engine:        "ollama",
		BaseWeight:    100,
		MaxConcurrent: 8,
		Models: []*pb.NodeModelSpec{
			{ModelId: "qwen2.5:7b", RemoteModelName: "qwen2.5:7b", ContextLength: 8192, IsWarm: true},
		},
	}

	if err := mgr.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}

	hb := &NodeHeartbeatData{
		NodeID:         "node-1",
		ActiveRequests: 1,
		WarmModels:     []string{"qwen2.5:7b"},
	}

	draining, active, err := mgr.Heartbeat(ctx, hb)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if draining {
		t.Error("expected draining to be false")
	}
	if active != 0 {
		t.Errorf("expected 0 active proxy leases, got %d", active)
	}

	// Verify state is HEALTHY
	st, err := mgr.GetNodeState(ctx, "node-1", hb)
	if err != nil || st != pb.NodeState_NODE_STATE_HEALTHY {
		t.Fatalf("expected HEALTHY state, got %v (err: %v)", st, err)
	}

	// Trigger manual drain
	if err := mgr.Drain(ctx, "node-1"); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	st, err = mgr.GetNodeState(ctx, "node-1", hb)
	if err != nil || st != pb.NodeState_NODE_STATE_DRAINING {
		t.Fatalf("expected DRAINING state after Drain, got %v", st)
	}
}

func TestSmoothWRRSelectorWithWarmColdFiltering(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	// Node 1: warm, weight 100
	_ = mgr.Register(ctx, &NodeMeta{
		NodeID:        "node-warm",
		Name:          "Warm Node",
		Endpoint:      "http://127.0.0.1:11434",
		Engine:        "ollama",
		BaseWeight:    100,
		MaxConcurrent: 10,
		Models: []*pb.NodeModelSpec{
			{ModelId: "qwen2.5:7b", IsWarm: true},
		},
	})
	_ = mgr.store.SaveHeartbeat(ctx, &NodeHeartbeatData{
		NodeID:        "node-warm",
		LastHeartbeat: time.Now(),
		WarmModels:    []string{"qwen2.5:7b"},
	}, 10*time.Second)

	// Node 2: cold, weight 200 (higher weight but cold)
	_ = mgr.Register(ctx, &NodeMeta{
		NodeID:        "node-cold",
		Name:          "Cold Node",
		Endpoint:      "http://127.0.0.1:11435",
		Engine:        "ollama",
		BaseWeight:    200,
		MaxConcurrent: 10,
		Models: []*pb.NodeModelSpec{
			{ModelId: "qwen2.5:7b", IsWarm: false},
		},
	})
	_ = mgr.store.SaveHeartbeat(ctx, &NodeHeartbeatData{
		NodeID:        "node-cold",
		LastHeartbeat: time.Now(),
	}, 10*time.Second)

	sel := NewSelector(mgr)

	// Because warm node has available capacity, cold node should NEVER be selected
	for i := 0; i < 5; i++ {
		cand, err := sel.SelectNode(ctx, "qwen2.5:7b")
		if err != nil {
			t.Fatalf("SelectNode: %v", err)
		}
		if cand.Snapshot.Meta.NodeID != "node-warm" {
			t.Fatalf("expected node-warm to be selected over cold node, got %s", cand.Snapshot.Meta.NodeID)
		}
	}
}

func TestInflightZSetSlotLeases(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)

	nodeID := "node-slots"
	maxConc := int32(2)

	// Acquire slot 1
	acq1, err := mgr.Slots().AcquireLease(ctx, nodeID, "req-1", maxConc, 5*time.Second)
	if err != nil || !acq1 {
		t.Fatalf("AcquireLease req-1 failed: %v", err)
	}

	// Acquire slot 2
	acq2, err := mgr.Slots().AcquireLease(ctx, nodeID, "req-2", maxConc, 5*time.Second)
	if err != nil || !acq2 {
		t.Fatalf("AcquireLease req-2 failed: %v", err)
	}

	// Slot 3 should be exhausted
	acq3, err := mgr.Slots().AcquireLease(ctx, nodeID, "req-3", maxConc, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLease req-3 error: %v", err)
	}
	if acq3 {
		t.Fatal("expected slot 3 to be rejected due to max concurrency")
	}

	// Release req-1
	if err := mgr.Slots().ReleaseLease(ctx, nodeID, "req-1"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	// Now req-3 should succeed
	acq3After, err := mgr.Slots().AcquireLease(ctx, nodeID, "req-3", maxConc, 5*time.Second)
	if err != nil || !acq3After {
		t.Fatalf("AcquireLease req-3 after release failed: %v", err)
	}
}

func TestCircuitBreakerTripsAndProbes(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	nodeID := "node-cb"

	cb := mgr.Circuit()
	broken, _ := cb.IsBroken(ctx, nodeID)
	if broken {
		t.Fatal("expected initial circuit breaker to not be broken")
	}

	// Record 3 failures (threshold is 3)
	_ = cb.RecordFailure(ctx, nodeID)
	_ = cb.RecordFailure(ctx, nodeID)
	_ = cb.RecordFailure(ctx, nodeID)

	broken, err := cb.IsBroken(ctx, nodeID)
	if err != nil || !broken {
		t.Fatalf("expected circuit breaker to be broken, got %v (err: %v)", broken, err)
	}

	// Half-open probe attempt
	probed1, err := cb.TryAcquireProbe(ctx, nodeID)
	if err != nil || !probed1 {
		t.Fatalf("expected first probe to succeed: %v", err)
	}

	// Second concurrent probe should fail
	probed2, err := cb.TryAcquireProbe(ctx, nodeID)
	if err != nil || probed2 {
		t.Fatalf("expected second concurrent probe to fail, got %v", probed2)
	}

	// Success clears the breaker
	_ = cb.RecordSuccess(ctx, nodeID)
	brokenAfter, _ := cb.IsBroken(ctx, nodeID)
	if brokenAfter {
		t.Fatal("expected circuit breaker to be reset after success")
	}
}

func TestTelemetryEWMAAtomicLua(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	nodeID := "node-telemetry"
	modelID := "test-model"

	tt := mgr.Telemetry()

	// Initial metrics
	_ = tt.RecordMetrics(ctx, nodeID, modelID, 50.0, 100.0)
	p1, err := tt.GetPerf(ctx, nodeID, modelID)
	if err != nil || p1 == nil {
		t.Fatalf("GetPerf: %v", err)
	}
	if p1.TPSEMA != 50.0 || p1.TTFTEMAMs != 100.0 {
		t.Fatalf("unexpected initial perf: %+v", p1)
	}

	// Second update (alpha = 0.2): new_tps = 0.2*100 + 0.8*50 = 60
	_ = tt.RecordMetrics(ctx, nodeID, modelID, 100.0, 50.0)
	p2, err := tt.GetPerf(ctx, nodeID, modelID)
	if err != nil || p2 == nil {
		t.Fatalf("GetPerf 2: %v", err)
	}
	if p2.TPSEMA != 60.0 {
		t.Errorf("expected TPS EMA 60.0, got %f", p2.TPSEMA)
	}
}

func TestPoolAdapter_SlotWaitAndAdmission(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	nodeID := "node-wait-test"
	modelID := "local-llama"

	meta := &NodeMeta{
		NodeID:        nodeID,
		Name:          "node-wait",
		Endpoint:      "http://localhost:8000",
		Engine:        "vllm",
		BaseWeight:    10.0,
		MaxConcurrent: 1, // Only 1 slot
		Models: []*pb.NodeModelSpec{
			{ModelId: modelID, IsWarm: true},
		},
	}
	if err := mgr.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, _, _ = mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: nodeID})

	selector := NewSelector(mgr)
	pm := &proxydb.Model{
		ID:             modelID,
		QueueTimeoutMs: 150,
	}
	pa := NewPoolAdapter(selector, nil, pm, 60*time.Second, nil)
	_ = mgr.Telemetry().RecordMetrics(ctx, nodeID, modelID, 1000.0, 50.0)

	// Fill the 1 available slot
	firstReqID := "req-1"
	acquired, err := mgr.Slots().AcquireLease(ctx, nodeID, firstReqID, 1, 60*time.Second)
	if err != nil || !acquired {
		t.Fatalf("failed to fill slot: %v, %v", acquired, err)
	}

	// 1. Test successful wait: slot is released in background after 40ms
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = mgr.Slots().ReleaseLease(context.Background(), nodeID, firstReqID)
	}()

	start := time.Now()
	cand, reqID, err := pa.acquireSlotWithWait(ctx, llm.NewChatContext())
	if err != nil {
		t.Fatalf("acquireSlotWithWait should succeed after slot release: %v", err)
	}
	if cand == nil || reqID == "" {
		t.Fatal("expected valid candidate and reqID")
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Errorf("expected to wait for slot release, but returned in %v", time.Since(start))
	}

	// 2. Test timeout: slot is occupied and NOT released
	timeoutPM := &proxydb.Model{
		ID:             modelID,
		QueueTimeoutMs: 60,
	}
	paTimeout := NewPoolAdapter(selector, nil, timeoutPM, 60*time.Second, nil)
	start = time.Now()
	_, _, err = paTimeout.acquireSlotWithWait(ctx, llm.NewChatContext())
	if !errors.Is(err, ErrNodeSlotExhausted) {
		t.Fatalf("expected ErrNodeSlotExhausted on timeout, got %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Errorf("expected to wait near timeout duration, but returned in %v", time.Since(start))
	}

	// 3. Test predictive admission drop:
	// Set huge QueueDepth on heartbeat so Wait_est >> timeout
	_, _, _ = mgr.Heartbeat(ctx, &NodeHeartbeatData{
		NodeID:     nodeID,
		QueueDepth: 100, // 100 requests in queue
	})
	_ = mgr.Telemetry().RecordMetrics(ctx, nodeID, modelID, 10.0, 5000.0) // 5000ms TTFT
	start = time.Now()
	_, _, err = paTimeout.acquireSlotWithWait(ctx, llm.NewChatContext())
	if !errors.Is(err, ErrNodeSlotExhausted) {
		t.Fatalf("expected predictive drop ErrNodeSlotExhausted, got %v", err)
	}
	// Must return almost immediately (< 20ms), not waiting the full 60ms timeout!
	if time.Since(start) > 40*time.Millisecond {
		t.Errorf("predictive drop took too long: %v", time.Since(start))
	}
}

func TestPoolAdapter_NoNodesFastFallback(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	selector := NewSelector(mgr)
	pm := &proxydb.Model{
		ID:             "non-existent-model",
		QueueTimeoutMs: 5000,
	}
	pa := NewPoolAdapter(selector, nil, pm, 60*time.Second, nil)
	start := time.Now()
	_, _, err := pa.acquireSlotWithWait(ctx, llm.NewChatContext())
	if !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("expected ErrNoAvailableNodes, got %v", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("fast fallback should not wait, took %v", time.Since(start))
	}
}
