package llm

import "github.com/samsaffron/term-llm/internal/llm/schema"

// Compatibility exports for the cycle-free schema package.

type (
	JSONSchemaPrimitiveType        = schema.JSONSchemaPrimitiveType
	JSONSchemaType                 = schema.JSONSchemaType
	JSONSchemaAdditionalProperties = schema.JSONSchemaAdditionalProperties
	JSONSchema                     = schema.JSONSchema
)

const (
	JSONSchemaString  = schema.JSONSchemaString
	JSONSchemaNumber  = schema.JSONSchemaNumber
	JSONSchemaBoolean = schema.JSONSchemaBoolean
	JSONSchemaInteger = schema.JSONSchemaInteger
	JSONSchemaObject  = schema.JSONSchemaObject
	JSONSchemaArray   = schema.JSONSchemaArray
	JSONSchemaNull    = schema.JSONSchemaNull
)

// ParseToolJSONSchema sanitizes and parses a raw tool schema value.
func ParseToolJSONSchema(raw interface{}) (*JSONSchema, error) {
	return schema.ParseToolJSONSchema(raw)
}

// ParseToolJSONSchemaMap parses a map-based ToolSpec.Schema.
func ParseToolJSONSchemaMap(s map[string]interface{}) (*JSONSchema, error) {
	return schema.ParseToolJSONSchemaMap(s)
}

func normalizeSchemaForOpenAI(s map[string]interface{}) map[string]interface{} {
	return schema.NormalizeSchemaForOpenAI(s)
}

func normalizeFreeFormMapProperties(s map[string]interface{}) map[string]interface{} {
	return schema.NormalizeFreeFormMapProperties(s)
}

func openAIParametersFromToolSchema(s map[string]interface{}, strict bool) map[string]interface{} {
	return schema.OpenAIParametersFromToolSchema(s, strict)
}

func deepCopyMap(m map[string]interface{}) map[string]interface{} { return schema.DeepCopyMap(m) }
