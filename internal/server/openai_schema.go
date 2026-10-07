package server

import (
	"encoding/json"
	"fmt"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

const maxSchemaDepth = 32

// jsonSchema is the subset of JSON Schema the proxy can carry; keywords it has
// no field for (enum, format, additionalProperties, ...) are dropped.
type jsonSchema struct {
	Type        json.RawMessage        `json:"type"`
	Description string                 `json:"description"`
	Properties  map[string]*jsonSchema `json:"properties"`
	Items       *jsonSchema            `json:"items"`
	Required    []string               `json:"required"`
	Nullable    bool                   `json:"nullable"`
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
	out := &pb.SchemaProperty{Type: typ, Description: s.Description, Required: s.Required, Nullable: nullable}
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
		Type:       root.Type,
		Properties: root.Properties,
		Items:      root.Items,
		Required:   root.Required,
	}, nil
}
