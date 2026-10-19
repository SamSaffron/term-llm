package llm

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
)

type countingSchemaValue struct {
	count *int
}

func (v countingSchemaValue) MarshalJSON() ([]byte, error) {
	*v.count = *v.count + 1
	return []byte(`"counted"`), nil
}

func TestToolSchemaJSONCacheReusesImmutableSchema(t *testing.T) {
	marshalCount := 0
	schema := map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
		"x-count":    countingSchemaValue{count: &marshalCount},
	}
	specs := []core.ToolSpec{{Name: "cached", Description: "cached schema", Schema: schema}}

	compatTools, err := opencompat.BuildCompatTools(specs)
	if err != nil {
		t.Fatalf("buildCompatTools first call: %v", err)
	}
	if len(compatTools) != 1 || len(compatTools[0].Function.Parameters) == 0 {
		t.Fatalf("buildCompatTools returned empty parameters: %#v", compatTools)
	}

	compatTools, err = opencompat.BuildCompatTools(specs)
	if err != nil {
		t.Fatalf("buildCompatTools second call: %v", err)
	}
	if len(compatTools) != 1 || len(compatTools[0].Function.Parameters) == 0 {
		t.Fatalf("buildCompatTools returned empty cached parameters: %#v", compatTools)
	}

	if marshalCount != 1 {
		t.Fatalf("schema marshaled %d times, want 1", marshalCount)
	}
}

func TestToolSchemaJSONCacheIsBounded(t *testing.T) {
	resetToolSchemaJSONCacheForTest(t)

	for i := 0; i < maxToolSchemaJSONCacheEntries+10; i++ {
		schema := map[string]interface{}{"type": "object", "x-index": i}
		if _, err := cachedToolSchemaJSON(schema); err != nil {
			t.Fatalf("cachedToolSchemaJSON(%d): %v", i, err)
		}
	}

	toolSchemaJSONCache.mu.Lock()
	entryCount := len(toolSchemaJSONCache.entries)
	orderCount := len(toolSchemaJSONCache.order)
	toolSchemaJSONCache.mu.Unlock()

	if entryCount != maxToolSchemaJSONCacheEntries || orderCount != maxToolSchemaJSONCacheEntries {
		t.Fatalf("cache size = entries %d order %d, want %d", entryCount, orderCount, maxToolSchemaJSONCacheEntries)
	}
}

func resetToolSchemaJSONCacheForTest(t *testing.T) {
	t.Helper()

	toolSchemaJSONCache.mu.Lock()
	oldEntries := toolSchemaJSONCache.entries
	oldOrder := toolSchemaJSONCache.order
	toolSchemaJSONCache.entries = make(map[uintptr]*toolSchemaJSONCacheEntry)
	toolSchemaJSONCache.order = nil
	toolSchemaJSONCache.mu.Unlock()

	t.Cleanup(func() {
		toolSchemaJSONCache.mu.Lock()
		toolSchemaJSONCache.entries = oldEntries
		toolSchemaJSONCache.order = oldOrder
		toolSchemaJSONCache.mu.Unlock()
	})
}
