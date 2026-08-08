package llm

import (
	"os"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/cache"
)

func getCachedModelInfos(apiKey, envName, cacheKey string, refreshInFlight *atomic.Bool, refresh func(string), fetch func(string) []ModelInfo) []ModelInfo {
	cached, err := cache.ReadModelCache(cacheKey)
	if err == nil && cache.IsCacheValid(cached) {
		return modelInfosFromCache(cached)
	}
	if apiKey == "" {
		apiKey = os.Getenv(envName)
	}
	if apiKey == "" {
		if cached != nil && len(cached.Models) > 0 {
			return modelInfosFromCache(cached)
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
		return modelInfosFromCache(cached)
	}
	return fetch(apiKey)
}
