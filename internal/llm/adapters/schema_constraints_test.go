package adapters

import (
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/ai-process/ktms/internal/llm"
)

func constrainedSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"status":  {Type: llm.SchemaPropertyTypeString, Enum: []string{"open", "closed"}},
			"score":   {Type: llm.SchemaPropertyTypeNumber, Format: "double"},
			"when":    {Type: llm.SchemaPropertyTypeString, Format: "date-time"},
			"phone":   {Type: llm.SchemaPropertyTypeString, Format: "phone", Description: "caller"},
			"comment": {Type: llm.SchemaPropertyTypeString, Enum: []string{"a"}, Nullable: true},
		},
		Required: []string{"status"},
	}
}

func TestOpenAISchemaCarriesConstraints(t *testing.T) {
	out := convertLLMResponseSchemaToOpenAISchema(constrainedSchema())
	props := out["properties"].(map[string]interface{})
	prop := func(name string) map[string]interface{} { return props[name].(map[string]interface{}) }

	if got := prop("status")["enum"].([]any); len(got) != 2 || got[0] != "open" {
		t.Fatalf("status enum = %v", got)
	}
	if prop("score")["type"] != "number" {
		t.Fatalf("score type = %v, want number", prop("score")["type"])
	}
	if _, ok := prop("score")["format"]; ok {
		t.Fatal("double is not a strict-mode string format and must not be sent")
	}
	if prop("when")["format"] != "date-time" {
		t.Fatalf("when format = %v", prop("when")["format"])
	}
	if _, ok := prop("phone")["format"]; ok || prop("phone")["description"] != "caller (format: phone)" {
		t.Fatalf("unsupported format must move to the description: %v", prop("phone"))
	}
	if got := prop("comment")["enum"].([]any); len(got) != 2 || got[1] != nil {
		t.Fatalf("a nullable enum must list null: %v", got)
	}
}

func TestOpenAISchemaKeepsTopLevelArrayItems(t *testing.T) {
	out := convertLLMResponseSchemaToOpenAISchema(&llm.ResponseSchema{
		Type:  llm.SchemaPropertyTypeArray,
		Items: &llm.SchemaProperty{Type: llm.SchemaPropertyTypeString, Enum: []string{"x"}},
	})
	items, ok := out["items"].(map[string]interface{})
	if !ok || items["type"] != "string" {
		t.Fatalf("items = %v, want the element schema", out["items"])
	}
}

func TestGenaiSchemaCarriesConstraints(t *testing.T) {
	out := convertSchemaToGenai(constrainedSchema())
	if got := out.Properties["status"].Enum; len(got) != 2 || got[1] != "closed" {
		t.Fatalf("status enum = %v", got)
	}
	if out.Properties["score"].Type != genai.TypeNumber || out.Properties["score"].Format != "double" {
		t.Fatalf("score = %+v, want NUMBER/double", out.Properties["score"])
	}
	if out.Properties["when"].Format != "date-time" {
		t.Fatalf("when format = %q", out.Properties["when"].Format)
	}
	phone := out.Properties["phone"]
	if phone.Format != "" || !strings.Contains(phone.Description, "format: phone") {
		t.Fatalf("unsupported format must move to the description: %+v", phone)
	}
}

// Strict mode needs every field required, but only in the copy sent to that
// vendor: the shared request schema decides what the next model must return.
func TestAdaptersDoNotMutateRequestSchema(t *testing.T) {
	chat := llm.NewChatContext()
	chat.AddUserMessage("hi")
	chat.SetResponseSchema(constrainedSchema())

	NewOpenAIAdapter(nil, "gpt", nil).buildCompletionParams(chat, nil, 0)
	NewGoogleAdapter(nil, "gemini", nil).buildGenerateConfig(chat)

	if got := chat.GetResponseSchema().Required; len(got) != 1 || got[0] != "status" {
		t.Fatalf("request schema required = %v, want only [status]", got)
	}
}

func TestNonStringEnumBecomesAHint(t *testing.T) {
	p := &llm.SchemaProperty{Type: llm.SchemaPropertyTypeInteger, Enum: []string{"1", "2"}, Description: "level"}
	oa := convertLLMSchemaPropertyToOpenAISchemaProperty(p)
	if _, ok := oa["enum"]; ok || oa["description"] != "level (allowed values: 1, 2)" {
		t.Fatalf("OpenAI property = %v, want the enum as a hint", oa)
	}
	g := convertPropertySchemaToGenai(p)
	if len(g.Enum) != 0 || !strings.Contains(g.Description, "allowed values: 1, 2") {
		t.Fatalf("Gemini property = %+v, want the enum as a hint", g)
	}
}

func TestObjectClosingRules(t *testing.T) {
	closed := false
	cases := []struct {
		name string
		prop *llm.SchemaProperty
		want bool
	}{
		{"with properties", &llm.SchemaProperty{Type: llm.SchemaPropertyTypeObject, Properties: map[string]*llm.SchemaProperty{"a": {Type: llm.SchemaPropertyTypeString}}}, true},
		{"free-form", &llm.SchemaProperty{Type: llm.SchemaPropertyTypeObject}, false},
		{"explicitly closed", &llm.SchemaProperty{Type: llm.SchemaPropertyTypeObject, AdditionalProperties: &closed}, true},
	}
	for _, tc := range cases {
		_, got := convertLLMSchemaPropertyToOpenAISchemaProperty(tc.prop)["additionalProperties"]
		if got != tc.want {
			t.Errorf("%s: additionalProperties sent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
