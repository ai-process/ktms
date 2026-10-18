package adapters

import (
	"github.com/ai-process/ktms/internal/llm"
	"math"
	"testing"

	"github.com/openai/openai-go/v3"
)

// A ceiling beyond int32 must clamp rather than wrap negative, which the vendor
// would read as a nonsense budget.
func TestBuildGenerateConfigClampsOversizedCeiling(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	chat.SetMaxOutputTokens(math.MaxInt32 + 1000)

	config := (&GoogleAdapter{}).buildGenerateConfig(chat)
	if config.MaxOutputTokens != math.MaxInt32 {
		t.Fatalf("want clamp to MaxInt32, got %d", config.MaxOutputTokens)
	}
	if config.MaxOutputTokens < 0 {
		t.Fatal("ceiling wrapped negative")
	}
}

func TestBuildCompletionParamsOmitsCeilingWhenZero(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")

	params := (&OpenAIAdapter{}).buildCompletionParams(chat, nil, 0)
	if params.MaxCompletionTokens.Valid() {
		t.Fatalf("a zero ceiling must send no max_completion_tokens, got %v", params.MaxCompletionTokens)
	}
}

// The uncapped retry passes ceiling 0, so this is what makes escalation actually
// lift the limit rather than resend the same budget.
func TestBuildCompletionParamsAppliesReasoningHeadroom(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")

	params := (&OpenAIAdapter{}).buildCompletionParams(chat, nil, llm.MaxTokensSmall)
	if !params.MaxCompletionTokens.Valid() {
		t.Fatal("a positive ceiling must reach max_completion_tokens")
	}
	want := int64(llm.MaxTokensSmall) * openAIReasoningHeadroom
	if got := params.MaxCompletionTokens.Or(0); got != want {
		t.Fatalf("want %d (ceiling x headroom), got %d", want, got)
	}
}

func TestTruncatedOnLength(t *testing.T) {
	cases := []struct {
		name string
		resp *openai.ChatCompletion
		want bool
	}{
		{"nil response", nil, false},
		{"no choices", &openai.ChatCompletion{}, false},
		{"stopped normally", &openai.ChatCompletion{
			Choices: []openai.ChatCompletionChoice{{FinishReason: "stop"}},
		}, false},
		{"hit the limit", &openai.ChatCompletion{
			Choices: []openai.ChatCompletionChoice{{FinishReason: openAIFinishReasonLength}},
		}, true},
		{"second choice hit the limit", &openai.ChatCompletion{
			Choices: []openai.ChatCompletionChoice{
				{FinishReason: "stop"},
				{FinishReason: openAIFinishReasonLength},
			},
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := truncatedOnLength(c.resp); got != c.want {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}

// Thinking budgets on these models can reach tens of thousands of tokens and are
// charged against the same ceiling, so a tier near the answer length would be
// spent on thinking alone and return nothing.
func TestCeilingsLeaveRoomForThinking(t *testing.T) {
	const minRoom = 8_000
	for name, tier := range map[string]int{
		"small":    llm.MaxTokensSmall,
		"plan":     llm.MaxTokensPlan,
		"theory":   llm.MaxTokensTheory,
		"large":    llm.MaxTokensLarge,
		"combined": llm.MaxTokensCombined,
	} {
		if tier < minRoom {
			t.Errorf("tier %s is %d, below the %d needed to cover thinking plus an answer", name, tier, minRoom)
		}
	}
}
