package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseSchemaFromJSONKeepsConstraints(t *testing.T) {
	s, err := responseSchemaFromJSON(json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"status": {"type": "string", "enum": ["open", "closed", null]},
			"score":  {"type": "number"},
			"when":   {"type": "string", "format": "date-time"},
			"meta":   {"type": "object", "additionalProperties": {"type": "string"}}
		},
		"required": ["status"]
	}`))
	if err != nil {
		t.Fatalf("responseSchemaFromJSON: %v", err)
	}
	if s.AdditionalProperties == nil || *s.AdditionalProperties {
		t.Fatalf("top-level additionalProperties = %v, want false", s.AdditionalProperties)
	}
	status := s.Properties["status"]
	if strings.Join(status.Enum, ",") != "open,closed" || !status.Nullable {
		t.Fatalf("status = %+v, want string enum plus nullable", status)
	}
	if s.Properties["score"].Type != "number" || s.Properties["when"].Format != "date-time" {
		t.Fatalf("score/when lost: %+v %+v", s.Properties["score"], s.Properties["when"])
	}
	if s.Properties["meta"].AdditionalProperties != nil {
		t.Fatal("a schema-valued additionalProperties allows extra keys and must not become false")
	}
}

func TestResponseSchemaFromJSONCarriesNonStringEnum(t *testing.T) {
	s, err := responseSchemaFromJSON(json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer","enum":[1,2]}}}`))
	if err != nil {
		t.Fatalf("a numeric enum must not fail the request: %v", err)
	}
	if got := strings.Join(s.Properties["n"].Enum, ","); got != "1,2" {
		t.Fatalf("enum = %q, want the literals as text", got)
	}
}

func TestAdditionalPropertiesNullAllowsExtraKeys(t *testing.T) {
	s, err := responseSchemaFromJSON(json.RawMessage(`{"type":"object","additionalProperties":null,"properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.AdditionalProperties != nil {
		t.Fatalf("null must not become false: %v", *s.AdditionalProperties)
	}
}
