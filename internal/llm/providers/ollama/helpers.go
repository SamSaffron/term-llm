package ollama

import (
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// newStreamingHTTPClient mirrors the root llm package's streaming client:
// transport-level timeouts only, no http.Client.Timeout, so active streams are
// never killed.
func newStreamingHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 100
	return &http.Client{Transport: transport}
}

var (
	httpClientMu sync.RWMutex
	httpClient   = newStreamingHTTPClient()
)

// SetHTTPClient replaces the package HTTP client (nil restores a fresh
// streaming client) and returns the previous one. It lets the root llm package
// or tests inject a transport.
func SetHTTPClient(c *http.Client) *http.Client {
	httpClientMu.Lock()
	defer httpClientMu.Unlock()
	prev := httpClient
	if c == nil {
		c = newStreamingHTTPClient()
	}
	httpClient = c
	return prev
}

func currentHTTPClient() *http.Client {
	httpClientMu.RLock()
	defer httpClientMu.RUnlock()
	return httpClient
}

// toolCallPart is the minimal tool call shape extracted from message parts.
type toolCallPart struct {
	Name      string
	Arguments string
}

// splitParts extracts text, tool calls, and reasoning content from message parts.
func splitParts(parts []core.Part) (string, []toolCallPart, string) {
	var textParts []string
	var toolCalls []toolCallPart
	var reasoning string
	for _, part := range parts {
		switch part.Type {
		case core.PartText, core.PartFile:
			if part.Text != "" {
				textParts = append(textParts, part.Text)
			}
			if part.ReasoningContent != "" {
				reasoning = part.ReasoningContent
			}
		case core.PartToolCall:
			if part.ToolCall == nil {
				continue
			}
			toolCalls = append(toolCalls, toolCallPart{
				Name:      part.ToolCall.Name,
				Arguments: string(part.ToolCall.Arguments),
			})
		}
	}
	return strings.Join(textParts, ""), toolCalls, reasoning
}

// isLeadingReasoningWhitespaceArtifact reports whether a whitespace-only text
// delta should be treated as part of the hidden reasoning channel.
func isLeadingReasoningWhitespaceArtifact(content, reasoning string, sawVisibleText bool) bool {
	return !sawVisibleText && reasoning != "" && strings.TrimSpace(content) == ""
}

func hasVisibleTextDelta(text string) bool {
	return strings.TrimSpace(text) != ""
}

func newSyntheticToolCallID() string {
	return "call_" + rand.Text()
}

func cloneEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	return append([]string(nil), efforts...)
}

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

func modelInfosFromCache(cached *cache.ModelCache) []core.ModelInfo {
	if cached == nil {
		return nil
	}
	if len(cached.ModelInfos) > 0 {
		models := make([]core.ModelInfo, 0, len(cached.ModelInfos))
		for _, m := range cached.ModelInfos {
			if m.ID == "" {
				continue
			}
			models = append(models, core.ModelInfo{
				ID:                m.ID,
				DisplayName:       m.DisplayName,
				Created:           m.Created,
				OwnedBy:           m.OwnedBy,
				InputLimit:        m.InputLimit,
				OutputLimit:       m.OutputLimit,
				ConfiguredContext: m.ConfiguredContext,
				InputPrice:        m.InputPrice,
				OutputPrice:       m.OutputPrice,
				ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
				ReasoningModes:    cloneEfforts(m.ReasoningModes),
			})
		}
		return models
	}
	models := make([]core.ModelInfo, 0, len(cached.Models))
	for _, id := range cached.Models {
		if id != "" {
			models = append(models, core.ModelInfo{ID: id})
		}
	}
	return models
}

func modelInfosToCache(models []core.ModelInfo) []cache.CachedModel {
	cached := make([]cache.CachedModel, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		cached = append(cached, cache.CachedModel{
			ID:                m.ID,
			DisplayName:       m.DisplayName,
			Created:           m.Created,
			OwnedBy:           m.OwnedBy,
			InputLimit:        m.InputLimit,
			OutputLimit:       m.OutputLimit,
			ConfiguredContext: m.ConfiguredContext,
			InputPrice:        m.InputPrice,
			OutputPrice:       m.OutputPrice,
			ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
			ReasoningModes:    cloneEfforts(m.ReasoningModes),
		})
	}
	return cached
}
