package llm

import (
	"context"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
)

const openRouterCacheKey = "openrouter"

var openRouterCacheRefreshInFlight atomic.Bool

func GetCachedOpenRouterModels(apiKey string) []string {
	models := GetCachedOpenRouterModelInfos(apiKey)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func GetCachedOpenRouterModelInfos(apiKey string) []ModelInfo {
	return getCachedModelInfos(apiKey, "OPENROUTER_API_KEY", openRouterCacheKey, &openRouterCacheRefreshInFlight, refreshOpenRouterCache, fetchOpenRouterModelInfosSync)
}

func fetchOpenRouterModelsSync(apiKey string) []string {
	models := fetchOpenRouterModelInfosSync(apiKey)
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

func fetchOpenRouterModelInfosSync(apiKey string) []ModelInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	provider := NewOpenRouterProvider(apiKey, "", "", "")
	models, err := provider.ListModels(ctx)
	if err != nil || len(models) == 0 {
		return nil
	}

	modelInfos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			modelInfos = append(modelInfos, m)
		}
	}
	sort.Slice(modelInfos, func(i, j int) bool { return modelInfos[i].ID < modelInfos[j].ID })

	_ = cache.WriteModelInfoCache(openRouterCacheKey, modelInfosToCache(modelInfos))
	return modelInfos
}

func refreshOpenRouterCache(apiKey string) {
	_ = fetchOpenRouterModelsSync(apiKey)
}

func RefreshOpenRouterCacheSync(apiKey string, models []ModelInfo) {
	if len(models) == 0 {
		return
	}

	modelInfos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			modelInfos = append(modelInfos, m)
		}
	}
	sort.Slice(modelInfos, func(i, j int) bool { return modelInfos[i].ID < modelInfos[j].ID })

	_ = cache.WriteModelInfoCache(openRouterCacheKey, modelInfosToCache(modelInfos))
}

func modelInfosFromCache(cached *cache.ModelCache) []ModelInfo {
	if cached == nil {
		return nil
	}
	if len(cached.ModelInfos) > 0 {
		models := make([]ModelInfo, 0, len(cached.ModelInfos))
		for _, m := range cached.ModelInfos {
			if m.ID == "" {
				continue
			}
			models = append(models, ModelInfo{
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
			})
		}
		return models
	}
	models := make([]ModelInfo, 0, len(cached.Models))
	for _, id := range cached.Models {
		if id != "" {
			models = append(models, ModelInfo{ID: id})
		}
	}
	return models
}

func modelInfosToCache(models []ModelInfo) []cache.CachedModel {
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
		})
	}
	return cached
}

func openRouterCachedInputLimit(model string) int {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return 0
	}
	cached, err := cache.ReadModelCache(openRouterCacheKey)
	if err != nil || cached == nil {
		return 0
	}
	for _, m := range cached.ModelInfos {
		if strings.ToLower(strings.TrimSpace(m.ID)) == model {
			return m.InputLimit
		}
	}
	return 0
}

func FilterOpenRouterModels(models []string, prefix string) []string {
	if prefix == "" {
		return models
	}

	var filtered []string
	for _, m := range models {
		if strings.HasPrefix(m, prefix) {
			filtered = append(filtered, m)
		}
	}
	return filtered
}
