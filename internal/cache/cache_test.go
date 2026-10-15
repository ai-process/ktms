package cache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/crypto"
)

func testKeyring(t testing.TB) *crypto.Keyring {
	t.Helper()
	kr, err := crypto.ParseKeyring("1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return kr
}

func setupTestCache(t *testing.T, ttl time.Duration, maxBytes int) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	kr := testKeyring(t)
	c := New(rClient, kr, ttl, maxBytes, "test")
	return c, mr
}

func TestCache_SetAndGet_Hit(t *testing.T) {
	c, mr := setupTestCache(t, time.Hour, 1024*1024)
	ctx := context.Background()

	req := &pb.GenerateTextRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "what is the capital of France?"},
		},
	}
	resp := &pb.GenerateTextResponse{
		Choices:        []string{"The capital of France is Paris."},
		ResolvedModel:  "gpt-4o",
		ResolvedVendor: "openai",
		MatchedRule:    "default-rule",
		Usage: &pb.TokensUsage{
			Input:  12,
			Output: 8,
		},
	}

	apiKeyID := "client-api-key-1"
	cfgVer := int64(10)

	// Cache Miss initially
	got, hit, err := c.Get(ctx, apiKeyID, cfgVer, req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hit || got != nil {
		t.Fatalf("expected cache miss, got hit=%t", hit)
	}

	// Store in Cache
	if err := c.Set(ctx, apiKeyID, cfgVer, req, resp); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Verify that data in Redis is encrypted
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected 1 key in redis, got %d", len(keys))
	}
	rawVal, err := mr.Get(keys[0])
	if err != nil {
		t.Fatalf("mr.Get: %v", err)
	}
	if strings.Contains(rawVal, "Paris") {
		t.Fatalf("expected encrypted cache in Redis, but plaintext 'Paris' leaked in: %s", rawVal)
	}
	if !strings.HasPrefix(rawVal, "1:") {
		t.Fatalf("expected versioned ciphertext prefix '1:', got %s", rawVal)
	}

	// Cache Hit
	got, hit, err = c.Get(ctx, apiKeyID, cfgVer, req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !hit || got == nil {
		t.Fatalf("expected cache hit, got hit=%t", hit)
	}
	if !got.Cached {
		t.Error("expected got.Cached == true")
	}
	if len(got.Choices) != 1 || got.Choices[0] != "The capital of France is Paris." {
		t.Errorf("choices mismatch: %v", got.Choices)
	}
	if got.ResolvedModel != "gpt-4o" || got.ResolvedVendor != "openai" {
		t.Errorf("resolved model/vendor mismatch: %s/%s", got.ResolvedModel, got.ResolvedVendor)
	}
}

func TestCache_MaxBytesLimit(t *testing.T) {
	// 50 bytes limit
	c, mr := setupTestCache(t, time.Hour, 50)
	ctx := context.Background()

	req := &pb.GenerateTextRequest{
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hi"}},
	}
	resp := &pb.GenerateTextResponse{
		Choices: []string{"A very long response choice that definitely exceeds 50 bytes of json representation"},
	}

	if err := c.Set(ctx, "k1", 1, req, resp); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if len(mr.Keys()) != 0 {
		t.Fatalf("expected 0 keys in redis when response exceeds maxBytes, got %d", len(mr.Keys()))
	}
}

func TestCache_TTLExpiration(t *testing.T) {
	c, mr := setupTestCache(t, 2*time.Second, 1024*1024)
	ctx := context.Background()

	req := &pb.GenerateTextRequest{
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "test"}},
	}
	resp := &pb.GenerateTextResponse{Choices: []string{"ok"}}

	if err := c.Set(ctx, "k1", 1, req, resp); err != nil {
		t.Fatalf("Set: %v", err)
	}

	mr.FastForward(3 * time.Second)

	got, hit, err := c.Get(ctx, "k1", 1, req)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hit || got != nil {
		t.Fatal("expected cache miss after TTL expiration")
	}
}
