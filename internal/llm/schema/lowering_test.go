package schema

import "testing"

func TestJSONSchemaToOpenResponsesParametersDoesNotApplyOpenAIStrictRules(t *testing.T) {
	t.Parallel()

	parsed, err := ParseToolJSONSchemaMap(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"optional": map[string]interface{}{"type": "string"},
			"env": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
		"required": []interface{}{"optional"},
	})
	if err != nil {
		t.Fatalf("ParseToolJSONSchemaMap: %v", err)
	}

	params := parsed.ToOpenResponsesParameters()
	props := params["properties"].(map[string]interface{})
	env := props["env"].(map[string]interface{})
	if env["type"] != "object" {
		t.Fatalf("provider-neutral env.type = %#v, want object", env["type"])
	}
	if _, ok := env["additionalProperties"].(map[string]interface{}); !ok {
		t.Fatalf("provider-neutral env.additionalProperties = %#v, want schema map", env["additionalProperties"])
	}
	required, ok := params["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "optional" {
		t.Fatalf("provider-neutral required = %#v, want only original required", params["required"])
	}
}

func TestJSONSchemaToOpenAIParametersAppliesStrictRules(t *testing.T) {
	t.Parallel()

	parsed, err := ParseToolJSONSchemaMap(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"optional": map[string]interface{}{"type": "string"},
			"env": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
		"required": []interface{}{"optional"},
	})
	if err != nil {
		t.Fatalf("ParseToolJSONSchemaMap: %v", err)
	}

	params := parsed.ToOpenAIParameters(true)
	props := params["properties"].(map[string]interface{})
	env := props["env"].(map[string]interface{})
	if env["type"] != "array" {
		t.Fatalf("OpenAI strict env.type = %#v, want array", env["type"])
	}
	required := params["required"].([]string)
	seen := map[string]bool{}
	for _, key := range required {
		seen[key] = true
	}
	if !seen["optional"] || !seen["env"] {
		t.Fatalf("OpenAI strict required = %#v, want all properties", required)
	}
	if params["additionalProperties"] != false {
		t.Fatalf("OpenAI strict additionalProperties = %#v, want false", params["additionalProperties"])
	}
}

func TestJSONSchemaToOpenAIParametersScrubsUnsupportedKeywords(t *testing.T) {
	t.Parallel()

	parsed, err := ParseToolJSONSchemaMap(map[string]interface{}{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]interface{}{
			"data": map[string]interface{}{
				"type":                 "object",
				"propertyNames":        map[string]interface{}{"type": "string"},
				"patternProperties":    map[string]interface{}{".*": map[string]interface{}{"type": "string"}},
				"additionalProperties": map[string]interface{}{"type": "string", "$ref": "#/$defs/value"},
			},
			"tuple": map[string]interface{}{
				"type":        "array",
				"prefixItems": []interface{}{map[string]interface{}{"type": "string"}},
			},
		},
		"$defs": map[string]interface{}{"value": map[string]interface{}{"type": "string"}},
	})
	if err != nil {
		t.Fatalf("ParseToolJSONSchemaMap: %v", err)
	}

	params := parsed.ToOpenAIParameters(false)
	assertNoUnsupportedOpenAIKeywords(t, params)
}

func assertNoUnsupportedOpenAIKeywords(t *testing.T, value interface{}) {
	t.Helper()
	switch v := value.(type) {
	case map[string]interface{}:
		for key := range unsupportedOpenAISchemaKeywords {
			if _, ok := v[key]; ok {
				t.Fatalf("found unsupported OpenAI schema keyword %q in %#v", key, v)
			}
		}
		for _, child := range v {
			assertNoUnsupportedOpenAIKeywords(t, child)
		}
	case []interface{}:
		for _, child := range v {
			assertNoUnsupportedOpenAIKeywords(t, child)
		}
	}
}
