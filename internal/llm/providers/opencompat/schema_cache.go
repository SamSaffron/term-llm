package opencompat

import (
	"encoding/json"
	"reflect"
	"sync"
)

const maxToolSchemaJSONCacheEntries = 4096

type toolSchemaJSONCacheEntry struct {
	// Strong reference keeps the schema map identity from being reused.
	schema map[string]interface{}
	raw    json.RawMessage
}

var toolSchemaJSONCache = struct {
	mu      sync.Mutex
	entries map[uintptr]*toolSchemaJSONCacheEntry
	order   []uintptr
}{
	entries: make(map[uintptr]*toolSchemaJSONCacheEntry),
}

// cachedToolSchemaJSON returns the JSON encoding of an immutable ToolSpec
// schema, cached by map identity (bounded).
func cachedToolSchemaJSON(schema map[string]interface{}) (json.RawMessage, error) {
	var key uintptr
	if schema != nil {
		key = reflect.ValueOf(schema).Pointer()
	}

	toolSchemaJSONCache.mu.Lock()
	if entry := toolSchemaJSONCache.entries[key]; entry != nil {
		raw := entry.raw
		toolSchemaJSONCache.mu.Unlock()
		return raw, nil
	}
	toolSchemaJSONCache.mu.Unlock()

	b, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	raw := json.RawMessage(b)

	toolSchemaJSONCache.mu.Lock()
	if entry := toolSchemaJSONCache.entries[key]; entry != nil {
		raw = entry.raw
	} else {
		if len(toolSchemaJSONCache.order) >= maxToolSchemaJSONCacheEntries {
			oldest := toolSchemaJSONCache.order[0]
			delete(toolSchemaJSONCache.entries, oldest)
			copy(toolSchemaJSONCache.order, toolSchemaJSONCache.order[1:])
			toolSchemaJSONCache.order = toolSchemaJSONCache.order[:len(toolSchemaJSONCache.order)-1]
		}
		toolSchemaJSONCache.entries[key] = &toolSchemaJSONCacheEntry{schema: schema, raw: raw}
		toolSchemaJSONCache.order = append(toolSchemaJSONCache.order, key)
	}
	toolSchemaJSONCache.mu.Unlock()

	return raw, nil
}
