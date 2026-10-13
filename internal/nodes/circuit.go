package nodes

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// CircuitBreaker manages failure tracking and half-open state for nodes.
// When error rate or consecutive errors trip the breaker, state transitions to CIRCUIT_BROKEN.
// A probe request can attempt half-open recovery using Redis SET NX.
type CircuitBreaker struct {
	rdb         redis.UniversalClient
	store       *Store
	failThresh  int
	cooldown    time.Duration
}

func NewCircuitBreaker(rdb redis.UniversalClient, store *Store, failThresh int, cooldown time.Duration) *CircuitBreaker {
	if failThresh <= 0 {
		failThresh = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &CircuitBreaker{
		rdb:        rdb,
		store:      store,
		failThresh: failThresh,
		cooldown:   cooldown,
	}
}

func (cb *CircuitBreaker) failCountKey(nodeID string) string {
	return fmt.Sprintf("%s:fails", cb.store.keys.NodeHeartbeat(nodeID))
}

func (cb *CircuitBreaker) brokenKey(nodeID string) string {
	return fmt.Sprintf("%s:broken", cb.store.keys.NodeHeartbeat(nodeID))
}

func (cb *CircuitBreaker) probeKey(nodeID string) string {
	return fmt.Sprintf("%s:probe", cb.store.keys.NodeHeartbeat(nodeID))
}

// RecordSuccess resets consecutive failure counters and clears circuit broken status.
func (cb *CircuitBreaker) RecordSuccess(ctx context.Context, nodeID string) error {
	pipe := cb.rdb.Pipeline()
	pipe.Del(ctx, cb.failCountKey(nodeID))
	pipe.Del(ctx, cb.brokenKey(nodeID))
	pipe.Del(ctx, cb.probeKey(nodeID))
	_, err := pipe.Exec(ctx)
	return err
}

// RecordFailure increments consecutive failures; trips breaker if threshold exceeded.
func (cb *CircuitBreaker) RecordFailure(ctx context.Context, nodeID string) error {
	key := cb.failCountKey(nodeID)
	val, err := cb.rdb.Incr(ctx, key).Result()
	if err != nil {
		return err
	}
	// Expire failure counter after 2x cooldown
	_ = cb.rdb.Expire(ctx, key, cb.cooldown*2).Err()

	if int(val) >= cb.failThresh {
		// Trip the breaker
		return cb.rdb.Set(ctx, cb.brokenKey(nodeID), "1", cb.cooldown).Err()
	}
	return nil
}

// IsBroken checks if the circuit breaker is tripped.
func (cb *CircuitBreaker) IsBroken(ctx context.Context, nodeID string) (bool, error) {
	val, err := cb.rdb.Exists(ctx, cb.brokenKey(nodeID)).Result()
	if err != nil {
		return false, err
	}
	return val > 0, nil
}

// TryAcquireProbe attempts to acquire single probe slot during half-open state.
func (cb *CircuitBreaker) TryAcquireProbe(ctx context.Context, nodeID string) (bool, error) {
	// Set NX probe lock with 15 second expiry
	return cb.rdb.SetNX(ctx, cb.probeKey(nodeID), "1", 15*time.Second).Result()
}
