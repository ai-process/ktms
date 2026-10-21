package llm

import (
	"errors"
	"strings"
	"testing"
)

func constrainedSchema() *ResponseSchema {
	closed := false
	return &ResponseSchema{
		Type:                 SchemaPropertyTypeObject,
		AdditionalProperties: &closed,
		Properties: map[string]*SchemaProperty{
			"status": {Type: SchemaPropertyTypeString, Enum: []string{"open", "closed"}},
			"score":  {Type: SchemaPropertyTypeNumber},
		},
		Required: []string{"status", "score"},
	}
}

func TestCheckResponseSchemaConstraints(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		issue string // "" = valid
	}{
		{"valid", `{"status":"open","score":0.75}`, ""},
		{"integer is a number", `{"status":"closed","score":3}`, ""},
		{"value outside enum", `{"status":"pending","score":1}`, `$.status should be one of`},
		{"number as string", `{"status":"open","score":"high"}`, `$.score should be a number`},
		{"forbidden extra key", `{"status":"open","score":1,"note":"x"}`, `$.note is not allowed`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckResponseSchema(tc.reply, constrainedSchema())
			if tc.issue == "" {
				if err != nil {
					t.Fatalf("valid reply rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrSchemaViolation) || !strings.Contains(err.Error(), tc.issue) {
				t.Fatalf("err = %v, want a violation mentioning %q", err, tc.issue)
			}
		})
	}
}

func TestExtraKeysAllowedUnlessForbidden(t *testing.T) {
	s := constrainedSchema()
	s.AdditionalProperties = nil
	if err := CheckResponseSchema(`{"status":"open","score":1,"note":"x"}`, s); err != nil {
		t.Fatalf("extra key rejected without additionalProperties=false: %v", err)
	}
}

func TestCheckToolCalls(t *testing.T) {
	tools := []*Tool{{Type: "function", Function: FunctionDefinition{Name: "set_status", Parameters: constrainedSchema()}}}
	good := []*ToolCall{{Function: FunctionCall{Name: "set_status", Arguments: `{"status":"open","score":1}`}}}
	if err := CheckToolCalls(good, tools); err != nil {
		t.Fatalf("valid call rejected: %v", err)
	}
	bad := []*ToolCall{{Function: FunctionCall{Name: "set_status", Arguments: `{"status":"maybe","score":1}`}}}
	if err := CheckToolCalls(bad, tools); !errors.Is(err, ErrSchemaViolation) || !strings.Contains(err.Error(), "set_status") {
		t.Fatalf("err = %v, want a violation naming the tool", err)
	}
	unknown := []*ToolCall{{Function: FunctionCall{Name: "other", Arguments: `not json`}}}
	if err := CheckToolCalls(unknown, tools); err != nil {
		t.Fatalf("calls to tools without parameters must pass: %v", err)
	}
}

// The request schema is shared by every model in the chain; strict-mode
// adjustments for one vendor must not leak into the others or the check.
func TestEnforceAllRequiredLeavesInputUntouched(t *testing.T) {
	s := &ResponseSchema{
		Type: SchemaPropertyTypeObject,
		Properties: map[string]*SchemaProperty{
			"a": {Type: SchemaPropertyTypeString},
			"b": {Type: SchemaPropertyTypeObject, Properties: map[string]*SchemaProperty{"c": {Type: SchemaPropertyTypeString}}},
		},
		Required: []string{"a"},
	}
	out := EnforceAllRequired(s)
	if strings.Join(out.Required, ",") != "a,b" || strings.Join(out.Properties["b"].Required, ",") != "c" {
		t.Fatalf("enforced copy: top %v nested %v", out.Required, out.Properties["b"].Required)
	}
	if strings.Join(s.Required, ",") != "a" || len(s.Properties["b"].Required) != 0 {
		t.Fatalf("input mutated: top %v nested %v", s.Required, s.Properties["b"].Required)
	}
}

func TestCloneIsDeep(t *testing.T) {
	s := constrainedSchema()
	c := s.Clone()
	c.Properties["status"].Enum[0] = "changed"
	*c.AdditionalProperties = true
	if s.Properties["status"].Enum[0] != "open" || *s.AdditionalProperties {
		t.Fatal("clone shares state with the original")
	}
}
