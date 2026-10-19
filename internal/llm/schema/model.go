package schema

import (
	"encoding/json"
	"fmt"
)

// JSONSchemaPrimitiveType is the JSON Schema primitive type subset supported by
// OpenAI tool schemas and by the sanitizer we use for broad MCP schemas.
type JSONSchemaPrimitiveType string

const (
	JSONSchemaString  JSONSchemaPrimitiveType = "string"
	JSONSchemaNumber  JSONSchemaPrimitiveType = "number"
	JSONSchemaBoolean JSONSchemaPrimitiveType = "boolean"
	JSONSchemaInteger JSONSchemaPrimitiveType = "integer"
	JSONSchemaObject  JSONSchemaPrimitiveType = "object"
	JSONSchemaArray   JSONSchemaPrimitiveType = "array"
	JSONSchemaNull    JSONSchemaPrimitiveType = "null"
)

// JSONSchemaType represents JSON Schema's `type`, which can be either one
// primitive type or a union of primitive types in source schemas. Provider
// adapters may serialize unions differently (for example OpenAI strict mode
// uses anyOf rather than a type array).
type JSONSchemaType struct {
	Types []JSONSchemaPrimitiveType
}

// JSONSchemaAdditionalProperties represents the two valid forms of
// additionalProperties: a boolean or a schema for free-form map values.
type JSONSchemaAdditionalProperties struct {
	Bool   *bool
	Schema *JSONSchema
}

// JSONSchema is a typed, intentionally small JSON Schema model for tool input
// schemas. Unknown keywords are preserved in Extras so first-party metadata such
// as default/title/minimum and MCP extension fields are not dropped while we
// gradually migrate away from raw map schemas.
type JSONSchema struct {
	Type                 *JSONSchemaType
	Description          string
	Enum                 []interface{}
	Items                *JSONSchema
	Properties           map[string]*JSONSchema
	Required             []string
	AdditionalProperties *JSONSchemaAdditionalProperties
	AnyOf                []*JSONSchema
	Extras               map[string]interface{}
}

var jsonSchemaCoreKeys = map[string]bool{
	"type":                 true,
	"description":          true,
	"enum":                 true,
	"items":                true,
	"properties":           true,
	"required":             true,
	"additionalProperties": true,
	"anyOf":                true,
}

// ParseToolJSONSchema sanitizes a raw JSON-schema-like value from first-party
// tools or MCP and parses it into a typed schema model. It is deliberately
// permissive: malformed schema fragments are coerced to broad string/object/
// array shapes rather than making an entire LLM request fail.
func ParseToolJSONSchema(raw interface{}) (*JSONSchema, error) {
	value := deepCopySchemaValue(raw)
	sanitized := sanitizeToolJSONSchemaValue(value)
	m, ok := sanitized.(map[string]interface{})
	if !ok {
		m = map[string]interface{}{"type": "string"}
	}
	return parseSanitizedToolJSONSchemaMap(m)
}

// ParseToolJSONSchemaMap is a convenience wrapper for map-based ToolSpec.Schema.
func ParseToolJSONSchemaMap(schema map[string]interface{}) (*JSONSchema, error) {
	if len(schema) == 0 {
		return parseSanitizedToolJSONSchemaMap(DefaultResponsesParametersSchema(nil))
	}
	return ParseToolJSONSchema(schema)
}

func sanitizeToolJSONSchemaValue(value interface{}) interface{} {
	switch v := value.(type) {
	case nil:
		return map[string]interface{}{"type": "string"}
	case bool:
		// Boolean JSON Schema form is valid JSON Schema but not valid as an OpenAI
		// tool parameter schema. Codex coerces this accept-all/deny-all form to a
		// broad string schema; do the same here.
		return map[string]interface{}{"type": "string"}
	case map[string]interface{}:
		return sanitizeToolJSONSchemaMap(v)
	case []interface{}:
		for i, item := range v {
			v[i] = sanitizeToolJSONSchemaValue(item)
		}
		return v
	default:
		return v
	}
}

func sanitizeToolJSONSchemaMap(schema map[string]interface{}) map[string]interface{} {
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for key, prop := range props {
			props[key] = sanitizeToolJSONSchemaValue(prop)
		}
	}
	if items, ok := schema["items"]; ok {
		schema["items"] = sanitizeToolJSONSchemaValue(items)
	}
	if ap, ok := schema["additionalProperties"]; ok {
		if _, isBool := ap.(bool); !isBool {
			schema["additionalProperties"] = sanitizeToolJSONSchemaValue(ap)
		}
	}
	for _, key := range []string{"prefixItems", "anyOf", "oneOf", "allOf"} {
		if value, ok := schema[key]; ok {
			schema[key] = sanitizeToolJSONSchemaValue(value)
		}
	}
	if constValue, ok := schema["const"]; ok {
		delete(schema, "const")
		schema["enum"] = []interface{}{constValue}
	}

	typeNames, hadType := normalizedToolJSONSchemaTypes(schema["type"])
	if !hadType || len(typeNames) == 0 {
		if _, hasAnyOf := schema["anyOf"]; !hasAnyOf {
			typeNames = []JSONSchemaPrimitiveType{inferToolJSONSchemaType(schema)}
		}
	}
	writeToolJSONSchemaTypes(schema, typeNames)
	ensureDefaultChildrenForToolJSONSchemaTypes(schema, typeNames)
	return schema
}

func normalizedToolJSONSchemaTypes(value interface{}) ([]JSONSchemaPrimitiveType, bool) {
	if value == nil {
		return nil, false
	}
	seen := map[JSONSchemaPrimitiveType]bool{}
	var result []JSONSchemaPrimitiveType
	add := func(s string) {
		if t, ok := parseJSONSchemaPrimitiveType(s); ok && !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	switch v := value.(type) {
	case string:
		add(v)
	case []string:
		for _, item := range v {
			add(item)
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	default:
		return nil, true
	}
	return result, true
}

func parseJSONSchemaPrimitiveType(s string) (JSONSchemaPrimitiveType, bool) {
	switch JSONSchemaPrimitiveType(s) {
	case JSONSchemaString, JSONSchemaNumber, JSONSchemaBoolean, JSONSchemaInteger, JSONSchemaObject, JSONSchemaArray, JSONSchemaNull:
		return JSONSchemaPrimitiveType(s), true
	default:
		return "", false
	}
}

func inferToolJSONSchemaType(schema map[string]interface{}) JSONSchemaPrimitiveType {
	if schema["properties"] != nil || schema["required"] != nil || schema["additionalProperties"] != nil {
		return JSONSchemaObject
	}
	if schema["items"] != nil || schema["prefixItems"] != nil {
		return JSONSchemaArray
	}
	if schema["enum"] != nil || schema["format"] != nil {
		return JSONSchemaString
	}
	for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		if schema[key] != nil {
			return JSONSchemaNumber
		}
	}
	return JSONSchemaString
}

func writeToolJSONSchemaTypes(schema map[string]interface{}, types []JSONSchemaPrimitiveType) {
	switch len(types) {
	case 0:
		delete(schema, "type")
	case 1:
		schema["type"] = string(types[0])
	default:
		items := make([]interface{}, 0, len(types))
		for _, t := range types {
			items = append(items, string(t))
		}
		schema["type"] = items
	}
}

func ensureDefaultChildrenForToolJSONSchemaTypes(schema map[string]interface{}, types []JSONSchemaPrimitiveType) {
	if containsJSONSchemaType(types, JSONSchemaObject) && schema["properties"] == nil {
		schema["properties"] = map[string]interface{}{}
	}
	if containsJSONSchemaType(types, JSONSchemaArray) && schema["items"] == nil {
		schema["items"] = map[string]interface{}{"type": "string"}
	}
}

func containsJSONSchemaType(types []JSONSchemaPrimitiveType, want JSONSchemaPrimitiveType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func schemaEnumValues(value interface{}) ([]interface{}, bool) {
	if value == nil {
		return nil, false
	}
	switch v := value.(type) {
	case []interface{}:
		return deepCopySlice(v), true
	case []string:
		items := make([]interface{}, len(v))
		for i, item := range v {
			items[i] = item
		}
		return items, true
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		var items []interface{}
		if err := json.Unmarshal(encoded, &items); err != nil {
			return nil, false
		}
		return items, true
	}
}

func parseSanitizedToolJSONSchemaMap(schema map[string]interface{}) (*JSONSchema, error) {
	result := &JSONSchema{}
	if typeNames, ok := normalizedToolJSONSchemaTypes(schema["type"]); ok && len(typeNames) > 0 {
		result.Type = &JSONSchemaType{Types: typeNames}
	}
	if desc, ok := schema["description"].(string); ok {
		result.Description = desc
	}
	if enumValues, ok := schemaEnumValues(schema["enum"]); ok {
		result.Enum = enumValues
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		parsed, err := parseSanitizedToolJSONSchemaMap(items)
		if err != nil {
			return nil, fmt.Errorf("parse items schema: %w", err)
		}
		result.Items = parsed
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		result.Properties = make(map[string]*JSONSchema, len(props))
		for key, value := range props {
			propMap, ok := value.(map[string]interface{})
			if !ok {
				propMap = map[string]interface{}{"type": "string"}
			}
			parsed, err := parseSanitizedToolJSONSchemaMap(propMap)
			if err != nil {
				return nil, fmt.Errorf("parse property %q schema: %w", key, err)
			}
			result.Properties[key] = parsed
		}
	}
	if required, ok := schema["required"].([]interface{}); ok {
		for _, item := range required {
			if s, ok := item.(string); ok {
				result.Required = append(result.Required, s)
			}
		}
	} else if required, ok := schema["required"].([]string); ok {
		result.Required = append(result.Required, required...)
	}
	if ap, ok := schema["additionalProperties"]; ok {
		parsed, err := parseToolJSONSchemaAdditionalProperties(ap)
		if err != nil {
			return nil, err
		}
		result.AdditionalProperties = parsed
	}
	if anyOf, ok := schema["anyOf"].([]interface{}); ok {
		for i, item := range anyOf {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				itemMap = map[string]interface{}{"type": "string"}
			}
			parsed, err := parseSanitizedToolJSONSchemaMap(itemMap)
			if err != nil {
				return nil, fmt.Errorf("parse anyOf[%d] schema: %w", i, err)
			}
			result.AnyOf = append(result.AnyOf, parsed)
		}
	}

	for key, value := range schema {
		if jsonSchemaCoreKeys[key] {
			continue
		}
		if result.Extras == nil {
			result.Extras = map[string]interface{}{}
		}
		result.Extras[key] = deepCopySchemaValue(value)
	}
	return result, nil
}

func parseToolJSONSchemaAdditionalProperties(value interface{}) (*JSONSchemaAdditionalProperties, error) {
	switch v := value.(type) {
	case bool:
		b := v
		return &JSONSchemaAdditionalProperties{Bool: &b}, nil
	case map[string]interface{}:
		parsed, err := parseSanitizedToolJSONSchemaMap(v)
		if err != nil {
			return nil, fmt.Errorf("parse additionalProperties schema: %w", err)
		}
		return &JSONSchemaAdditionalProperties{Schema: parsed}, nil
	default:
		b := false
		return &JSONSchemaAdditionalProperties{Bool: &b}, nil
	}
}

// ToMap serializes the typed schema back to a map representation accepted by
// the existing provider-specific normalizers.
func (s *JSONSchema) ToMap() map[string]interface{} {
	if s == nil {
		return map[string]interface{}{"type": "string"}
	}
	result := map[string]interface{}{}
	for key, value := range s.Extras {
		result[key] = deepCopySchemaValue(value)
	}
	if s.Type != nil && len(s.Type.Types) > 0 {
		if len(s.Type.Types) == 1 {
			result["type"] = string(s.Type.Types[0])
		} else {
			items := make([]interface{}, 0, len(s.Type.Types))
			for _, t := range s.Type.Types {
				items = append(items, string(t))
			}
			result["type"] = items
		}
	}
	if s.Description != "" {
		result["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		result["enum"] = deepCopySlice(s.Enum)
	}
	if s.Items != nil {
		result["items"] = s.Items.ToMap()
	}
	if s.Properties != nil {
		props := make(map[string]interface{}, len(s.Properties))
		for key, prop := range s.Properties {
			props[key] = prop.ToMap()
		}
		result["properties"] = props
	}
	if s.Required != nil {
		required := make([]string, len(s.Required))
		copy(required, s.Required)
		result["required"] = required
	}
	if s.AdditionalProperties != nil {
		if s.AdditionalProperties.Bool != nil {
			result["additionalProperties"] = *s.AdditionalProperties.Bool
		} else if s.AdditionalProperties.Schema != nil {
			result["additionalProperties"] = s.AdditionalProperties.Schema.ToMap()
		}
	}
	if s.AnyOf != nil {
		items := make([]interface{}, 0, len(s.AnyOf))
		for _, schema := range s.AnyOf {
			items = append(items, schema.ToMap())
		}
		result["anyOf"] = items
	}
	return result
}

// MarshalJSON keeps the typed model easy to inspect in debug output and tests.
func (s *JSONSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.ToMap())
}
