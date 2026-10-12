package adapters

import (
	"encoding/json"
	"testing"

	"github.com/ai-process/llm-proxy/internal/llm"
)

func TestBuildCompletionParamsThinkingOff(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")

	for _, c := range []struct {
		name string
		off  map[string]any
		want map[string]string
	}{
		{"none", nil, nil},
		{"deepseek", map[string]any{"thinking": map[string]string{"type": "disabled"}}, map[string]string{"thinking": `{"type":"disabled"}`}},
		{"ollama", map[string]any{"reasoning_effort": "none"}, map[string]string{"reasoning_effort": `"none"`}},
	} {
		a := &OpenAIAdapter{}
		a.SetThinkingOff(c.off)
		body, err := json.Marshal(a.buildCompletionParams(chat, nil, 0))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		_, thinking := got["thinking"]
		_, effort := got["reasoning_effort"]
		if c.want == nil && (thinking || effort) {
			t.Fatalf("%s: no switch expected, got %s", c.name, body)
		}
		for k, v := range c.want {
			if string(got[k]) != v {
				t.Fatalf("%s: %s = %s, want %s", c.name, k, got[k], v)
			}
		}
	}
}
