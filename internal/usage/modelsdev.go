package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// modelsDevURL is the OpenCode model directory: a community-maintained catalog
// of every model's rates, refreshed far more often than a release of this
// program. It is the source of truth when it is available; the bundled table
// stays as the offline answer.
var modelsDevURL = "https://models.dev/api.json"

const (
	// The published catalog is several megabytes, so it is fetched rarely and
	// only the price fields are kept.
	modelsDevTTL       = 12 * time.Hour
	modelsDevCacheFile = "models-dev-pricing.json"
	// modelsDevCacheVersion invalidates projections written by an older build.
	// A projection is a derived artifact: if the rules that built it change, the
	// file is garbage rather than merely stale.
	modelsDevCacheVersion = 2
)

// modelsDevCost is one model's rates in USD per million tokens.
type modelsDevCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	// Tiers reprice a whole request once its context reaches the tier size.
	Tiers []struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
		Tier       struct {
			Size int    `json:"size"`
			Type string `json:"type"`
		} `json:"tier"`
	} `json:"tiers"`
}

// modelsDevCacheFileFormat is the derived projection as stored on disk.
type modelsDevCacheFileFormat struct {
	Version int                     `json:"version"`
	Entries map[string]ModelPricing `json:"entries"`
}

type modelsDevCatalog struct {
	Models map[string]struct {
		Cost *modelsDevCost `json:"cost"`
	} `json:"models"`
}

// pricing converts a catalog entry, reporting whether it carries any rate at
// all. A model listed with no cost (free tiers, subscription-only entries) must
// not be reported as costing zero.
func (c modelsDevCost) pricing() (ModelPricing, bool) {
	const perMillion = 1_000_000
	if c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0 {
		return ModelPricing{}, false
	}
	pricing := ModelPricing{
		InputCostPerToken:           c.Input / perMillion,
		OutputCostPerToken:          c.Output / perMillion,
		CacheReadInputTokenCost:     c.CacheRead / perMillion,
		CacheCreationInputTokenCost: c.CacheWrite / perMillion,
	}
	for _, tier := range c.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
			continue
		}
		pricing.InputCostPerTokenAbove200k = tier.Input / perMillion
		pricing.OutputCostPerTokenAbove200k = tier.Output / perMillion
		pricing.CacheReadCostAbove200k = tier.CacheRead / perMillion
		pricing.CacheCreationCostAbove200k = tier.CacheWrite / perMillion
		pricing.TieredThreshold = tier.Tier.Size
		pricing.WholeRequestTier = true
		break
	}
	return pricing, true
}

// modelsDevPricing is the derived catalog: model name to rates, keyed both bare
// and provider-qualified. Only this projection is cached on disk, so a refresh
// costs one parse and every later lookup reads kilobytes rather than megabytes.
type modelsDevPricing struct {
	mu sync.RWMutex
	// dir records which cache the entries came from. Two fetchers may point at
	// different directories — tests always do — and one must never answer with
	// the other's catalog.
	dir     string
	loaded  bool
	entries map[string]ModelPricing
}

var modelsDevStore = &modelsDevPricing{}

// lookupModelsDevPricing resolves a model against the cached catalog. It never
// fetches: cost rendering runs on the display path and must not block on the
// network.
func lookupModelsDevPricing(cacheDir, modelName string) (ModelPricing, bool) {
	if !modelsDevStore.ensureLoaded(cacheDir) {
		return ModelPricing{}, false
	}
	modelsDevStore.mu.RLock()
	defer modelsDevStore.mu.RUnlock()
	for _, candidate := range pricingLookupCandidates(modelName) {
		if pricing, ok := modelsDevStore.entries[candidate]; ok {
			return pricing, true
		}
	}
	return ModelPricing{}, false
}

func (s *modelsDevPricing) ensureLoaded(cacheDir string) bool {
	s.mu.RLock()
	// Evaluate under the read lock: replace() writes entries under the write
	// lock, so reading the map header outside it is a race.
	if s.loaded && s.dir == cacheDir {
		available := len(s.entries) > 0
		s.mu.RUnlock()
		return available
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded && s.dir == cacheDir {
		return len(s.entries) > 0
	}
	s.loaded, s.dir, s.entries = true, cacheDir, nil
	data, err := os.ReadFile(filepath.Join(cacheDir, modelsDevCacheFile))
	if err != nil {
		return false
	}
	var cached modelsDevCacheFileFormat
	if json.Unmarshal(data, &cached) != nil || cached.Version != modelsDevCacheVersion {
		return false
	}
	s.entries = cached.Entries
	return len(s.entries) > 0
}

func (s *modelsDevPricing) replace(cacheDir string, entries map[string]ModelPricing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries, s.loaded, s.dir = entries, true, cacheDir
}

// RefreshDirectoryInBackground refreshes the cached model directory without
// blocking the caller. Prices are only ever read from the cache, so a program
// that starts, runs and exits before the fetch lands simply uses the previous
// catalog or the bundled table.
func RefreshDirectoryInBackground(ctx context.Context) {
	// A program told to stay offline prices from the bundled table only. Tests
	// set this too: a background fetch that lands mid-run would swap the rates
	// their assertions are written against.
	if strings.TrimSpace(os.Getenv("TERM_LLM_OFFLINE")) != "" {
		return
	}
	go func() {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_ = NewPricingFetcher().RefreshModelsDevPricing(refreshCtx)
	}()
}

// RefreshModelsDevPricing updates the cached model directory when the copy on
// disk is older than the refresh interval. It is safe to call on a background
// goroutine at startup; callers that want the bundled table only can skip it.
func (p *PricingFetcher) RefreshModelsDevPricing(ctx context.Context) error {
	if p == nil || p.httpClient == nil {
		return nil
	}
	cacheFile := filepath.Join(p.cacheDir, modelsDevCacheFile)
	if info, err := os.Stat(cacheFile); err == nil && time.Since(info.ModTime()) < modelsDevTTL {
		return nil
	}
	entries, err := fetchModelsDevPricing(ctx, p.httpClient)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("models.dev catalog carried no priced models")
	}
	modelsDevStore.replace(p.cacheDir, entries)
	data, err := json.Marshal(modelsDevCacheFileFormat{Version: modelsDevCacheVersion, Entries: entries})
	if err != nil {
		return err
	}
	// Write through a temp file: a reader that catches a truncated write would
	// otherwise see the catalog as unavailable. A failed write only costs the
	// next process a refresh.
	temp := cacheFile + ".tmp"
	if os.WriteFile(temp, data, 0o644) == nil && os.Rename(temp, cacheFile) != nil {
		_ = os.Remove(temp)
	}
	return nil
}

func fetchModelsDevPricing(ctx context.Context, client *http.Client) (map[string]ModelPricing, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev catalog: unexpected status %d", response.StatusCode)
	}
	var catalog map[string]modelsDevCatalog
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	return projectModelsDevPricing(catalog), nil
}

// firstPartyProviders claim a bare model name ahead of the dozens of resellers
// that relist the same models. A reseller's rate is a real price, but it is not
// the price of "opus" — and resellers frequently publish only input and output,
// which would silently drop cache rates that dominate an agent's bill.
var firstPartyProviders = []string{
	"anthropic", "openai", "google", "google-vertex", "xai", "deepseek", "zai", "z-ai",
	"moonshotai", "minimax", "mistral", "meta", "amazon-bedrock", "azure", "cohere",
	"opencode",
}

func providerRank(provider string) int {
	for rank, known := range firstPartyProviders {
		if strings.EqualFold(provider, known) {
			return rank
		}
	}
	return len(firstPartyProviders)
}

// projectModelsDevPricing flattens the catalog. A bare model name is claimed by
// the highest-ranked provider that lists it; the qualified "provider/model" key
// always resolves exactly.
func projectModelsDevPricing(catalog map[string]modelsDevCatalog) map[string]ModelPricing {
	entries := make(map[string]ModelPricing, 512)
	providers := make([]string, 0, len(catalog))
	for provider := range catalog {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	sortByRank(providers)
	for _, provider := range providers {
		for model, entry := range catalog[provider].Models {
			if entry.Cost == nil {
				continue
			}
			pricing, ok := entry.Cost.pricing()
			if !ok {
				continue
			}
			entries[provider+"/"+model] = pricing
			if _, exists := entries[model]; !exists {
				entries[model] = pricing
			}
		}
	}
	return entries
}

// sortByRank is stable, so providers of equal rank keep their alphabetical
// order and the projection is deterministic.
func sortByRank(providers []string) {
	sort.SliceStable(providers, func(i, j int) bool {
		return providerRank(providers[i]) < providerRank(providers[j])
	})
}
