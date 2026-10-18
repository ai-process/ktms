package adapters

import (
	"github.com/ai-process/ktms/internal/llm"
	"strings"
	"testing"
)

func TestBuildSystemInstructionTextReadsV2Base(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddSystemMessage("You are a linguist.")
	chat.AddUserMessage("hello")

	got := buildSystemInstructionText(chat)
	if !strings.Contains(got, "You are a linguist.") {
		t.Fatalf("v2 contexts keep system text in BaseSystemInstruction; got %q", got)
	}
}

func TestBuildSystemInstructionTextJoinsStackedMessages(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddSystemMessage("first")
	chat.AddSystemMessage("second")

	got := buildSystemInstructionText(chat)
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("both stacked system messages must survive, got %q", got)
	}
}

func TestBuildSystemInstructionTextAppendsPerRequestOnce(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddSystemMessage("base")
	chat.SetRequestInstruction("delta")

	got := buildSystemInstructionText(chat)
	if !strings.Contains(got, "base") || !strings.Contains(got, "delta") {
		t.Fatalf("want base and delta, got %q", got)
	}
	// Reading clears it, which is why adapters must build the instruction once.
	if second := buildSystemInstructionText(chat); strings.Contains(second, "delta") {
		t.Fatalf("per-request instruction should be consumed by the first read, got %q", second)
	}
}

func TestBuildSystemInstructionTextEmptyWhenNoSystemText(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("hi")
	if got := buildSystemInstructionText(chat); got != "" {
		t.Fatalf("want empty so callers can apply their own default, got %q", got)
	}
}

// The bug this guards: ChatContext v2 stores system text in BaseSystemInstruction
// rather than the messages array, so an adapter reading only GetMessages() sent
// every OpenAI fallback request with no instructions at all.
func TestToOpenAiLeadsWithSystemInstruction(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddSystemMessage("Answer with JSON only.")
	chat.AddAssistantMessage("previous question")
	chat.AddUserMessage("student answer")

	msgs := (&OpenAIAdapter{}).toOpenAi(chat)
	if len(msgs) != 3 {
		t.Fatalf("want system + assistant + user, got %d messages", len(msgs))
	}
	if msgs[0].OfSystem == nil {
		t.Fatal("the first message must be the system instruction")
	}
	if got := msgs[0].OfSystem.Content.OfString.Value; !strings.Contains(got, "Answer with JSON only.") {
		t.Fatalf("system message lost the instruction, got %q", got)
	}
	if msgs[1].OfAssistant == nil || msgs[2].OfUser == nil {
		t.Fatal("conversation order must be preserved after the system message")
	}
}

func TestToOpenAiDoesNotDuplicateLegacySystemMessages(t *testing.T) {
	chat := llm.NewChatContext()
	chat.Version = llm.ChatContextVersionLegacy
	chat.AddSystemMessage("legacy instruction")
	chat.AddUserMessage("hi")

	msgs := (&OpenAIAdapter{}).toOpenAi(chat)
	systemCount := 0
	for _, m := range msgs {
		if m.OfSystem != nil {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Fatalf("legacy system text must be sent exactly once, got %d system messages", systemCount)
	}
}

func TestToOpenAiOmitsSystemMessageWhenNoInstruction(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("hi")

	msgs := (&OpenAIAdapter{}).toOpenAi(chat)
	if len(msgs) != 1 || msgs[0].OfUser == nil {
		t.Fatalf("want just the user message, got %d messages", len(msgs))
	}
}
