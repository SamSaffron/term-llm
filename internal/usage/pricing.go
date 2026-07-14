package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

	// TieredThreshold overrides the legacy 200K threshold. WholeRequestTier
	// means crossing the threshold reprices every token in the request rather
	// than only tokens above the threshold (the GPT-5.6 pricing contract).
	TieredThreshold  int  `json:"-"`
	WholeRequestTier bool `json:"-"`
}

// PricingFetcher fetches and caches model pricing from LiteLLM
type PricingFetcher struct {
	mu           sync.RWMutex
	cache        map[string]ModelPricing
	lastFetch    time.Time
	cacheDir     string
	httpClient   *http.Client
	localOnce    sync.Once
	localLoadErr error
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
	"sambanova/",
	"openrouter/openai/",
}

var bundledPricing = map[string]ModelPricing{
	// GPT-5.6 official OpenAI API pricing, USD per token, published 2026-07-09.
	// Requests above 272K total input use the long-context prices for the whole
	// request: 2x input/read/write and 1.5x output.
	"gpt-5.6-sol":   gpt56Pricing(5, 0.50, 6.25, 30),
	"gpt-5.6-terra": gpt56Pricing(2.50, 0.25, 3.125, 15),
	"gpt-5.6-luna":  gpt56Pricing(1, 0.10, 1.25, 6),

	// SambaNova official pricing, USD per token. Synced from
	// https://cloud.sambanova.ai/plans/pricing on 2026-05-21.
	"sambanova/DeepSeek-R1-Distill-Llama-70B": {
		InputCostPerToken: 0.70 / 1_000_000, OutputCostPerToken: 1.40 / 1_000_000,
	},
	"sambanova/DeepSeek-V3.1-cb": {
		InputCostPerToken: 0.15 / 1_000_000, OutputCostPerToken: 0.75 / 1_000_000,
	},
	"sambanova/DeepSeek-V3.1": {
		InputCostPerToken: 3.00 / 1_000_000, OutputCostPerToken: 4.50 / 1_000_000,
	},
	"sambanova/DeepSeek-V3.2": {
		InputCostPerToken: 3.00 / 1_000_000, OutputCostPerToken: 4.50 / 1_000_000,
	},
	"sambanova/gemma-3-12b-it": {
		InputCostPerToken: 0.35 / 1_000_000, OutputCostPerToken: 0.59 / 1_000_000,
	},
	"sambanova/gpt-oss-120b": {
		InputCostPerToken: 0.22 / 1_000_000, OutputCostPerToken: 0.59 / 1_000_000,
	},
	"sambanova/Llama-4-Maverick-17B-128E-Instruct": {
		InputCostPerToken: 0.63 / 1_000_000, OutputCostPerToken: 1.80 / 1_000_000,
	},
	"sambanova/Meta-Llama-3.3-70B-Instruct": {
		InputCostPerToken: 0.60 / 1_000_000, OutputCostPerToken: 1.20 / 1_000_000,
	},
	"sambanova/MiniMax-M2.7": {
		InputCostPerToken: 0.60 / 1_000_000, OutputCostPerToken: 2.40 / 1_000_000,
	},
}

func gpt56Pricing(input, read, write, output float64) ModelPricing {
	const perMillion = 1_000_000
	return ModelPricing{
		InputCostPerToken:           input / perMillion,
		OutputCostPerToken:          output / perMillion,
		CacheCreationInputTokenCost: write / perMillion,
		CacheReadInputTokenCost:     read / perMillion,
		InputCostPerTokenAbove200k:  input * 2 / perMillion,
		OutputCostPerTokenAbove200k: output * 1.5 / perMillion,
		CacheCreationCostAbove200k:  write * 2 / perMillion,
		CacheReadCostAbove200k:      read * 2 / perMillion,
		TieredThreshold:             272_000,
		WholeRequestTier:            true,
	}
}

// GetPricing returns pricing for a model, fetching if necessary
func (p *PricingFetcher) GetPricing(modelName string) (ModelPricing, error) {
	if pricing, ok := lookupBundledPricing(modelName); ok {
		return pricing, nil
	}

	if err := p.ensureLoaded(); err != nil {
		return ModelPricing{}, err
	}
	return p.lookupLoadedPricing(modelName)
}

func lookupBundledPricing(modelName string) (ModelPricing, bool) {
	candidates := []string{modelName}
	trimmed := strings.TrimSpace(modelName)
	if slash := strings.LastIndex(trimmed, "/"); slash >= 0 {
		candidates = append(candidates, trimmed[slash+1:])
	}
	for _, candidate := range append([]string(nil), candidates...) {
		for _, suffix := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
			if strings.HasSuffix(candidate, "-"+suffix) {
				candidates = append(candidates, strings.TrimSuffix(candidate, "-"+suffix))
				break
			}
		}
	}
	for _, candidate := range candidates {
		if pricing, ok := bundledPricing[candidate]; ok {
			return pricing, true
		}
	}
	for _, prefix := range providerPrefixes {
		if prefix == "" {
			continue
		}
		for _, candidate := range candidates {
			if pricing, ok := bundledPricing[prefix+candidate]; ok {
				return pricing, true
			}
		}
	}
	return ModelPricing{}, false
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

// GetPricingLocal returns bundled pricing or pricing from the existing on-disk
// cache. It never performs a network request and accepts stale cache data: exit
// summaries must remain immediate even when the network is unavailable.
func (p *PricingFetcher) GetPricingLocal(modelName string) (ModelPricing, error) {
	if pricing, ok := lookupBundledPricing(modelName); ok {
		return pricing, nil
	}
	p.localOnce.Do(func() {
		cacheFile := filepath.Join(p.cacheDir, "pricing.json")
		data, err := os.ReadFile(cacheFile)
		if err != nil {
			p.localLoadErr = fmt.Errorf("local pricing unavailable: %w", err)
			return
		}
		p.mu.Lock()
		p.localLoadErr = p.parseData(data)
		p.mu.Unlock()
	})
	if p.localLoadErr != nil {
		return ModelPricing{}, p.localLoadErr
	}
	return p.lookupLoadedPricing(modelName)
}

func (p *PricingFetcher) lookupLoadedPricing(modelName string) (ModelPricing, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if pricing, ok := p.cache[modelName]; ok {
		return pricing, nil
	}
	for _, prefix := range providerPrefixes {
		if pricing, ok := p.cache[prefix+modelName]; ok {
			return pricing, nil
		}
	}
	lower := strings.ToLower(modelName)
	keys := make([]string, 0, len(p.cache))
	for key := range p.cache {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		keyLower := strings.ToLower(key)
		if strings.Contains(keyLower, lower) || strings.Contains(lower, keyLower) {
			return p.cache[key], nil
		}
	}
	return ModelPricing{}, fmt.Errorf("pricing not found for model: %s", modelName)
}

// CalculateCostLocal calculates cost without fetching pricing from the network.
func (p *PricingFetcher) CalculateCostLocal(entry UsageEntry) (float64, error) {
	if entry.Model == "" {
		return 0, nil
	}
	pricing, err := p.GetPricingLocal(entry.Model)
	if err != nil {
		return 0, err
	}
	return calculateCostWithPricing(entry, pricing), nil
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
	return calculateCostWithPricing(entry, pricing), nil
}

func calculateCostWithPricing(entry UsageEntry, pricing ModelPricing) float64 {
	if pricing.WholeRequestTier {
		threshold := pricing.TieredThreshold
		if threshold <= 0 {
			threshold = tieredThreshold
		}
		totalInput := entry.InputTokens + entry.CacheReadTokens + entry.CacheWriteTokens
		longContext := totalInput > threshold
		price := func(base, tiered float64) float64 {
			if longContext && tiered > 0 {
				return tiered
			}
			return base
		}
		return float64(entry.InputTokens)*price(pricing.InputCostPerToken, pricing.InputCostPerTokenAbove200k) +
			float64(entry.OutputTokens)*price(pricing.OutputCostPerToken, pricing.OutputCostPerTokenAbove200k) +
			float64(entry.CacheWriteTokens)*price(pricing.CacheCreationInputTokenCost, pricing.CacheCreationCostAbove200k) +
			float64(entry.CacheReadTokens)*price(pricing.CacheReadInputTokenCost, pricing.CacheReadCostAbove200k)
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

	return cost
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
