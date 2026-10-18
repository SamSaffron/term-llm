package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/openaiapi"
)

// endpointModelCacheKey scopes a live model catalog to one endpoint and
// credential. The API key itself is never stored; only its hash contributes
// to the key, so two providers sharing a base URL with different keys (or
// the same provider after a key change) never see each other's models.
func endpointModelCacheKey(providerCfg config.ProviderConfig) string {
	baseURL := endpointModelBaseURL(providerCfg)
	apiKey := providerCfg.ResolvedAPIKey
	if apiKey == "" {
		apiKey = providerCfg.APIKey
	}
	sum := sha256.Sum256([]byte(baseURL + "\x00" + apiKey))
	return fmt.Sprintf("openai-endpoint-%x", sum[:8])
}

// endpointModelBaseURL returns the effective API root for catalog requests:
// the configured base_url when set, otherwise the resolved OpenAI endpoint
// (OPENAI_BASE_URL or the public API root).
func endpointModelBaseURL(providerCfg config.ProviderConfig) string {
	if baseURL := strings.TrimSpace(providerCfg.BaseURL); baseURL != "" {
		return NewOpenAICompatProvider(baseURL, "", "", "").BaseURL()
	}
	return providerCfg.OpenAIEndpoint().BaseURL
}

// UsesEndpointModelCatalog reports whether the provider's model catalog is
// defined by its endpoint rather than by a curated per-type list: generic
// OpenAI-compatible servers always, and OpenAI providers pointed at a
// non-public base URL (for example a custom-named provider fronting another
// vendor's OpenAI-compatible API). Curated type models must never be offered
// for such providers; they belong to a different endpoint.
func UsesEndpointModelCatalog(provider string, providerCfg config.ProviderConfig) bool {
	switch config.InferProviderType(provider, providerCfg.Type) {
	case config.ProviderTypeOpenAICompat, config.ProviderTypeVLLM:
		return true
	case config.ProviderTypeOpenAI:
		return endpointModelBaseURL(providerCfg) != openaiapi.DefaultBaseURL
	default:
		return false
	}
}

// RefreshEndpointModelsIfStale refreshes the endpoint-specific model cache
// when it is missing or stale. A failed refresh leaves stale entries usable.
func RefreshEndpointModelsIfStale(ctx context.Context, providerCfg config.ProviderConfig) error {
	cacheKey := endpointModelCacheKey(providerCfg)
	cached, err := cache.ReadModelCache(cacheKey)
	if err == nil && cache.IsCacheValid(cached) && (len(cached.ModelInfos) > 0 || len(cached.Models) > 0) {
		return nil
	}
	apiKey := providerCfg.ResolvedAPIKey
	if apiKey == "" {
		apiKey = providerCfg.APIKey
	}
	models, err := NewOpenAICompatProvider(endpointModelBaseURL(providerCfg), apiKey, providerCfg.Model, "").ListModels(ctx)
	if err != nil {
		return err
	}
	RefreshEndpointCacheSync(providerCfg, models)
	return nil
}

// GetCachedEndpointModels returns the last live model list fetched for this
// provider's endpoint and credential without performing network access.
// Stale entries remain usable as a completion fallback.
func GetCachedEndpointModels(providerCfg config.ProviderConfig) []string {
	cached, err := cache.ReadModelCache(endpointModelCacheKey(providerCfg))
	if err != nil || cached == nil {
		return nil
	}
	infos := modelInfosFromCache(cached)
	ids := make([]string, 0, len(infos))
	for _, model := range infos {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// RefreshEndpointCacheSync stores a freshly fetched endpoint catalog for
// completion and offline provider/model pickers.
func RefreshEndpointCacheSync(providerCfg config.ProviderConfig, models []ModelInfo) {
	if len(models) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(endpointModelCacheKey(providerCfg), modelInfosToCache(models))
}
