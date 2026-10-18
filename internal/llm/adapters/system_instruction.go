package adapters

import (
	"github.com/ai-process/ktms/internal/llm"
	"strings"
)

// defaultSystemInstruction is what a chat carrying no system text at all has
// always been sent with.
const defaultSystemInstruction = "You are a helpful assistant."

// systemInstructionSeparator matches how ChatContext.AddSystemMessage joins
// stacked system messages, so a chat reads the same however its text arrived.
const systemInstructionSeparator = "\n\n---\n\n"

// buildSystemInstructionText assembles a chat's system instruction: the persistent
// base plus any per-request addition, falling back to inline system messages for
// legacy contexts. Reading the per-request instruction clears it, so every adapter
// must call this exactly once per request.
func buildSystemInstructionText(chat *llm.ChatContext) string {
	var parts []string
	if base := chat.GetBaseSystemInstruction(); base != "" {
		parts = append(parts, base)
	}
	if perRequest := chat.GetRequestInstruction(); perRequest != "" {
		parts = append(parts, perRequest)
	}
	if len(parts) == 0 {
		for _, message := range chat.GetMessages() {
			if message.Type == llm.MessageTypeSystem {
				parts = append(parts, message.Text)
			}
		}
	}
	return strings.Join(parts, systemInstructionSeparator)
}
