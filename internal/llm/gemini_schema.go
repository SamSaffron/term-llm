package llm

// normalizeSchemaForGemini ensures schema meets Gemini's requirements by removing
// unsupported format values and ensuring required fields are complete.
func normalizeSchemaForGemini(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return schema
	}
	return normalizeGeminiSchemaRecursive(geminiDeepCopyMap(schema))
}

func geminiDeepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = geminiDeepCopyMap(val)
		case []interface{}:
			result[k] = geminiDeepCopySlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func geminiDeepCopySlice(s []interface{}) []interface{} {
	if s == nil {
		return nil
	}
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]interface{}:
			result[i] = geminiDeepCopyMap(val)
		case []interface{}:
			result[i] = geminiDeepCopySlice(val)
		default:
			result[i] = v
		}
	}
	return result
}

// normalizeGeminiSchemaRecursive applies Gemini normalization recursively
func normalizeGeminiSchemaRecursive(schema map[string]interface{}) map[string]interface{} {
	// Remove fields Gemini doesn't support
	unsupportedFields := []string{
		"$schema",
		"format",
		"exclusiveMinimum",
		"exclusiveMaximum",
		"minimum",
		"maximum",
		"minLength",
		"maxLength",
		"minItems",
		"maxItems",
		"uniqueItems",
		"pattern",
		"default",
		"examples",
		"const",
		"additionalProperties",
		"title",
	}
	for _, field := range unsupportedFields {
		delete(schema, field)
	}

	// Handle properties
	if props, ok := schema["properties"].(map[string]interface{}); ok && len(props) > 0 {
		// Recursively normalize each property
		for key, val := range props {
			if propSchema, ok := val.(map[string]interface{}); ok {
				props[key] = normalizeGeminiSchemaRecursive(propSchema)
			}
		}

		// Ensure required includes all property keys for Gemini
		required := make([]string, 0, len(props))
		for key := range props {
			required = append(required, key)
		}
		schema["required"] = required
	}

	// Handle array items
	if items, ok := schema["items"].(map[string]interface{}); ok {
		schema["items"] = normalizeGeminiSchemaRecursive(items)
	}

	// Handle anyOf, oneOf, allOf
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for i, item := range arr {
				if itemSchema, ok := item.(map[string]interface{}); ok {
					arr[i] = normalizeGeminiSchemaRecursive(itemSchema)
				}
			}
		}
	}

	return schema
}

func schemaToGemini(schema map[string]interface{}) *geminiSchema {
	if schema == nil {
		return &geminiSchema{Type: geminiSchemaTypeString}
	}

	genSchema := &geminiSchema{
		Type:        schemaTypeFromValue(schema),
		Description: stringField(schema, "description"),
		Required:    requiredFields(schema),
	}

	if props, ok := schema["properties"].(map[string]interface{}); ok {
		genSchema.Properties = make(map[string]*geminiSchema, len(props))
		for name, prop := range props {
			if propMap, ok := prop.(map[string]interface{}); ok {
				genSchema.Properties[name] = schemaToGemini(propMap)
			}
		}
	}

	if items, ok := schema["items"].(map[string]interface{}); ok {
		genSchema.Items = schemaToGemini(items)
	}

	return genSchema
}

func schemaTypeFromValue(schema map[string]interface{}) geminiSchemaType {
	if t, ok := schema["type"].(string); ok {
		switch t {
		case "string":
			return geminiSchemaTypeString
		case "integer":
			return geminiSchemaTypeInteger
		case "number":
			return geminiSchemaTypeNumber
		case "boolean":
			return geminiSchemaTypeBoolean
		case "array":
			return geminiSchemaTypeArray
		case "object":
			return geminiSchemaTypeObject
		}
	}
	return geminiSchemaTypeString
}

func requiredFields(schema map[string]interface{}) []string {
	if required, ok := schema["required"].([]string); ok {
		return required
	}
	if required, ok := schema["required"].([]interface{}); ok {
		result := make([]string, 0, len(required))
		for _, r := range required {
			if s, ok := r.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

func stringField(schema map[string]interface{}, key string) string {
	if v, ok := schema[key].(string); ok {
		return v
	}
	return ""
}
