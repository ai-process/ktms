package nodes

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func pct(p uint32) *uint32 { return &p }

func snapshotAt(maxConc int32, p *uint32) *NodeSnapshot {
	return &NodeSnapshot{
		Meta:      &NodeMeta{NodeID: "n", MaxConcurrent: maxConc},
		Heartbeat: &NodeHeartbeatData{NodeID: "n", CapacityPercent: p},
	}
}

func TestCapacitySlotsAndCooldown(t *testing.T) {
	const T = 20 * time.Second
	cases := []struct {
		name     string
		maxConc  int32
		p        *uint32
		slots    int32
		cooldown time.Duration
	}{
		{"one slot at half", 1, pct(50), 1, T},
		{"four slots at half", 4, pct(50), 2, 0},
		{"three slots at half", 3, pct(50), 2, T / 3},
		{"paused", 1, pct(0), 0, 0},
		{"full", 8, pct(100), 8, 0},
		{"old agent", 8, nil, 8, 0},
		{"above 100 reads as full", 2, pct(150), 2, 0},
		{"tiny level caps the cooldown", 1, pct(1), 1, maxCooldown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := snapshotAt(c.maxConc, c.p)
			if got := n.Slots(); got != c.slots {
				t.Errorf("Slots() = %d, want %d", got, c.slots)
			}
			if got := n.Cooldown(T); got != c.cooldown {
				t.Errorf("Cooldown(%v) = %v, want %v", T, got, c.cooldown)
			}
		})
	}
	if got := (&NodeSnapshot{Meta: &NodeMeta{MaxConcurrent: 4}}).Slots(); got != 4 {
		t.Errorf("no heartbeat: Slots() = %d, want 4", got)
	}
}

func TestGetNodeStatePausedPriority(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	hb := &NodeHeartbeatData{NodeID: "n", LastHeartbeat: time.Now(), CapacityPercent: pct(0)}

	for range 3 {
		if err := mgr.Circuit().RecordFailure(ctx, "n"); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := mgr.GetNodeState(ctx, "n", hb); err != nil || st != pb.NodeState_NODE_STATE_PAUSED {
		t.Fatalf("paused and broken: state = %v (%v), want PAUSED", st, err)
	}
	if err := mgr.Drain(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if st, err := mgr.GetNodeState(ctx, "n", hb); err != nil || st != pb.NodeState_NODE_STATE_DRAINING {
		t.Fatalf("paused and draining: state = %v (%v), want DRAINING", st, err)
	}
}

func registerWithLevel(t *testing.T, mgr *Manager, id string, maxConc int32, p *uint32) {
	t.Helper()
	ctx := context.Background()
	if err := mgr.Register(ctx, &NodeMeta{
		NodeID: id, Endpoint: "http://127.0.0.1:11434", Engine: "ollama", BaseWeight: 100, MaxConcurrent: maxConc,
		Models: []*pb.NodeModelSpec{{ModelId: "m", IsWarm: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.store.SaveHeartbeat(ctx, &NodeHeartbeatData{
		NodeID: id, LastHeartbeat: time.Now(), WarmModels: []string{"m"}, CapacityPercent: p,
	}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSelectorSkipsPausedAndHonorsSlots(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	sel := NewSelector(mgr)

	registerWithLevel(t, mgr, "paused", 4, pct(0))
	if _, err := sel.SelectNode(ctx, "m"); !errors.Is(err, ErrNoAvailableNodes) {
		t.Fatalf("all paused: err = %v, want ErrNoAvailableNodes", err)
	}

	registerWithLevel(t, mgr, "half", 4, pct(50))
	for i := range 2 {
		cand, err := sel.SelectNode(ctx, "m")
		if err != nil || cand.Snapshot.Meta.NodeID != "half" {
			t.Fatalf("pick %d: got %v (%v), want node half", i, cand, err)
		}
		if ok, err := mgr.Slots().AcquireLease(ctx, "half", strconv.Itoa(i), cand.Snapshot.Slots(), time.Minute); err != nil || !ok {
			t.Fatalf("lease %d: ok=%v err=%v", i, ok, err)
		}
	}
	if _, err := sel.SelectNode(ctx, "m"); !errors.Is(err, ErrNodePoolFull) {
		t.Fatalf("two of four slots at 50%%: err = %v, want ErrNodePoolFull", err)
	}
}

func TestReleaseWithCooldownKeepsSlot(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	now := time.UnixMilli(1_000_000)
	mgr.slots.now = func() time.Time { return now }

	if ok, err := mgr.Slots().AcquireLease(ctx, "n", "r1", 1, time.Minute); err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if err := mgr.Slots().ReleaseLease(ctx, "n", "r1", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if n, err := mgr.store.GetActiveCount(ctx, "n", now); err != nil || n != 0 {
		t.Fatalf("active during cooldown = %d (%v), want 0", n, err)
	}
	if ok, _ := mgr.Slots().AcquireLease(ctx, "n", "r2", 1, time.Minute); ok {
		t.Fatal("slot free during cooldown")
	}
	now = now.Add(20*time.Second + time.Millisecond)
	if ok, err := mgr.Slots().AcquireLease(ctx, "n", "r2", 1, time.Minute); err != nil || !ok {
		t.Fatalf("slot still taken after cooldown: ok=%v err=%v", ok, err)
	}
	if n, err := mgr.store.GetActiveCount(ctx, "n", now); err != nil || n != 1 {
		t.Fatalf("active = %d (%v), want 1", n, err)
	}
}

// Under constant demand, a one-slot node at 50% spends half the time busy.
func TestHalfLevelHalvesBusyTime(t *testing.T) {
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	now := time.UnixMilli(1_000_000)
	mgr.slots.now = func() time.Time { return now }
	node := snapshotAt(1, pct(50))

	const (
		reqDur  = 20 * time.Second
		horizon = time.Hour
	)
	var busy time.Duration
	end := now.Add(horizon)
	for i := 0; now.Before(end); i++ {
		reqID := strconv.Itoa(i)
		ok, err := mgr.Slots().AcquireLease(ctx, "n", reqID, node.Slots(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			now = now.Add(time.Second)
			continue
		}
		now = now.Add(reqDur)
		busy += reqDur
		if err := mgr.Slots().ReleaseLease(ctx, "n", reqID, node.Cooldown(reqDur)); err != nil {
			t.Fatal(err)
		}
	}
	if share := float64(busy) / float64(horizon); share < 0.45 || share > 0.55 {
		t.Errorf("busy share = %.2f, want about 0.50", share)
	}
}
