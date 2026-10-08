// Package throttle enforces per-model RPM and daily token budgets in Redis,
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
	rpmWindowTTL  = 2 * time.Minute // covers the current minute plus clock skew
	budgetTTL     = 48 * time.Hour  // crash-leaked reservations self-heal
	settleTimeout = 5 * time.Second
)

type Redis struct {
	client *redis.Client
	keys   rediskeys.Keys
	// now is swappable for tests.
	now func() time.Time
}

func New(client *redis.Client, keyPrefix string) *Redis {
	return &Redis{client: client, keys: rediskeys.New(keyPrefix), now: time.Now}
}

// AllowRPM counts this attempt against the model's fixed per-minute window.
func (t *Redis) AllowRPM(ctx context.Context, keyName string, m *proxydb.Model) bool {
	if t.client == nil || m.RPM <= 0 {
		return true
	}
	key := t.keys.RPM(keyName, m.ID, t.now().UTC().Unix()/60)
	pipe := t.client.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, rpmWindowTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Warn().Err(err).Msg("throttle: rpm check failed, allowing (fail-open)")
		return true
	}
	return incr.Val() <= int64(m.RPM)
}

// Reserve books the estimate against every applicable daily budget. On a
// tripped budget the increments are undone and the budget kind returned.
func (t *Redis) Reserve(ctx context.Context, m *proxydb.Model, keyName, userID string, estimate int64) (*router.Reservation, string) {
	if t.client == nil {
		return nil, ""
	}
	day := t.now().UTC().Format("20060102")

	type budget struct {
		key   string
		limit int64
		kind  string
	}
	var budgets []budget
	if m.DailyTokensPerKey > 0 {
		budgets = append(budgets, budget{t.keys.BudgetKey(keyName, m.ID, day), m.DailyTokensPerKey, "budget_key"})
	}
	if m.DailyTokensPerUser > 0 && userID != "" {
		budgets = append(budgets, budget{t.keys.BudgetUser(userID, m.ID, day), m.DailyTokensPerUser, "budget_user"})
	}
	if len(budgets) == 0 {
		return nil, ""
	}

	res := &router.Reservation{Estimate: estimate}
	for _, b := range budgets {
		newVal, err := t.client.IncrBy(ctx, b.key, estimate).Result()
		if err != nil {
			log.Warn().Err(err).Msg("throttle: budget reserve failed, allowing (fail-open)")
			continue // this budget is not reserved and not enforced
		}
		t.client.Expire(ctx, b.key, budgetTTL)
		if newVal > b.limit {
			// Undo this and every prior reservation of this call.
			t.client.DecrBy(ctx, b.key, estimate)
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
	day := t.now().UTC().Format("20060102")

	if m.DailyTokensPerKey > 0 {
		key := t.keys.BudgetKey(keyName, m.ID, day)
		val, err := t.client.Get(ctx, key).Int64()
		if err == nil && val >= m.DailyTokensPerKey {
			return "budget_key"
		}
	}
	if m.DailyTokensPerUser > 0 && userID != "" {
		key := t.keys.BudgetUser(userID, m.ID, day)
		val, err := t.client.Get(ctx, key).Int64()
		if err == nil && val >= m.DailyTokensPerUser {
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
	day := t.now().UTC().Format("20060102")
	var keys []string
	if m.DailyTokensPerKey > 0 {
		keys = append(keys, t.keys.BudgetKey(keyName, m.ID, day))
	}
	if m.DailyTokensPerUser > 0 && userID != "" {
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
