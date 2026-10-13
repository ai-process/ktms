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

	var healthyWarm, healthyCold, suspectWarm, suspectCold, probeCandidates []*Candidate
	var matchingHealthyOrSuspect int

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

		if n.State == pb.NodeState_NODE_STATE_HEALTHY || n.State == pb.NodeState_NODE_STATE_SUSPECT {
			matchingHealthyOrSuspect++
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
		} else if n.State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
			// Probe only if no healthy/suspect candidates exist or to test recovery
			probeAcquired, _ := s.mgr.Circuit().TryAcquireProbe(ctx, n.Meta.NodeID)
			if probeAcquired {
				probeCandidates = append(probeCandidates, cand)
			}
		}
	}

	// Two-stage filtering:
	// Stage 1: HEALTHY over SUSPECT
	// Stage 2: Warm over Cold
	// Stage 3: Circuit broken probe (if no healthy/suspect nodes)
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
	case len(probeCandidates) > 0:
		eligible = probeCandidates
	default:
		if matchingHealthyOrSuspect > 0 {
			return nil, ErrNodePoolFull
		}
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

