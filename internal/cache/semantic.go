package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/latent"
)

const (
	semanticIDBytes = 16
	// maxSemanticCandidates bounds the response fetches for one lookup when the
	// best-scoring entry's response has already expired.
	maxSemanticCandidates = 3
)

// SemanticOptions tunes the latent-space cache.
type SemanticOptions struct {
	// Threshold is the minimum cosine similarity that counts as a hit.
	Threshold float64
	// MaxEntries bounds how many vectors one bucket keeps and a lookup scans.
	MaxEntries int
	TTL        time.Duration
	MaxBytes   int
	Prefix     string
}

// SemanticCache serves responses for prompts that mean the same thing as an
// earlier one. Everything except the final user utterance must match exactly
// (the bucket); only that utterance is compared in latent space.
type SemanticCache struct {
	client     *redis.Client
	keyring    *crypto.Keyring
	enc        latent.Encoder
	threshold  float32
	maxEntries int
	ttl        time.Duration
	maxBytes   int
	prefix     string
}

// NewSemantic builds a semantic cache over Redis; vectors and responses are
// encrypted at rest like the exact-match cache, since embeddings can be inverted.
func NewSemantic(client *redis.Client, keyring *crypto.Keyring, enc latent.Encoder, opts SemanticOptions) *SemanticCache {
	if opts.Threshold <= 0 || opts.Threshold > 1 {
		opts.Threshold = 0.95
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 256
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 1048576
	}
	return &SemanticCache{
		client:     client,
		keyring:    keyring,
		enc:        enc,
		threshold:  float32(opts.Threshold),
		maxEntries: opts.MaxEntries,
		ttl:        opts.TTL,
		maxBytes:   opts.MaxBytes,
		prefix:     opts.Prefix,
	}
}

// SemanticText returns the utterance compared in latent space: the final user
// message, or the request instruction when there are no messages. Requests
// ending in tool traffic or non-text parts are not semantically cacheable.
func SemanticText(req *pb.GenerateTextRequest) (string, bool) {
	msgs := req.GetMessages()
	if len(msgs) == 0 {
		t := strings.TrimSpace(req.GetRequestInstruction())
		return t, t != ""
	}
	last := msgs[len(msgs)-1]
	if last.GetRole() != pb.MessageRole_MESSAGE_ROLE_USER || len(last.GetToolCalls()) > 0 || last.GetToolCallId() != "" {
		return "", false
	}
	var b strings.Builder
	b.WriteString(last.GetText())
	for _, p := range last.GetParts() {
		tp, ok := p.GetPart().(*pb.ContentPart_Text)
		if !ok {
			return "", false
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(tp.Text.GetText())
	}
	t := strings.TrimSpace(b.String())
	return t, t != ""
}

// semanticBucket hashes the exact-match context around the utterance. Numbers
// in the utterance are part of it: "convert 100 USD" and "convert 1000 USD"
// embed close together but must never share an answer.
func semanticBucket(apiKeyID string, configVersion int64, emb latent.Embedding, req *pb.GenerateTextRequest, text string) string {
	h := sha256.New()
	fmt.Fprintf(h, "semantic-v1\nkey_id:%s\ncfg_ver:%d\nmodel:%s\ndims:%d\n", apiKeyID, configVersion, emb.Model, len(emb.Vector))
	msgs := req.GetMessages()
	if len(msgs) == 0 {
		writeParams(h, req, false)
	} else {
		writeParams(h, req, true)
		for i, m := range msgs[:len(msgs)-1] {
			writeMessage(h, i, m)
		}
	}
	for _, tok := range latent.Tokenize(text) {
		if strings.IndexFunc(tok, unicode.IsDigit) >= 0 {
			fmt.Fprintf(h, "num:%s\n", tok)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *SemanticCache) indexKey(apiKeyID, bucket string) string {
	if c.prefix != "" {
		return fmt.Sprintf("%s:semcache:%s:%s:idx", c.prefix, apiKeyID, bucket)
	}
	return fmt.Sprintf("semcache:%s:%s:idx", apiKeyID, bucket)
}

func (c *SemanticCache) responseKey(apiKeyID, bucket, id string) string {
	if c.prefix != "" {
		return fmt.Sprintf("%s:semcache:%s:%s:r:%s", c.prefix, apiKeyID, bucket, id)
	}
	return fmt.Sprintf("semcache:%s:%s:r:%s", apiKeyID, bucket, id)
}

func (c *SemanticCache) usable() bool {
	return c != nil && c.client != nil && c.keyring != nil && c.enc != nil
}

func (c *SemanticCache) embed(ctx context.Context, req *pb.GenerateTextRequest) (latent.Embedding, string, bool, error) {
	text, ok := SemanticText(req)
	if !ok {
		return latent.Embedding{}, "", false, nil
	}
	embs, err := c.enc.Encode(ctx, []string{text})
	if err != nil {
		return latent.Embedding{}, "", false, fmt.Errorf("encode prompt: %w", err)
	}
	if len(embs) != 1 || len(embs[0].Vector) == 0 {
		return latent.Embedding{}, "", false, fmt.Errorf("encode prompt: %w", latent.ErrInvalidVector)
	}
	return embs[0], text, true, nil
}

type semanticCandidate struct {
	id    string
	score float32
}

// Get returns the cached response whose prompt is most similar to req, with
// its similarity, when that similarity reaches the threshold.
func (c *SemanticCache) Get(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest) (*pb.GenerateTextResponse, float32, bool, error) {
	if !c.usable() {
		return nil, 0, false, nil
	}
	emb, text, ok, err := c.embed(ctx, req)
	if err != nil || !ok {
		return nil, 0, false, err
	}
	bucket := semanticBucket(apiKeyID, configVersion, emb, req, text)
	idx := c.indexKey(apiKeyID, bucket)

	entries, err := c.client.LRange(ctx, idx, 0, int64(c.maxEntries-1)).Result()
	if err != nil {
		return nil, 0, false, fmt.Errorf("redis lrange semantic index: %w", err)
	}
	var cands []semanticCandidate
	for _, e := range entries {
		id, vec, err := c.decodeEntry(e)
		if err != nil {
			log.Warn().Err(err).Str("key", idx).Msg("semantic cache entry unreadable, skipping")
			continue
		}
		if s := latent.Dot(emb.Vector, vec); s >= c.threshold {
			cands = append(cands, semanticCandidate{id: id, score: s})
		}
	}
	if len(cands) == 0 {
		return nil, 0, false, nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > maxSemanticCandidates {
		cands = cands[:maxSemanticCandidates]
	}
	keys := make([]string, len(cands))
	for i, cd := range cands {
		keys[i] = c.responseKey(apiKeyID, bucket, cd.id)
	}
	vals, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, 0, false, fmt.Errorf("redis mget semantic responses: %w", err)
	}
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		plaintext, err := c.keyring.DecodeVersioned(s)
		if err != nil {
			log.Warn().Err(err).Str("key", keys[i]).Msg("semantic cache decryption failed, treating as miss")
			continue
		}
		var resp pb.GenerateTextResponse
		if err := protojson.Unmarshal([]byte(plaintext), &resp); err != nil {
			log.Warn().Err(err).Str("key", keys[i]).Msg("semantic cache unmarshal failed, treating as miss")
			continue
		}
		resp.Cached = true
		return &resp, cands[i].score, true, nil
	}
	return nil, 0, false, nil
}

// Set indexes resp under the latent vector of req's utterance. Responses with
// tool calls are skipped: their arguments echo specifics of the exact prompt.
func (c *SemanticCache) Set(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest, resp *pb.GenerateTextResponse) error {
	if !c.usable() || resp == nil {
		return nil
	}
	for _, d := range resp.GetDetails() {
		if len(d.GetToolCalls()) > 0 {
			return nil
		}
	}
	emb, text, ok, err := c.embed(ctx, req)
	if err != nil || !ok {
		return err
	}

	toCache := proto.Clone(resp).(*pb.GenerateTextResponse)
	toCache.Cached = false
	raw, err := protojson.Marshal(toCache)
	if err != nil {
		return fmt.Errorf("marshal semantic cache response: %w", err)
	}
	if len(raw) > c.maxBytes {
		log.Debug().Int("bytes", len(raw)).Int("max", c.maxBytes).Msg("response exceeds semantic cache max size, skipping")
		return nil
	}
	encResp, err := c.seal(string(raw))
	if err != nil {
		return fmt.Errorf("encrypt semantic cache response: %w", err)
	}

	idBytes := make([]byte, semanticIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return fmt.Errorf("semantic cache entry id: %w", err)
	}
	id := hex.EncodeToString(idBytes)
	encEntry, err := c.seal(string(idBytes) + string(latent.MarshalVector(emb.Vector)))
	if err != nil {
		return fmt.Errorf("encrypt semantic cache entry: %w", err)
	}

	bucket := semanticBucket(apiKeyID, configVersion, emb, req, text)
	idx := c.indexKey(apiKeyID, bucket)
	if _, err := c.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Set(ctx, c.responseKey(apiKeyID, bucket, id), encResp, c.ttl)
		p.LPush(ctx, idx, encEntry)
		p.LTrim(ctx, idx, 0, int64(c.maxEntries-1))
		p.Expire(ctx, idx, c.ttl)
		return nil
	}); err != nil {
		return fmt.Errorf("redis store semantic cache entry: %w", err)
	}
	return nil
}

func (c *SemanticCache) seal(plaintext string) (string, error) {
	blob, version, err := c.keyring.Encrypt(plaintext)
	if err != nil {
		return "", err
	}
	return crypto.EncodeVersioned(blob, version), nil
}

var errBadSemanticEntry = errors.New("malformed semantic cache entry")

func (c *SemanticCache) decodeEntry(encoded string) (string, []float32, error) {
	plaintext, err := c.keyring.DecodeVersioned(encoded)
	if err != nil {
		return "", nil, err
	}
	if len(plaintext) <= semanticIDBytes {
		return "", nil, errBadSemanticEntry
	}
	vec, err := latent.UnmarshalVector([]byte(plaintext[semanticIDBytes:]))
	if err != nil {
		return "", nil, err
	}
	return hex.EncodeToString([]byte(plaintext[:semanticIDBytes])), vec, nil
}
