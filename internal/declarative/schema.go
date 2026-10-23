package declarative

import (
	"encoding/json"

	"google.golang.org/protobuf/reflect/protoreflect"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// SchemaURL is where the published schema lives, for editors' $schema hints.
const SchemaURL = "https://raw.githubusercontent.com/ai-process/ktms/main/schema/ktms-config.schema.json"

// Schema returns the JSON Schema of the file format. The Admin API messages
// are described from their proto descriptors, so the schema follows ktms.v1.
func Schema() []byte {
	obj := func(props map[string]any, required ...string) map[string]any {
		o := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
		if len(required) > 0 {
			o["required"] = required
		}
		return o
	}
	str := map[string]any{"type": "string"}
	strs := map[string]any{"type": "array", "items": str}
	list := func(item any) map[string]any { return map[string]any{"type": "array", "items": item} }

	s := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  SchemaURL,
		"title":                "Keystone (KTMS) configuration",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"api_version"},
		"properties": map[string]any{
			"api_version":      map[string]any{"const": APIVersion},
			"models":           list(messageSchema((&pb.ModelSpec{}).ProtoReflect().Descriptor(), "id", "vendor")),
			"rules":            list(messageSchema((&pb.RuleSpec{}).ProtoReflect().Descriptor(), "name", "priority", "use")),
			"user_rate_limits": list(messageSchema((&pb.UserRateLimitSpec{}).ProtoReflect().Descriptor(), "user_id", "model_id")),
			"organizations": list(obj(map[string]any{
				"name": str,
				"teams": list(obj(map[string]any{
					"name":               str,
					"monthly_budget_usd": map[string]any{"type": "number", "minimum": 0},
					"allowed_models":     strs,
				}, "name")),
			}, "name")),
			"api_keys": list(obj(map[string]any{
				"name":         str,
				"scopes":       map[string]any{"type": "array", "items": map[string]any{"enum": []string{"generate", "admin", "admin:read"}}},
				"organization": str,
				"team":         str,
				"policy":       messageSchema((&pb.APIKeyPolicy{}).ProtoReflect().Descriptor()),
			}, "name")),
			"vendor_keys": list(obj(map[string]any{
				"api_key_name":  str,
				"vendor":        str,
				"plain_key_env": map[string]any{"type": "string", "description": "Environment variable holding the vendor key; the key itself never goes in the file."},
			}, "api_key_name", "vendor", "plain_key_env")),
			"spend_limits": list(obj(map[string]any{
				"subject_type":  map[string]any{"enum": []string{"key", "user"}},
				"subject_id":    map[string]any{"type": "string", "description": "Key name or user id, * for every subject of that type."},
				"period":        map[string]any{"enum": []string{"day", "month"}},
				"limit_usd":     map[string]any{"type": "number", "exclusiveMinimum": 0},
				"limit_nanousd": map[string]any{"type": "integer", "exclusiveMinimum": 0},
			}, "subject_type", "subject_id", "period")),
		},
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

// messageSchema describes a message as protojson reads it: proto field
// names, enums by name, 64-bit integers as numbers or strings.
func messageSchema(md protoreflect.MessageDescriptor, required ...string) map[string]any {
	props := map[string]any{}
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		var t any
		switch {
		case fd.IsMap():
			t = map[string]any{"type": "object", "additionalProperties": kindSchema(fd.MapValue())}
		case fd.IsList():
			t = map[string]any{"type": "array", "items": kindSchema(fd)}
		default:
			t = kindSchema(fd)
		}
		props[string(fd.Name())] = t
	}
	o := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func kindSchema(fd protoreflect.FieldDescriptor) map[string]any {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind, protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind:
		return map[string]any{"type": "integer"}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind, protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind:
		return map[string]any{"type": []string{"integer", "string"}}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		names := make([]string, vals.Len())
		for i := range names {
			names[i] = string(vals.Get(i).Name())
		}
		return map[string]any{"enum": names}
	case protoreflect.MessageKind:
		return messageSchema(fd.Message())
	}
	return map[string]any{}
}
