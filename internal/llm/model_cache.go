package llm

import (
	"os"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
)

func getCachedModelInfos(apiKey, envName, cacheKey string, refreshInFlight *atomic.Bool, refresh func(string), fetch func(string) []core.ModelInfo) []core.ModelInfo {
	cached, err := cache.ReadModelCache(cacheKey)
	if err == nil && cache.IsCacheValid(cached) {
		return opencompat.ModelInfosFromCache(cached)
	}
	if apiKey == "" {
		apiKey = os.Getenv(envName)
	}
	if apiKey == "" {
		if cached != nil && len(cached.Models) > 0 {
			return opencompat.ModelInfosFromCache(cached)
		}
		return nil
	}
	if cached != nil && len(cached.Models) > 0 {
		if refreshInFlight.CompareAndSwap(false, true) {
			go func() {
				defer refreshInFlight.Store(false)
				refresh(apiKey)
			}()
		}
		return opencompat.ModelInfosFromCache(cached)
	}
	return fetch(apiKey)
}
