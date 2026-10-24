package nodes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/observability"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

type Config struct {
	HeartbeatInterval time.Duration
	HeartbeatTTL      time.Duration
	RetentionTTL      time.Duration
	AllowLocalhost    bool
	UsageRetention    time.Duration // how long each node's request log is kept; 0 = no log
}

type Manager struct {
	store          *Store
	slots          *LeaseManager
	circuit        *CircuitBreaker
	telemetry      *TelemetryTracker
	usage          *UsageLog
	cfg            Config
	stopCh         chan struct{}
	wg             sync.WaitGroup
	selectorTokens sync.Map // map[string]*nodeWRR
}

func NewManager(store *Store, slots *LeaseManager, circuit *CircuitBreaker, telemetry *TelemetryTracker, cfg Config) *Manager {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.HeartbeatTTL <= 0 {
		cfg.HeartbeatTTL = 15 * time.Second
	}
	if cfg.RetentionTTL <= 0 {
		cfg.RetentionTTL = 24 * time.Hour
	}

	return &Manager{
		store:     store,
		slots:     slots,
		circuit:   circuit,
		telemetry: telemetry,
		usage:     NewUsageLog(store, cfg.UsageRetention),
		cfg:       cfg,
		stopCh:    make(chan struct{}),
	}
}

func (m *Manager) Start(ctx context.Context) {
	m.wg.Add(1)
	go m.sweepLoop(ctx)
}

func (m *Manager) Stop() {
	close(m.stopCh)
	m.wg.Wait()
}

func (m *Manager) Store() *Store                { return m.store }
func (m *Manager) Slots() *LeaseManager         { return m.slots }
func (m *Manager) Circuit() *CircuitBreaker     { return m.circuit }
func (m *Manager) Telemetry() *TelemetryTracker { return m.telemetry }
func (m *Manager) Usage() *UsageLog             { return m.usage }
func (m *Manager) Config() Config               { return m.cfg }

// Register handles node self-registration.
func (m *Manager) Register(ctx context.Context, meta *NodeMeta) error {
	// 1. SSRF validation
	if err := ValidateEndpoint(ctx, meta.Endpoint, m.cfg.AllowLocalhost); err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && (dnsErr.IsTimeout || dnsErr.IsTemporary) {
			return fmt.Errorf("validate endpoint: %w", err) // our resolver's hiccup: the agent should retry
		}
		return fmt.Errorf("%w: validate endpoint: %w", ErrInvalidNode, err)
	}

	meta.RegisteredAt = time.Now()
	// Meta persists with retention TTL
	if err := m.store.SaveMeta(ctx, meta, m.cfg.RetentionTTL); err != nil {
		return fmt.Errorf("save node meta: %w", err)
	}

	// 2. Clear circuit breaker if any
	if recErr := m.circuit.RecordSuccess(ctx, meta.NodeID); recErr != nil {
		log.Warn().Err(recErr).Str("node_id", meta.NodeID).Msg("failed to clear circuit breaker on register")
	}
	return nil
}

// Heartbeat processes node heartbeat and returns current state.
func (m *Manager) Heartbeat(ctx context.Context, hb *NodeHeartbeatData) (drainRequested bool, activeProxyRequests int32, err error) {
	// Check if node is registered
	if _, err := m.store.GetMeta(ctx, hb.NodeID); err != nil {
		return false, 0, err
	}

	// Save heartbeat record with TTL
	hb.LastHeartbeat = time.Now()
	if err := m.store.SaveHeartbeat(ctx, hb, m.cfg.HeartbeatTTL); err != nil {
		return false, 0, err
	}

	// If heartbeat reported draining, mark drain in Redis
	if hb.Draining {
		if setErr := m.store.SetDrain(ctx, hb.NodeID); setErr != nil {
			log.Warn().Err(setErr).Str("node_id", hb.NodeID).Msg("failed to set node drain flag")
		}
	}

	// Check if operator requested drain
	draining, err := m.store.IsDraining(ctx, hb.NodeID)
	if err != nil {
		return false, 0, err
	}

	// Cooldown holds are left out: a pause or drain waits for requests, not for them.
	_, active, err := m.store.GetInflight(ctx, hb.NodeID, hb.LastHeartbeat)
	if err != nil {
		return false, 0, err
	}

	return draining, active, nil
}

// Unregister handles graceful node unregistration.
func (m *Manager) Unregister(ctx context.Context, nodeID string) error {
	return m.store.RemoveNode(ctx, nodeID)
}

// Drain marks a node for draining.
func (m *Manager) Drain(ctx context.Context, nodeID string) error {
	return m.store.SetDrain(ctx, nodeID)
}

// GetNodeState calculates node state based on:
// 1. OFFLINE (heartbeat missing / expired)
// 2. DRAINING (operator requested or self reported)
// 3. PAUSED (the node's schedule is at 0%)
// 4. CIRCUIT_BROKEN (circuit tripped)
// 5. SUSPECT (GPU temp >= 88C or high utilization)
// 6. HEALTHY
func (m *Manager) GetNodeState(ctx context.Context, nodeID string, hb *NodeHeartbeatData) (pb.NodeState, error) {
	if hb == nil {
		return pb.NodeState_NODE_STATE_OFFLINE, nil
	}
	if time.Since(hb.LastHeartbeat) > m.cfg.HeartbeatTTL {
		return pb.NodeState_NODE_STATE_OFFLINE, nil
	}

	isDraining, err := m.store.IsDraining(ctx, nodeID)
	if err != nil {
		return pb.NodeState_NODE_STATE_OFFLINE, fmt.Errorf("read drain flag: %w", err)
	}
	if isDraining || hb.Draining {
		return pb.NodeState_NODE_STATE_DRAINING, nil
	}
	if capacityOf(hb) == 0 {
		return pb.NodeState_NODE_STATE_PAUSED, nil
	}

	broken, err := m.circuit.IsBroken(ctx, nodeID)
	if err != nil {
		return pb.NodeState_NODE_STATE_OFFLINE, fmt.Errorf("read circuit state: %w", err)
	}
	if broken {
		return pb.NodeState_NODE_STATE_CIRCUIT_BROKEN, nil
	}

	if hb.GPUTempC >= 88 || hb.GPUUtilPercent >= 98 {
		return pb.NodeState_NODE_STATE_SUSPECT, nil
	}

	return pb.NodeState_NODE_STATE_HEALTHY, nil
}

// ListNodes builds snapshot list for all known nodes.
func (m *Manager) ListNodes(ctx context.Context) ([]*NodeSnapshot, error) {
	nodeIDs, err := m.store.GetAllNodeIDs(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var out []*NodeSnapshot
	for _, id := range nodeIDs {
		// Unread state is never guessed at (an inflight of 0 bypasses capacity).
		// A Redis error aborts the walk: one timeout, then the caller falls back.
		meta, err := m.store.GetMeta(ctx, id)
		if errors.Is(err, ErrNodeNotFound) {
			continue
		}
		if errors.Is(err, ErrCorruptNode) {
			log.Warn().Err(err).Str("node_id", id).Msg("node record unreadable, skipping node")
			continue
		}
		if err != nil {
			return nil, err
		}
		hb, err := m.store.GetHeartbeat(ctx, id)
		if errors.Is(err, ErrCorruptNode) {
			hb = nil // a garbled heartbeat reads as none: the node shows OFFLINE
		} else if err != nil {
			return nil, err
		}
		st, err := m.GetNodeState(ctx, id, hb)
		if err != nil {
			return nil, err
		}
		inflight, active, err := m.store.GetInflight(ctx, id, now)
		if err != nil {
			return nil, err
		}

		perfMap := make(map[string]*NodePerfData)
		for _, model := range meta.Models {
			p, _ := m.telemetry.GetPerf(ctx, id, model.ModelId)
			if p != nil {
				perfMap[model.ModelId] = p
			}
		}

		out = append(out, &NodeSnapshot{
			Meta:      meta,
			Heartbeat: hb,
			Perf:      perfMap,
			Inflight:  inflight,
			Active:    active,
			State:     st,
		})
	}
	return out, nil
}

func (m *Manager) sweepLoop(ctx context.Context) {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sweep(ctx)
		}
	}
}

// sweep cleans up stale inflight leases and removes nodes inactive past RetentionTTL.
func (m *Manager) sweep(ctx context.Context) {
	nodeIDs, err := m.store.GetAllNodeIDs(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("sweep: failed to list node IDs")
		return
	}

	now := time.Now()
	for _, id := range nodeIDs {
		meta, err := m.store.GetMeta(ctx, id)
		if err != nil && !errors.Is(err, ErrNodeNotFound) && !errors.Is(err, ErrCorruptNode) {
			// Unknown, not expired: evicting on a Redis error would drop live nodes.
			observability.DependencyErrorsTotal.WithLabelValues("redis", "node_sweep").Inc()
			log.Warn().Err(err).Str("node_id", id).Msg("sweep: node meta unreadable, keeping node")
			continue
		}
		if err != nil {
			// Meta expired past RetentionTTL -> remove from all set
			if remErr := m.store.RemoveNode(ctx, id); remErr != nil {
				log.Warn().Err(remErr).Str("node_id", id).Msg("sweep: failed to remove expired node")
			}
			continue
		}

		hb, err := m.store.GetHeartbeat(ctx, id)
		if err != nil && !errors.Is(err, ErrCorruptNode) {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "node_sweep").Inc()
			log.Warn().Err(err).Str("node_id", id).Msg("sweep: node heartbeat unreadable, keeping node")
			continue
		}
		if hb == nil && now.Sub(meta.RegisteredAt) > m.cfg.RetentionTTL {
			log.Info().Str("node_id", id).Msg("sweep: evicting dead node past retention TTL")
			if remErr := m.store.RemoveNode(ctx, id); remErr != nil {
				log.Warn().Err(remErr).Str("node_id", id).Msg("sweep: failed to evict dead node")
			}
			continue
		}

		// Purge expired leases from inflight ZSET
		if _, _, cntErr := m.store.GetInflight(ctx, id, now); cntErr != nil {
			log.Warn().Err(cntErr).Str("node_id", id).Msg("sweep: failed to purge expired leases")
		}
	}
}
