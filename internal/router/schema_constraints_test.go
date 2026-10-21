package router

import (
	"context"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func enumChat() *llm.ChatContext {
	chat := llm.NewChatContext()
	chat.SetResponseSchema(&llm.ResponseSchema{
		Type:       llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{"status": {Type: llm.SchemaPropertyTypeString, Enum: []string{"open", "closed"}}},
		Required:   []string{"status"},
	})
	return chat
}

func TestExecuteSoftSchemaEnumViolationFallsBack(t *testing.T) {
	soft := &fakeAdapter{responses: []func() (*llm.Response, error){ok(`{"status":"pending"}`)}}
	good := &fakeAdapter{responses: []func() (*llm.Response, error){ok(`{"status":"open"}`)}}
	adapters := map[string]llm.ClientAdapter{"soft": soft, "strict": good}
	resp, m, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:      []*proxydb.Model{model("soft", registry.VendorDeepSeek), model("strict", registry.VendorOpenAI)},
		Chat:       enumChat(),
		Adapter:    func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
		SoftSchema: func(m *proxydb.Model) bool { return m.ID == "soft" },
	})
	if err != nil || m.ID != "strict" || resp.Choices[0].Text != `{"status":"open"}` {
		t.Fatalf("m=%v err=%v: a value outside the enum must not reach the caller", m, err)
	}
	if soft.calls != schemaAttempts {
		t.Fatalf("soft model called %d times, want %d", soft.calls, schemaAttempts)
	}
}

func toolReply(args string) func() (*llm.Response, error) {
	return func() (*llm.Response, error) {
		r := &llm.Response{Usage: llm.TokensUsage{Input: 3, Output: 2}}
		r.AddMessage(&llm.Message{Type: llm.MessageTypeBot, ToolCalls: []*llm.ToolCall{
			{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "set_status", Arguments: args}},
		}})
		return r, nil
	}
}

func toolChat(strict bool) *llm.ChatContext {
	chat := llm.NewChatContext()
	chat.SetTools([]*llm.Tool{{Type: "function", Function: llm.FunctionDefinition{
		Name:   "set_status",
		Strict: strict,
		Parameters: &llm.ResponseSchema{
			Type:       llm.SchemaPropertyTypeObject,
			Properties: map[string]*llm.SchemaProperty{"status": {Type: llm.SchemaPropertyTypeString, Enum: []string{"open", "closed"}}},
			Required:   []string{"status"},
		},
	}}})
	return chat
}

func TestExecuteStrictToolCallArgumentsAreChecked(t *testing.T) {
	sloppy := &fakeAdapter{responses: []func() (*llm.Response, error){toolReply(`{"status":"maybe"}`)}}
	good := &fakeAdapter{responses: []func() (*llm.Response, error){toolReply(`{"status":"open"}`)}}
	adapters := map[string]llm.ClientAdapter{"gemini": sloppy, "gpt": good}
	_, m, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:   []*proxydb.Model{model("gemini", registry.VendorGoogle), model("gpt", registry.VendorOpenAI)},
		Chat:    toolChat(true),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	})
	if err != nil || m.ID != "gpt" {
		t.Fatalf("m=%v err=%v: a strict tool call outside its schema must fall back", m, err)
	}
}

func TestExecuteNonStrictToolCallsPassThrough(t *testing.T) {
	sloppy := &fakeAdapter{responses: []func() (*llm.Response, error){toolReply(`{"status":"maybe"}`)}}
	_, m, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:   []*proxydb.Model{model("gemini", registry.VendorGoogle)},
		Chat:    toolChat(false),
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return sloppy, nil },
	})
	if err != nil || m.ID != "gemini" || sloppy.calls != 1 {
		t.Fatalf("m=%v err=%v calls=%d: best-effort tools are the caller's to validate", m, err, sloppy.calls)
	}
}
