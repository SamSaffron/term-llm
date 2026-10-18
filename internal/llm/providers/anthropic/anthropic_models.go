package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// anthropicModelCacheKey stores the live public-API catalog. Custom base URLs
// (gateways) are never cached under it because their catalogs differ.
const anthropicModelCacheKey = "anthropic"

// anthropicInputReserve is subtracted from a model's context window to leave
// room for output, matching the curated 1M→980K / 200K→180K budgets.
const anthropicInputReserve = 20_000

// anthropicEffortOrder is the canonical display order for effort levels.
var anthropicEffortOrder = []string{"low", "medium", "high", "xhigh", "max"}

// ListModels returns available models from Anthropic. Results from the public
// API refresh the shared cache used by completion and model pickers.
func (p *AnthropicProvider) ListModels(ctx context.Context) ([]core.ModelInfo, error) {
	records, err := p.client.listModels(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]core.ModelInfo, 0, len(records))
	for _, record := range records {
		models = append(models, anthropicModelInfo(record))
	}
	if p.client.usesPublicAPI() {
		RefreshAnthropicCacheSync(models)
	}
	return models, nil
}

func anthropicModelInfo(record anthropicModelRecord) core.ModelInfo {
	info := core.ModelInfo{
		ID:               record.ID,
		DisplayName:      record.DisplayName,
		Created:          record.CreatedAt.Unix(),
		InputLimit:       inputLimitForModel(record.ID),
		OutputLimit:      record.MaxTokens,
		ReasoningEfforts: anthropicSupportedEfforts(record.Capabilities.Effort),
		ReasoningModes:   anthropicSupportedThinkingTypes(record),
	}
	if record.MaxInputTokens > anthropicInputReserve*2 {
		info.InputLimit = record.MaxInputTokens - anthropicInputReserve
	}
	if info.OutputLimit == 0 {
		info.OutputLimit = outputLimitForModel(record.ID)
	}
	return info
}

func anthropicSupportedEfforts(effort map[string]json.RawMessage) []string {
	var supported bool
	if raw, ok := effort["supported"]; !ok || json.Unmarshal(raw, &supported) != nil || !supported {
		return nil
	}
	var efforts []string
	for _, level := range anthropicEffortOrder {
		var support anthropicSupport
		if raw, ok := effort[level]; ok && json.Unmarshal(raw, &support) == nil && support.Supported {
			efforts = append(efforts, level)
		}
	}
	return efforts
}

func anthropicSupportedThinkingTypes(record anthropicModelRecord) []string {
	thinking := record.Capabilities.Thinking
	if !thinking.Supported {
		return nil
	}
	var types []string
	for _, kind := range []string{"adaptive", "enabled"} {
		if thinking.Types[kind].Supported {
			types = append(types, kind)
		}
	}
	return types
}

// RefreshAnthropicCacheSync stores freshly fetched public-API model metadata.
func RefreshAnthropicCacheSync(models []core.ModelInfo) {
	if len(models) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(anthropicModelCacheKey, modelInfosToCache(models))
}

// RefreshAnthropicModelsIfStale refreshes the public-API catalog cache when it
// is missing or stale. Custom base URLs are skipped. A failed refresh leaves
// stale entries (or the curated fallback) available.
func RefreshAnthropicModelsIfStale(ctx context.Context, providerCfg config.ProviderConfig) error {
	if !AnthropicUsesPublicCatalog(providerCfg) || AnthropicModelCacheFresh() {
		return nil
	}
	provider, err := NewAnthropicProviderWithOptions(providerCfg.ResolvedAPIKey, providerCfg.Model, providerCfg.Credentials, AnthropicProviderOptions{Headers: providerCfg.Headers})
	if err != nil {
		return err
	}
	if !provider.client.usesPublicAPI() {
		return nil
	}
	_, err = provider.ListModels(ctx)
	return err
}

// AnthropicUsesPublicCatalog reports whether providerCfg talks to Anthropic's
// public API (no base_url and no ANTHROPIC_BASE_URL override), the only
// endpoint whose catalog is cached and refreshed for completion.
func AnthropicUsesPublicCatalog(providerCfg config.ProviderConfig) bool {
	return strings.TrimSpace(providerCfg.BaseURL) == "" && strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")) == ""
}

// AnthropicModelCacheFresh reports whether the public-API catalog cache exists
// and is within its TTL.
func AnthropicModelCacheFresh() bool {
	cached, err := cache.ReadModelCache(anthropicModelCacheKey)
	return err == nil && cache.IsCacheValid(cached) && len(cached.ModelInfos) > 0
}

// anthropicRefreshAttemptKey records the last completion-driven refresh
// attempt so a slow or failing refresh is not retried on every keystroke.
const anthropicRefreshAttemptKey = "anthropic-refresh-attempt"

// anthropicRefreshRetryInterval spaces out completion-driven refresh attempts.
const anthropicRefreshRetryInterval = 2 * time.Minute

// ClaimAnthropicCatalogRefresh reports whether a completion-driven refresh may
// start now and, if so, records the attempt. It returns false while a recent
// attempt (successful or not) is within the retry interval.
func ClaimAnthropicCatalogRefresh() bool {
	if cached, err := cache.ReadModelCache(anthropicRefreshAttemptKey); err == nil && time.Since(cached.FetchedAt) < anthropicRefreshRetryInterval {
		return false
	}
	_ = cache.WriteModelCache(anthropicRefreshAttemptKey, nil)
	return true
}

// GetCachedAnthropicModelInfos returns the last live public-API catalog without
// network access. Stale entries remain useful for completion.
func GetCachedAnthropicModelInfos() []core.ModelInfo {
	cached, err := cache.ReadModelCache(anthropicModelCacheKey)
	if err != nil || cached == nil || len(cached.ModelInfos) == 0 {
		return nil
	}
	return modelInfosFromCache(cached)
}

// GetCachedAnthropicModels returns cached live model IDs, or nil.
func GetCachedAnthropicModels() []string {
	infos := GetCachedAnthropicModelInfos()
	ids := make([]string, 0, len(infos))
	for _, info := range infos {
		if id := strings.TrimSpace(info.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// anthropicDatedSuffix matches snapshot suffixes such as -20251001.
var anthropicDatedSuffix = regexp.MustCompile(`-\d{8}$`)

// anthropicBedrockQualified matches Bedrock model IDs such as
// us.anthropic.claude-haiku-4-5-20251001-v1:0 or anthropic.claude-sonnet-4-6.
var anthropicBedrockQualified = regexp.MustCompile(`^(?:[a-z]+\.)?anthropic\.(claude-.+?)(?:-v\d+(?::\d+)?)?$`)

// anthropicModelAlias reduces a model ID to its undated Claude name so
// claude-haiku-4-5, claude-haiku-4-5-20251001, and the Bedrock ID
// us.anthropic.claude-haiku-4-5-20251001-v1:0 refer to the same model.
func anthropicModelAlias(model string) string {
	// The -1m context suffix is a term-llm alias, never part of a catalog ID;
	// strip it before the Bedrock version and snapshot date
	// (us.anthropic.claude-sonnet-4-5-20250929-v1:0-1m).
	model = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(model)), "-1m")
	if m := anthropicBedrockQualified.FindStringSubmatch(model); m != nil {
		model = m[1]
	}
	return anthropicDatedSuffix.ReplaceAllString(model, "")
}

// anthropicCachedModelInfo finds model in the live cache, accepting either the
// dated snapshot ID or its undated alias.
func anthropicCachedModelInfo(model string) (core.ModelInfo, bool) {
	return findAnthropicModelInfo(GetCachedAnthropicModelInfos(), model)
}

// anthropicCachedModelInfoForRequest also accepts an effort-suffixed request
// model (claude-opus-4-7-high) by retrying without the suffix.
func anthropicCachedModelInfoForRequest(model string) (core.ModelInfo, bool) {
	infos := GetCachedAnthropicModelInfos()
	if info, ok := findAnthropicModelInfo(infos, model); ok {
		return info, true
	}
	if base, ok := trimKnownEffortSuffix(strings.ToLower(model)); ok {
		return findAnthropicModelInfo(infos, base)
	}
	return core.ModelInfo{}, false
}

// anthropicClampOutputTokens caps requested output at the live catalog's
// max_tokens when known, falling back to the curated tables.
func anthropicClampOutputTokens(requested int, model string) int {
	if info, ok := anthropicCachedModelInfoForRequest(model); ok && info.OutputLimit > 0 {
		if requested > info.OutputLimit {
			return info.OutputLimit
		}
		return requested
	}
	return clampOutputTokens(requested, model)
}

func findAnthropicModelInfo(infos []core.ModelInfo, model string) (core.ModelInfo, bool) {
	alias := anthropicModelAlias(model)
	if alias == "" {
		return core.ModelInfo{}, false
	}
	for _, info := range infos {
		if strings.EqualFold(info.ID, model) {
			return info, true
		}
	}
	// Several dated snapshots can share an alias; prefer the newest.
	var best core.ModelInfo
	found := false
	for _, info := range infos {
		if anthropicModelAlias(info.ID) == alias && (!found || info.ID > best.ID) {
			best, found = info, true
		}
	}
	return best, found
}

// anthropicReasoningEfforts returns the effort levels for a direct Anthropic
// model: live catalog first, then the curated fallback list, then a
// generation heuristic for models newer than both.
func anthropicReasoningEfforts(model string) []string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "arn:") {
		return nil // inference profiles hide the model; assume no effort control
	}
	if info, ok := anthropicCachedModelInfoForRequest(model); ok {
		return cloneEfforts(info.ReasoningEfforts)
	}
	if entry, ok := anthropicStaticEntry(model); ok {
		return cloneEfforts(entry.ReasoningEfforts)
	}
	if anthropicIsLegacyModel(model) {
		return nil
	}
	return cloneEfforts(anthropicFullEffortVariants)
}

func anthropicStaticEntry(model string) (CuratedModel, bool) {
	if ParentHooks.Curated == nil {
		return CuratedModel{}, false
	}
	alias := anthropicModelAlias(model)
	for _, entry := range ParentHooks.Curated() {
		if anthropicModelAlias(entry.ID) == alias {
			return entry, true
		}
	}
	return CuratedModel{}, false
}

// anthropicLegacyModels predate adaptive thinking and per-request effort
// beyond what the curated list records.
var anthropicLegacyModels = map[string]bool{
	"claude-haiku-4-5": true, "claude-sonnet-4-5": true, "claude-opus-4-5": true,
	"claude-opus-4-1": true, "claude-sonnet-4": true, "claude-opus-4": true, "claude-haiku-4": true,
}

func anthropicIsLegacyModel(model string) bool {
	alias := anthropicModelAlias(model)
	return anthropicLegacyModels[alias] || strings.HasPrefix(alias, "claude-3")
}

// anthropicSupportsAdaptiveThinking reports whether -thinking should use
// adaptive thinking (rather than a token budget) for model.
func anthropicSupportsAdaptiveThinking(model string) bool {
	// Bedrock inference-profile ARNs hide the underlying model; keep the
	// conservative budget-based thinking they have always used.
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "arn:") {
		return false
	}
	if info, ok := anthropicCachedModelInfoForRequest(model); ok {
		// A catalog hit decides, including an explicit "no thinking".
		return slices.Contains(info.ReasoningModes, "adaptive")
	}
	return !anthropicIsLegacyModel(model)
}

var anthropicFullEffortVariants = []string{"low", "medium", "high", "xhigh", "max"}
