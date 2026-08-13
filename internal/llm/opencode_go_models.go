package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/samsaffron/term-llm/internal/cache"
	"golang.org/x/sync/singleflight"
)

const (
	opencodeGoBaseURL     = "https://opencode.ai/zen/go/v1"
	opencodeGoCatalogURL  = "https://models.opencode.ai/api.json"
	opencodeGoDisplayName = "OpenCode Go"
	// v2 invalidates early catalog caches that predated protocol and
	// budget-derived reasoning metadata.
	opencodeGoModelCacheKey      = "opencode-go-v2"
	opencodeGoCatalogProviderKey = "opencode-go"
	opencodeGoModelCacheTTL      = 5 * time.Minute
	opencodeGoRefreshBackoff     = 30 * time.Second
	opencodeGoRefreshTimeout     = 10 * time.Second
	opencodeGoOutputTokenMax     = 32_000
)

type opencodeGoProtocol string

const (
	opencodeGoProtocolChat      opencodeGoProtocol = "chat"
	opencodeGoProtocolMessages  opencodeGoProtocol = "messages"
	opencodeGoProtocolResponses opencodeGoProtocol = "responses"
)

type opencodeGoModel struct {
	ModelInfo
	OutputLimit      int
	Protocol         opencodeGoProtocol
	ReasoningBudgets map[string]int64
	Deprecated       bool
	Available        bool
}

type opencodeGoModelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

type opencodeGoCatalogProvider struct {
	NPM    string                            `json:"npm"`
	Models map[string]opencodeGoCatalogModel `json:"models"`
}

type opencodeGoCatalogModel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Cost   *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
	Limit struct {
		Context int `json:"context"`
		Input   int `json:"input"`
		Output  int `json:"output"`
	} `json:"limit"`
	Provider struct {
		NPM      string `json:"npm"`
		Endpoint string `json:"endpoint"`
	} `json:"provider"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
		Min    int      `json:"min"`
		Max    int      `json:"max"`
	} `json:"reasoning_options"`
}

type opencodeGoModelCatalog struct {
	mu          sync.RWMutex
	cacheKey    string
	models      map[string]opencodeGoModel
	fetchedAt   time.Time
	lastAttempt time.Time
	lastErr     error
	refresh     singleflight.Group
}

type opencodeGoFetchResult struct {
	models           []opencodeGoModel
	metadataComplete bool
}

type opencodeGoLoadResult struct {
	models []opencodeGoModel
	fresh  bool
}

func (c *opencodeGoModelCatalog) load(ctx context.Context, client *http.Client, apiKey, baseURL, catalogURL string) ([]opencodeGoModel, error) {
	result, err := c.loadWithFreshness(ctx, client, apiKey, baseURL, catalogURL)
	return result.models, err
}

func (c *opencodeGoModelCatalog) loadWithFreshness(ctx context.Context, client *http.Client, apiKey, baseURL, catalogURL string) (opencodeGoLoadResult, error) {
	if models, ok := c.freshSnapshot(); ok {
		return opencodeGoLoadResult{models: models, fresh: true}, nil
	}
	if cached, err := readCachedOpenCodeGoModels(c.cacheKey); err == nil && len(cached.models) > 0 && time.Since(cached.fetchedAt) < opencodeGoModelCacheTTL {
		c.store(cached.models, cached.fetchedAt, time.Time{}, nil)
		return opencodeGoLoadResult{models: sortedOpenCodeGoModels(cached.models), fresh: true}, nil
	}
	if models, err, ok := c.recentAttemptResult(); ok {
		return opencodeGoLoadResult{models: models}, err
	}

	resultCh := c.refresh.DoChan("refresh", func() (any, error) {
		// Another caller may have populated memory or disk while this call waited
		// for the singleflight slot.
		if models, ok := c.freshSnapshot(); ok {
			return opencodeGoLoadResult{models: models, fresh: true}, nil
		}
		if cached, err := readCachedOpenCodeGoModels(c.cacheKey); err == nil && len(cached.models) > 0 && time.Since(cached.fetchedAt) < opencodeGoModelCacheTTL {
			c.store(cached.models, cached.fetchedAt, time.Time{}, nil)
			return opencodeGoLoadResult{models: sortedOpenCodeGoModels(cached.models), fresh: true}, nil
		}
		if models, err, ok := c.recentAttemptResult(); ok {
			return opencodeGoLoadResult{models: models}, err
		}

		staleModels := c.snapshot()
		if len(staleModels) == 0 {
			if cached, err := readCachedOpenCodeGoModels(c.cacheKey); err == nil && len(cached.models) > 0 {
				staleModels = cached.models
			}
		}

		// A refresh should not be canceled just because the first coalesced caller
		// goes away. Every waiter still observes its own context below.
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opencodeGoRefreshTimeout)
		defer cancel()
		fetched, err := fetchOpenCodeGoModels(refreshCtx, client, apiKey, baseURL, catalogURL)
		now := time.Now()
		if err != nil {
			if len(staleModels) > 0 {
				c.store(staleModels, time.Time{}, now, nil)
				return opencodeGoLoadResult{models: sortedOpenCodeGoModels(staleModels)}, nil
			}
			c.store(nil, time.Time{}, now, err)
			return nil, err
		}
		if !fetched.metadataComplete {
			models := openCodeGoModelMap(fetched.models)
			if len(staleModels) > 0 {
				models = staleModels
			}
			// Availability is enough for a safe Chat Completions fallback. Retain it
			// briefly so sequential inference requests do not refetch on every turn.
			c.store(models, time.Time{}, now, nil)
			return opencodeGoLoadResult{models: sortedOpenCodeGoModels(models)}, nil
		}

		modelMap := openCodeGoModelMap(fetched.models)
		c.store(modelMap, now, now, nil)
		_ = writeCachedOpenCodeGoModels(c.cacheKey, fetched.models)
		return opencodeGoLoadResult{models: fetched.models, fresh: true}, nil
	})

	select {
	case <-ctx.Done():
		return opencodeGoLoadResult{}, ctx.Err()
	case result := <-resultCh:
		if result.Err != nil {
			return opencodeGoLoadResult{}, result.Err
		}
		loaded, _ := result.Val.(opencodeGoLoadResult)
		return loaded, nil
	}
}

func (c *opencodeGoModelCatalog) freshSnapshot() ([]opencodeGoModel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.models) == 0 || c.fetchedAt.IsZero() || time.Since(c.fetchedAt) >= opencodeGoModelCacheTTL {
		return nil, false
	}
	return sortedOpenCodeGoModels(c.models), true
}

func (c *opencodeGoModelCatalog) snapshot() map[string]opencodeGoModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.models) == 0 {
		return nil
	}
	out := make(map[string]opencodeGoModel, len(c.models))
	for id, model := range c.models {
		out[id] = cloneOpenCodeGoModel(model)
	}
	return out
}

func (c *opencodeGoModelCatalog) recentAttemptResult() ([]opencodeGoModel, error, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastAttempt.IsZero() || time.Since(c.lastAttempt) >= opencodeGoRefreshBackoff {
		return nil, nil, false
	}
	return sortedOpenCodeGoModels(c.models), c.lastErr, true
}

func (c *opencodeGoModelCatalog) store(models map[string]opencodeGoModel, fetchedAt, lastAttempt time.Time, lastErr error) {
	c.mu.Lock()
	c.models = cloneOpenCodeGoModelMap(models)
	c.fetchedAt = fetchedAt
	c.lastAttempt = lastAttempt
	c.lastErr = lastErr
	c.mu.Unlock()
}

func (c *opencodeGoModelCatalog) model(ctx context.Context, client *http.Client, apiKey, baseURL, catalogURL, id string) (opencodeGoModel, error) {
	models, err := c.load(ctx, client, apiKey, baseURL, catalogURL)
	if err != nil {
		return opencodeGoModel{}, err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	for _, model := range models {
		if strings.EqualFold(model.ID, id) {
			return model, nil
		}
	}
	for _, model := range models {
		base := strings.ToLower(model.ID)
		for _, effort := range model.ReasoningEfforts {
			if id == base+"-"+strings.ToLower(strings.TrimSpace(effort)) {
				return model, nil
			}
		}
	}
	return unknownOpenCodeGoModel(id), nil
}

func unknownOpenCodeGoModel(id string) opencodeGoModel {
	// The availability endpoint can lead the metadata catalog. Its provider-level
	// default is OpenAI-compatible chat, which the Go gateway translates to the
	// selected upstream protocol.
	return opencodeGoModel{ModelInfo: ModelInfo{ID: strings.TrimSpace(id), InputPrice: -1, OutputPrice: -1}, Protocol: opencodeGoProtocolChat}
}

func fetchOpenCodeGoModels(ctx context.Context, client *http.Client, apiKey, baseURL, catalogURL string) (opencodeGoFetchResult, error) {
	if client == nil {
		client = defaultHTTPClient
	}
	type result struct {
		body []byte
		err  error
	}
	fetch := func(url, authHeader string, ch chan<- result) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			ch <- result{err: err}
			return
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := client.Do(req)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		if readErr == nil && resp.StatusCode != http.StatusOK {
			readErr = newHTTPStatusError("", resp, body)
		}
		ch <- result{body: body, err: readErr}
	}

	liveAuth := ""
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		liveAuth = "Bearer " + apiKey
	}
	liveCh, catalogCh := make(chan result, 1), make(chan result, 1)
	go fetch(strings.TrimRight(baseURL, "/")+"/models", liveAuth, liveCh)
	// Never forward the subscription credential to the separate catalog origin.
	go fetch(catalogURL, "", catalogCh)
	liveResult, catalogResult := <-liveCh, <-catalogCh
	if liveResult.err != nil {
		return opencodeGoFetchResult{}, fmt.Errorf("list OpenCode Go models: %w", liveResult.err)
	}
	var live opencodeGoModelsResponse
	if err := json.Unmarshal(liveResult.body, &live); err != nil {
		return opencodeGoFetchResult{}, fmt.Errorf("decode OpenCode Go models: %w", err)
	}

	provider, metadataComplete := decodeOpenCodeGoCatalog(catalogResult)
	models := mergeOpenCodeGoModels(live, provider)
	return opencodeGoFetchResult{models: models, metadataComplete: metadataComplete}, nil
}

func decodeOpenCodeGoCatalog(result struct {
	body []byte
	err  error
}) (opencodeGoCatalogProvider, bool) {
	if result.err != nil {
		slog.Debug("OpenCode Go metadata catalog unavailable; using availability-only models", "error", result.err)
		return opencodeGoCatalogProvider{}, false
	}
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(result.body, &catalog); err != nil {
		slog.Debug("OpenCode Go metadata catalog is invalid; using availability-only models", "error", err)
		return opencodeGoCatalogProvider{}, false
	}
	rawProvider, ok := catalog[opencodeGoCatalogProviderKey]
	if !ok {
		slog.Debug("OpenCode Go metadata provider is missing; using availability-only models", "provider", opencodeGoCatalogProviderKey)
		return opencodeGoCatalogProvider{}, false
	}
	var provider opencodeGoCatalogProvider
	if err := json.Unmarshal(rawProvider, &provider); err != nil {
		slog.Debug("OpenCode Go provider metadata is invalid; using availability-only models", "error", err)
		return opencodeGoCatalogProvider{}, false
	}
	if len(provider.Models) == 0 {
		slog.Debug("OpenCode Go provider metadata has no models; using availability-only models", "provider", opencodeGoCatalogProviderKey)
		return opencodeGoCatalogProvider{}, false
	}
	return provider, true
}

func mergeOpenCodeGoModels(live opencodeGoModelsResponse, provider opencodeGoCatalogProvider) []opencodeGoModel {
	models := make([]opencodeGoModel, 0, len(live.Data))
	for _, item := range live.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		model := unknownOpenCodeGoModel(item.ID)
		model.Available = true
		model.Created = item.Created
		model.OwnedBy = item.OwnedBy
		if metadata, ok := provider.Models[item.ID]; ok {
			model.DisplayName = metadata.Name
			model.InputLimit = effectiveOpenCodeGoInputLimit(metadata.Limit.Context, metadata.Limit.Input, metadata.Limit.Output)
			model.OutputLimit = metadata.Limit.Output
			if metadata.Cost != nil {
				model.InputPrice = metadata.Cost.Input
				model.OutputPrice = metadata.Cost.Output
			}
			model.Deprecated = strings.EqualFold(metadata.Status, "deprecated")
			model.Protocol = openCodeGoProtocolForMetadata(firstNonEmpty(metadata.Provider.Endpoint, metadata.Provider.NPM, provider.NPM))
			model.ReasoningEfforts, model.ReasoningBudgets = openCodeGoReasoningMetadata(metadata)
		}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func openCodeGoReasoningMetadata(metadata opencodeGoCatalogModel) ([]string, map[string]int64) {
	for _, option := range metadata.ReasoningOptions {
		if option.Type == "effort" {
			return normalizeReasoningEfforts(option.Values), nil
		}
	}
	for _, option := range metadata.ReasoningOptions {
		if option.Type != "budget_tokens" {
			continue
		}
		maximum := option.Max
		if maximum <= 0 || maximum >= opencodeGoOutputTokenMax {
			maximum = opencodeGoOutputTokenMax - 1
		}
		if metadata.Limit.Output > 0 && maximum >= metadata.Limit.Output {
			maximum = metadata.Limit.Output - 1
		}
		if maximum <= 0 {
			return nil, nil
		}
		high := (maximum + 1) / 2
		if option.Min > high {
			high = option.Min
		}
		if high > maximum {
			high = maximum
		}
		return []string{"high", "max"}, map[string]int64{"high": int64(high), "max": int64(maximum)}
	}
	return nil, nil
}

func effectiveOpenCodeGoInputLimit(contextLimit, inputLimit, outputLimit int) int {
	if inputLimit > 0 {
		return inputLimit
	}
	if contextLimit <= 0 {
		return 0
	}
	if outputLimit > 0 && outputLimit < contextLimit {
		return contextLimit - outputLimit
	}
	return contextLimit
}

func openCodeGoProtocolForMetadata(value string) opencodeGoProtocol {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "responses", value == "@ai-sdk/openai", strings.HasSuffix(value, "/responses"):
		return opencodeGoProtocolResponses
	case value == "messages", value == "@ai-sdk/anthropic", strings.HasSuffix(value, "/messages"):
		return opencodeGoProtocolMessages
	default:
		return opencodeGoProtocolChat
	}
}

func openCodeGoModelMap(models []opencodeGoModel) map[string]opencodeGoModel {
	out := make(map[string]opencodeGoModel, len(models))
	for _, model := range models {
		out[strings.ToLower(model.ID)] = model
	}
	return out
}

func cloneOpenCodeGoModel(model opencodeGoModel) opencodeGoModel {
	model.ReasoningEfforts = append([]string(nil), model.ReasoningEfforts...)
	model.ReasoningBudgets = cloneOpenCodeGoReasoningBudgets(model.ReasoningBudgets)
	return model
}

func cloneOpenCodeGoModelMap(models map[string]opencodeGoModel) map[string]opencodeGoModel {
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]opencodeGoModel, len(models))
	for id, model := range models {
		out[id] = cloneOpenCodeGoModel(model)
	}
	return out
}

func sortedOpenCodeGoModels(models map[string]opencodeGoModel) []opencodeGoModel {
	out := make([]opencodeGoModel, 0, len(models))
	for _, model := range models {
		out = append(out, cloneOpenCodeGoModel(model))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type cachedOpenCodeGoCatalog struct {
	models    map[string]opencodeGoModel
	fetchedAt time.Time
}

func readCachedOpenCodeGoModels(cacheKey string) (cachedOpenCodeGoCatalog, error) {
	entry, err := cache.ReadModelCache(cacheKey)
	if err != nil {
		return cachedOpenCodeGoCatalog{}, err
	}
	models := make(map[string]opencodeGoModel, len(entry.ModelInfos))
	for _, item := range entry.ModelInfos {
		model := opencodeGoModel{
			ModelInfo:        ModelInfo{ID: item.ID, DisplayName: item.DisplayName, Created: item.Created, OwnedBy: item.OwnedBy, InputLimit: item.InputLimit, InputPrice: item.InputPrice, OutputPrice: item.OutputPrice, ReasoningEfforts: append([]string(nil), item.ReasoningEfforts...)},
			OutputLimit:      item.OutputLimit,
			Protocol:         opencodeGoProtocol(item.Protocol),
			ReasoningBudgets: cloneOpenCodeGoReasoningBudgets(item.ReasoningBudgets),
			Deprecated:       item.Deprecated,
			Available:        true,
		}
		if model.Protocol == "" {
			model.Protocol = opencodeGoProtocolChat
		}
		models[strings.ToLower(model.ID)] = model
	}
	return cachedOpenCodeGoCatalog{models: models, fetchedAt: entry.FetchedAt}, nil
}

func writeCachedOpenCodeGoModels(cacheKey string, models []opencodeGoModel) error {
	items := make([]cache.CachedModel, 0, len(models))
	for _, model := range models {
		items = append(items, cache.CachedModel{ID: model.ID, DisplayName: model.DisplayName, Created: model.Created, OwnedBy: model.OwnedBy, InputLimit: model.InputLimit, OutputLimit: model.OutputLimit, InputPrice: model.InputPrice, OutputPrice: model.OutputPrice, ReasoningEfforts: append([]string(nil), model.ReasoningEfforts...), ReasoningBudgets: cloneOpenCodeGoReasoningBudgets(model.ReasoningBudgets), Protocol: string(model.Protocol), Deprecated: model.Deprecated})
	}
	return cache.WriteModelInfoCache(cacheKey, items)
}

func cloneOpenCodeGoReasoningBudgets(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for effort, budget := range in {
		out[effort] = budget
	}
	return out
}

func cachedOpenCodeGoModelMap() map[string]opencodeGoModel {
	catalog := latestOpenCodeGoCatalog()
	if catalog == nil {
		return nil
	}
	if models := catalog.snapshot(); len(models) > 0 {
		return models
	}
	cached, err := readCachedOpenCodeGoModels(catalog.cacheKey)
	if err != nil {
		return nil
	}
	return cached.models
}

func opencodeGoCachedInputLimit(model string) int {
	models := cachedOpenCodeGoModelMap()
	model = strings.ToLower(strings.TrimSpace(model))
	entry, found := models[model]
	if !found {
		if base, hasSuffix := trimKnownEffortSuffix(model); hasSuffix {
			entry, found = models[base]
		}
	}
	if !found {
		return 0
	}
	return entry.InputLimit
}

func opencodeGoCachedReasoningEfforts(model string) []string {
	models := cachedOpenCodeGoModelMap()
	model = strings.ToLower(strings.TrimSpace(model))
	if entry, found := models[model]; found {
		return cloneEfforts(entry.ReasoningEfforts)
	}
	for _, entry := range models {
		base := strings.ToLower(strings.TrimSpace(entry.ID))
		for _, effort := range entry.ReasoningEfforts {
			if model == base+"-"+strings.ToLower(strings.TrimSpace(effort)) {
				return cloneEfforts(entry.ReasoningEfforts)
			}
		}
	}
	return nil
}

// CachedOpenCodeGoModels returns the last complete OpenCode Go model metadata
// without performing network access. The freshness result uses the provider's
// short catalog TTL so callers can show cached capabilities immediately while
// scheduling a background refresh when needed.
func CachedOpenCodeGoModels() ([]ModelInfo, bool, error) {
	catalog := latestOpenCodeGoCatalog()
	if catalog == nil {
		return nil, false, fmt.Errorf("cached OpenCode Go model metadata is unavailable")
	}
	cached, err := readCachedOpenCodeGoModels(catalog.cacheKey)
	if err != nil {
		return nil, false, err
	}
	models := sortedOpenCodeGoModels(cached.models)
	if len(models) == 0 {
		return nil, false, fmt.Errorf("cached OpenCode Go model metadata is empty")
	}
	infos := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if !model.Deprecated && model.ID != "" {
			infos = append(infos, model.ModelInfo)
		}
	}
	if len(infos) == 0 {
		return nil, false, fmt.Errorf("cached OpenCode Go model metadata has no active models")
	}
	fresh := !cached.fetchedAt.IsZero() && time.Since(cached.fetchedAt) < opencodeGoModelCacheTTL
	return infos, fresh, nil
}

// GetCachedOpenCodeGoModels returns active model IDs from the last successful
// merged OpenCode Go catalog refresh without performing network access.
func GetCachedOpenCodeGoModels() []string {
	return activeOpenCodeGoModelIDs(cachedOpenCodeGoModelMap())
}

// GetCachedOpenCodeGoModelsForAPIKey returns active model IDs from the on-disk
// OpenCode Go catalog cache for the given API key. Unlike GetCachedOpenCodeGoModels,
// it does not require an in-memory catalog instance, so shell completion (which
// runs in a fresh process where no provider has been constructed) can read the
// cache directly.
func GetCachedOpenCodeGoModelsForAPIKey(apiKey string) []string {
	cacheKey := openCodeGoCatalogScope(apiKey, opencodeGoBaseURL)
	cached, err := readCachedOpenCodeGoModels(cacheKey)
	if err != nil || len(cached.models) == 0 {
		return nil
	}
	return activeOpenCodeGoModelIDs(cached.models)
}

func activeOpenCodeGoModelIDs(models map[string]opencodeGoModel) []string {
	sorted := sortedOpenCodeGoModels(models)
	ids := make([]string, 0, len(sorted))
	for _, model := range sorted {
		if !model.Deprecated && model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids
}

func newOpenCodeGoAnthropicProvider(apiKey, baseURL, model string, client *http.Client) *AnthropicProvider {
	// anthropic-sdk-go appends /v1/messages itself. OpenCode publishes a base URL
	// ending in /v1, so pass the parent to avoid /v1/v1/messages.
	anthropicBaseURL := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	opts := []option.RequestOption{option.WithAPIKey(apiKey), option.WithBaseURL(anthropicBaseURL)}
	if client != nil {
		opts = append(opts, option.WithHTTPClient(client))
	}
	anthropicClient := anthropic.NewClient(opts...)
	return &AnthropicProvider{client: &anthropicClient, model: model, credential: "api_key"}
}
