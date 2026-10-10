package adapters

import (
	"encoding/json"
	"testing"

	"github.com/ai-process/llm-proxy/internal/llm"
)

func TestBuildCompletionParamsThinkingSwitch(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")

	for _, off := range []bool{false, true} {
		a := &OpenAIAdapter{}
		a.SetNoThinking(off)
		body, err := json.Marshal(a.buildCompletionParams(chat, nil, 0))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		thinking, sent := got["thinking"]
		if sent != off {
			t.Fatalf("noThinking=%v: thinking sent=%v (%s)", off, sent, body)
		}
		if off && string(thinking) != `{"type":"disabled"}` {
			t.Fatalf("want thinking disabled, got %s", thinking)
		}
	}
}
