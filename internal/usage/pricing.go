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
	cacheDir := defaultPricingCacheDir()

	return &PricingFetcher{
		cache:    make(map[string]ModelPricing),
		cacheDir: cacheDir,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// defaultPricingCacheDir returns the directory holding the fetched pricing
// cache. It follows XDG semantics ($XDG_CACHE_HOME/term-llm, else
// ~/.cache/term-llm) so cached prices survive a reboot, and only falls back to
// a temp directory when neither location can be determined or created.
func defaultPricingCacheDir() string {
	xdgCacheHome := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME"))
	homeDir, _ := os.UserHomeDir()
	// Pricing a stats page constructs a fetcher per row and per child. Resolving
	// the directory means a home lookup and a mkdir, so the answer is cached for
	// the environment that produced it rather than recomputed hundreds of times.
	cacheKey := xdgCacheHome + "\x00" + homeDir
	pricingCacheDirMu.Lock()
	defer pricingCacheDirMu.Unlock()
	if dir, ok := pricingCacheDirs[cacheKey]; ok {
		return dir
	}
	dir := resolvePricingCacheDir(xdgCacheHome, homeDir, os.TempDir(), func(dir string) error {
		return os.MkdirAll(dir, 0o755)
	})
	if pricingCacheDirs == nil {
		pricingCacheDirs = map[string]string{}
	}
	pricingCacheDirs[cacheKey] = dir
	return dir
}

var (
	pricingCacheDirMu sync.Mutex
	pricingCacheDirs  map[string]string
)

// resolvePricingCacheDir picks the first candidate cache directory that
// mkdirAll can create, falling back to a temp directory. mkdirAll is a
// parameter so the resolution order is testable without creating directories
// under the real home directory.
func resolvePricingCacheDir(xdgCacheHome, homeDir, tempDir string, mkdirAll func(string) error) string {
	var candidates []string
	if xdgCacheHome != "" {
		candidates = append(candidates, filepath.Join(xdgCacheHome, "term-llm"))
	}
	if homeDir != "" {
		candidates = append(candidates, filepath.Join(homeDir, ".cache", "term-llm"))
	}
	for _, candidate := range candidates {
		if err := mkdirAll(candidate); err == nil {
			return candidate
		}
	}

	fallback := filepath.Join(tempDir, "term-llm-pricing")
	_ = mkdirAll(fallback)
	return fallback
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

// Anthropic Claude list pricing grouped by family: the bare claude-bin aliases
// (which are what sessions store as the model name) and the canonical Anthropic
// model names for that family share the same published rates.
//
// The bare aliases follow the CLI's current model, so they track the current
// generation; superseded generations keep their own rates so historical
// sessions are not repriced. Cache writes use the 5-minute TTL.
var (
	// Opus 5 and 4.8, verified 2026-09-14.
	claudeOpusPricing = anthropicPricing(5, 6.25, 0.50, 25)
	// Opus 4.7 and earlier.
	claudeLegacyOpusPricing = anthropicPricing(15, 18.75, 1.50, 75)
	// Sonnet 5. The increase to $3/$15 scheduled for 2026-09-01 was cancelled,
	// so the launch rates are now standard.
	claudeSonnetPricing = anthropicPricing(2, 2.50, 0.20, 10)
	// Sonnet 4.6 and earlier.
	claudeLegacySonnetPricing = anthropicPricing(3, 3.75, 0.30, 15)
	claudeFablePricing        = anthropicPricing(10, 12.50, 1.00, 50)
	claudeHaikuPricing        = anthropicPricing(1, 1.25, 0.10, 5)
)

// bundledPricing is the offline answer. The refreshed models.dev directory wins
// where it has an entry, so this table only has to cover the models this program
// is usually pointed at, and to keep working with no network at all.
var bundledPricing = map[string]ModelPricing{
	// OpenAI list pricing, verified 2026-09-15 against developers.openai.com.
	// Requests above 272K total input use the long-context prices for the whole
	// request. gpt-5.6-sol is promotional through at least 2026-11-21.
	// gpt-5.3-codex-spark is deliberately absent: it is a ChatGPT-only research
	// preview with no published API rate, and guessing would be worse than the
	// dash an unpriced model shows.
	"gpt-6-astra":   openAIPricing(10, 1.00, 12.50, 50),
	"gpt-5.6-sol":   openAIPricing(4, 0.40, 5.00, 20),
	"gpt-5.6-terra": openAIPricing(2, 0.20, 2.50, 12),
	"gpt-5.6-luna":  openAIPricing(0.20, 0.02, 0.25, 1.20),
	"gpt-5.5":       openAIPricing(5, 0.50, 0, 30),
	"gpt-5.3-codex": openAIPricing(1.75, 0.175, 0, 14),

	// xAI list pricing, verified 2026-09-15 against docs.x.ai. Cache writes are
	// not billed; a prompt of 200K or more reprices the whole request.
	"grok-4.6": longContextPricing(2, 0.50, 0, 6, 200_000, 2, 2),
	"grok-4.5": longContextPricing(2, 0.30, 0, 6, 200_000, 2, 2),

	// Google list pricing, verified 2026-09-15 against ai.google.dev and Google
	// Cloud. Flash rates are promotional: every figure doubles on 2027-01-01.
	// Gemini caching bills storage per hour, which usage counters cannot see.
	"gemini-3.8-flash": flatPricing(0.75, 0.075, 0, 3.75),
	"gemini-3.7-flash": flatPricing(0.75, 0.075, 0, 3.75),
	"gemini-3.6-flash": flatPricing(0.75, 0.075, 0, 3.75),
	"gemini-3.1-pro":   longContextPricing(2, 0.20, 0, 12, 200_000, 2, 1.5),

	// Other first-party list pricing, verified 2026-09-15.
	// DeepSeek bills half rate off-peak (outside 01:00-04:00 and 06:00-10:00
	// UTC on weekdays); the peak rate is used so an estimate is never low.
	"deepseek-flash":  flatPricing(0.30, 0.006, 0, 1.20),
	"deepseek-v4-pro": flatPricing(1.32, 0.044, 0, 3.96),
	"glm-5.3":         flatPricing(1.40, 0.26, 0, 4.40),
	"kimi-k3":         flatPricing(3.00, 0.30, 0, 15.00),
	"MiniMax-M2.7":    flatPricing(0.30, 0.06, 0.375, 1.20),

	// Anthropic Claude official list pricing, USD per token. Aliases and
	// canonical names are listed together so cost estimates resolve offline.
	"opus":   claudeOpusPricing,
	"sonnet": claudeSonnetPricing,
	"fable":  claudeFablePricing,
	"haiku":  claudeHaikuPricing,

	"claude-opus-5":     claudeOpusPricing,
	"claude-opus-4-8":   claudeOpusPricing,
	"claude-opus-4-7":   claudeLegacyOpusPricing,
	"claude-opus-4-6":   claudeLegacyOpusPricing,
	"claude-opus-4-5":   claudeLegacyOpusPricing,
	"claude-opus-4":     claudeLegacyOpusPricing,
	"claude-sonnet-5":   claudeSonnetPricing,
	"claude-sonnet-4-6": claudeLegacySonnetPricing,
	// Sonnet 4.5 and 4 are 200K models: with the 1M beta, a request at or above
	// 200K reprices in full at 2x input and 1.5x output.
	"claude-sonnet-4-5": longContextPricing(3, 0.30, 3.75, 15, 200_000, 2, 1.5),
	"claude-sonnet-4":   longContextPricing(3, 0.30, 3.75, 15, 200_000, 2, 1.5),
	"claude-fable-5":    claudeFablePricing,
	"claude-haiku-4-5":  claudeHaikuPricing,

	// SambaNova reseller pricing, USD per token. Synced from
	// https://cloud.sambanova.ai/plans/pricing on 2026-05-21 and not
	// re-verified since; these keys stay prefixed so a bare model name resolves
	// to its first-party rate instead.
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

// openAIPricing builds OpenAI list pricing. A request whose input crosses the
// long-context threshold is repriced in full: 2x input, cache read and cache
// write, 1.5x output.
func openAIPricing(input, read, write, output float64) ModelPricing {
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

// longContextPricing builds pricing whose whole request reprices once the input
// reaches threshold. xAI and Google both bill this way; the multipliers differ
// per vendor, so they are passed in.
func longContextPricing(input, read, write, output float64, threshold int, inputMultiple, outputMultiple float64) ModelPricing {
	const perMillion = 1_000_000
	return ModelPricing{
		InputCostPerToken:           input / perMillion,
		OutputCostPerToken:          output / perMillion,
		CacheCreationInputTokenCost: write / perMillion,
		CacheReadInputTokenCost:     read / perMillion,
		InputCostPerTokenAbove200k:  input * inputMultiple / perMillion,
		OutputCostPerTokenAbove200k: output * outputMultiple / perMillion,
		CacheCreationCostAbove200k:  write * inputMultiple / perMillion,
		CacheReadCostAbove200k:      read * inputMultiple / perMillion,
		TieredThreshold:             threshold,
		WholeRequestTier:            true,
	}
}

// flatPricing builds pricing with no long-context tier.
func flatPricing(input, read, write, output float64) ModelPricing {
	const perMillion = 1_000_000
	return ModelPricing{
		InputCostPerToken:           input / perMillion,
		OutputCostPerToken:          output / perMillion,
		CacheCreationInputTokenCost: write / perMillion,
		CacheReadInputTokenCost:     read / perMillion,
	}
}

// anthropicPricing builds Claude list pricing from per-million USD rates.
// Anthropic has no long-context tier, so the above-200K fields stay unset.
func anthropicPricing(input, cacheWrite, cacheRead, output float64) ModelPricing {
	const perMillion = 1_000_000
	return ModelPricing{
		InputCostPerToken:           input / perMillion,
		OutputCostPerToken:          output / perMillion,
		CacheCreationInputTokenCost: cacheWrite / perMillion,
		CacheReadInputTokenCost:     cacheRead / perMillion,
	}
}

// GetPricing returns pricing for a model, fetching if necessary
func (p *PricingFetcher) GetPricing(modelName string) (ModelPricing, error) {
	if pricing, ok := p.directoryOrBundledPricing(modelName); ok {
		return pricing, nil
	}

	if err := p.ensureLoaded(); err != nil {
		return ModelPricing{}, err
	}
	return p.lookupLoadedPricing(modelName)
}

// directoryOrBundledPricing prefers the refreshed model directory over the
// bundled table: a rate card published after this binary was built is more
// likely to be right than one compiled into it. The bundled table remains the
// answer offline, and for the handful of models the directory does not price.
func (p *PricingFetcher) directoryOrBundledPricing(modelName string) (ModelPricing, bool) {
	bundled, hasBundled := lookupBundledPricing(modelName)
	pricing, ok := lookupModelsDevPricing(p.cacheDir, modelName)
	if !ok {
		return bundled, hasBundled
	}
	// A catalog entry that omits cache rates would otherwise price cache reads
	// at zero, which for an agent is most of the bill. Fill only what the
	// directory does not state.
	if hasBundled {
		if pricing.CacheReadInputTokenCost == 0 {
			pricing.CacheReadInputTokenCost = bundled.CacheReadInputTokenCost
		}
		if pricing.CacheCreationInputTokenCost == 0 {
			pricing.CacheCreationInputTokenCost = bundled.CacheCreationInputTokenCost
		}
	}
	return pricing, true
}

// pricingLookupCandidates expands a stored model name into the names a rate
// table might use: as given, without its provider prefix, and without a
// reasoning-effort suffix.
func pricingLookupCandidates(modelName string) []string {
	candidates := []string{modelName}
	trimmed := strings.TrimSpace(modelName)
	if slash := strings.LastIndex(trimmed, "/"); slash >= 0 {
		candidates = append(candidates, trimmed[slash+1:])
	}
	for _, candidate := range append([]string(nil), candidates...) {
		// Keep the former ultra alias for pricing historical persisted selections.
		for _, suffix := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
			if strings.HasSuffix(candidate, "-"+suffix) {
				candidates = append(candidates, strings.TrimSuffix(candidate, "-"+suffix))
				break
			}
		}
	}
	return candidates
}

func lookupBundledPricing(modelName string) (ModelPricing, bool) {
	candidates := pricingLookupCandidates(modelName)
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
	if pricing, ok := p.directoryOrBundledPricing(modelName); ok {
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
	// Summed counters carry no request boundaries, so no tier can be attributed
	// to them: pricing a session's whole history as one enormous request would
	// inflate every tiered model by its long-context multiple. Base rates are
	// the honest floor.
	if entry.PreAggregated {
		pricing.WholeRequestTier = false
		pricing.InputCostPerTokenAbove200k = 0
		pricing.OutputCostPerTokenAbove200k = 0
		pricing.CacheCreationCostAbove200k = 0
		pricing.CacheReadCostAbove200k = 0
	}
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
