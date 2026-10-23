package nodes

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script to acquire a lease:
// KEYS[1]: ZSET key (node:inflight:{node_id})
// ARGV[1]: current time (millis)
// ARGV[2]: lease deadline (millis)
// ARGV[3]: max concurrent requests
// ARGV[4]: request ID (UUID)
// Returns 1 if acquired, 0 if full.
const acquireLeaseScript = `
local zkey = KEYS[1]
local now = tonumber(ARGV[1])
local deadline = tonumber(ARGV[2])
local max_conc = tonumber(ARGV[3])
local req_id = ARGV[4]

-- purge expired leases
redis.call('ZREMRANGEBYSCORE', zkey, '-inf', now)

-- check cardinality
local current = redis.call('ZCARD', zkey)
if current >= max_conc then
    return 0
end

redis.call('ZADD', zkey, deadline, req_id)
return 1
`

// Lua script to renew a lease:
// KEYS[1]: ZSET key
// ARGV[1]: new deadline (millis)
// ARGV[2]: request ID
// Returns 1 if renewed, 0 if member wasn't present.
const renewLeaseScript = `
local zkey = KEYS[1]
local deadline = tonumber(ARGV[1])
local req_id = ARGV[2]

local score = redis.call('ZSCORE', zkey, req_id)
if not score then
    return 0
end

redis.call('ZADD', zkey, deadline, req_id)
return 1
`

// Lua script to release a lease, optionally keeping the slot for a cooldown:
// KEYS[1]: ZSET key
// ARGV[1]: request ID
// ARGV[2]: cooldown member (cd:{request ID})
// ARGV[3]: cooldown deadline (millis)
// The swap is atomic so no acquire sees the slot free in between.
const releaseCooldownScript = `
local zkey = KEYS[1]
redis.call('ZREM', zkey, ARGV[1])
redis.call('ZADD', zkey, tonumber(ARGV[3]), ARGV[2])
return 1
`

type LeaseManager struct {
	rdb        redis.UniversalClient
	store      *Store
	acquireSha string
	renewSha   string
	now        func() time.Time // lease deadlines are wall-clock millis; tests move it
}

func NewLeaseManager(rdb redis.UniversalClient, store *Store) *LeaseManager {
	return &LeaseManager{
		rdb:   rdb,
		store: store,
		now:   time.Now,
	}
}

// AcquireLease attempts to take a slot on nodeID. Returns true if acquired.
func (lm *LeaseManager) AcquireLease(ctx context.Context, nodeID, reqID string, maxConc int32, ttl time.Duration) (bool, error) {
	now := lm.now().UnixMilli()
	deadline := now + ttl.Milliseconds()
	key := lm.store.keys.NodeInflight(nodeID)

	res, err := lm.rdb.Eval(ctx, acquireLeaseScript, []string{key}, now, deadline, maxConc, reqID).Int64()
	if err != nil {
		return false, fmt.Errorf("eval acquire lease: %w", err)
	}
	return res == 1, nil
}

// RenewLease extends lease expiration for streaming or long requests.
func (lm *LeaseManager) RenewLease(ctx context.Context, nodeID, reqID string, extension time.Duration) (bool, error) {
	deadline := lm.now().Add(extension).UnixMilli()
	key := lm.store.keys.NodeInflight(nodeID)

	res, err := lm.rdb.Eval(ctx, renewLeaseScript, []string{key}, deadline, reqID).Int64()
	if err != nil {
		return false, fmt.Errorf("eval renew lease: %w", err)
	}
	return res == 1, nil
}

// ReleaseLease frees the slot, or with a positive cooldown keeps it taken
// until the cooldown ends; the hold then expires like any lease.
func (lm *LeaseManager) ReleaseLease(ctx context.Context, nodeID, reqID string, cooldown time.Duration) error {
	key := lm.store.keys.NodeInflight(nodeID)
	if cooldown <= 0 {
		return lm.rdb.ZRem(ctx, key, reqID).Err()
	}
	deadline := lm.now().Add(cooldown).UnixMilli()
	if err := lm.rdb.Eval(ctx, releaseCooldownScript, []string{key}, reqID, cooldownPrefix+reqID, deadline).Err(); err != nil {
		return fmt.Errorf("eval release lease: %w", err)
	}
	return nil
}
