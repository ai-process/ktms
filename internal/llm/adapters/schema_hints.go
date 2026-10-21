package adapters

import (
	"strings"

	"github.com/ai-process/ktms/internal/llm"
)

// schemaHints splits a property into what the vendor can enforce and what it
// would reject. The rest moves into the description so the model still sees
// it: formats the vendor does not know, and enums on non-string types, which
// strict modes and Gemini only accept as strings.
func schemaHints(p *llm.SchemaProperty, formatOK func(llm.SchemaPropertyType, string) bool) (description, format string, enum []string) {
	description = p.Description
	if p.Format != "" {
		if formatOK(p.Type, p.Format) {
			format = p.Format
		} else {
			description = joinHint(description, "format: "+p.Format)
		}
	}
	if len(p.Enum) > 0 {
		if p.Type == llm.SchemaPropertyTypeString {
			enum = p.Enum
		} else {
			description = joinHint(description, "allowed values: "+strings.Join(p.Enum, ", "))
		}
	}
	return description, format, enum
}

func joinHint(description, hint string) string {
	if description == "" {
		return hint
	}
	return description + " (" + hint + ")"
}
