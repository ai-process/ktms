package throttle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func keyLimiter(t *testing.T, now *time.Time) *Redis {
	t.Helper()
	mr := miniredis.RunT(t)
	rl := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "")
	rl.now = func() time.Time { return *now }
	return rl
}

func TestKeyRPMAllowsAMinuteThenSpacesCalls(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rl := keyLimiter(t, &now)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, kind, _ := rl.AdmitKey(ctx, "k", 3, 0, 0); kind != "" {
			t.Fatalf("call %d refused (%s) within the burst", i+1, kind)
		}
	}
	_, kind, retry := rl.AdmitKey(ctx, "k", 3, 0, 0)
	if kind != KindKeyRPM || retry <= 0 || retry > 20*time.Second {
		t.Fatalf("4th call: kind %q retry %v, want key_rpm within 20s", kind, retry)
	}
	now = now.Add(20 * time.Second)
	if _, kind, _ := rl.AdmitKey(ctx, "k", 3, 0, 0); kind != "" {
		t.Fatalf("after one interval: refused (%s)", kind)
	}
	if _, kind, _ := rl.AdmitKey(ctx, "other", 3, 0, 0); kind != "" {
		t.Fatalf("another key shares the first key's rate (%s)", kind)
	}
}

func TestKeyTPMBooksEstimateThenActual(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rl := keyLimiter(t, &now)
	ctx := context.Background()

	settle, kind, _ := rl.AdmitKey(ctx, "k", 0, 1000, 600)
	if kind != "" {
		t.Fatalf("first call refused: %s", kind)
	}
	if _, kind, _ := rl.AdmitKey(ctx, "k", 0, 1000, 600); kind != KindKeyTPM {
		t.Fatalf("600+600 of 1000 tokens: kind %q, want key_tpm", kind)
	}
	settle(100) // used far less than booked: the rest is returned
	if _, kind, _ := rl.AdmitKey(ctx, "k", 0, 1000, 600); kind != "" {
		t.Fatalf("100+600 of 1000 tokens refused: %s", kind)
	}

	_, kind, retry := rl.AdmitKey(ctx, "k", 0, 1000, 1001)
	if kind != KindKeyTPM || retry != 0 {
		t.Fatalf("a call above a minute of tokens: kind %q retry %v, want key_tpm with no retry", kind, retry)
	}
}

func TestKeyTPMSettlesOverspendIntoTheNextMinute(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rl := keyLimiter(t, &now)
	ctx := context.Background()
	settle, _, _ := rl.AdmitKey(ctx, "k", 0, 1000, 10)
	settle(1500) // the estimate was low: the key is in debt for half a minute
	_, kind, retry := rl.AdmitKey(ctx, "k", 0, 1000, 10)
	if kind != KindKeyTPM || retry < 29*time.Second || retry > 31*time.Second {
		t.Fatalf("kind %q retry %v, want key_tpm in about 30s", kind, retry)
	}
}

// Concurrent calls never exceed the rate together.
func TestKeyRPMHoldsUnderConcurrency(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rl := keyLimiter(t, &now)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, kind, _ := rl.AdmitKey(context.Background(), "k", 10, 0, 0); kind == "" {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := admitted.Load(); got != 10 {
		t.Fatalf("admitted %d of 50 concurrent calls, want 10", got)
	}
}

func TestKeyLimitsFailOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rl := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "")
	mr.Close()
	if _, kind, _ := rl.AdmitKey(context.Background(), "k", 1, 1, 5); kind != "" {
		t.Fatalf("Redis down refused the call: %s", kind)
	}
	if _, kind, _ := (&Redis{}).AdmitKey(context.Background(), "k", 1, 1, 5); kind != "" {
		t.Fatalf("no Redis refused the call: %s", kind)
	}
}

// A stream that outlives its token key still books what it used.
func TestKeyTPMSettlesAfterTheKeyExpired(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	rl := keyLimiter(t, &now)
	ctx := context.Background()
	settle, _, _ := rl.AdmitKey(ctx, "k", 0, 1000, 10)
	now = now.Add(3 * time.Minute)
	if err := rl.client.Del(ctx, rl.keys.KeyTPM("k")).Err(); err != nil {
		t.Fatal(err)
	}
	settle(1510)
	_, kind, retry := rl.AdmitKey(ctx, "k", 0, 1000, 10)
	if kind != KindKeyTPM || retry < 29*time.Second || retry > 31*time.Second {
		t.Fatalf("kind %q retry %v, want key_tpm in about 30s", kind, retry)
	}
}
