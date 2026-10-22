package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ai-process/ktms/internal/rediskeys"
)

type nodeCapacityError struct {
	msg string
}

func (e *nodeCapacityError) Error() string           { return e.msg }
func (e *nodeCapacityError) SkipToNextModel() bool   { return true }
func (e *nodeCapacityError) CapacityExhausted() bool { return true }

// poolStateError is a node-pool bookkeeping failure (Redis down or erroring).
// The pool's state is unknown, not its nodes broken: the chain moves on to the
// next model and the vendor breaker is left alone.
type poolStateError struct {
	op  string
	err error
}

func (e *poolStateError) Error() string {
	return "nodes: pool state unavailable (" + e.op + "): " + e.err.Error()
}
func (e *poolStateError) Unwrap() error           { return e.err }
func (e *poolStateError) SkipToNextModel() bool   { return true }
func (e *poolStateError) CapacityExhausted() bool { return true }

var (
	ErrNodeNotFound      = errors.New("nodes: node not found")
	ErrInvalidNode       = errors.New("nodes: invalid registration")   // the node's fault, not the pool's
	ErrCorruptNode       = errors.New("nodes: unreadable node record") // Redis answered; the data is bad
	ErrNodeSlotExhausted = &nodeCapacityError{msg: "nodes: max concurrent requests reached"}
	ErrNodePoolFull      = &nodeCapacityError{msg: "nodes: all candidate nodes at capacity"}
	ErrNoAvailableNodes  = &nodeCapacityError{msg: "nodes: no healthy nodes available for model"}
)

// Store interacts with Redis to persist and query node status.
type Store struct {
	rdb  redis.UniversalClient
	keys rediskeys.Keys
}

func NewStore(rdb redis.UniversalClient, keys rediskeys.Keys) *Store {
	return &Store{rdb: rdb, keys: keys}
}

// SaveMeta saves static node metadata and adds nodeID to nodes:all set.
func (s *Store) SaveMeta(ctx context.Context, meta *NodeMeta, ttl time.Duration) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal node meta: %w", err)
	}

	pipe := s.rdb.Pipeline()
	pipe.SAdd(ctx, s.keys.NodesSet(), meta.NodeID)
	pipe.Set(ctx, s.keys.NodeMeta(meta.NodeID), data, ttl)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("save node meta: %w", err)
	}
	return nil
}

// GetMeta retrieves node metadata.
func (s *Store) GetMeta(ctx context.Context, nodeID string) (*NodeMeta, error) {
	val, err := s.rdb.Get(ctx, s.keys.NodeMeta(nodeID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get node meta: %w", err)
	}
	var meta NodeMeta
	if err := json.Unmarshal(val, &meta); err != nil {
		return nil, fmt.Errorf("%w: unmarshal node meta: %w", ErrCorruptNode, err)
	}
	return &meta, nil
}

// UpdateMeta rewrites a node's meta in place when fn reports a change. It
// keeps the remaining TTL and, through WATCH, gives up rather than overwrite
// a registration or removal that happened meanwhile; a missing node is left
// missing.
func (s *Store) UpdateMeta(ctx context.Context, nodeID string, fn func(*NodeMeta) bool) (bool, error) {
	key := s.keys.NodeMeta(nodeID)
	changed := false
	err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
		val, err := tx.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		var meta NodeMeta
		if err := json.Unmarshal(val, &meta); err != nil {
			return fmt.Errorf("%w: unmarshal node meta: %w", ErrCorruptNode, err)
		}
		if !fn(&meta) {
			return nil
		}
		data, err := json.Marshal(&meta)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.SetArgs(ctx, key, data, redis.SetArgs{KeepTTL: true})
			return nil
		})
		changed = err == nil
		return err
	}, key)
	if errors.Is(err, redis.TxFailedErr) {
		return false, nil // changed under us: the newer write wins
	}
	return changed, err
}

// SaveHeartbeat updates the dynamic heartbeat record with TTL.
func (s *Store) SaveHeartbeat(ctx context.Context, hb *NodeHeartbeatData, ttl time.Duration) error {
	data, err := json.Marshal(hb)
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}
	err = s.rdb.Set(ctx, s.keys.NodeHeartbeat(hb.NodeID), data, ttl).Err()
	if err != nil {
		return fmt.Errorf("save heartbeat: %w", err)
	}
	return nil
}

// GetHeartbeat retrieves node heartbeat.
func (s *Store) GetHeartbeat(ctx context.Context, nodeID string) (*NodeHeartbeatData, error) {
	val, err := s.rdb.Get(ctx, s.keys.NodeHeartbeat(nodeID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil // No active heartbeat
	}
	if err != nil {
		return nil, fmt.Errorf("get heartbeat: %w", err)
	}
	var hb NodeHeartbeatData
	if err := json.Unmarshal(val, &hb); err != nil {
		return nil, fmt.Errorf("%w: unmarshal heartbeat: %w", ErrCorruptNode, err)
	}
	return &hb, nil
}

// SetDrain marks a node for draining.
func (s *Store) SetDrain(ctx context.Context, nodeID string) error {
	return s.rdb.Set(ctx, s.keys.NodeDrain(nodeID), "1", 0).Err()
}

// IsDraining checks if node is marked for draining.
func (s *Store) IsDraining(ctx context.Context, nodeID string) (bool, error) {
	val, err := s.rdb.Exists(ctx, s.keys.NodeDrain(nodeID)).Result()
	if err != nil {
		return false, err
	}
	return val > 0, nil
}

// ClearDrain removes draining flag.
func (s *Store) ClearDrain(ctx context.Context, nodeID string) error {
	return s.rdb.Del(ctx, s.keys.NodeDrain(nodeID)).Err()
}

// GetAllNodeIDs returns all node IDs in the pool.
func (s *Store) GetAllNodeIDs(ctx context.Context) ([]string, error) {
	return s.rdb.SMembers(ctx, s.keys.NodesSet()).Result()
}

// RemoveNode completely evicts a node from Redis.
func (s *Store) RemoveNode(ctx context.Context, nodeID string) error {
	pipe := s.rdb.Pipeline()
	pipe.SRem(ctx, s.keys.NodesSet(), nodeID)
	pipe.Del(ctx, s.keys.NodeMeta(nodeID))
	pipe.Del(ctx, s.keys.NodeHeartbeat(nodeID))
	pipe.Del(ctx, s.keys.NodeInflight(nodeID))
	pipe.Del(ctx, s.keys.NodeDrain(nodeID))
	_, err := pipe.Exec(ctx)
	return err
}

// GetInflightCount returns the number of active leases in the ZSET.
func (s *Store) GetInflightCount(ctx context.Context, nodeID string, now time.Time) (int32, error) {
	// First clean up expired leases
	pipe := s.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, s.keys.NodeInflight(nodeID), "-inf", fmt.Sprintf("%d", now.UnixMilli()))
	card := pipe.ZCard(ctx, s.keys.NodeInflight(nodeID))
	_, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return int32(card.Val()), nil
}

// Keys returns the configured rediskeys.Keys.
func (s *Store) Keys() rediskeys.Keys {
	return s.keys
}
