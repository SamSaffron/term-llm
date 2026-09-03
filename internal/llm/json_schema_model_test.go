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
	if !reflect.DeepEqual(firstStrict, secondStrict) {
		t.Fatalf("strict cache returned different content: first=%#v second=%#v", firstStrict, secondStrict)
	}
	if reflect.ValueOf(firstStrict).Pointer() == reflect.ValueOf(secondStrict).Pointer() {
		t.Fatalf("strict cache returned shared mutable map: first=%#v second=%#v", firstStrict, secondStrict)
	}

	firstStrict["x-mutated"] = true
	if props, ok := firstStrict["properties"].(map[string]interface{}); ok {
		props["name"] = map[string]interface{}{"type": "number"}
	}
	thirdStrict := openAIParametersFromToolSchema(schema, true)
	if _, ok := thirdStrict["x-mutated"]; ok {
		t.Fatalf("mutation of returned parameters leaked into cache: %#v", thirdStrict)
	}
	props, ok := thirdStrict["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("thirdStrict properties = %#v, want map", thirdStrict["properties"])
	}
	name, ok := props["name"].(map[string]interface{})
	if !ok || name["type"] != "string" {
		t.Fatalf("nested mutation of returned parameters leaked into cache: %#v", thirdStrict)
	}

	firstNonStrict := openAIParametersFromToolSchema(schema, false)
	secondNonStrict := openAIParametersFromToolSchema(schema, false)
	if !reflect.DeepEqual(firstNonStrict, secondNonStrict) {
		t.Fatalf("non-strict cache returned different content: first=%#v second=%#v", firstNonStrict, secondNonStrict)
	}
	if reflect.DeepEqual(thirdStrict, firstNonStrict) {
		t.Fatalf("strict and non-strict schemas should be cached separately")
	}
}

func TestOpenAIParametersCacheIsBounded(t *testing.T) {
	resetOpenAIParametersCacheForTest(t)

	schemas := make([]map[string]interface{}, maxOpenAIParametersCacheEntries+10)
	for i := range schemas {
		schemas[i] = map[string]interface{}{"type": "object", "x-index": i}
		openAIParametersFromToolSchema(schemas[i], true)
	}

	firstKey := openAIParametersCacheKey{
		schemaPtr: reflect.ValueOf(schemas[0]).Pointer(),
		strict:    true,
	}
	lastKey := openAIParametersCacheKey{
		schemaPtr: reflect.ValueOf(schemas[len(schemas)-1]).Pointer(),
		strict:    true,
	}

	openAIParametersCache.mu.Lock()
	entryCount := len(openAIParametersCache.entries)
	orderCount := len(openAIParametersCache.order)
	_, hasFirst := openAIParametersCache.entries[firstKey]
	_, hasLast := openAIParametersCache.entries[lastKey]
	openAIParametersCache.mu.Unlock()

	if entryCount != maxOpenAIParametersCacheEntries || orderCount != maxOpenAIParametersCacheEntries {
		t.Fatalf("cache size = entries %d order %d, want %d", entryCount, orderCount, maxOpenAIParametersCacheEntries)
	}
	if hasFirst {
		t.Fatal("oldest schema was not evicted")
	}
	if !hasLast {
		t.Fatal("newest schema was not cached")
	}
}

func resetOpenAIParametersCacheForTest(t *testing.T) {
	t.Helper()

	openAIParametersCache.mu.Lock()
	oldEntries := openAIParametersCache.entries
	oldOrder := openAIParametersCache.order
	oldNext := openAIParametersCache.next
	openAIParametersCache.entries = make(map[openAIParametersCacheKey]*openAIParametersCacheEntry)
	openAIParametersCache.order = nil
	openAIParametersCache.next = 0
	openAIParametersCache.mu.Unlock()

	t.Cleanup(func() {
		openAIParametersCache.mu.Lock()
		openAIParametersCache.entries = oldEntries
		openAIParametersCache.order = oldOrder
		openAIParametersCache.next = oldNext
		openAIParametersCache.mu.Unlock()
	})
}
