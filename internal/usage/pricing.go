package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	liteLLMPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	pricingCacheTTL   = 5 * time.Minute
	tieredThreshold   = 200_000 // Token threshold for tiered pricing
)

// ModelPricing contains pricing information for a model
type ModelPricing struct {
	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	InputCostPerTokenAbove200k  float64 `json:"input_cost_per_token_above_200k_tokens"`
	OutputCostPerTokenAbove200k float64 `json:"output_cost_per_token_above_200k_tokens"`
	CacheCreationCostAbove200k  float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`
	CacheReadCostAbove200k      float64 `json:"cache_read_input_token_cost_above_200k_tokens"`
}

// PricingFetcher fetches and caches model pricing from LiteLLM
type PricingFetcher struct {
	mu         sync.RWMutex
	cache      map[string]ModelPricing
	lastFetch  time.Time
	cacheDir   string
	httpClient *http.Client
}

// NewPricingFetcher creates a new pricing fetcher
func NewPricingFetcher() *PricingFetcher {
	cacheDir := filepath.Join(os.TempDir(), "term-llm-pricing")
	os.MkdirAll(cacheDir, 0755)

	return &PricingFetcher{
		cache:    make(map[string]ModelPricing),
		cacheDir: cacheDir,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// providerPrefixes are common prefixes to try when looking up model names
var providerPrefixes = []string{
	"",
	"anthropic/",
	"openai/",
	"google/",
	"azure/",
	"openrouter/openai/",
}

// GetPricing returns pricing for a model, fetching if necessary
func (p *PricingFetcher) GetPricing(modelName string) (ModelPricing, error) {
	if err := p.ensureLoaded(); err != nil {
		return ModelPricing{}, err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	// Try exact match first
	if pricing, ok := p.cache[modelName]; ok {
		return pricing, nil
	}

	// Try with different provider prefixes
	for _, prefix := range providerPrefixes {
		key := prefix + modelName
		if pricing, ok := p.cache[key]; ok {
			return pricing, nil
		}
	}

	// Try case-insensitive partial matching
	lower := strings.ToLower(modelName)
	for key, pricing := range p.cache {
		keyLower := strings.ToLower(key)
		if strings.Contains(keyLower, lower) || strings.Contains(lower, keyLower) {
			return pricing, nil
		}
	}

	return ModelPricing{}, fmt.Errorf("pricing not found for model: %s", modelName)
}

// ensureLoaded ensures pricing data is loaded and fresh
func (p *PricingFetcher) ensureLoaded() error {
	p.mu.RLock()
	if len(p.cache) > 0 && time.Since(p.lastFetch) < pricingCacheTTL {
		p.mu.RUnlock()
		return nil
	}
	p.mu.RUnlock()

	return p.fetch()
}

// fetch retrieves pricing data from LiteLLM
func (p *PricingFetcher) fetch() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if len(p.cache) > 0 && time.Since(p.lastFetch) < pricingCacheTTL {
		return nil
	}

	// Try to load from disk cache first
	cacheFile := filepath.Join(p.cacheDir, "pricing.json")
	if info, err := os.Stat(cacheFile); err == nil {
		if time.Since(info.ModTime()) < pricingCacheTTL {
			if data, err := os.ReadFile(cacheFile); err == nil {
				if err := p.parseData(data); err == nil {
					return nil
				}
			}
		}
	}

	// Fetch from network
	resp, err := p.httpClient.Get(liteLLMPricingURL)
	if err != nil {
		// Try disk cache even if stale
		if data, err := os.ReadFile(cacheFile); err == nil {
			if err := p.parseData(data); err == nil {
				return nil
			}
		}
		return fmt.Errorf("failed to fetch pricing: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch pricing: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read pricing data: %w", err)
	}

	if err := p.parseData(data); err != nil {
		return err
	}

	// Save to disk cache
	os.WriteFile(cacheFile, data, 0644)

	return nil
}

// parseData parses the LiteLLM pricing JSON
func (p *PricingFetcher) parseData(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("failed to parse pricing JSON: %w", err)
	}

	newCache := make(map[string]ModelPricing)
	for key, value := range raw {
		var pricing ModelPricing
		if err := json.Unmarshal(value, &pricing); err != nil {
			continue // Skip invalid entries
		}
		newCache[key] = pricing
	}

	p.cache = newCache
	p.lastFetch = time.Now()
	return nil
}

// CalculateCost calculates the cost for a usage entry
func (p *PricingFetcher) CalculateCost(entry UsageEntry) (float64, error) {
	if entry.Model == "" {
		return 0, nil
	}

	pricing, err := p.GetPricing(entry.Model)
	if err != nil {
		return 0, err
	}

	var cost float64

	// Input tokens
	cost += calculateTieredCost(
		entry.InputTokens,
		pricing.InputCostPerToken,
		pricing.InputCostPerTokenAbove200k,
	)

	// Output tokens
	cost += calculateTieredCost(
		entry.OutputTokens,
		pricing.OutputCostPerToken,
		pricing.OutputCostPerTokenAbove200k,
	)

	// Cache write tokens
	cost += calculateTieredCost(
		entry.CacheWriteTokens,
		pricing.CacheCreationInputTokenCost,
		pricing.CacheCreationCostAbove200k,
	)

	// Cache read tokens
	cost += calculateTieredCost(
		entry.CacheReadTokens,
		pricing.CacheReadInputTokenCost,
		pricing.CacheReadCostAbove200k,
	)

	return cost, nil
}

// calculateTieredCost calculates cost with tiered pricing (200k threshold)
func calculateTieredCost(tokens int, basePrice, tieredPrice float64) float64 {
	if tokens <= 0 {
		return 0
	}

	if tokens > tieredThreshold && tieredPrice > 0 {
		belowThreshold := min(tokens, tieredThreshold)
		aboveThreshold := tokens - tieredThreshold

		cost := float64(aboveThreshold) * tieredPrice
		if basePrice > 0 {
			cost += float64(belowThreshold) * basePrice
		}
		return cost
	}

	if basePrice > 0 {
		return float64(tokens) * basePrice
	}

	return 0
}
