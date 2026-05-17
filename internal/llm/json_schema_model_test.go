package llm

import (
	"reflect"
	"testing"
)

func TestParseToolJSONSchemaMap_SanitizesAndPreservesExtras(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"data": map[string]interface{}{
				"type":  []interface{}{"array", "null", "bogus"},
				"items": true,
			},
			"mode": map[string]interface{}{
				"const":   "fast",
				"default": "fast",
				"title":   "Mode",
			},
			"detail": map[string]interface{}{
				"type": "string",
				"enum": []string{"low", "high"},
			},
		},
	}

	parsed, err := ParseToolJSONSchemaMap(schema)
	if err != nil {
		t.Fatalf("ParseToolJSONSchemaMap: %v", err)
	}
	got := parsed.ToMap()
	props := got["properties"].(map[string]interface{})

	data := props["data"].(map[string]interface{})
	typeArray, ok := data["type"].([]interface{})
	if !ok || len(typeArray) != 2 || typeArray[0] != "array" || typeArray[1] != "null" {
		t.Fatalf("data.type = %#v, want [array null]", data["type"])
	}
	items := data["items"].(map[string]interface{})
	if items["type"] != "string" {
		t.Fatalf("data.items.type = %#v, want string", items["type"])
	}

	mode := props["mode"].(map[string]interface{})
	if mode["type"] != "string" {
		t.Fatalf("mode.type = %#v, want string", mode["type"])
	}
	enumValues := mode["enum"].([]interface{})
	if len(enumValues) != 1 || enumValues[0] != "fast" {
		t.Fatalf("mode.enum = %#v, want [fast]", mode["enum"])
	}
	if mode["default"] != "fast" || mode["title"] != "Mode" {
		t.Fatalf("mode extras not preserved: %#v", mode)
	}

	detail := props["detail"].(map[string]interface{})
	detailEnum := detail["enum"].([]interface{})
	if len(detailEnum) != 2 || detailEnum[0] != "low" || detailEnum[1] != "high" {
		t.Fatalf("detail.enum = %#v, want [low high]", detail["enum"])
	}
}

func TestSanitizedResponsesParametersSchema_DefaultsEmptyObject(t *testing.T) {
	got := openAIParametersFromToolSchema(map[string]interface{}{}, true)
	if got["type"] != "object" {
		t.Fatalf("type = %#v, want object", got["type"])
	}
	props, ok := got["properties"].(map[string]interface{})
	if !ok || len(props) != 0 {
		t.Fatalf("properties = %#v, want empty map", got["properties"])
	}
}

func TestOpenAIParametersFromToolSchema_CachesLoweredParametersBySchemaIdentityAndStrictness(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{"type": "string"},
		},
	}

	firstStrict := openAIParametersFromToolSchema(schema, true)
	secondStrict := openAIParametersFromToolSchema(schema, true)
	if reflect.ValueOf(firstStrict).Pointer() != reflect.ValueOf(secondStrict).Pointer() {
		t.Fatalf("strict cache miss: first=%#v second=%#v", firstStrict, secondStrict)
	}

	firstNonStrict := openAIParametersFromToolSchema(schema, false)
	secondNonStrict := openAIParametersFromToolSchema(schema, false)
	if reflect.ValueOf(firstNonStrict).Pointer() != reflect.ValueOf(secondNonStrict).Pointer() {
		t.Fatalf("non-strict cache miss: first=%#v second=%#v", firstNonStrict, secondNonStrict)
	}
	if reflect.ValueOf(firstStrict).Pointer() == reflect.ValueOf(firstNonStrict).Pointer() {
		t.Fatalf("strict and non-strict schemas should be cached separately")
	}
}
