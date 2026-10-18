package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/latent"
)

// conceptEncoder stands in for a JEPA encoder: texts naming the same concept
// map to the same direction regardless of wording.
type conceptEncoder struct {
	concepts map[string][]float32
	fail     error
	calls    int
}

func (e *conceptEncoder) Encode(_ context.Context, texts []string) ([]latent.Embedding, error) {
	e.calls++
	if e.fail != nil {
		return nil, e.fail
	}
	out := make([]latent.Embedding, len(texts))
	for i, t := range texts {
		v, ok := e.concepts[t]
		if !ok {
			v = []float32{0, 0, 1}
		}
		cp := append([]float32(nil), v...)
		if err := latent.Normalize(cp); err != nil {
			return nil, err
		}
		out[i] = latent.Embedding{Model: "jepa-test", Vector: cp}
	}
	return out, nil
}

func newConceptEncoder() *conceptEncoder {
	return &conceptEncoder{concepts: map[string][]float32{
		"How to sort a list in Go?":    {1, 0.01, 0},
		"Sort a Go slice":              {1, 0, 0},
		"How do I reverse a Go slice?": {0.8, 0.6, 0},
		"convert 100 USD to EUR":       {0, 1, 0},
		"convert 1000 USD to EUR":      {0, 1, 0},
	}}
}

func setupSemantic(t *testing.T, enc latent.Encoder, opts SemanticOptions) (*SemanticCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if opts.Prefix == "" {
		opts.Prefix = "test"
	}
	return NewSemantic(client, testKeyring(t), enc, opts), mr
}

func userReq(text string) *pb.GenerateTextRequest {
	return &pb.GenerateTextRequest{
		Effort:                pb.Effort_EFFORT_MEDIUM,
		BaseSystemInstruction: "you are helpful",
		Messages:              []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: text}},
	}
}

func textResp(text string) *pb.GenerateTextResponse {
	return &pb.GenerateTextResponse{
		Choices:       []string{text},
		Details:       []*pb.Choice{{Text: text, FinishReason: "stop"}},
		ResolvedModel: "gpt-test",
	}
}

func TestSemanticCache_ParaphraseHit(t *testing.T) {
	c, _ := setupSemantic(t, newConceptEncoder(), SemanticOptions{Threshold: 0.95})
	ctx := context.Background()

	if err := c.Set(ctx, "key-1", 1, userReq("How to sort a list in Go?"), textResp("use slices.Sort")); err != nil {
		t.Fatal(err)
	}
	got, score, hit, err := c.Get(ctx, "key-1", 1, userReq("Sort a Go slice"))
	if err != nil || !hit {
		t.Fatalf("expected hit, got hit=%v err=%v", hit, err)
	}
	if !got.GetCached() || got.GetChoices()[0] != "use slices.Sort" {
		t.Fatalf("unexpected response %+v", got)
	}
	if score < 0.95 || score > 1.0001 {
		t.Fatalf("score = %v", score)
	}
}

func TestSemanticCache_BelowThresholdMiss(t *testing.T) {
	c, _ := setupSemantic(t, newConceptEncoder(), SemanticOptions{Threshold: 0.95})
	ctx := context.Background()
	if err := c.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), textResp("use slices.Sort")); err != nil {
		t.Fatal(err)
	}
	// cos = 0.8: related but a different question.
	if _, _, hit, err := c.Get(ctx, "key-1", 1, userReq("How do I reverse a Go slice?")); err != nil || hit {
		t.Fatalf("expected miss, got hit=%v err=%v", hit, err)
	}
	lenient, _ := setupSemantic(t, newConceptEncoder(), SemanticOptions{Threshold: 0.75})
	if err := lenient.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), textResp("use slices.Sort")); err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := lenient.Get(ctx, "key-1", 1, userReq("How do I reverse a Go slice?")); err != nil || !hit {
		t.Fatalf("threshold 0.75 should hit, got hit=%v err=%v", hit, err)
	}
}

func TestSemanticCache_Isolation(t *testing.T) {
	c, _ := setupSemantic(t, newConceptEncoder(), SemanticOptions{})
	ctx := context.Background()
	if err := c.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), textResp("a")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		key string
		ver int64
		req *pb.GenerateTextRequest
	}{
		"tenant":         {"key-2", 1, userReq("Sort a Go slice")},
		"config version": {"key-1", 2, userReq("Sort a Go slice")},
		"system prompt": {"key-1", 1, func() *pb.GenerateTextRequest {
			r := userReq("Sort a Go slice")
			r.BaseSystemInstruction = "answer in French"
			return r
		}()},
		"history": {"key-1", 1, func() *pb.GenerateTextRequest {
			r := userReq("Sort a Go slice")
			r.Messages = append([]*pb.ChatMessage{
				{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "earlier"},
				{Role: pb.MessageRole_MESSAGE_ROLE_ASSISTANT, Text: "ok"},
			}, r.Messages...)
			return r
		}()},
		"effort": {"key-1", 1, func() *pb.GenerateTextRequest {
			r := userReq("Sort a Go slice")
			r.Effort = pb.Effort_EFFORT_HIGH
			return r
		}()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, hit, err := c.Get(ctx, tc.key, tc.ver, tc.req); err != nil || hit {
				t.Fatalf("expected miss, got hit=%v err=%v", hit, err)
			}
		})
	}
}

func TestSemanticCache_NumbersMustMatch(t *testing.T) {
	c, _ := setupSemantic(t, newConceptEncoder(), SemanticOptions{})
	ctx := context.Background()
	if err := c.Set(ctx, "key-1", 1, userReq("convert 100 USD to EUR"), textResp("92 EUR")); err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := c.Get(ctx, "key-1", 1, userReq("convert 1000 USD to EUR")); err != nil || hit {
		t.Fatalf("different amounts must miss even with identical vectors, hit=%v err=%v", hit, err)
	}
	if _, _, hit, err := c.Get(ctx, "key-1", 1, userReq("convert 100 USD to EUR")); err != nil || !hit {
		t.Fatalf("same amount should hit, hit=%v err=%v", hit, err)
	}
}

func TestSemanticCache_HashEncoderLexicalVariants(t *testing.T) {
	c, _ := setupSemantic(t, latent.NewHashEncoder(0), SemanticOptions{Threshold: 0.95})
	ctx := context.Background()
	if err := c.Set(ctx, "key-1", 1, userReq("How do I sort a slice in Go?"), textResp("slices.Sort")); err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := c.Get(ctx, "key-1", 1, userReq("  how do i SORT a slice in go  ")); err != nil || !hit {
		t.Fatalf("case/whitespace variant should hit, hit=%v err=%v", hit, err)
	}
	if _, _, hit, err := c.Get(ctx, "key-1", 1, userReq("How do I shuffle a slice in Go?")); err != nil || hit {
		t.Fatalf("different verb should miss, hit=%v err=%v", hit, err)
	}
}

func TestSemanticText(t *testing.T) {
	text := func(s string) *pb.ContentPart {
		return &pb.ContentPart{Part: &pb.ContentPart_Text{Text: &pb.TextPart{Text: s}}}
	}
	cases := []struct {
		name string
		req  *pb.GenerateTextRequest
		want string
		ok   bool
	}{
		{"user text", userReq(" hi "), "hi", true},
		{"instruction only", &pb.GenerateTextRequest{RequestInstruction: "summarize"}, "summarize", true},
		{"text parts", &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Parts: []*pb.ContentPart{text("a"), text("b")}}}}, "a\nb", true},
		{"assistant last", &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_ASSISTANT, Text: "x"}}}, "", false},
		{"tool result", &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x", ToolCallId: "c1"}}}, "", false},
		{"image", &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Parts: []*pb.ContentPart{text("a"), {Part: &pb.ContentPart_Image{Image: &pb.ImagePart{Url: "https://x/y.png"}}}}}}}, "", false},
		{"empty", userReq("   "), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SemanticText(tc.req)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("SemanticText = %q,%v want %q,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSemanticCache_SkipsIneligible(t *testing.T) {
	enc := newConceptEncoder()
	c, mr := setupSemantic(t, enc, SemanticOptions{})
	ctx := context.Background()
	toolResp := textResp("")
	toolResp.Details[0].ToolCalls = []*pb.ToolCall{{Id: "c1", Function: &pb.FunctionCall{Name: "f"}}}
	if err := c.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), toolResp); err != nil {
		t.Fatal(err)
	}
	if len(mr.Keys()) != 0 {
		t.Fatalf("tool-call responses must not be cached, keys=%v", mr.Keys())
	}
	if _, _, hit, err := c.Get(ctx, "key-1", 1, &pb.GenerateTextRequest{}); err != nil || hit {
		t.Fatalf("ineligible request must miss silently, hit=%v err=%v", hit, err)
	}
	if enc.calls != 0 {
		t.Fatalf("ineligible requests must not reach the encoder, calls=%d", enc.calls)
	}
}

func TestSemanticCache_EncoderAndRedisErrors(t *testing.T) {
	enc := newConceptEncoder()
	c, mr := setupSemantic(t, enc, SemanticOptions{})
	ctx := context.Background()
	enc.fail = errors.New("encoder down")
	if _, _, _, err := c.Get(ctx, "key-1", 1, userReq("Sort a Go slice")); err == nil {
		t.Fatal("encoder failure must surface")
	}
	if err := c.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), textResp("a")); err == nil {
		t.Fatal("encoder failure must surface on set")
	}
	enc.fail = nil
	mr.Close()
	if _, _, _, err := c.Get(ctx, "key-1", 1, userReq("Sort a Go slice")); err == nil {
		t.Fatal("redis failure must surface")
	}
}

func TestSemanticCache_TTLAndBounds(t *testing.T) {
	c, mr := setupSemantic(t, newConceptEncoder(), SemanticOptions{TTL: time.Minute, MaxEntries: 2, MaxBytes: 200})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := c.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), textResp(fmt.Sprintf("answer %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	var idx string
	for _, k := range mr.Keys() {
		if strings.HasSuffix(k, ":idx") {
			idx = k
		}
	}
	if n, err := mr.List(idx); err != nil || len(n) != 2 {
		t.Fatalf("index should be trimmed to 2, got %d (%v)", len(n), err)
	}
	got, _, hit, err := c.Get(ctx, "key-1", 1, userReq("Sort a Go slice"))
	if err != nil || !hit || got.GetChoices()[0] != "answer 4" {
		t.Fatalf("newest entry should win ties, got %+v hit=%v err=%v", got, hit, err)
	}

	if err := c.Set(ctx, "key-1", 1, userReq("How to sort a list in Go?"), textResp(strings.Repeat("x", 500))); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Minute)
	if _, _, hit, err := c.Get(ctx, "key-1", 1, userReq("Sort a Go slice")); err != nil || hit {
		t.Fatalf("expired entries must miss, hit=%v err=%v", hit, err)
	}
}

func TestSemanticCache_ExpiredBestFallsBackToNextCandidate(t *testing.T) {
	c, mr := setupSemantic(t, newConceptEncoder(), SemanticOptions{Threshold: 0.9})
	ctx := context.Background()
	if err := c.Set(ctx, "key-1", 1, userReq("How to sort a list in Go?"), textResp("older")); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "key-1", 1, userReq("Sort a Go slice"), textResp("best")); err != nil {
		t.Fatal(err)
	}
	for _, k := range mr.Keys() {
		if v, err := mr.Get(k); err == nil && strings.Contains(k, ":r:") {
			plain, err := c.keyring.DecodeVersioned(v)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plain, "best") {
				mr.Del(k)
			}
		}
	}
	got, _, hit, err := c.Get(ctx, "key-1", 1, userReq("Sort a Go slice"))
	if err != nil || !hit || got.GetChoices()[0] != "older" {
		t.Fatalf("expected fallback to next candidate, got %+v hit=%v err=%v", got, hit, err)
	}
}

func TestSemanticCache_NilSafe(t *testing.T) {
	var c *SemanticCache
	if _, _, hit, err := c.Get(context.Background(), "k", 1, userReq("x")); hit || err != nil {
		t.Fatal("nil cache must miss")
	}
	if err := c.Set(context.Background(), "k", 1, userReq("x"), textResp("y")); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkSemanticCacheLookup measures the whole lookup path against a full
// bucket: local encode, decrypting and scoring every vector, and the fetch.
func BenchmarkSemanticCacheLookup(b *testing.B) {
	for _, entries := range []int{64, 256} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			mr := miniredis.RunT(b)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			enc := latent.NewMemo(latent.NewHashEncoder(latent.DefaultHashDims), 1024)
			kr := testKeyring(b)
			c := NewSemantic(client, kr, enc, SemanticOptions{MaxEntries: entries, Threshold: 0.95, Prefix: "bench"})
			ctx := context.Background()
			for i := 0; i < entries; i++ {
				if err := c.Set(ctx, "key-1", 1, userReq(fmt.Sprintf("question about topic%c%c", 'a'+i%26, 'a'+i/26)), textResp("answer")); err != nil {
					b.Fatal(err)
				}
			}
			req := userReq("question about topicaa")
			b.ReportAllocs()
			for b.Loop() {
				if _, _, hit, err := c.Get(ctx, "key-1", 1, req); err != nil || !hit {
					b.Fatalf("hit=%v err=%v", hit, err)
				}
			}
		})
	}
}
