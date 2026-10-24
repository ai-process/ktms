package nodes

import (
	"context"
	"math"
	"sync"

	"github.com/rs/zerolog/log"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/registry"
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
// 1. Filter by model support, state (HEALTHY over SUSPECT, probe CIRCUIT_BROKEN when no healthy/suspect).
// 2. Strict warm over cold filter: if any warm node has capacity, cold nodes are eliminated.
// 3. Compute effective weights: W_eff = base_weight * F_perf * F_load.
// 4. Smooth WRR selection.
func (s *Selector) SelectNode(ctx context.Context, modelID string) (*Candidate, error) {
	return s.SelectNodeForCapability(ctx, modelID, "")
}

// SelectNodeForCapability performs candidate filtering and weighted selection,
// optionally filtering for a specific capability (e.g. "judge").
func (s *Selector) SelectNodeForCapability(ctx context.Context, modelID string, capability string) (*Candidate, error) {
	nodes, err := s.mgr.ListNodes(ctx)
	if err != nil {
		return nil, err
	}

	var healthyWarm, healthyCold, suspectWarm, suspectCold, circuitBrokenCandidates []*Candidate
	var matchingHealthyOrSuspect int

	// Reference pool averages for F_perf calculation
	var totalTPS, totalTTFT float64
	var tpsCount, ttftCount int

	for _, n := range nodes {
		for _, m := range n.Meta.Models {
			if m.ModelId == modelID {
				if capability != "" {
					if !hasCapability(m.Capabilities, capability) {
						continue
					}
				} else if isJudgeOnly(m.Capabilities) {
					continue
				}
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
		spec := eligibleSpec(n, modelID, capability)
		if spec == nil {
			continue
		}

		if n.State == pb.NodeState_NODE_STATE_HEALTHY || n.State == pb.NodeState_NODE_STATE_SUSPECT {
			matchingHealthyOrSuspect++
		}

		// Check capacity; cooldown holds count as taken slots
		slots := n.Slots()
		if n.Inflight >= slots {
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

		// F_load: max(0.1, 1 - util^2). Other clients' requests waiting in the
		// engine count as load, so a node they keep busy draws less traffic.
		fLoad := 1.0
		if slots > 0 {
			util := (float64(n.Inflight) + float64(externalLoad(n))) / float64(slots)
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
			circuitBrokenCandidates = append(circuitBrokenCandidates, cand)
		}
	}

	// Two-stage filtering:
	// Stage 1: HEALTHY over SUSPECT
	// Stage 2: Warm over Cold
	// Stage 3: Circuit broken probe (only if no healthy/suspect candidates available)
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
		// When no healthy or suspect nodes are available, try circuit-broken nodes one by one.
		// Stop at the first successfully acquired probe lease so other nodes are not locked.
		var orderedBroken []*Candidate
		for _, c := range circuitBrokenCandidates {
			if c.IsWarm {
				orderedBroken = append(orderedBroken, c)
			}
		}
		for _, c := range circuitBrokenCandidates {
			if !c.IsWarm {
				orderedBroken = append(orderedBroken, c)
			}
		}
		for _, cand := range orderedBroken {
			probeAcquired, err := s.mgr.Circuit().TryAcquireProbe(ctx, cand.Snapshot.Meta.NodeID)
			if err != nil {
				log.Warn().Err(err).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("nodes: failed to acquire circuit probe lease")
				continue
			}
			if probeAcquired {
				s.mu.Lock()
				s.curWeight[cand.Snapshot.Meta.NodeID] = 0
				s.mu.Unlock()
				return cand, nil
			}
		}
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

// eligibleSpec is the node's spec for modelID if the node may take it at all,
// before capacity and load balancing; nil otherwise.
func eligibleSpec(n *NodeSnapshot, modelID, capability string) *pb.NodeModelSpec {
	if n.State == pb.NodeState_NODE_STATE_OFFLINE ||
		n.State == pb.NodeState_NODE_STATE_DRAINING ||
		n.State == pb.NodeState_NODE_STATE_PAUSED {
		return nil
	}
	for _, m := range n.Meta.Models {
		if m.ModelId != modelID {
			continue
		}
		if capability != "" {
			if !hasCapability(m.Capabilities, capability) {
				return nil
			}
		} else if isJudgeOnly(m.Capabilities) {
			return nil
		}
		return m
	}
	return nil
}

// Serves reports whether some node may take modelID now, by the same filter
// SelectNode applies; ExplainRoute asks it instead of guessing.
func Serves(ctx context.Context, mgr interface {
	ListNodes(context.Context) ([]*NodeSnapshot, error)
}, modelID string) (bool, error) {
	nodes, err := mgr.ListNodes(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if eligibleSpec(n, modelID, "") != nil {
			return true, nil
		}
	}
	return false, nil
}

func hasCapability(caps []string, target string) bool {
	for _, c := range caps {
		if c == target {
			return true
		}
	}
	return false
}

func isJudgeOnly(caps []string) bool {
	return len(caps) == 1 && caps[0] == registry.CapabilityJudge
}
