package adapters

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/openai/openai-go/v3"

	"github.com/ai-process/ktms/internal/llm"
)

// Each vendor reports cached input its own way; Input always includes it.
func TestCacheUsagePerVendor(t *testing.T) {
	var msg anthropic.Message
	if err := json.Unmarshal([]byte(`{"usage":{"input_tokens":1000,"cache_read_input_tokens":10000,
		"cache_creation_input_tokens":2000,"output_tokens":500}}`), &msg); err != nil {
		t.Fatal(err)
	}
	check(t, "anthropic", anthropicUsage(&msg), llm.TokensUsage{Input: 13000, Output: 500, CacheRead: 10000, CacheWrite: 2000})

	var oa openai.CompletionUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10000,"completion_tokens":1000,"total_tokens":11000,
		"prompt_tokens_details":{"cached_tokens":8000}}`), &oa); err != nil {
		t.Fatal(err)
	}
	check(t, "openai", completionUsage(oa), llm.TokensUsage{Input: 10000, Output: 1000, CacheRead: 8000})

	var ds openai.CompletionUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10000,"completion_tokens":10,"total_tokens":10010,
		"prompt_cache_hit_tokens":9000,"prompt_cache_miss_tokens":1000}`), &ds); err != nil {
		t.Fatal(err)
	}
	check(t, "deepseek", completionUsage(ds), llm.TokensUsage{Input: 10000, Output: 10, CacheRead: 9000})

	var dsFloat openai.CompletionUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,"prompt_cache_hit_tokens":90.0}`), &dsFloat); err != nil {
		t.Fatal(err)
	}
	check(t, "deepseek-compatible", completionUsage(dsFloat), llm.TokensUsage{Input: 100, Output: 1, CacheRead: 90})

	if got := geminiCacheRead(80000, 100000); got != 80000 {
		t.Fatalf("gemini cache read = %d, want 80000", got)
	}
	if got := geminiCacheRead(5, 3); got != 3 {
		t.Fatalf("gemini cache read above the prompt = %d, want 3", got)
	}

	check(t, "bedrock", bedrockUsage(&types.TokenUsage{
		InputTokens: aws.Int32(1000), OutputTokens: aws.Int32(500),
		CacheReadInputTokens: aws.Int32(10000), CacheWriteInputTokens: aws.Int32(2000),
	}), llm.TokensUsage{Input: 13000, Output: 500, CacheRead: 10000, CacheWrite: 2000})
}

func check(t *testing.T, vendor string, got, want llm.TokensUsage) {
	t.Helper()
	if got != want {
		t.Errorf("%s usage = %+v, want %+v", vendor, got, want)
	}
}
