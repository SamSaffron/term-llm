package llm

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
)

const cursorBinModelCacheKey = "cursor-bin"

// GetCachedCursorBinModels returns the last live model list fetched from
// Cursor Agent. It never spawns cursor-agent, so it is safe for shell
// completion and provider pickers. Run `term-llm models --provider cursor-bin`
// to refresh it.
func GetCachedCursorBinModels() []string {
	models := GetCachedCursorBinModelInfos()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// GetCachedCursorBinModelInfos returns cached live Cursor Agent model metadata.
// Stale cache entries are still returned because they are preferable to the
// small curated fallback and keep non-subprocess callers fast.
func GetCachedCursorBinModelInfos() []ModelInfo {
	cached, err := cache.ReadModelCache(cursorBinModelCacheKey)
	if err != nil || cached == nil || (len(cached.ModelInfos) == 0 && len(cached.Models) == 0) {
		return nil
	}
	return modelInfosFromCache(cached)
}

// RefreshCursorBinCacheSync stores a freshly fetched Cursor Agent model list
// for completions and offline provider/model pickers. IDs are normalized to
// term-llm's cursor-bin naming (auto-smart, grok-4.5-*).
func RefreshCursorBinCacheSync(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	modelInfos := make([]ModelInfo, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		id := normalizeCursorListedModelID(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m.ID = id
		modelInfos = append(modelInfos, m)
	}
	if len(modelInfos) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(cursorBinModelCacheKey, modelInfosToCache(modelInfos))
}

// normalizeCursorListedModelID maps Cursor Agent model IDs onto the names
// cursor-bin already uses for config, pickers, and --model arguments.
func normalizeCursorListedModelID(id string) string {
	id = strings.TrimSpace(id)
	switch id {
	case "":
		return ""
	case "auto":
		return "auto-smart"
	}
	if strings.HasPrefix(id, "cursor-grok-") {
		return strings.TrimPrefix(id, "cursor-")
	}
	return id
}
