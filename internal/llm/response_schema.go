package llm

import "sort"

type SchemaPropertyType string

const (
	SchemaPropertyTypeString  SchemaPropertyType = "string"
	SchemaPropertyTypeInteger SchemaPropertyType = "integer"
	SchemaPropertyTypeNumber  SchemaPropertyType = "number"
	SchemaPropertyTypeBoolean SchemaPropertyType = "boolean"
	SchemaPropertyTypeObject  SchemaPropertyType = "object"
	SchemaPropertyTypeArray   SchemaPropertyType = "array"
)

type SchemaProperty struct {
	Type        SchemaPropertyType         `json:"type"`
	Description string                     `json:"description,omitempty"`
	Properties  map[string]*SchemaProperty `json:"properties,omitempty"` // For object types
	Items       *SchemaProperty            `json:"items,omitempty"`      // For array types
	Required    []string                   `json:"required,omitempty"`   // Required fields for object types
	Nullable    bool                       `json:"nullable,omitempty"`   // If true, allows null values (type becomes ["type", "null"] in JSON Schema)
	Enum        []string                   `json:"enum,omitempty"`       // Allowed values for string types
	Format      string                     `json:"format,omitempty"`     // e.g. "date-time", "email", "uuid"
	// AdditionalProperties false forbids keys beyond Properties; nil leaves them allowed.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
}

type ResponseSchema struct {
	Type                 SchemaPropertyType         `json:"type"`
	Properties           map[string]*SchemaProperty `json:"properties"`
	Items                *SchemaProperty            `json:"items,omitempty"`
	Required             []string                   `json:"required,omitempty"`
	AdditionalProperties *bool                      `json:"additionalProperties,omitempty"`
}

// Clone deep-copies the schema so adapters can adjust it for one vendor
// without changing what the rest of the chain, and the reply check, see.
func (s *ResponseSchema) Clone() *ResponseSchema {
	if s == nil {
		return nil
	}
	return &ResponseSchema{
		Type:                 s.Type,
		Properties:           cloneProperties(s.Properties),
		Items:                s.Items.Clone(),
		Required:             append([]string(nil), s.Required...),
		AdditionalProperties: cloneBool(s.AdditionalProperties),
	}
}

// Clone deep-copies the property.
func (p *SchemaProperty) Clone() *SchemaProperty {
	if p == nil {
		return nil
	}
	c := *p
	c.Properties = cloneProperties(p.Properties)
	c.Items = p.Items.Clone()
	c.Required = append([]string(nil), p.Required...)
	c.Enum = append([]string(nil), p.Enum...)
	c.AdditionalProperties = cloneBool(p.AdditionalProperties)
	return &c
}

// Property views the top level as a property, for code that walks both alike.
func (s *ResponseSchema) Property() *SchemaProperty {
	if s == nil {
		return nil
	}
	return &SchemaProperty{
		Type:                 s.Type,
		Properties:           s.Properties,
		Items:                s.Items,
		Required:             s.Required,
		AdditionalProperties: s.AdditionalProperties,
	}
}

func cloneProperties(in map[string]*SchemaProperty) map[string]*SchemaProperty {
	if in == nil {
		return nil
	}
	out := make(map[string]*SchemaProperty, len(in))
	for k, v := range in {
		out[k] = v.Clone()
	}
	return out
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

// EnforceAllRequired returns a copy of the schema in which every object-level
// field is required, recursively (strict structured-output modes demand it).
// The input is left untouched: it is shared by every model in the chain.
func EnforceAllRequired(schema *ResponseSchema) *ResponseSchema {
	if schema == nil {
		return nil
	}
	out := schema.Clone()
	for _, child := range out.Properties {
		enforceAllRequiredOnProperty(child)
	}
	if out.Items != nil {
		enforceAllRequiredOnProperty(out.Items)
	}
	if out.Type == SchemaPropertyTypeObject && len(out.Properties) > 0 {
		out.Required = allKeys(out.Properties)
	}
	return out
}

func enforceAllRequiredOnProperty(p *SchemaProperty) {
	if p == nil {
		return
	}

	if p.Type == SchemaPropertyTypeObject && len(p.Properties) > 0 {
		p.Required = allKeys(p.Properties)
		for _, child := range p.Properties {
			enforceAllRequiredOnProperty(child)
		}
	}

	if p.Type == SchemaPropertyTypeArray && p.Items != nil {
		enforceAllRequiredOnProperty(p.Items)
	}
}

func allKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Deterministic order helps with tests/logging and avoids needless diffs.
	sort.Strings(keys)
	return keys
}
