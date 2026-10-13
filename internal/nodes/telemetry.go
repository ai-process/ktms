package nodes

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script to update EWMA performance metrics atomically:
// KEYS[1]: perf key (node:perf:{node_id}:{model_id})
// ARGV[1]: alpha (e.g. "0.2")
// ARGV[2]: measured TPS (float string, e.g. "45.2", or "" / "-1" if unary/unmeasured)
// ARGV[3]: measured TTFT (float string ms per 1k input tokens, e.g. "120.5", or "" / "-1" if unary/unmeasured)
// ARGV[4]: updated_at (unix millis)
// ARGV[5]: TTL seconds (e.g. 86400)
const updatePerfScript = `
local pkey = KEYS[1]
local alpha = tonumber(ARGV[1])
local tps = tonumber(ARGV[2])
local ttft = tonumber(ARGV[3])
local now_ms = ARGV[4]
local ttl = tonumber(ARGV[5])

local current_tps = redis.call('HGET', pkey, 'tps_ema')
local current_ttft = redis.call('HGET', pkey, 'ttft_ema_ms')

local new_tps = current_tps
if tps and tps > 0 then
    if current_tps then
        new_tps = (alpha * tps) + ((1.0 - alpha) * tonumber(current_tps))
    else
        new_tps = tps
    end
    redis.call('HSET', pkey, 'tps_ema', tostring(new_tps))
end

local new_ttft = current_ttft
if ttft and ttft > 0 then
    if current_ttft then
        new_ttft = (alpha * ttft) + ((1.0 - alpha) * tonumber(current_ttft))
    else
        new_ttft = ttft
    end
    redis.call('HSET', pkey, 'ttft_ema_ms', tostring(new_ttft))
end

redis.call('HSET', pkey, 'updated_at', now_ms)
redis.call('EXPIRE', pkey, ttl)
return 1
`

type TelemetryTracker struct {
	rdb   redis.UniversalClient
	store *Store
	alpha float64
	ttl   time.Duration
}

func NewTelemetryTracker(rdb redis.UniversalClient, store *Store, alpha float64, ttl time.Duration) *TelemetryTracker {
	if alpha <= 0 || alpha > 1.0 {
		alpha = 0.2
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &TelemetryTracker{
		rdb:   rdb,
		store: store,
		alpha: alpha,
		ttl:   ttl,
	}
}

// RecordMetrics updates EWMA for a node & model.
// For unary calls, ttftNormalizedMs may be <= 0.
func (tt *TelemetryTracker) RecordMetrics(ctx context.Context, nodeID, modelID string, tps float64, ttftNormalizedMs float64) error {
	key := tt.store.keys.NodePerf(nodeID, modelID)
	now := time.Now().UnixMilli()

	tpsArg := "-1"
	if tps > 0 {
		tpsArg = fmt.Sprintf("%.4f", tps)
	}
	ttftArg := "-1"
	if ttftNormalizedMs > 0 {
		ttftArg = fmt.Sprintf("%.4f", ttftNormalizedMs)
	}

	return tt.rdb.Eval(ctx, updatePerfScript, []string{key},
		fmt.Sprintf("%.4f", tt.alpha),
		tpsArg,
		ttftArg,
		fmt.Sprintf("%d", now),
		int(tt.ttl.Seconds()),
	).Err()
}

// GetPerf retrieves performance metrics for node and model.
func (tt *TelemetryTracker) GetPerf(ctx context.Context, nodeID, modelID string) (*NodePerfData, error) {
	key := tt.store.keys.NodePerf(nodeID, modelID)
	vals, err := tt.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, nil
	}

	perf := &NodePerfData{
		NodeID:  nodeID,
		ModelID: modelID,
	}
	if v, ok := vals["tps_ema"]; ok {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			perf.TPSEMA = f
		}
	}
	if v, ok := vals["ttft_ema_ms"]; ok {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			perf.TTFTEMAMs = f
		}
	}
	return perf, nil
}
