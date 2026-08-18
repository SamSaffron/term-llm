package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
)

func ollamaModelCacheKey(baseURL string) string {
	normalized := NewOllamaChatProvider(baseURL, "", OllamaOptions{}).baseURL
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("ollama-v2-%x", sum[:8])
}

// RefreshOllamaModelsIfStale refreshes the endpoint-specific Ollama model cache
// when it is missing or stale. A failed refresh leaves stale completions usable.
func RefreshOllamaModelsIfStale(ctx context.Context, baseURL, model string) error {
	cacheKey := ollamaModelCacheKey(baseURL)
	cached, err := cache.ReadModelCache(cacheKey)
	if err == nil && cache.IsCacheValid(cached) && (len(cached.ModelInfos) > 0 || len(cached.Models) > 0) {
		return nil
	}
	_, err = NewOllamaChatProvider(baseURL, model, OllamaOptions{}).ListModels(ctx)
	return err
}

// GetCachedOllamaModelInfos returns the last live endpoint-specific Ollama
// model metadata without performing network access.
func GetCachedOllamaModelInfos(baseURL string) []ModelInfo {
	cached, err := cache.ReadModelCache(ollamaModelCacheKey(baseURL))
	if err != nil || cached == nil {
		return nil
	}
	return modelInfosFromCache(cached)
}

// GetCachedOllamaModels returns the last live model list fetched for an Ollama
// endpoint without performing network access. Stale entries remain usable as a
// completion fallback.
func GetCachedOllamaModels(baseURL string) []string {
	infos := GetCachedOllamaModelInfos(baseURL)
	ids := make([]string, 0, len(infos))
	for _, model := range infos {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids
}

// CachedOllamaModelEffort resolves a live Ollama model or one of its advertised
// effort variants using endpoint-specific /api/show metadata.
func CachedOllamaModelEffort(baseURL, model string) (base, effort string, efforts []string, ok bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", nil, false
	}
	infos := GetCachedOllamaModelInfos(baseURL)
	for _, info := range infos {
		base = strings.TrimSpace(info.ID)
		if strings.EqualFold(model, base) && len(info.ReasoningEfforts) > 0 {
			return base, "", cloneEfforts(info.ReasoningEfforts), true
		}
	}
	for _, info := range infos {
		base = strings.TrimSpace(info.ID)
		for _, candidateEffort := range info.ReasoningEfforts {
			candidateEffort = strings.ToLower(strings.TrimSpace(candidateEffort))
			if candidateEffort != "" && strings.EqualFold(model, base+"-"+candidateEffort) {
				return base, candidateEffort, cloneEfforts(info.ReasoningEfforts), true
			}
		}
	}
	return "", "", nil, false
}

func CachedOllamaReasoningEfforts(baseURL, model string) []string {
	_, _, efforts, ok := CachedOllamaModelEffort(baseURL, model)
	if ok {
		return efforts
	}
	return nil
}

func ExpandCachedOllamaReasoningVariants(baseURL string, models []string) []string {
	byModel := make(map[string][]string)
	for _, info := range GetCachedOllamaModelInfos(baseURL) {
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
			add(model + "-" + effort)
		}
	}
	return expanded
}

func refreshOllamaModelCache(baseURL string, models []ModelInfo) {
	_ = cache.WriteModelInfoCache(ollamaModelCacheKey(baseURL), modelInfosToCache(models))
}
