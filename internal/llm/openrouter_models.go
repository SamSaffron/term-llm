package llm

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
)

const openRouterCacheKey = "openrouter"

func GetCachedOpenRouterModels(apiKey string) []string {
	cached, err := cache.ReadModelCache(openRouterCacheKey)
	if err == nil && cache.IsCacheValid(cached) {
		return cached.Models
	}

	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if apiKey == "" {
		if cached != nil && len(cached.Models) > 0 {
			return cached.Models
		}
		return nil
	}

	if cached != nil && len(cached.Models) > 0 {
		go refreshOpenRouterCache(apiKey)
		return cached.Models
	}

	return fetchOpenRouterModelsSync(apiKey)
}

func fetchOpenRouterModelsSync(apiKey string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	provider := NewOpenRouterProvider(apiKey, "", "", "")
	models, err := provider.ListModels(ctx)
	if err != nil || len(models) == 0 {
		return nil
	}

	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ID)
	}
	sort.Strings(modelIDs)

	_ = cache.WriteModelCache(openRouterCacheKey, modelIDs)
	return modelIDs
}

func refreshOpenRouterCache(apiKey string) {
	_ = fetchOpenRouterModelsSync(apiKey)
}

func RefreshOpenRouterCacheSync(apiKey string, models []ModelInfo) {
	if len(models) == 0 {
		return
	}

	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ID)
	}
	sort.Strings(modelIDs)

	_ = cache.WriteModelCache(openRouterCacheKey, modelIDs)
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
