package throttle

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
)

func newTestThrottle(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	th := New(client, "test")
	th.now = time.Now
	return th, mr
}

func model(rpm int32, perKey, perUser int64) *proxydb.Model {
	return &proxydb.Model{ID: "m1", Vendor: "openai", RPM: rpm,
		DailyTokensPerKey: perKey, DailyTokensPerUser: perUser, Enabled: true}
}

func TestAllowRPMWindow(t *testing.T) {
	th, mr := newTestThrottle(t)
	ctx := context.Background()
	m := model(2, 0, 0)
	m.Burst = 1

	if !th.AllowRPM(ctx, "gen", "", m) || !th.AllowRPM(ctx, "gen", "", m) {
		t.Fatal("first two calls must pass")
	}
	if th.AllowRPM(ctx, "gen", "", m) {
		t.Fatal("third call in rapid succession must be throttled by GCRA")
	}
	// Another client key has its own rate limiter.
	if !th.AllowRPM(ctx, "other", "", m) {
		t.Fatal("another key must not share the window")
	}

	// Advance time past emission interval: allows another call.
	base := th.now
	th.now = func() time.Time { return base().Add(31 * time.Second) }
	if !th.AllowRPM(ctx, "gen", "", m) {
		t.Fatal("after emission interval must allow next call")
	}

	// Key has TTL.
	th.now = base
	key := th.keys.GCRAKey("gen", m.ID)
	if mr.TTL(key) <= 0 {
		t.Fatal("gcra key has no TTL")
	}
}

func TestAllowRPMUserOverride(t *testing.T) {
	th, _ := newTestThrottle(t)
	ctx := context.Background()
	m := model(100, 0, 0) // key limit is 100 RPM
	m.Burst = 10

	// Set user override for u_slow: 1 RPM, burst 0
	th.SetUserRateLimitResolver(func(userID, modelID string) *proxydb.UserRateLimit {
		if userID == "u_slow" {
			return &proxydb.UserRateLimit{UserID: "u_slow", ModelID: modelID, RPM: 1, Burst: 1}
		}
		return nil
	})

	if !th.AllowRPM(ctx, "gen", "u_slow", m) || !th.AllowRPM(ctx, "gen", "u_slow", m) {
		t.Fatal("first two calls for u_slow should pass (1 RPM + 1 burst)")
	}
	if th.AllowRPM(ctx, "gen", "u_slow", m) {
		t.Fatal("third call for u_slow should be throttled by user limit")
	}

	// Another user without override has normal key limit
	if !th.AllowRPM(ctx, "gen", "u_normal", m) {
		t.Fatal("call for u_normal should pass")
	}
}

func TestReserveOverLimitRestoresCounter(t *testing.T) {
	th, mr := newTestThrottle(t)
	ctx := context.Background()
	m := model(0, 100, 0)
	day := time.Now().UTC().Format("20060102")
	key := th.keys.BudgetKey("gen", m.ID, day)

	res, tripped := th.Reserve(ctx, m, "gen", "", 60, 0)
	if tripped != "" || len(res.Keys) != 1 {
		t.Fatalf("res=%v tripped=%q", res, tripped)
	}
	// 60 + 60 > 100 without grace margin: refused, and the counter must roll back to 60.
	res2, tripped := th.Reserve(ctx, m, "gen", "", 60, 0)
	if res2 != nil || tripped != "budget_key" {
		t.Fatalf("res=%v tripped=%q, want budget_key", res2, tripped)
	}
	val, _ := mr.Get(key)
	if val != "60" {
		t.Fatalf("counter = %s, want 60 after rollback", val)
	}
	if mr.TTL(key) <= 0 {
		t.Fatal("budget key has no TTL")
	}
}

func TestReserveBudgetGraceMargin(t *testing.T) {
	th, mr := newTestThrottle(t)
	ctx := context.Background()
	m := model(0, 100, 0)
	m.BudgetGraceMargin = 0.20 // 20% grace allows up to 120 total if used < 100 beforehand
	day := time.Now().UTC().Format("20060102")
	key := th.keys.BudgetKey("gen", m.ID, day)

	// 1. Used = 90 (below 100 limit)
	res, tripped := th.Reserve(ctx, m, "gen", "", 90, 0)
	if tripped != "" || res == nil {
		t.Fatalf("initial reservation failed: %s", tripped)
	}

	// 2. Next request: estimate 25.
	// Used before was 90 < 100.
	// New total = 115 <= 120 (within 20% grace). Must be allowed!
	res2, tripped2 := th.Reserve(ctx, m, "gen", "", 25, 0)
	if tripped2 != "" || res2 == nil {
		t.Fatalf("call within grace margin must pass, got: %s", tripped2)
	}
	val, _ := mr.Get(key)
	if val != "115" {
		t.Fatalf("counter = %s, want 115", val)
	}

	// 3. Now used = 115 >= 100. Balance is exhausted. Even a request of size 1 must be refused!
	res3, tripped3 := th.Reserve(ctx, m, "gen", "", 1, 0)
	if tripped3 != "budget_key" || res3 != nil {
		t.Fatalf("subsequent call once exhausted must fail, got: %s", tripped3)
	}

	// Counter stays at 115.
	val, _ = mr.Get(key)
	if val != "115" {
		t.Fatalf("counter = %s, want 115 after rejected reservation", val)
	}
}

func TestReserveUserBudget(t *testing.T) {
	th, _ := newTestThrottle(t)
	ctx := context.Background()
	m := model(0, 0, 50)

	// Without a user id the user budget simply never applies.
	res, tripped := th.Reserve(ctx, m, "gen", "", 999, 0)
	if tripped != "" || res != nil && len(res.Keys) != 0 {
		t.Fatalf("res=%v tripped=%q, want no enforcement", res, tripped)
	}

	if _, tripped = th.Reserve(ctx, m, "gen", "u1", 40, 0); tripped != "" {
		t.Fatalf("first user reserve tripped: %q", tripped)
	}
	if _, tripped = th.Reserve(ctx, m, "gen", "u1", 40, 0); tripped != "budget_user" {
		t.Fatalf("tripped=%q, want budget_user", tripped)
	}
	// A different user has their own budget.
	if _, tripped = th.Reserve(ctx, m, "gen", "u2", 40, 0); tripped != "" {
		t.Fatalf("other user tripped: %q", tripped)
	}
}

func TestReserveTripsSecondBudgetUndoesFirst(t *testing.T) {
	th, mr := newTestThrottle(t)
	ctx := context.Background()
	m := model(0, 1000, 50) // key budget generous, user budget tight
	day := time.Now().UTC().Format("20060102")

	res, tripped := th.Reserve(ctx, m, "gen", "u1", 60, 0)
	if res != nil || tripped != "budget_user" {
		t.Fatalf("res=%v tripped=%q", res, tripped)
	}
	// The key-budget increment must have been rolled back.
	val, _ := mr.Get(th.keys.BudgetKey("gen", m.ID, day))
	if val != "0" && val != "" {
		t.Fatalf("key budget = %q, want rolled back", val)
	}
}

func TestSettleAdjustsToActuals(t *testing.T) {
	th, mr := newTestThrottle(t)
	ctx := context.Background()
	m := model(0, 1000, 0)
	day := time.Now().UTC().Format("20060102")
	key := th.keys.BudgetKey("gen", m.ID, day)

	res, _ := th.Reserve(ctx, m, "gen", "", 100, 0)

	// Actuals higher than the estimate: counter goes up.
	th.Settle(res, llm.TokensUsage{Input: 90, Output: 60})
	waitFor(t, func() bool { v, _ := mr.Get(key); return v == "150" })

	// Failure path: zero actuals release the whole reservation.
	res2, _ := th.Reserve(ctx, m, "gen", "", 100, 0)
	th.Settle(res2, llm.TokensUsage{})
	waitFor(t, func() bool { v, _ := mr.Get(key); return v == "150" })
}

func TestFailOpenWhenRedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	th := New(client, "test")
	mr.Close() // Redis just died

	ctx := context.Background()
	m := model(1, 100, 0)
	if !th.AllowRPM(ctx, "gen", "", m) {
		t.Fatal("rpm must fail open")
	}
	res, tripped := th.Reserve(ctx, m, "gen", "", 50, 0)
	if tripped != "" {
		t.Fatalf("reserve tripped %q, must fail open", tripped)
	}
	th.Settle(res, llm.TokensUsage{Input: 1, Output: 1}) // must not panic
}

func TestNilClientAllowsEverything(t *testing.T) {
	th := New(nil, "")
	ctx := context.Background()
	m := model(1, 1, 1)
	if !th.AllowRPM(ctx, "gen", "", m) {
		t.Fatal("nil client must allow")
	}
	if res, tripped := th.Reserve(ctx, m, "gen", "u", 999, 0); res != nil || tripped != "" {
		t.Fatal("nil client must not reserve or trip")
	}
	th.Settle(nil, llm.TokensUsage{})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestAllowRPMAtomicUserRejectDoesNotChargeKey(t *testing.T) {
	th, _ := newTestThrottle(t)
	ctx := context.Background()
	m := model(10, 0, 0)
	m.Burst = 1

	th.SetUserRateLimitResolver(func(userID, modelID string) *proxydb.UserRateLimit {
		if userID == "u_blocked" {
			return &proxydb.UserRateLimit{UserID: "u_blocked", ModelID: modelID, RPM: 1, Burst: -1}
		}
		return nil
	})

	if !th.AllowRPM(ctx, "gen", "u_blocked", m) {
		t.Fatal("first call for u_blocked must pass")
	}

	if th.AllowRPM(ctx, "gen", "u_blocked", m) {
		t.Fatal("second call for u_blocked should fail due to user limit")
	}

	if !th.AllowRPM(ctx, "gen", "u_other", m) {
		t.Fatal("u_other should not be throttled because rejected user call did not charge key")
	}
}

func TestReserveGraceMarginDisabled(t *testing.T) {
	th, mr := newTestThrottle(t)
	ctx := context.Background()
	m := model(0, 100, 0)
	m.BudgetGraceMargin = -1.0
	day := time.Now().UTC().Format("20060102")
	key := th.keys.BudgetKey("gen", m.ID, day)

	res, tripped := th.Reserve(ctx, m, "gen", "", 90, 0)
	if tripped != "" || res == nil {
		t.Fatalf("first reserve failed: %s", tripped)
	}

	res2, tripped2 := th.Reserve(ctx, m, "gen", "", 25, 0)
	if tripped2 != "budget_key" || res2 != nil {
		t.Fatalf("call exceeding limit with grace disabled must fail, got res=%v tripped=%s", res2, tripped2)
	}
	val, _ := mr.Get(key)
	if val != "90" {
		t.Fatalf("counter = %s, want 90 after rejected reservation", val)
	}
}
