package server

import (
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

const maxSchemaDepth = 32

// jsonSchema is the subset of JSON Schema the proxy can carry. Validation
// keywords it has no field for (minimum, pattern, oneOf, ...) are dropped.
type jsonSchema struct {
	Type        json.RawMessage        `json:"type"`
	Description string                 `json:"description"`
	Properties  map[string]*jsonSchema `json:"properties"`
	Items       *jsonSchema            `json:"items"`
	Required    []string               `json:"required"`
	Nullable    bool                   `json:"nullable"`
	Enum        []json.RawMessage      `json:"enum"`
	Format      string                 `json:"format"`
	// A bool, or a schema for the extra values (carried as "allowed").
	AdditionalProperties json.RawMessage `json:"additionalProperties"`
}

// enumValues carries strings as they are and other literals (1, true) as their
// JSON text: vendors take enums on non-string types only as a hint, and the
// adapters pass them on that way instead of dropping them.
func (s *jsonSchema) enumValues() (values []string, nullable bool) {
	for _, raw := range s.Enum {
		text := strings.TrimSpace(string(raw))
		if text == "null" {
			nullable = true
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			v = text
		}
		values = append(values, v)
	}
	return values, nullable
}

// additionalProperties is set only for a literal true or false. A schema (the
// shape of extra values) or null allows extra keys, which is the default.
func (s *jsonSchema) additionalProperties() *bool {
	switch strings.TrimSpace(string(s.AdditionalProperties)) {
	case "true":
		v := true
		return &v
	case "false":
		v := false
		return &v
	}
	return nil
}

// schemaType resolves "type", which is either a string or a list such as
// ["string","null"] (how strict-mode schemas spell an optional field).
func (s *jsonSchema) schemaType() (typ string, nullable bool, err error) {
	if len(s.Type) == 0 {
		return "", s.Nullable, nil
	}
	var one string
	if json.Unmarshal(s.Type, &one) == nil {
		return one, s.Nullable || one == "null", nil
	}
	var many []string
	if err := json.Unmarshal(s.Type, &many); err != nil {
		return "", false, fmt.Errorf("schema type must be a string or a list of strings")
	}
	nullable = s.Nullable
	for _, t := range many {
		if t == "null" {
			nullable = true
		} else if typ == "" {
			typ = t
		}
	}
	return typ, nullable, nil
}

func (s *jsonSchema) toProperty(depth int) (*pb.SchemaProperty, error) {
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("schema is nested deeper than %d levels", maxSchemaDepth)
	}
	typ, nullable, err := s.schemaType()
	if err != nil {
		return nil, err
	}
	enum, enumNull := s.enumValues()
	out := &pb.SchemaProperty{
		Type: typ, Description: s.Description, Required: s.Required, Nullable: nullable || enumNull,
		Enum: enum, Format: s.Format, AdditionalProperties: s.additionalProperties(),
	}
	if len(s.Properties) > 0 {
		out.Properties = make(map[string]*pb.SchemaProperty, len(s.Properties))
		for name, p := range s.Properties {
			if p == nil {
				continue
			}
			if out.Properties[name], err = p.toProperty(depth + 1); err != nil {
				return nil, err
			}
		}
	}
	if s.Items != nil {
		if out.Items, err = s.Items.toProperty(depth + 1); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// responseSchemaFromJSON converts a JSON Schema document into the proxy's
// ResponseSchema.
func responseSchemaFromJSON(raw json.RawMessage) (*pb.ResponseSchema, error) {
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid json_schema.schema: %w", err)
	}
	root, err := s.toProperty(0)
	if err != nil {
		return nil, err
	}
	return &pb.ResponseSchema{
		Type:                 root.Type,
		Properties:           root.Properties,
		Items:                root.Items,
		Required:             root.Required,
		AdditionalProperties: root.AdditionalProperties,
	}, nil
}
