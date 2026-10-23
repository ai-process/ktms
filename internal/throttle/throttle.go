// Package throttle enforces per-model RPM via GCRA, daily token budgets and
// monetary spend limits in Redis, shared across replicas. A broken Redis
// degrades to "no limits", never "no service", unless spend fails closed.
package throttle

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/panicsafe"
	"github.com/ai-process/ktms/internal/pricing"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/redisguard"
	"github.com/ai-process/ktms/internal/rediskeys"
	"github.com/ai-process/ktms/internal/router"
	"github.com/ai-process/ktms/internal/spendlimit"
)

const (
	budgetTTL     = 48 * time.Hour // crash-leaked reservations self-heal
	settleTimeout = 5 * time.Second
	// spendGrace keeps a counter readable for a day past its period, so a call
	// reserved just before midnight still settles against the right window.
	spendGrace = 24 * time.Hour
	// DefaultOutputReserve is the output tokens reserved when a call sets no
	// ceiling of its own.
	DefaultOutputReserve = 4096
)

// spendReserveScript books ARGV[1] nano-USD on every counter, or on none: it
// returns the 1-based index of the first counter whose limit (ARGV[2i]) the
// amount would cross, 0 when booked. ARGV[2i+1] is that counter's TTL.
var spendReserveScript = redis.NewScript(`
local amount = tonumber(ARGV[1])
for i = 1, #KEYS do
    local cur = tonumber(redis.call('GET', KEYS[i]) or '0')
    if cur + amount > tonumber(ARGV[2 * i]) then
        return i
    end
end
for i = 1, #KEYS do
    redis.call('INCRBY', KEYS[i], amount)
    redis.call('EXPIRE', KEYS[i], tonumber(ARGV[2 * i + 1]))
end
return 0
`)

// spendAdjustScript moves every counter by ARGV[1], never below zero. A
// negative delta skips counters that are gone, so a release cannot resurrect
// an expired window; ARGV[1+i] is the TTL for a counter this call creates.
var spendAdjustScript = redis.NewScript(`
local delta = tonumber(ARGV[1])
for i = 1, #KEYS do
    if delta > 0 or redis.call('EXISTS', KEYS[i]) == 1 then
        local v = redis.call('INCRBY', KEYS[i], delta)
        if v < 0 then
            redis.call('INCRBY', KEYS[i], -v)
        end
        if redis.call('TTL', KEYS[i]) < 0 then
            redis.call('EXPIRE', KEYS[i], tonumber(ARGV[1 + i]))
        end
    end
end
return 0
`)

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

// SpendLimitResolver returns the cap in nano-USD for a subject and period, 0 for none.
type SpendLimitResolver func(subjectType, subjectID, period string) int64

type Redis struct {
	client            *redis.Client
	keys              rediskeys.Keys
	graceMargin       float64
	userLimitResolver UserRateLimitResolver
	spendLimits       SpendLimitResolver
	spendFailClosed   bool
	outputReserve     int64
	// now is swappable for tests.
	now func() time.Time
}

func New(client *redis.Client, keyPrefix string) *Redis {
	return &Redis{client: client, keys: rediskeys.New(keyPrefix), graceMargin: 0.05,
		outputReserve: DefaultOutputReserve}
}

func (t *Redis) SetSpendLimitResolver(resolver SpendLimitResolver) {
	t.spendLimits = resolver
}

// SetSpendFailClosed refuses calls under a spend limit while Redis is
// unreachable instead of letting them through unmetered.
func (t *Redis) SetSpendFailClosed(closed bool) {
	t.spendFailClosed = closed
}

// SetDefaultOutputReserve sets the output tokens priced into a spend
// reservation when the call has no output ceiling.
func (t *Redis) SetDefaultOutputReserve(tokens int64) {
	if tokens > 0 {
		t.outputReserve = tokens
	}
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
	} else {
		t.graceMargin = 0
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
		observability.DependencyErrorsTotal.WithLabelValues("redis", "rpm").Inc()
		redisguard.Log(err).Err(err).Strs("keys", keys).Msg("throttle: gcra check failed, allowing (fail-open)")
		return true
	}
	return res == 1
}

// tokenBudget is one daily token budget that applies to a call.
type tokenBudget struct {
	key   string
	limit int64
	kind  string
}

func (t *Redis) tokenBudgets(m *proxydb.Model, keyName, userID, day string) []tokenBudget {
	var budgets []tokenBudget
	if m.DailyTokensPerKey > 0 {
		budgets = append(budgets, tokenBudget{t.keys.BudgetKey(keyName, m.ID, day), m.DailyTokensPerKey, "budget_key"})
	}
	userLimit := m.DailyTokensPerUser
	if t.userLimitResolver != nil && userID != "" {
		if ul := t.userLimitResolver(userID, m.ID); ul != nil && ul.DailyTokens > 0 {
			userLimit = ul.DailyTokens
		}
	}
	if userLimit > 0 && userID != "" {
		budgets = append(budgets, tokenBudget{t.keys.BudgetUser(userID, m.ID, day), userLimit, "budget_user"})
	}
	return budgets
}

// spendTarget is one spend counter that applies to a call.
type spendTarget struct {
	key    string
	limit  int64
	ttl    int64 // seconds
	reason string
}

func (t *Redis) spendTargets(keyName, userID, teamID string, now time.Time) []spendTarget {
	if t.spendLimits == nil {
		return nil
	}
	var targets []spendTarget
	for _, e := range spendlimit.Enforced {
		subjectID := keyName
		switch e.Subject {
		case spendlimit.SubjectUser:
			subjectID = userID
		case spendlimit.SubjectTeam:
			subjectID = teamID
		}
		if subjectID == "" {
			continue
		}
		limit := t.spendLimits(e.Subject, subjectID, e.Period)
		if limit <= 0 {
			continue
		}
		window, end := spendlimit.Window(e.Period, now)
		targets = append(targets, spendTarget{
			key:    t.keys.Spend(spendShort(e.Subject), subjectID, window),
			limit:  limit,
			ttl:    int64((end.Sub(now) + spendGrace) / time.Second),
			reason: spendlimit.Reason(e.Subject, e.Period),
		})
	}
	return targets
}

// spendShort is the subject tag in its Redis spend counter key.
func spendShort(subject string) string {
	switch subject {
	case spendlimit.SubjectUser:
		return "u"
	case spendlimit.SubjectTeam:
		return "t"
	}
	return "k"
}

// SpendSoFar reads a subject's spend counter for the current window, the one
// Reserve checks; nano-USD, 0 when nothing has been spent yet.
func (t *Redis) SpendSoFar(ctx context.Context, subject, subjectID, period string) (int64, time.Time, error) {
	window, end := spendlimit.Window(period, t.currentTime())
	val, err := t.client.Get(ctx, t.keys.Spend(spendShort(subject), subjectID, window)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, end, nil
	}
	return val, end, err
}

// Reserve books the estimate against every applicable daily budget, then the
// priced worst case against every spend limit. Supports budget_grace_margin:
// allows the call if the token budget was not already exhausted before.
func (t *Redis) Reserve(ctx context.Context, m *proxydb.Model, keyName, userID, teamID string, estimate, maxOutput int64) (*router.Reservation, string) {
	if t.client == nil {
		return nil, ""
	}
	now := t.currentTime().UTC()
	budgets := t.tokenBudgets(m, keyName, userID, now.Format("20060102"))
	targets := t.spendTargets(keyName, userID, teamID, now)
	if len(budgets) == 0 && len(targets) == 0 {
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
			observability.DependencyErrorsTotal.WithLabelValues("redis", "token_budget").Inc()
			redisguard.Log(err).Err(err).Msg("throttle: budget reserve failed, allowing (fail-open)")
			continue // this budget is not reserved and not enforced
		}
		if expErr := t.client.Expire(ctx, b.key, budgetTTL).Err(); expErr != nil {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "token_budget").Inc()
			redisguard.Log(expErr).Err(expErr).Str("key", b.key).Msg("throttle: expire budget key failed")
		}

		prevVal := newVal - estimate
		if prevVal >= b.limit || newVal > maxAllowed {
			// Undo this and every prior reservation of this call.
			if decrErr := t.client.DecrBy(ctx, b.key, estimate).Err(); decrErr != nil {
				observability.DependencyErrorsTotal.WithLabelValues("redis", "token_budget").Inc()
				redisguard.Log(decrErr).Err(decrErr).Str("key", b.key).Msg("throttle: undo reservation failed")
			}
			t.rollback(ctx, res)
			return nil, b.kind
		}
		res.Keys = append(res.Keys, b.key)
	}

	if len(targets) > 0 {
		hold, tripped := t.reserveSpend(ctx, m, keyName, userID, targets, estimate, maxOutput, now)
		if tripped != "" {
			t.rollback(ctx, res)
			return nil, tripped
		}
		res.Spend = hold
	}
	return res, ""
}

// reserveSpend books the call's worst-case price on every spend counter in
// one script run, so concurrent calls can never jointly overshoot a limit.
func (t *Redis) reserveSpend(ctx context.Context, m *proxydb.Model, keyName, userID string,
	targets []spendTarget, estimate, maxOutput int64, now time.Time) (*router.SpendHold, string) {
	if maxOutput <= 0 {
		maxOutput = t.outputReserve
	}
	mode := pricing.ModeAt(m, now)
	// The cache split is known only after the call, so input is booked at its
	// dearest rate: a prompt written to the cache costs more than plain input.
	amount := pricing.Cost(m, llm.TokensUsage{Output: int(maxOutput)}, mode)
	if in := pricing.MaxInputCost(m, estimate, mode); in > math.MaxInt64-amount {
		amount = math.MaxInt64
	} else {
		amount += in
	}

	hold := &router.SpendHold{Reserved: amount, Model: m, Mode: mode}
	args := make([]any, 0, 1+2*len(targets))
	args = append(args, amount)
	for _, tg := range targets {
		hold.Keys = append(hold.Keys, tg.key)
		hold.TTLs = append(hold.TTLs, tg.ttl)
		args = append(args, tg.limit, tg.ttl)
	}

	idx, err := spendReserveScript.Run(ctx, t.client, hold.Keys, args...).Int()
	if err != nil {
		observability.DependencyErrorsTotal.WithLabelValues("redis", "spend_reserve").Inc()
		if t.spendFailClosed {
			log.Error().Err(err).Str("key", keyName).Str("user", userID).
				Msg("throttle: spend reserve failed, refusing (SPEND_LIMITS_FAIL_MODE=closed)")
			return nil, spendlimit.ReasonUnavailable
		}
		redisguard.Log(err).Err(err).Str("key", keyName).Str("user", userID).
			Msg("throttle: spend reserve failed, allowing unmetered (fail-open)")
		return nil, ""
	}
	if idx > 0 && idx <= len(targets) {
		return nil, targets[idx-1].reason
	}
	return hold, ""
}

func (t *Redis) rollback(ctx context.Context, res *router.Reservation) {
	for _, key := range res.Keys {
		if err := t.client.DecrBy(ctx, key, res.Estimate).Err(); err != nil {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "token_budget").Inc()
			redisguard.Log(err).Err(err).Str("key", key).Msg("throttle: rollback failed")
		}
	}
}

// Settle adjusts every reserved budget from the estimate to the vendor's
// actual token count, and every spend counter from the reserved worst case to
// the priced actuals. Fire-and-forget: settling must never block a response.
func (t *Redis) Settle(res *router.Reservation, usage llm.TokensUsage) {
	if t.client == nil || res == nil {
		return
	}
	var tokenDelta int64
	if len(res.Keys) > 0 {
		tokenDelta = int64(usage.Input+usage.Output) - res.Estimate
	}
	var spendDelta int64
	if res.Spend != nil {
		spendDelta = pricing.Cost(res.Spend.Model, usage, res.Spend.Mode) - res.Spend.Reserved
	}
	if tokenDelta == 0 && spendDelta == 0 {
		return
	}
	go func() {
		defer panicsafe.Guard("throttle settle")
		ctx, cancel := context.WithTimeout(context.Background(), settleTimeout)
		defer cancel()
		if tokenDelta != 0 {
			for _, key := range res.Keys {
				if err := t.client.IncrBy(ctx, key, tokenDelta).Err(); err != nil {
					observability.DependencyErrorsTotal.WithLabelValues("redis", "settle").Inc()
					redisguard.Log(err).Err(err).Str("key", key).Msg("throttle: settle failed")
				}
			}
		}
		if spendDelta != 0 {
			t.adjustSpend(ctx, res.Spend.Keys, res.Spend.TTLs, spendDelta)
		}
	}()
}

func (t *Redis) adjustSpend(ctx context.Context, keys []string, ttls []int64, delta int64) {
	args := make([]any, 0, 1+len(ttls))
	args = append(args, delta)
	for _, ttl := range ttls {
		args = append(args, ttl)
	}
	if err := spendAdjustScript.Run(ctx, t.client, keys, args...).Err(); err != nil {
		observability.DependencyErrorsTotal.WithLabelValues("redis", "settle").Inc()
		redisguard.Log(err).Err(err).Strs("keys", keys).Int64("delta_nanousd", delta).Msg("throttle: spend adjust failed")
	}
}

// CheckDailyBudget reports a daily token budget or spend limit for this model
// that is already at or beyond its cap, or "" if the call may proceed.
func (t *Redis) CheckDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID, teamID string) string {
	if t.client == nil {
		return ""
	}
	now := t.currentTime().UTC()
	for _, b := range t.tokenBudgets(m, keyName, userID, now.Format("20060102")) {
		val, err := t.client.Get(ctx, b.key).Int64()
		if err != nil && !errors.Is(err, redis.Nil) {
			// Token budgets fail open, like Reserve; say so instead of passing silently.
			observability.DependencyErrorsTotal.WithLabelValues("redis", "token_budget").Inc()
			redisguard.Log(err).Err(err).Str("key", b.key).Msg("throttle: budget check failed, allowing (fail-open)")
			continue
		}
		if val >= b.limit {
			return b.kind
		}
	}
	for _, tg := range t.spendTargets(keyName, userID, teamID, now) {
		val, err := t.client.Get(ctx, tg.key).Int64()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if t.spendFailClosed {
				observability.DependencyErrorsTotal.WithLabelValues("redis", "spend_check").Inc()
				log.Error().Err(err).Str("key", tg.key).Msg("throttle: spend check failed, refusing (fail-closed)")
				return spendlimit.ReasonUnavailable
			}
			observability.DependencyErrorsTotal.WithLabelValues("redis", "spend_check").Inc()
			redisguard.Log(err).Err(err).Str("key", tg.key).Msg("throttle: spend check failed, allowing (fail-open)")
			continue
		}
		if val >= tg.limit {
			return tg.reason
		}
	}
	return ""
}

// ChargeDailyBudget books actual batch usage against the daily token budgets
// and, priced at the batch rate, against the spend limits.
func (t *Redis) ChargeDailyBudget(ctx context.Context, m *proxydb.Model, keyName, userID, teamID string, usage llm.TokensUsage) {
	tokens := int64(usage.Input + usage.Output)
	if t.client == nil || tokens <= 0 {
		return
	}
	now := t.currentTime().UTC()
	var keys []string
	for _, b := range t.tokenBudgets(m, keyName, userID, now.Format("20060102")) {
		keys = append(keys, b.key)
	}
	var spendKeys []string
	var spendTTLs []int64
	for _, tg := range t.spendTargets(keyName, userID, teamID, now) {
		spendKeys = append(spendKeys, tg.key)
		spendTTLs = append(spendTTLs, tg.ttl)
	}
	cost := pricing.Cost(m, usage, pricing.ModeBatch)
	if len(keys) == 0 && (len(spendKeys) == 0 || cost <= 0) {
		return
	}

	go func() {
		defer panicsafe.Guard("throttle charge batch usage")
		c, cancel := context.WithTimeout(context.Background(), settleTimeout)
		defer cancel()
		for _, k := range keys {
			pipe := t.client.Pipeline()
			pipe.IncrBy(c, k, tokens)
			pipe.Expire(c, k, budgetTTL)
			if _, err := pipe.Exec(c); err != nil {
				observability.DependencyErrorsTotal.WithLabelValues("redis", "batch_charge").Inc()
				redisguard.Log(err).Err(err).Str("key", k).Msg("throttle: charge batch tokens failed")
			}
		}
		if len(spendKeys) > 0 && cost > 0 {
			t.adjustSpend(c, spendKeys, spendTTLs, cost)
		}
	}()
}

// Every refusal reason is exported at 0 from the start, so an alert on
// increase() also catches the first refusal after a restart.
func init() {
	reasons := []string{"rpm", "budget_key", "budget_user", spendlimit.ReasonUnavailable}
	for _, e := range spendlimit.Enforced {
		reasons = append(reasons, spendlimit.Reason(e.Subject, e.Period))
	}
	for _, r := range reasons {
		observability.ThrottledTotal.WithLabelValues(r)
	}
}
