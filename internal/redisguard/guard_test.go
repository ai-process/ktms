package redisguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestGuardOpensAndRecovers(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	g := New()
	g.now = func() time.Time { return now }
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	rdb.AddHook(g)
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	if err := rdb.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("healthy Set: %v", err)
	}
	mr.Close()
	for i := 0; i < tripAfter; i++ {
		_ = rdb.Get(ctx, "k").Err()
	}
	if !g.Open() {
		t.Fatal("guard must open after consecutive transport failures")
	}
	start := time.Now()
	if err := rdb.Get(ctx, "k").Err(); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen without network IO", err)
	}
	if time.Since(start) > 10*time.Millisecond {
		t.Fatal("an open guard must answer immediately")
	}

	// Redis comes back; after openFor one probe goes through and closes the guard.
	if err := mr.StartAddr(addr); err != nil {
		t.Fatal(err)
	}
	now = now.Add(openFor)
	if err := rdb.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("probe Set: %v", err)
	}
	if g.Open() {
		t.Fatal("guard must close once a probe succeeds")
	}
}

// Answers from a live Redis (a missing key, a script error) are not outages.
func TestRedisRepliesDoNotOpenGuard(t *testing.T) {
	mr := miniredis.RunT(t)
	g := New()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdb.AddHook(g)
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	for i := 0; i < 2*tripAfter; i++ {
		_ = rdb.Get(ctx, "missing").Err()
		_ = rdb.Eval(ctx, "return redis.error_reply('boom')", nil).Err()
	}
	if g.Open() {
		t.Fatal("redis.Nil and script errors must not open the guard")
	}
}
