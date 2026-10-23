package throttle

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/redisguard"
)

// Refusal kinds of a key's own rates.
const (
	KindKeyRPM = "key_rpm"
	KindKeyTPM = "key_tpm"
)

// keyAdmitScript is GCRA on a key's requests (KEYS[1]) and tokens (KEYS[2]),
// with a minute as burst; both pass before either is booked. Returns {0, 0}, or
// {1 requests | 2 tokens, ms until admitted, -1 never}.
var keyAdmitScript = redis.NewScript(`
local now = tonumber(ARGV[1])
if not now or now <= 0 then
    local t = redis.call('TIME')
    now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
local rpm, tpm, tokens = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local window = 60000

local function tat(key)
    local v = tonumber(redis.call('GET', key))
    if not v or v < now then return now end
    return v
end

local rpm_tat, tpm_tat
if KEYS[1] ~= '' and rpm > 0 then
    local interval = window / rpm
    rpm_tat = tat(KEYS[1]) + interval
    if rpm_tat - now > window then
        return {1, math.ceil(rpm_tat - now - window)}
    end
end
if KEYS[2] ~= '' and tpm > 0 then
    if tokens > tpm then return {2, -1} end
    tpm_tat = tat(KEYS[2]) + tokens * window / tpm
    if tpm_tat - now > window then
        return {2, math.ceil(tpm_tat - now - window)}
    end
end
if rpm_tat then
    redis.call('SET', KEYS[1], string.format('%.3f', rpm_tat), 'PX', math.ceil(rpm_tat - now) + window)
end
if tpm_tat then
    redis.call('SET', KEYS[2], string.format('%.3f', tpm_tat), 'PX', math.ceil(tpm_tat - now) + window)
end
return {0, 0}
`)

// keySettleScript books the tokens a call used beyond its estimate, or returns
// the rest. Overspend lands even on an expired key: a long stream outlives it.
var keySettleScript = redis.NewScript(`
local v = tonumber(redis.call('GET', KEYS[1]))
local now = tonumber(ARGV[3])
if not now or now <= 0 then
    local t = redis.call('TIME')
    now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
local d = tonumber(ARGV[1]) * 60000 / tonumber(ARGV[2])
local new
if d > 0 then
    if not v or v < now then v = now end
    new = v + d
else
    if not v or v <= now then return 0 end
    new = v + d
end
if new <= now then
    redis.call('DEL', KEYS[1])
    return 0
end
redis.call('SET', KEYS[1], string.format('%.3f', new), 'PX', math.ceil(new - now) + 60000)
return 0
`)

// AdmitKey takes a request and books estimate tokens on the key's rates;
// settle(actual) rebooks the real tokens. A refusal names its kind and wait
// (0: never fits). Fails open, like every other limit.
func (t *Redis) AdmitKey(ctx context.Context, keyName string, rpm int32, tpm, estimate int64) (settle func(actual int64), kind string, retry time.Duration) {
	settle = func(int64) {}
	if t.client == nil || (rpm <= 0 && tpm <= 0) {
		return settle, "", 0
	}
	keys := []string{"", ""}
	if rpm > 0 {
		keys[0] = t.keys.KeyRPM(keyName)
	}
	if tpm > 0 {
		keys[1] = t.keys.KeyTPM(keyName)
	}
	estimate = max(estimate, 0)
	var nowMs int64
	if t.now != nil {
		nowMs = t.now().UTC().UnixMilli()
	}
	out, err := keyAdmitScript.Run(ctx, t.client, keys, nowMs, rpm, tpm, estimate).Int64Slice()
	if err != nil || len(out) != 2 {
		observability.DependencyErrorsTotal.WithLabelValues("redis", "key_rate").Inc()
		redisguard.Log(err).Err(err).Str("key", keyName).Msg("throttle: key rate check failed, allowing (fail-open)")
		return settle, "", 0
	}
	switch out[0] {
	case 1:
		return settle, KindKeyRPM, time.Duration(out[1]) * time.Millisecond
	case 2:
		return settle, KindKeyTPM, time.Duration(max(out[1], 0)) * time.Millisecond
	}
	if tpm <= 0 {
		return settle, "", 0
	}
	return func(actual int64) {
		if delta := max(actual, 0) - estimate; delta != 0 {
			// Detached: the call's context may be done by the time it settles.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			var now int64
			if t.now != nil {
				now = t.now().UTC().UnixMilli()
			}
			if err := keySettleScript.Run(sctx, t.client, keys[1:], delta, tpm, now).Err(); err != nil {
				observability.DependencyErrorsTotal.WithLabelValues("redis", "key_rate_settle").Inc()
				redisguard.Log(err).Err(err).Str("key", keyName).Msg("throttle: key token settle failed")
			}
		}
	}, "", 0
}
