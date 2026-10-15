package throttle

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/router"
	"github.com/ai-process/llm-proxy/internal/spendlimit"
)

// pricedModel bills $1/Mtok in and $2/Mtok out: 1000 nano per input token,
// 2000 per output token.
func pricedModel() *proxydb.Model {
	return &proxydb.Model{ID: "m1", Vendor: "openai", Enabled: true,
		PriceInPerMtok: 1, PriceOutPerMtok: 2}
}

type limits map[[3]string]int64

func (l limits) resolve(subjectType, subjectID, period string) int64 {
	return l[[3]string{subjectType, subjectID, period}]
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func counter(t *testing.T, mr *miniredis.Miniredis, key string) int64 {
	t.Helper()
	if !mr.Exists(key) {
		return 0
	}
	v, err := mr.Get(key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", key, v, err)
	}
	return n
}

func dayKey(th *Redis, subject, id string, now time.Time) string {
	w, _ := spendlimit.Window(spendlimit.PeriodDay, now)
	return th.keys.Spend(subject, id, w)
}

func monthKey(th *Redis, subject, id string, now time.Time) string {
	w, _ := spendlimit.Window(spendlimit.PeriodMonth, now)
	return th.keys.Spend(subject, id, w)
}

func TestSpendReserveThenSettleToActualCost(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	th.now = fixedNow(now)
	th.SetSpendLimitResolver(limits{{"key", "gen", "day"}: 10_000_000}.resolve)
	key := dayKey(th, "k", "gen", now)

	res, tripped := th.Reserve(context.Background(), pricedModel(), "gen", "", 1000, 1000)
	if tripped != "" || res == nil || res.Spend == nil {
		t.Fatalf("res=%+v tripped=%q", res, tripped)
	}
	// Worst case: 1000 in * 1000 + 1000 out * 2000.
	if got := counter(t, mr, key); got != 3_000_000 {
		t.Fatalf("reserved %d, want 3000000", got)
	}

	th.Settle(res, llm.TokensUsage{Input: 500, Output: 200})
	waitFor(t, func() bool { return counter(t, mr, key) == 900_000 })

	ttl := mr.TTL(key)
	if want := 12*time.Hour + spendGrace; ttl <= 0 || ttl > want {
		t.Fatalf("ttl = %s, want (0, %s]", ttl, want)
	}
}

func TestSpendRefusesCrossingCapAndRollsBackTokens(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Now().UTC()
	th.now = fixedNow(now)
	th.SetSpendLimitResolver(limits{{"key", "gen", "day"}: 5_000_000}.resolve)
	m := pricedModel()
	m.DailyTokensPerKey = 1_000_000
	ctx := context.Background()
	spendKey := dayKey(th, "k", "gen", now)
	tokenKey := th.keys.BudgetKey("gen", m.ID, now.Format("20060102"))

	if _, tripped := th.Reserve(ctx, m, "gen", "", 1000, 1000); tripped != "" {
		t.Fatalf("first call tripped %q", tripped)
	}
	// 3M booked; another 3M would reach 6M > 5M.
	res, tripped := th.Reserve(ctx, m, "gen", "", 1000, 1000)
	if res != nil || tripped != "spend_key_daily" {
		t.Fatalf("res=%v tripped=%q, want spend_key_daily", res, tripped)
	}
	if got := counter(t, mr, spendKey); got != 3_000_000 {
		t.Fatalf("spend counter = %d, want 3000000 (refusal books nothing)", got)
	}
	if got := counter(t, mr, tokenKey); got != 1000 {
		t.Fatalf("token budget = %d, want 1000 (refused call's tokens rolled back)", got)
	}

	// A cheaper call that still fits is admitted: limits are hard, not sticky.
	if _, tripped := th.Reserve(ctx, m, "gen", "", 1000, 500); tripped != "" {
		t.Fatalf("2M call into 2M headroom tripped %q", tripped)
	}
}

func TestSpendIsAllOrNothingAcrossCounters(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Now().UTC()
	th.now = fixedNow(now)
	th.SetSpendLimitResolver(limits{
		{"key", "gen", "day"}:    1_000_000_000,
		{"key", "gen", "month"}:  1_000_000_000,
		{"user", "u1", "month"}:  2_000_000,
		{"user", "u2", "month"}:  1_000_000_000,
		{"user", "u1", "day"}:    0, // explicitly unlimited
		{"user", "*", "month"}:   1, // wildcards are resolved by the registry, not here
		{"team", "t1", "month"}:  1, // not enforced
		{"key", "other", "day"}:  1,
		{"user", "svc:x", "day"}: 1,
	}.resolve)

	res, tripped := th.Reserve(context.Background(), pricedModel(), "gen", "u1", 1000, 1000)
	if res != nil || tripped != "spend_user_monthly" {
		t.Fatalf("res=%v tripped=%q, want spend_user_monthly", res, tripped)
	}
	for _, k := range []string{dayKey(th, "k", "gen", now), monthKey(th, "k", "gen", now), monthKey(th, "u", "u1", now)} {
		if mr.Exists(k) {
			t.Fatalf("%s was booked by a refused call", k)
		}
	}

	res, tripped = th.Reserve(context.Background(), pricedModel(), "gen", "u2", 1000, 1000)
	if tripped != "" || len(res.Spend.Keys) != 3 {
		t.Fatalf("u2: res=%+v tripped=%q, want 3 counters booked", res, tripped)
	}
}

func TestSpendDefaultOutputReserve(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Now().UTC()
	th.now = fixedNow(now)
	th.SetSpendLimitResolver(limits{{"key", "gen", "day"}: 1_000_000_000}.resolve)
	th.SetDefaultOutputReserve(100)

	if _, tripped := th.Reserve(context.Background(), pricedModel(), "gen", "", 10, 0); tripped != "" {
		t.Fatalf("tripped %q", tripped)
	}
	if got := counter(t, mr, dayKey(th, "k", "gen", now)); got != 10*1000+100*2000 {
		t.Fatalf("reserved %d, want in + default output reserve", got)
	}
}

// TestSpendConcurrentReservationsNeverOvershoot is the load test: many callers
// race for one cap; exactly the calls that fit are admitted and settling them
// leaves the counter at the sum of their actual costs, with no double charge.
func TestSpendConcurrentReservationsNeverOvershoot(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Now().UTC()
	th.now = fixedNow(now)
	const perCall = 3_000_000 // 1000 in + 1000 out
	const fits = 50
	th.SetSpendLimitResolver(limits{
		{"key", "gen", "day"}:   fits * perCall,
		{"user", "u1", "month"}: 10 * fits * perCall,
	}.resolve)
	m := pricedModel()
	ctx := context.Background()

	var (
		admitted atomic.Int64
		refused  atomic.Int64
		wg       sync.WaitGroup
		mu       sync.Mutex
		holds    []*router.Reservation
	)
	for i := 0; i < 4*fits; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, tripped := th.Reserve(ctx, m, "gen", "u1", 1000, 1000)
			switch tripped {
			case "":
				admitted.Add(1)
				mu.Lock()
				holds = append(holds, res)
				mu.Unlock()
			case "spend_key_daily":
				refused.Add(1)
			default:
				t.Errorf("unexpected trip %q", tripped)
			}
		}()
	}
	wg.Wait()

	if admitted.Load() != fits || refused.Load() != 3*fits {
		t.Fatalf("admitted=%d refused=%d, want %d/%d", admitted.Load(), refused.Load(), fits, 3*fits)
	}
	keyDay, userMonth := dayKey(th, "k", "gen", now), monthKey(th, "u", "u1", now)
	if got := counter(t, mr, keyDay); got != fits*perCall {
		t.Fatalf("key counter = %d, want exactly the cap %d", got, fits*perCall)
	}

	// Settle concurrently: each call actually cost 1000 in + 250 out = 1.5M.
	for _, res := range holds {
		wg.Add(1)
		go func(res *router.Reservation) {
			defer wg.Done()
			th.Settle(res, llm.TokensUsage{Input: 1000, Output: 250})
		}(res)
	}
	wg.Wait()
	const actual = 1_500_000
	waitFor(t, func() bool {
		return counter(t, mr, keyDay) == fits*actual && counter(t, mr, userMonth) == fits*actual
	})
}

func TestSpendFailModes(t *testing.T) {
	for _, closed := range []bool{false, true} {
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
		t.Cleanup(func() {
			if err := client.Close(); err != nil {
				t.Logf("close redis client: %v", err)
			}
		})
		th := New(client, "test")
		th.SetSpendLimitResolver(limits{{"key", "gen", "day"}: 1_000_000_000}.resolve)
		th.SetSpendFailClosed(closed)
		mr.Close() // Redis just died

		ctx := context.Background()
		res, tripped := th.Reserve(ctx, pricedModel(), "gen", "", 10, 10)
		check := th.CheckDailyBudget(ctx, pricedModel(), "gen", "")
		if closed {
			if res != nil || tripped != spendlimit.ReasonUnavailable || check != spendlimit.ReasonUnavailable {
				t.Fatalf("closed: res=%v tripped=%q check=%q, want spend_unavailable", res, tripped, check)
			}
			continue
		}
		if tripped != "" || check != "" {
			t.Fatalf("open: tripped=%q check=%q, want pass-through", tripped, check)
		}
		th.Settle(res, llm.TokensUsage{Input: 1}) // must not panic
	}
}

func TestSpendFailClosedIgnoresCallsWithoutLimits(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Logf("close redis client: %v", err)
		}
	})
	th := New(client, "test")
	th.SetSpendLimitResolver(limits{}.resolve)
	th.SetSpendFailClosed(true)
	mr.Close()

	if _, tripped := th.Reserve(context.Background(), pricedModel(), "gen", "u1", 10, 10); tripped != "" {
		t.Fatalf("no limit applies, yet tripped %q", tripped)
	}
}

func TestSpendSettleAcrossMidnightHitsReservedWindow(t *testing.T) {
	th, mr := newTestThrottle(t)
	before := time.Date(2026, 10, 15, 23, 59, 59, 0, time.UTC)
	th.now = fixedNow(before)
	th.SetSpendLimitResolver(limits{{"key", "gen", "day"}: 1_000_000_000}.resolve)

	res, tripped := th.Reserve(context.Background(), pricedModel(), "gen", "", 1000, 1000)
	if tripped != "" {
		t.Fatalf("tripped %q", tripped)
	}
	after := before.Add(2 * time.Second)
	th.now = fixedNow(after)
	th.Settle(res, llm.TokensUsage{Input: 1000})

	waitFor(t, func() bool { return counter(t, mr, dayKey(th, "k", "gen", before)) == 1_000_000 })
	if mr.Exists(dayKey(th, "k", "gen", after)) {
		t.Fatal("settling must not touch the next day's counter")
	}
}

func TestSpendReleaseNeverGoesNegativeOrResurrects(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Now().UTC()
	th.now = fixedNow(now)
	th.SetSpendLimitResolver(limits{{"key", "gen", "day"}: 1_000_000_000}.resolve)
	key := dayKey(th, "k", "gen", now)
	ctx := context.Background()

	res, tripped := th.Reserve(ctx, pricedModel(), "gen", "", 1000, 1000)
	if tripped != "" {
		t.Fatalf("tripped %q", tripped)
	}
	// The counter drifted below the hold (e.g. an operator reset).
	if err := mr.Set(key, "100"); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
	th.Settle(res, llm.TokensUsage{})
	waitFor(t, func() bool { return counter(t, mr, key) == 0 })

	res, tripped = th.Reserve(ctx, pricedModel(), "gen", "", 1000, 1000)
	if tripped != "" {
		t.Fatalf("tripped %q", tripped)
	}
	if !mr.Del(key) { // window expired or flushed
		t.Fatalf("%s missing before release", key)
	}
	th.Settle(res, llm.TokensUsage{})
	time.Sleep(100 * time.Millisecond)
	if mr.Exists(key) {
		t.Fatal("a release must not recreate a missing counter")
	}
}

func TestSpendCheckAndChargeForBatches(t *testing.T) {
	th, mr := newTestThrottle(t)
	now := time.Now().UTC()
	th.now = fixedNow(now)
	th.SetSpendLimitResolver(limits{{"user", "u1", "month"}: 1_000_000}.resolve)
	m := pricedModel() // batch rate: half of standard
	ctx := context.Background()
	key := monthKey(th, "u", "u1", now)

	if tripped := th.CheckDailyBudget(ctx, m, "gen", "u1"); tripped != "" {
		t.Fatalf("fresh month tripped %q", tripped)
	}
	// 1000 in * 500 + 500 out * 1000 = 1M at the batch rate.
	th.ChargeDailyBudget(ctx, m, "gen", "u1", llm.TokensUsage{Input: 1000, Output: 500})
	waitFor(t, func() bool { return counter(t, mr, key) == 1_000_000 })
	if mr.TTL(key) <= 0 {
		t.Fatal("charged counter has no TTL")
	}
	if tripped := th.CheckDailyBudget(ctx, m, "gen", "u1"); tripped != "spend_user_monthly" {
		t.Fatalf("exhausted month: tripped %q, want spend_user_monthly", tripped)
	}
}
