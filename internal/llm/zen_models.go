package llm

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
)

const zenModelCacheKey = "zen"

// RefreshZenModelsIfStale refreshes the live Zen model cache when it is missing
// or stale. A failed refresh leaves stale completions available.
func RefreshZenModelsIfStale(ctx context.Context, apiKey, model string) error {
	cached, err := cache.ReadModelCache(zenModelCacheKey)
	if err == nil && cache.IsCacheValid(cached) && (len(cached.ModelInfos) > 0 || len(cached.Models) > 0) {
		return nil
	}
	_, err = NewZenProvider(apiKey, model).ListModels(ctx)
	return err
}

// GetCachedZenModelInfos returns the last live Zen model catalog without
// performing network access. Stale entries remain useful for shell completion.
func GetCachedZenModelInfos() []ModelInfo {
	cached, err := cache.ReadModelCache(zenModelCacheKey)
	if err != nil || cached == nil || (len(cached.ModelInfos) == 0 && len(cached.Models) == 0) {
		return nil
	}
	return modelInfosFromCache(cached)
}

// GetCachedZenModels returns model IDs from the last successful Zen catalog
// refresh without performing network access.
func GetCachedZenModels() []string {
	infos := GetCachedZenModelInfos()
	ids := make([]string, 0, len(infos))
	for _, model := range infos {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ExpandCachedZenReasoningVariants adds the effort suffixes advertised by the
// live Zen catalog while preserving its model order.
func ExpandCachedZenReasoningVariants(models []string) []string {
	byModel := make(map[string][]string)
	for _, info := range GetCachedZenModelInfos() {
		if len(info.ReasoningEfforts) > 0 {
			byModel[info.ID] = info.ReasoningEfforts
		}
	}

	expanded := make([]string, 0, len(models))
	seen := make(map[string]bool)
	add := func(model string) {
		if model != "" && !seen[model] {
			seen[model] = true
			expanded = append(expanded, model)
		}
	}
	for _, model := range models {
		add(model)
		for _, effort := range byModel[model] {
			effort = strings.ToLower(strings.TrimSpace(effort))
			if effort != "" {
				add(model + "-" + effort)
			}
		}
	}
	return expanded
}

// RefreshZenCacheSync stores freshly fetched Zen model metadata for completion
// and offline provider/model pickers.
func RefreshZenCacheSync(models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(zenModelCacheKey, modelInfosToCache(models))
}
