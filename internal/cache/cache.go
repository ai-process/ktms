package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/crypto"
)

// Cache provides encrypted exact-match caching for LLM completions in Redis.
type Cache struct {
	client   *redis.Client
	keyring  *crypto.Keyring
	ttl      time.Duration
	maxBytes int
	prefix   string
}

// New creates a new Cache instance.
func New(client *redis.Client, keyring *crypto.Keyring, ttl time.Duration, maxBytes int, prefix string) *Cache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if maxBytes <= 0 {
		maxBytes = 1048576 // 1MB default
	}
	return &Cache{
		client:   client,
		keyring:  keyring,
		ttl:      ttl,
		maxBytes: maxBytes,
		prefix:   prefix,
	}
}

func (c *Cache) redisKey(apiKeyID, hash string) string {
	if c.prefix != "" {
		return fmt.Sprintf("%s:cache:%s:%s", c.prefix, apiKeyID, hash)
	}
	return fmt.Sprintf("cache:%s:%s", apiKeyID, hash)
}

// Get retrieves and decrypts a cached response for the given request.
func (c *Cache) Get(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest) (*pb.GenerateTextResponse, bool, error) {
	if c == nil || c.client == nil || c.keyring == nil {
		return nil, false, nil
	}

	hash := ComputeKey(apiKeyID, configVersion, req)
	key := c.redisKey(apiKeyID, hash)

	val, err := c.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redis get cache: %w", err)
	}

	plaintext, err := c.keyring.DecodeVersioned(val)
	if err != nil {
		log.Warn().Err(err).Str("key", key).Msg("cache decryption failed, treating as miss")
		return nil, false, nil
	}

	var resp pb.GenerateTextResponse
	if err := protojson.Unmarshal([]byte(plaintext), &resp); err != nil {
		log.Warn().Err(err).Str("key", key).Msg("cache unmarshal failed, treating as miss")
		return nil, false, nil
	}

	resp.Cached = true
	return &resp, true, nil
}

// Set encrypts and stores a response in Redis under the deterministic key.
func (c *Cache) Set(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest, resp *pb.GenerateTextResponse) error {
	if c == nil || c.client == nil || c.keyring == nil || resp == nil {
		return nil
	}

	// Avoid mutating the original response
	toCache := proto.Clone(resp).(*pb.GenerateTextResponse)
	toCache.Cached = false

	raw, err := protojson.Marshal(toCache)
	if err != nil {
		return fmt.Errorf("marshal cache response: %w", err)
	}

	if len(raw) > c.maxBytes {
		log.Debug().Int("bytes", len(raw)).Int("max", c.maxBytes).Msg("response exceeds cache max size, skipping")
		return nil
	}

	blob, version, err := c.keyring.Encrypt(string(raw))
	if err != nil {
		return fmt.Errorf("encrypt cache response: %w", err)
	}

	encoded := crypto.EncodeVersioned(blob, version)
	hash := ComputeKey(apiKeyID, configVersion, req)
	key := c.redisKey(apiKeyID, hash)

	if err := c.client.Set(ctx, key, encoded, c.ttl).Err(); err != nil {
		return fmt.Errorf("redis set cache: %w", err)
	}

	return nil
}
