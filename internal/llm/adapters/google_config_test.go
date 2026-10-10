package adapters

import (
	"github.com/ai-process/llm-proxy/internal/llm"
	"testing"

	"google.golang.org/genai"
)

func TestContextToContentsParallelTools(t *testing.T) {
	adapter := &GoogleAdapter{}
	messages := []*llm.Message{
		{
			Type: llm.MessageTypeUser,
			Text: "What is the weather and time in Tokyo?",
		},
		{
			Type: llm.MessageTypeBot,
			ToolCalls: []*llm.ToolCall{
				{
					ID: "call_weather_1",
					Function: llm.FunctionCall{
						Name:      "get_weather",
						Arguments: `{"city":"Tokyo"}`,
					},
				},
				{
					ID: "call_time_1",
					Function: llm.FunctionCall{
						Name:      "get_time",
						Arguments: `{"city":"Tokyo"}`,
					},
				},
			},
		},
		{
			Type:       llm.MessageTypeTool,
			ToolCallID: "call_weather_1",
			Text:       `{"temperature":20}`,
		},
		{
			Type:       llm.MessageTypeTool,
			ToolCallID: "call_time_1",
			Text:       `{"time":"10:00"}`,
		},
	}

	contents := adapter.contextToContents(messages)
	if len(contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(contents))
	}

	if contents[0].Role != genai.RoleUser {
		t.Errorf("expected RoleUser, got %s", contents[0].Role)
	}
	if contents[1].Role != genai.RoleModel {
		t.Errorf("expected RoleModel, got %s", contents[1].Role)
	}
	if len(contents[1].Parts) != 2 {
		t.Fatalf("expected 2 parts in model turn, got %d", len(contents[1].Parts))
	}

	toolTurn := contents[2]
	if toolTurn.Role != genai.RoleUser {
		t.Errorf("expected RoleUser for tool responses, got %s", toolTurn.Role)
	}
	if len(toolTurn.Parts) != 2 {
		t.Fatalf("expected 2 parts in tool turn, got %d", len(toolTurn.Parts))
	}

	p0 := toolTurn.Parts[0].FunctionResponse
	if p0 == nil || p0.Name != "get_weather" || p0.ID != "call_weather_1" {
		t.Errorf("unexpected part 0: %+v", p0)
	}

	p1 := toolTurn.Parts[1].FunctionResponse
	if p1 == nil || p1.Name != "get_time" || p1.ID != "call_time_1" {
		t.Errorf("unexpected part 1: %+v", p1)
	}
}

func TestBuildGenerateConfigSendsOutputCeiling(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddSystemMessage("be brief")
	chat.AddUserMessage("go")
	chat.SetMaxOutputTokens(llm.MaxTokensTheory)

	config := (&GoogleAdapter{}).buildGenerateConfig(chat)
	if config.MaxOutputTokens != int32(llm.MaxTokensTheory) {
		t.Fatalf("ceiling must reach the vendor config: want %d, got %d", llm.MaxTokensTheory, config.MaxOutputTokens)
	}
}

// An unset ceiling must stay absent rather than become an explicit 0, which the
// vendor would read as "generate nothing".
func TestBuildGenerateConfigOmitsUnsetCeiling(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")

	config := (&GoogleAdapter{}).buildGenerateConfig(chat)
	if config.MaxOutputTokens != 0 {
		t.Fatalf("want no ceiling field set, got %d", config.MaxOutputTokens)
	}
}

func TestBuildGenerateConfigCarriesSystemInstruction(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddSystemMessage("you are a linguist")

	config := (&GoogleAdapter{}).buildGenerateConfig(chat)
	if config.SystemInstruction == nil || len(config.SystemInstruction.Parts) == 0 {
		t.Fatal("system instruction must be attached to the config")
	}
	if got := config.SystemInstruction.Parts[0].Text; got != "you are a linguist" {
		t.Fatalf("want the system text verbatim, got %q", got)
	}
}

func TestBuildGenerateConfigSetsJSONModeWithSchema(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("go")
	chat.SetResponseSchema(&llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"word": {Type: llm.SchemaPropertyTypeString},
		},
	})

	config := (&GoogleAdapter{}).buildGenerateConfig(chat)
	if config.ResponseSchema == nil {
		t.Fatal("schema must be forwarded to the vendor config")
	}
	if config.ResponseMIMEType != "application/json" {
		t.Fatalf("a response schema requires JSON mime type, got %q", config.ResponseMIMEType)
	}
}
