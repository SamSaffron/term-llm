package llm

import (
	"reflect"
	"sync"
)

// ToOpenResponsesParameters serializes the typed schema into the provider-neutral
// Open Responses function-tool `parameters` shape. It intentionally avoids
// OpenAI-specific strict-schema rewrites such as forcing every property into
// `required` or converting free-form maps to arrays.
func (s *JSONSchema) ToOpenResponsesParameters() map[string]interface{} {
	if s == nil {
		return defaultResponsesParametersSchema(nil)
	}
	params := s.ToMap()
	if len(params) == 0 {
		return defaultResponsesParametersSchema(nil)
	}
	return params
}

// ToOpenAIParameters serializes the typed schema into the OpenAI Responses
// function-tool `parameters` shape. In strict mode this applies OpenAI's
// stricter structured-output subset on top of the provider-neutral sanitized
// schema.
func (s *JSONSchema) ToOpenAIParameters(strict bool) map[string]interface{} {
	params := s.ToOpenResponsesParameters()
	scrubUnsupportedOpenAISchemaKeywords(params)
	if strict {
		return normalizeSchemaForOpenAIStrict(params)
	}
	return params
}

var unsupportedOpenAISchemaKeywords = map[string]bool{
	"$schema":               true,
	"$id":                   true,
	"$defs":                 true,
	"definitions":           true,
	"$ref":                  true,
	"propertyNames":         true,
	"patternProperties":     true,
	"unevaluatedProperties": true,
	"dependentSchemas":      true,
	"dependentRequired":     true,
	"contains":              true,
	"minContains":           true,
	"maxContains":           true,
	"if":                    true,
	"then":                  true,
	"else":                  true,
	"not":                   true,
	"prefixItems":           true,
	"examples":              true,
}

func scrubUnsupportedOpenAISchemaKeywords(value interface{}) {
	switch v := value.(type) {
	case map[string]interface{}:
		for key := range unsupportedOpenAISchemaKeywords {
			delete(v, key)
		}
		for _, child := range v {
			scrubUnsupportedOpenAISchemaKeywords(child)
		}
	case []interface{}:
		for _, item := range v {
			scrubUnsupportedOpenAISchemaKeywords(item)
		}
	}
}

type openAIParametersCacheKey struct {
	schemaPtr uintptr
	strict    bool
}

type openAIParametersCacheEntry struct {
	// Hold a strong reference to the immutable schema map so its identity cannot
	// be reused for a different schema while the cached lowered parameters remain
	// live.
	schema map[string]interface{}
	once   sync.Once
	params map[string]interface{}
}

var openAIParametersCache sync.Map

func openAIParametersFromToolSchema(schema map[string]interface{}, strict bool) map[string]interface{} {
	key := openAIParametersCacheKey{strict: strict}
	if len(schema) > 0 {
		key.schemaPtr = reflect.ValueOf(schema).Pointer()
	}

	cached, _ := openAIParametersCache.LoadOrStore(key, &openAIParametersCacheEntry{schema: schema})
	entry := cached.(*openAIParametersCacheEntry)
	entry.once.Do(func() {
		parsed, err := ParseToolJSONSchemaMap(schema)
		if err != nil {
			parsed, _ = ParseToolJSONSchemaMap(nil)
		}
		entry.params = parsed.ToOpenAIParameters(strict)
	})
	return entry.params
}
