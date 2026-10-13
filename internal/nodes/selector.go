package nodes

import (
	"context"
	"math"
	"sync"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

type Candidate struct {
	Snapshot        *NodeSnapshot
	ModelSpec       *pb.NodeModelSpec
	EffectiveWeight float64
	IsWarm          bool
}

type Selector struct {
	mgr       *Manager
	mu        sync.Mutex
	curWeight map[string]float64 // nodeID -> current smooth WRR weight
}

func NewSelector(mgr *Manager) *Selector {
	return &Selector{
		mgr:       mgr,
		curWeight: make(map[string]float64),
	}
}

// SelectNode performs candidate filtering and weighted selection:
// 1. Filter by model support, state (HEALTHY over SUSPECT, exclude DRAINING/OFFLINE/CIRCUIT_BROKEN), free slots.
// 2. Strict warm over cold filter: if any warm node has capacity, cold nodes are eliminated.
// 3. Compute effective weights: W_eff = base_weight * F_perf * F_load.
// 4. Smooth WRR selection.
func (s *Selector) SelectNode(ctx context.Context, modelID string) (*Candidate, error) {
	nodes, err := s.mgr.ListNodes(ctx)
	if err != nil {
		return nil, err
	}

	var healthyWarm, healthyCold, suspectWarm, suspectCold []*Candidate

	// Reference pool averages for F_perf calculation
	var totalTPS, totalTTFT float64
	var tpsCount, ttftCount int

	for _, n := range nodes {
		for _, m := range n.Meta.Models {
			if m.ModelId == modelID {
				if perf, ok := n.Perf[modelID]; ok {
					if perf.TPSEMA > 0 {
						totalTPS += perf.TPSEMA
						tpsCount++
					}
					if perf.TTFTEMAMs > 0 {
						totalTTFT += perf.TTFTEMAMs
						ttftCount++
					}
				}
			}
		}
	}

	avgTPS := 0.0
	if tpsCount > 0 {
		avgTPS = totalTPS / float64(tpsCount)
	}
	avgTTFT := 0.0
	if ttftCount > 0 {
		avgTTFT = totalTTFT / float64(ttftCount)
	}

	for _, n := range nodes {
		if n.State == pb.NodeState_NODE_STATE_OFFLINE ||
			n.State == pb.NodeState_NODE_STATE_DRAINING {
			continue
		}
		if n.State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
			// Check if half-open probe can be acquired
			probeAcquired, _ := s.mgr.Circuit().TryAcquireProbe(ctx, n.Meta.NodeID)
			if !probeAcquired {
				continue
			}
		}

		// Find model spec
		var spec *pb.NodeModelSpec
		for _, m := range n.Meta.Models {
			if m.ModelId == modelID {
				spec = m
				break
			}
		}
		if spec == nil {
			continue
		}

		// Check capacity
		if n.Inflight >= n.Meta.MaxConcurrent {
			continue
		}

		// Check warm status
		isWarm := spec.IsWarm
		if n.Heartbeat != nil {
			for _, wm := range n.Heartbeat.WarmModels {
				if wm == modelID {
					isWarm = true
					break
				}
			}
		}

		// Calculate F_perf: clamp(0.7 * (TPS / avgTPS) + 0.3 * (avgTTFT / TTFT), 0.1, 2.0)
		fPerf := 1.0
		if perf, ok := n.Perf[modelID]; ok {
			tpsRatio := 1.0
			if avgTPS > 0 && perf.TPSEMA > 0 {
				tpsRatio = perf.TPSEMA / avgTPS
			}
			ttftRatio := 1.0
			if avgTTFT > 0 && perf.TTFTEMAMs > 0 {
				ttftRatio = avgTTFT / perf.TTFTEMAMs
			}
			fPerf = math.Max(0.1, math.Min(2.0, 0.7*tpsRatio+0.3*ttftRatio))
		}

		// Calculate F_load: max(0.1, 1 - (inflight / max_concurrent)^2)
		fLoad := 1.0
		if n.Meta.MaxConcurrent > 0 {
			util := float64(n.Inflight) / float64(n.Meta.MaxConcurrent)
			fLoad = math.Max(0.1, 1.0-util*util)
		}

		effWeight := n.Meta.BaseWeight * fPerf * fLoad
		if effWeight <= 0 {
			effWeight = 1.0
		}

		cand := &Candidate{
			Snapshot:        n,
			ModelSpec:       spec,
			EffectiveWeight: effWeight,
			IsWarm:          isWarm,
		}

		if n.State == pb.NodeState_NODE_STATE_HEALTHY {
			if isWarm {
				healthyWarm = append(healthyWarm, cand)
			} else {
				healthyCold = append(healthyCold, cand)
			}
		} else if n.State == pb.NodeState_NODE_STATE_SUSPECT {
			if isWarm {
				suspectWarm = append(suspectWarm, cand)
			} else {
				suspectCold = append(suspectCold, cand)
			}
		}
	}

	// Two-stage filtering:
	// Stage 1: HEALTHY over SUSPECT
	// Stage 2: Warm over Cold
	var eligible []*Candidate
	switch {
	case len(healthyWarm) > 0:
		eligible = healthyWarm
	case len(healthyCold) > 0:
		eligible = healthyCold
	case len(suspectWarm) > 0:
		eligible = suspectWarm
	case len(suspectCold) > 0:
		eligible = suspectCold
	default:
		return nil, ErrNoAvailableNodes
	}

	// Smooth Weighted Round Robin
	s.mu.Lock()
	defer s.mu.Unlock()

	var totalWeight float64
	var best *Candidate
	var maxCur float64

	for i, c := range eligible {
		totalWeight += c.EffectiveWeight
		s.curWeight[c.Snapshot.Meta.NodeID] += c.EffectiveWeight
		cur := s.curWeight[c.Snapshot.Meta.NodeID]
		if i == 0 || cur > maxCur {
			maxCur = cur
			best = c
		}
	}

	if best != nil {
		s.curWeight[best.Snapshot.Meta.NodeID] -= totalWeight
	}
	return best, nil
}

// PoolCapacity aggregates total active slots and average duration EMA for a model.
func (s *Selector) PoolCapacity(ctx context.Context, modelID string) (totalSlots int32, avgDurationMs float64) {
	nodes, err := s.mgr.ListNodes(ctx)
	if err != nil || len(nodes) == 0 {
		return 1, 2000
	}
	var slots int32
	var totalDuration float64
	var count int
	for _, n := range nodes {
		if n.State == pb.NodeState_NODE_STATE_OFFLINE || n.State == pb.NodeState_NODE_STATE_DRAINING || n.State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
			continue
		}
		for _, m := range n.Meta.Models {
			if m.ModelId == modelID {
				slots += n.Meta.MaxConcurrent
				if perf, ok := n.Perf[modelID]; ok && perf.TPSEMA > 0 {
					dur := (100.0 / perf.TPSEMA) * 1000.0
					if dur < 500 {
						dur = 500
					}
					if dur > 10000 {
						dur = 10000
					}
					totalDuration += dur
					count++
				}
				break
			}
		}
	}
	if slots <= 0 {
		slots = 1
	}
	if count > 0 {
		avgDurationMs = totalDuration / float64(count)
	} else {
		avgDurationMs = 2000
	}
	return slots, avgDurationMs
}

