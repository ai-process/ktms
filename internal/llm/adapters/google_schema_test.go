package adapters

import (
	"github.com/ai-process/ktms/internal/llm"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/genai"
)

func TestConvertLLMSchemaPropertyTypeToGenaiType(t *testing.T) {
	tests := []struct {
		name     string
		llmType  llm.SchemaPropertyType
		expected genai.Type
	}{
		{
			name:     "String type",
			llmType:  llm.SchemaPropertyTypeString,
			expected: genai.TypeString,
		},
		{
			name:     "Integer type",
			llmType:  llm.SchemaPropertyTypeInteger,
			expected: genai.TypeInteger,
		},
		{
			name:     "Boolean type",
			llmType:  llm.SchemaPropertyTypeBoolean,
			expected: genai.TypeBoolean,
		},
		{
			name:     "Object type",
			llmType:  llm.SchemaPropertyTypeObject,
			expected: genai.TypeObject,
		},
		{
			name:     "Array type",
			llmType:  llm.SchemaPropertyTypeArray,
			expected: genai.TypeArray,
		},
		{
			name:     "Unknown type",
			llmType:  llm.SchemaPropertyType("unknown"),
			expected: genai.TypeUnspecified,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertLLMSchemaPropertyTypeToGenaiType(tt.llmType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestConvertPropertySchemaToGenai(t *testing.T) {
	t.Run("nil property", func(t *testing.T) {
		result := convertPropertySchemaToGenai(nil)
		assert.Nil(t, result)
	})

	t.Run("simple string property", func(t *testing.T) {
		llmProp := &llm.SchemaProperty{
			Type:        llm.SchemaPropertyTypeString,
			Description: "A simple string",
		}

		result := convertPropertySchemaToGenai(llmProp)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeString, result.Type)
		assert.Equal(t, "A simple string", result.Description)
		assert.Nil(t, result.Properties)
		assert.Nil(t, result.Items)
	})

	t.Run("object property with nested properties", func(t *testing.T) {
		llmProp := &llm.SchemaProperty{
			Type:        llm.SchemaPropertyTypeObject,
			Description: "An object",
			Properties: map[string]*llm.SchemaProperty{
				"name": {
					Type:        llm.SchemaPropertyTypeString,
					Description: "Name field",
				},
				"age": {
					Type:        llm.SchemaPropertyTypeInteger,
					Description: "Age field",
				},
			},
		}

		result := convertPropertySchemaToGenai(llmProp)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeObject, result.Type)
		assert.Equal(t, "An object", result.Description)
		assert.NotNil(t, result.Properties)
		assert.Len(t, result.Properties, 2)

		nameProp, ok := result.Properties["name"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, nameProp.Type)
		assert.Equal(t, "Name field", nameProp.Description)

		ageProp, ok := result.Properties["age"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeInteger, ageProp.Type)
		assert.Equal(t, "Age field", ageProp.Description)
	})

	t.Run("array property with items", func(t *testing.T) {
		llmProp := &llm.SchemaProperty{
			Type:        llm.SchemaPropertyTypeArray,
			Description: "An array of strings",
			Items: &llm.SchemaProperty{
				Type:        llm.SchemaPropertyTypeString,
				Description: "String item",
			},
		}

		result := convertPropertySchemaToGenai(llmProp)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeArray, result.Type)
		assert.Equal(t, "An array of strings", result.Description)
		assert.NotNil(t, result.Items)
		assert.Equal(t, genai.TypeString, result.Items.Type)
		assert.Equal(t, "String item", result.Items.Description)
	})

	t.Run("complex nested structure", func(t *testing.T) {
		llmProp := &llm.SchemaProperty{
			Type:        llm.SchemaPropertyTypeObject,
			Description: "Complex object",
			Properties: map[string]*llm.SchemaProperty{
				"items": {
					Type:        llm.SchemaPropertyTypeArray,
					Description: "Array of items",
					Items: &llm.SchemaProperty{
						Type:        llm.SchemaPropertyTypeObject,
						Description: "Item object",
						Properties: map[string]*llm.SchemaProperty{
							"id": {
								Type:        llm.SchemaPropertyTypeString,
								Description: "Item ID",
							},
							"active": {
								Type:        llm.SchemaPropertyTypeBoolean,
								Description: "Is active",
							},
						},
					},
				},
			},
		}

		result := convertPropertySchemaToGenai(llmProp)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeObject, result.Type)

		itemsProp, ok := result.Properties["items"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeArray, itemsProp.Type)
		assert.NotNil(t, itemsProp.Items)

		itemObj := itemsProp.Items
		assert.Equal(t, genai.TypeObject, itemObj.Type)
		assert.NotNil(t, itemObj.Properties)
		assert.Len(t, itemObj.Properties, 2)

		idProp, ok := itemObj.Properties["id"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, idProp.Type)

		activeProp, ok := itemObj.Properties["active"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeBoolean, activeProp.Type)
	})
}

func TestConvertSchemaToGenai(t *testing.T) {
	t.Run("nil schema", func(t *testing.T) {
		result := convertSchemaToGenai(nil)
		assert.Nil(t, result)
	})

	t.Run("simple object schema", func(t *testing.T) {
		llmSchema := &llm.ResponseSchema{
			Type: llm.SchemaPropertyTypeObject,
			Properties: map[string]*llm.SchemaProperty{
				"message": {
					Type:        llm.SchemaPropertyTypeString,
					Description: "A message",
				},
			},
			Required: []string{"message"},
		}

		result := convertSchemaToGenai(llmSchema)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeObject, result.Type)
		assert.Equal(t, []string{"message"}, result.Required)
		assert.NotNil(t, result.Properties)
		assert.Len(t, result.Properties, 1)

		messageProp, ok := result.Properties["message"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, messageProp.Type)
		assert.Equal(t, "A message", messageProp.Description)
	})

	t.Run("complex schema with multiple properties", func(t *testing.T) {
		llmSchema := &llm.ResponseSchema{
			Type: llm.SchemaPropertyTypeObject,
			Properties: map[string]*llm.SchemaProperty{
				"botMessage": {
					Type:        llm.SchemaPropertyTypeString,
					Description: "Bot message",
				},
				"suggestions": {
					Type:        llm.SchemaPropertyTypeArray,
					Description: "Array of suggestions",
					Items: &llm.SchemaProperty{
						Type: llm.SchemaPropertyTypeObject,
						Properties: map[string]*llm.SchemaProperty{
							"i": {
								Type:        llm.SchemaPropertyTypeString,
								Description: "Lesson ID",
							},
							"n": {
								Type:        llm.SchemaPropertyTypeString,
								Description: "Lesson name",
							},
							"c": {
								Type:        llm.SchemaPropertyTypeString,
								Description: "Course name",
							},
							"e": {
								Type:        llm.SchemaPropertyTypeString,
								Description: "Explanation",
							},
						},
						Required: []string{"i", "n", "c", "e"},
					},
				},
			},
			Required: []string{"suggestions"},
		}

		result := convertSchemaToGenai(llmSchema)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeObject, result.Type)
		assert.Equal(t, []string{"suggestions"}, result.Required)
		assert.NotNil(t, result.Properties)
		assert.Len(t, result.Properties, 2)

		// Check botMessage property
		botMsgProp, ok := result.Properties["botMessage"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, botMsgProp.Type)

		// Check suggestions property
		suggestionsProp, ok := result.Properties["suggestions"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeArray, suggestionsProp.Type)
		assert.NotNil(t, suggestionsProp.Items)

		// Check items structure
		itemSchema := suggestionsProp.Items
		assert.Equal(t, genai.TypeObject, itemSchema.Type)
		assert.NotNil(t, itemSchema.Properties)
		assert.Len(t, itemSchema.Properties, 4)
		// Verify required fields are set
		assert.Equal(t, []string{"i", "n", "c", "e"}, itemSchema.Required)

		// Verify all item properties
		iProp, ok := itemSchema.Properties["i"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, iProp.Type)

		nProp, ok := itemSchema.Properties["n"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, nProp.Type)

		cProp, ok := itemSchema.Properties["c"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, cProp.Type)

		eProp, ok := itemSchema.Properties["e"]
		assert.True(t, ok)
		assert.Equal(t, genai.TypeString, eProp.Type)
	})

	t.Run("schema with empty properties", func(t *testing.T) {
		llmSchema := &llm.ResponseSchema{
			Type:       llm.SchemaPropertyTypeObject,
			Properties: map[string]*llm.SchemaProperty{},
			Required:   []string{},
		}

		result := convertSchemaToGenai(llmSchema)
		assert.NotNil(t, result)
		assert.Equal(t, genai.TypeObject, result.Type)
		assert.Empty(t, result.Required)
		// Properties should be nil or empty map
		if result.Properties != nil {
			assert.Empty(t, result.Properties)
		}
	})

	t.Run("schema without required fields", func(t *testing.T) {
		llmSchema := &llm.ResponseSchema{
			Type: llm.SchemaPropertyTypeObject,
			Properties: map[string]*llm.SchemaProperty{
				"optional": {
					Type: llm.SchemaPropertyTypeString,
				},
			},
			Required: nil,
		}

		result := convertSchemaToGenai(llmSchema)
		assert.NotNil(t, result)
		assert.Nil(t, result.Required)
	})
}
