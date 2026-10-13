// Package throttle enforces per-model RPM via GCRA and daily token budgets in Redis,
// shared across replicas. Everything fails open: a broken Redis must degrade
// to "no limits", never to "no service".
package throttle

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/panicsafe"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/rediskeys"
	"github.com/ai-process/llm-proxy/internal/router"
)

const (
	budgetTTL     = 48 * time.Hour // crash-leaked reservations self-heal
	settleTimeout = 5 * time.Second
)

var gcraScript = redis.NewScript(`
local now = tonumber(ARGV[1])
if not now or now <= 0 then
    local time_res = redis.call('TIME')
    now = tonumber(time_res[1]) * 1000 + math.floor(tonumber(time_res[2]) / 1000)
end

local function check_gcra(key, rate, burst)
    if not key or key == '' or not rate or rate <= 0 then
        return nil, nil, nil
    end
    local emission_interval = math.floor(60000 / rate)
    if emission_interval < 1 then
        emission_interval = 1
    end
    if burst < 0 then
        burst = 0
    end
    local burst_tolerance = burst * emission_interval
    local tat = redis.call('GET', key)
    if not tat then
        tat = now
    else
        tat = tonumber(tat)
        if tat < now then
            tat = now
        end
    end
    if (tat - now) > burst_tolerance then
        return false, nil, nil
    end
    local new_tat = tat + emission_interval
    local ttl_ms = new_tat - now + 60000
    local ttl_sec = math.ceil(ttl_ms / 1000)
    if ttl_sec < 1 then
        ttl_sec = 1
    end
    return true, new_tat, ttl_sec
end

local ok1, new_tat1, ttl_sec1 = check_gcra(KEYS[1], tonumber(ARGV[2]), tonumber(ARGV[3]))
if ok1 == false then
    return 0
end

local ok2, new_tat2, ttl_sec2
if #KEYS >= 2 then
    ok2, new_tat2, ttl_sec2 = check_gcra(KEYS[2], tonumber(ARGV[4]), tonumber(ARGV[5]))
    if ok2 == false then
        return 0
    end
end

if ok1 == true then
    redis.call('SET', KEYS[1], new_tat1, 'EX', ttl_sec1)
end
if ok2 == true then
    redis.call('SET', KEYS[2], new_tat2, 'EX', ttl_sec2)
end

return 1
`)

type UserRateLimitResolver func(userID, modelID string) *proxydb.UserRateLimit

type Redis struct {
	client            *redis.Client
	keys              rediskeys.Keys
	graceMargin       float64
	userLimitResolver UserRateLimitResolver
	// now is swappable for tests.
	now func() time.Time
}

func New(client *redis.Client, keyPrefix string) *Redis {
	return &Redis{client: client, keys: rediskeys.New(keyPrefix), graceMargin: 0.05}
}

func (t *Redis) currentTime() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *Redis) SetGraceMargin(margin float64) {
	if margin >= 0 {
		t.graceMargin = margin
	}
}

func (t *Redis) SetUserRateLimitResolver(resolver UserRateLimitResolver) {
	t.userLimitResolver = resolver
}

// AllowRPM evaluates this attempt against the key's and user's rate limits using GCRA atomically.
func (t *Redis) AllowRPM(ctx context.Context, keyName, userID string, m *proxydb.Model) bool {
	if t.client == nil {
		return true
	}

	var (
		key1, key2     string
		rate1, rate2   int32
		burst1, burst2 int32
	)

	if m.RPM > 0 {
		key1 = t.keys.GCRAKey(keyName, m.ID)
		rate1 = m.RPM
		burst1 = m.EffectiveBurst()
	}

	if t.userLimitResolver != nil && userID != "" {
		uLimit := t.userLimitResolver(userID, m.ID)
		if uLimit != nil && uLimit.RPM > 0 {
			key2 = t.keys.GCRAUser(userID, m.ID)
			rate2 = uLimit.RPM
			burst2 = uLimit.EffectiveBurst()
		}
	}

	if rate1 <= 0 && rate2 <= 0 {
		return true
	}

	var (
		keys []string
		args []any
	)

	var nowMs int64
	if t.now != nil {
		nowMs = t.now().UTC().UnixNano() / int64(time.Millisecond)
	}

	if rate1 > 0 && rate2 > 0 {
		keys = []string{key1, key2}
		args = []any{nowMs, rate1, burst1, rate2, burst2}
	} else if rate1 > 0 {
		keys = []string{key1}
		args = []any{nowMs, rate1, burst1, 0, 0}
	} else {
		keys = []string{key2}
		args = []any{nowMs, rate2, burst2, 0, 0}
	}

	res, err := gcraScript.Run(ctx, t.client, keys, args...).Int()
	if err != nil {
		log.Warn().Err(err).Strs("keys", keys).Msg("throttle: gcra check failed, allowing (fail-open)")
		return true
	}
	return res == 1
}

// Reserve books the estimate against every applicable daily budget.
// Supports budget_grace_margin: allows the call if the budget was not already exhausted before.
func (t *Redis) Reserve(ctx context.Context, m *proxydb.Model, keyName, userID string, estimate int64) (*router.Reservation, string) {
	if t.client == nil {
		return nil, ""
	}
	day := t.currentTime().UTC().Format("20060102")

	type budget struct {
		key   string
		limit int64
		kind  string
	}
	var budgets []budget
	if m.DailyTokensPerKey > 0 {
		budgets = append(budgets, budget{t.keys.BudgetKey(keyName, m.ID, day), m.DailyTokensPerKey, "budget_key"})
	}

	userLimit := m.DailyTokensPerUser
	if t.userLimitResolver != nil && userID != "" {
		if ul := t.userLimitResolver(userID, m.ID); ul != nil && ul.DailyTokens > 0 {
			userLimit = ul.DailyTokens
		}
	}
	if userLimit > 0 && userID != "" {
		budgets = append(budgets, budget{t.keys.BudgetUser(userID, m.ID, day), userLimit, "budget_user"})
	}

	if len(budgets) == 0 {
		return nil, ""
	}

	res := &router.Reservation{Estimate: estimate}
	margin := m.BudgetGraceMargin
	if margin == 0 {
		margin = t.graceMargin
	}
	if margin < 0 {
		margin = 0
	}
	for _, b := range budgets {
		maxAllowed := int64(float64(b.limit) * (1.0 + margin))

		newVal, err := t.client.IncrBy(ctx, b.key, estimate).Result()
		if err != nil {
			log.Warn().Err(err).Msg("throttle: budget reserve failed, allowing (fail-open)")
			continue // this budget is not reserved and not enforced
		}
		if expErr := t.client.Expire(ctx, b.key, budgetTTL).Err(); expErr != nil {
			log.Warn().Err(expErr).Str("key", b.key).Msg("throttle: expire budget key failed")
		}

		prevVal := newVal - estimate
		if prevVal >= b.limit || newVal > maxAllowed {
			// Undo this and every prior reservation of this call.
			if decrErr := t.client.DecrBy(ctx, b.key, estimate).Err(); decrErr != nil {
				log.Warn().Err(decrErr).Str("key", b.key).Msg("throttle: undo reservation failed")
			}
			t.rollback(ctx, res)
			return nil, b.kind
		}
		res.Keys = append(res.Keys, b.key)
	}
	return res, ""
}

func (t *Redis) rollback(ctx context.Context, res *router.Reservation) {
	for _, key := range res.Keys {
		if err := t.client.DecrBy(ctx, key, res.Estimate).Err(); err != nil {
			log.Warn().Err(err).Str("key", key).Msg("throttle: rollback failed")
		}
	}
}

// Settle adjusts every reserved budget from the estimate to the vendor's
// actual token count. Fire-and-forget: settling must never block a response.
func (t *Redis) Settle(res *router.Reservation, usage llm.TokensUsage) {
	if t.client == nil || res == nil || len(res.Keys) == 0 {
		return
	}
	delta := int64(usage.Input+usage.Output) - res.Estimate
	if delta == 0 {
		return
	}
	go func() {
		defer panicsafe.Guard("throttle settle")
		ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
		defer cancel()
		for _, key := range res.Keys {
			if err := t.client.IncrBy(ctx, key, delta).Err(); err != nil {
				log.Warn().Err(err).Str("key", key).Msg("throttle: settle failed")
			}
		}
	}()
}

// CheckDailyBudget checks if either daily budget for this model is already at or beyond limit.
func (t *Redis) CheckDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string) string {
	if t.client == nil {
		return ""
	}
	day := t.currentTime().UTC().Format("20060102")

	if m.DailyTokensPerKey > 0 {
		key := t.keys.BudgetKey(keyName, m.ID, day)
		val, err := t.client.Get(ctx, key).Int64()
		if err == nil && val >= m.DailyTokensPerKey {
			return "budget_key"
		}
	}

	userLimit := m.DailyTokensPerUser
	if t.userLimitResolver != nil && userID != "" {
		if ul := t.userLimitResolver(userID, m.ID); ul != nil && ul.DailyTokens > 0 {
			userLimit = ul.DailyTokens
		}
	}
	if userLimit > 0 && userID != "" {
		key := t.keys.BudgetUser(userID, m.ID, day)
		val, err := t.client.Get(ctx, key).Int64()
		if err == nil && val >= userLimit {
			return "budget_user"
		}
	}
	return ""
}

// ChargeDailyBudget increments the daily budgets with actual batch tokens.
func (t *Redis) ChargeDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID string, tokens int64) {
	if t.client == nil || tokens <= 0 {
		return
	}
	day := t.currentTime().UTC().Format("20060102")
	var keys []string
	if m.DailyTokensPerKey > 0 {
		keys = append(keys, t.keys.BudgetKey(keyName, m.ID, day))
	}

	userLimit := m.DailyTokensPerUser
	if t.userLimitResolver != nil && userID != "" {
		if ul := t.userLimitResolver(userID, m.ID); ul != nil && ul.DailyTokens > 0 {
			userLimit = ul.DailyTokens
		}
	}
	if userLimit > 0 && userID != "" {
		keys = append(keys, t.keys.BudgetUser(userID, m.ID, day))
	}
	if len(keys) == 0 {
		return
	}

	go func() {
		defer panicsafe.Guard("throttle charge batch tokens")
		c, cancel := context.WithTimeout(context.Background(), settleTimeout)
		defer cancel()
		for _, k := range keys {
			pipe := t.client.Pipeline()
			pipe.IncrBy(c, k, tokens)
			pipe.Expire(c, k, budgetTTL)
			if _, err := pipe.Exec(c); err != nil {
				log.Warn().Err(err).Str("key", k).Msg("throttle: charge batch tokens failed")
			}
		}
	}()
}
