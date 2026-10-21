package throttle

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ai-process/ktms/internal/config"
	"github.com/ai-process/ktms/internal/proxydb"
)

// A Redis that accepts connections and never answers (a network blackhole)
// must cost a request about REDIS_TIMEOUT, not go-redis's multi-second
// defaults, before the limit fails open.
func TestBlackholedRedisFailsOpenQuickly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() }) // hold it open, say nothing
		}
	}()

	cfg := &config.Config{RedisAddr: ln.Addr().String(), RedisTimeout: 200 * time.Millisecond}
	client := redis.NewClient(cfg.RedisOptions())
	t.Cleanup(func() { _ = client.Close() })
	th := New(client, "t")

	start := time.Now()
	allowed := th.AllowRPM(context.Background(), "k", "u", &proxydb.Model{ID: "m", RPM: 10})
	elapsed := time.Since(start)
	if !allowed {
		t.Fatal("RPM must fail open while Redis is unreachable")
	}
	// Two attempts (one retry) of one 200ms read each, plus slack.
	if elapsed > time.Second {
		t.Fatalf("a blackholed Redis cost %v; want about 2x REDIS_TIMEOUT", elapsed)
	}
}
