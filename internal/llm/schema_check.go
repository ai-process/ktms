package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrSchemaViolation marks a reply that parsed as JSON but does not match the
// requested schema. Separate from a parse failure so callers can tell "not
// JSON at all" from "JSON of the wrong shape" in logs.
var ErrSchemaViolation = errors.New("response does not match the requested schema")

// maxSchemaIssues bounds the reported mismatches: the message goes into a log
// line and a retry instruction, and the first few are enough to act on.
const maxSchemaIssues = 5

// CheckResponseSchema validates a model's raw reply against the schema the
// caller asked for. It exists for vendors whose API cannot enforce a schema
// (DeepSeek among them): the proxy asks for JSON in the prompt, then verifies
// the shape here rather than handing an unchecked document to the caller.
//
// Checked: types, required keys, array element shape, enum values and, where
// the schema sets additionalProperties false, unknown keys. Descriptions and
// format are hints for the vendor and are not enforced here.
func CheckResponseSchema(raw string, schema *ResponseSchema) error {
	if schema == nil {
		return nil
	}
	return checkJSON(RemoveMarkdownCodeBlocks(raw), schema.Property())
}

// CheckToolCalls validates each call's arguments against the parameters of the
// tool it names, so a malformed call is caught like a malformed reply.
func CheckToolCalls(calls []*ToolCall, tools []*Tool) error {
	if len(calls) == 0 || len(tools) == 0 {
		return nil
	}
	params := make(map[string]*ResponseSchema, len(tools))
	for _, t := range tools {
		if t != nil && t.Function.Parameters != nil {
			params[t.Function.Name] = t.Function.Parameters
		}
	}
	for _, c := range calls {
		if c == nil {
			continue
		}
		schema, ok := params[c.Function.Name]
		if !ok {
			continue
		}
		args := c.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if err := checkJSON(args, schema.Property()); err != nil {
			return fmt.Errorf("tool call %s: %w", c.Function.Name, err)
		}
	}
	return nil
}

func checkJSON(raw string, want *SchemaProperty) error {
	text := strings.TrimSpace(raw)
	if text == "" {
		return fmt.Errorf("%w: empty response", ErrSchemaViolation)
	}
	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return fmt.Errorf("%w: not valid JSON: %v", ErrSchemaViolation, err)
	}
	issues := checkValue(doc, want, "$", nil)
	if len(issues) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrSchemaViolation, strings.Join(issues, "; "))
}

// checkValue walks the document and the schema together, collecting readable
// paths for whatever does not line up.
func checkValue(value any, want *SchemaProperty, path string, issues []string) []string {
	if want == nil || len(issues) >= maxSchemaIssues {
		return issues
	}
	if value == nil {
		if !want.Nullable {
			issues = append(issues, path+" is null")
		}
		return issues
	}

	switch want.Type {
	case SchemaPropertyTypeObject:
		obj, ok := value.(map[string]any)
		if !ok {
			return append(issues, fmt.Sprintf("%s should be an object, got %s", path, jsonKind(value)))
		}
		for _, key := range want.Required {
			if _, present := obj[key]; !present {
				issues = append(issues, fmt.Sprintf("%s.%s is missing", path, key))
				if len(issues) >= maxSchemaIssues {
					return issues
				}
			}
		}
		for key, child := range want.Properties {
			if v, present := obj[key]; present {
				issues = checkValue(v, child, path+"."+key, issues)
				if len(issues) >= maxSchemaIssues {
					return issues
				}
			}
		}
		if want.AdditionalProperties != nil && !*want.AdditionalProperties {
			for _, key := range sortedKeys(obj) {
				if _, known := want.Properties[key]; !known {
					issues = append(issues, fmt.Sprintf("%s.%s is not allowed", path, key))
					if len(issues) >= maxSchemaIssues {
						return issues
					}
				}
			}
		}
	case SchemaPropertyTypeArray:
		arr, ok := value.([]any)
		if !ok {
			return append(issues, fmt.Sprintf("%s should be an array, got %s", path, jsonKind(value)))
		}
		for i, item := range arr {
			issues = checkValue(item, want.Items, fmt.Sprintf("%s[%d]", path, i), issues)
			if len(issues) >= maxSchemaIssues {
				return issues
			}
		}
	case SchemaPropertyTypeString:
		str, ok := value.(string)
		if !ok {
			issues = append(issues, fmt.Sprintf("%s should be a string, got %s", path, jsonKind(value)))
		} else if len(want.Enum) > 0 && !slices.Contains(want.Enum, str) {
			issues = append(issues, fmt.Sprintf("%s should be one of %q, got %q", path, want.Enum, str))
		}
	case SchemaPropertyTypeNumber:
		if _, ok := value.(float64); !ok {
			issues = append(issues, fmt.Sprintf("%s should be a number, got %s", path, jsonKind(value)))
		}
	case SchemaPropertyTypeInteger:
		// Every JSON number decodes to float64; integers are whole numbers.
		n, ok := value.(float64)
		if !ok {
			issues = append(issues, fmt.Sprintf("%s should be an integer, got %s", path, jsonKind(value)))
		} else if n != float64(int64(n)) {
			issues = append(issues, fmt.Sprintf("%s should be an integer, got %v", path, n))
		}
	case SchemaPropertyTypeBoolean:
		if _, ok := value.(bool); !ok {
			issues = append(issues, fmt.Sprintf("%s should be a boolean, got %s", path, jsonKind(value)))
		}
	}
	return issues
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func jsonKind(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	default:
		return "an unknown value"
	}
}

// SchemaPromptInstruction describes the wanted shape in words, for models that
// cannot be handed a schema through the API. Appended to the system
// instruction so the reply is JSON in the first place.
func SchemaPromptInstruction(schema *ResponseSchema) string {
	if schema == nil {
		return ""
	}
	shape, err := json.Marshal(schema)
	if err != nil {
		return "Reply with a single JSON document and nothing else."
	}
	return "Reply with a single JSON document and nothing else: no prose, no markdown fences. " +
		"It must match this JSON Schema:\n" + string(shape)
}
