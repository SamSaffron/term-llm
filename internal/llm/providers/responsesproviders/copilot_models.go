package responsesproviders

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
)

const copilotModelCacheKey = "copilot"

// GetCachedCopilotModels returns the last live model list fetched from Copilot.
// It never performs network or auth work, so it is safe for shell completion and
// provider pickers. Run `term-llm models --provider copilot` to refresh it.
func GetCachedCopilotModels() []string {
	models := GetCachedCopilotModelInfos()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// GetCachedCopilotModelInfos returns cached live Copilot model metadata. Stale
// cache entries are still returned because they are preferable to hardcoded
// model lists and keep non-network callers fast.
func GetCachedCopilotModelInfos() []ModelInfo {
	cached, err := cache.ReadModelCache(copilotModelCacheKey)
	if err != nil || cached == nil || (len(cached.ModelInfos) == 0 && len(cached.Models) == 0) {
		return nil
	}
	return modelInfosFromCache(cached)
}

func copilotCachedInputLimit(model string) int {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return 0
	}
	models := GetCachedCopilotModelInfos()
	lookup := func(id string) int {
		for _, m := range models {
			if strings.ToLower(strings.TrimSpace(m.ID)) == id {
				return m.InputLimit
			}
		}
		return 0
	}
	if limit := lookup(model); limit > 0 {
		return limit
	}
	if base, ok := trimKnownEffortSuffix(model); ok {
		return lookup(base)
	}
	return 0
}

// RefreshCopilotCacheSync stores a freshly fetched Copilot model list for
// completions and offline provider/model pickers.
func RefreshCopilotCacheSync(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	modelInfos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			modelInfos = append(modelInfos, m)
		}
	}
	_ = cache.WriteModelInfoCache(copilotModelCacheKey, modelInfosToCache(modelInfos))
}
