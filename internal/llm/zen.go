package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	zenBaseURL     = "https://opencode.ai/zen/v1"
	zenDisplayName = "OpenCode Zen"
	// zenCatalogProviderKey selects the Zen (Chat Completions) provider entry
	// in the shared OpenCode model catalog.
	zenCatalogProviderKey = "opencode"
)

// Overridable for tests.
var (
	modelsDevURL  = "https://models.dev/api.json"
	zenCatalogURL = opencodeGoCatalogURL
)

// ZenProvider wraps OpenAICompatProvider with models.dev pricing data.
type ZenProvider struct {
	*OpenAICompatProvider
}

// NewZenProvider creates a ZenProvider preconfigured for OpenCode Zen.
// API key is optional: empty for supported free models, or set ZEN_API_KEY for paid models.
func NewZenProvider(apiKey, model string) *ZenProvider {
	model = normalizeZenModel(model)
	return &ZenProvider{
		OpenAICompatProvider: NewOpenAICompatProvider(zenBaseURL, apiKey, model, zenDisplayName),
	}
}

func normalizeZenModel(model string) string {
	trimmed := strings.TrimSpace(model)
	switch strings.ToLower(trimmed) {
	case "bigpickle", "big_pickle", "big pickle":
		return "big-pickle"
	default:
		return trimmed
	}
}

// Stream normalizes Zen-specific model aliases before delegating to the
// OpenAI-compatible implementation.
func (p *ZenProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	req.Model = normalizeZenModel(req.Model)
	return p.OpenAICompatProvider.Stream(ctx, req)
}

// modelsDevResponse represents the models.dev API response structure.
type modelsDevResponse struct {
	OpenCode struct {
		Models map[string]modelsDevModel `json:"models"`
	} `json:"opencode"`
}

type modelsDevModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Cost *struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	} `json:"cost"`
}

// ListModels returns available models with pricing from models.dev and
// reasoning-effort metadata from the shared OpenCode catalog.
func (p *ZenProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	// Reasoning-effort metadata is fetched concurrently with the model list;
	// failures are non-fatal and simply leave efforts unset.
	effortsCh := make(chan map[string][]string, 1)
	go func() { effortsCh <- fetchZenReasoningEfforts(ctx) }()

	// Fetch models.dev data for pricing info
	pricing, err := fetchModelsDevPricing(ctx)
	if err != nil {
		// Fall back to the basic listing without replacing a previously enriched
		// cache with entries that lack pricing and reasoning metadata.
		return p.OpenAICompatProvider.ListModels(ctx)
	}

	// Fetch available models from Zen API
	resp, err := p.makeRequest(ctx, "GET", "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, newHTTPStatusError("", resp, body)
	}

	var modelsResp oaiModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("failed to parse models response: %w", err)
	}

	models := make([]ModelInfo, 0, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		info := ModelInfo{
			ID:          m.ID,
			Created:     m.Created,
			OwnedBy:     m.OwnedBy,
			InputLimit:  InputLimitForModel(m.ID),
			InputPrice:  -1, // Unknown by default
			OutputPrice: -1,
		}

		// Enrich with models.dev data
		if devModel, ok := pricing[m.ID]; ok {
			info.DisplayName = devModel.Name
			if devModel.Cost != nil {
				info.InputPrice = devModel.Cost.Input
				info.OutputPrice = devModel.Cost.Output
			}
		}

		models = append(models, info)
	}

	if efforts := <-effortsCh; len(efforts) > 0 {
		for i := range models {
			if e, ok := efforts[models[i].ID]; ok {
				models[i].ReasoningEfforts = e
			}
		}
	}

	// Sort: free models first, then by input price
	sort.Slice(models, func(i, j int) bool {
		isFreeI := models[i].InputPrice == 0 && models[i].OutputPrice == 0
		isFreeJ := models[j].InputPrice == 0 && models[j].OutputPrice == 0
		if isFreeI != isFreeJ {
			return isFreeI // Free models come first
		}
		return models[i].InputPrice < models[j].InputPrice
	})

	RefreshZenCacheSync(models)
	return models, nil
}

// fetchModelsDevPricing fetches model metadata from models.dev.
func fetchModelsDevPricing(ctx context.Context) (map[string]modelsDevModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", modelsDevURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models.dev returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data modelsDevResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	return data.OpenCode.Models, nil
}

// fetchZenReasoningEfforts returns per-model reasoning-effort levels from the
// shared OpenCode model catalog (the same source the OpenCode Go provider
// uses). Only explicit "effort" options are advertised: toggle- and
// budget-based models do not accept plain reasoning_effort strings on the Zen
// Chat Completions endpoint. Failures return nil so callers can proceed
// without effort metadata.
func fetchZenReasoningEfforts(ctx context.Context) map[string][]string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", zenCatalogURL, nil)
	if err != nil {
		return nil
	}

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil
	}
	raw, ok := catalog[zenCatalogProviderKey]
	if !ok {
		return nil
	}
	var provider opencodeGoCatalogProvider
	if err := json.Unmarshal(raw, &provider); err != nil {
		return nil
	}

	efforts := make(map[string][]string, len(provider.Models))
	for id, model := range provider.Models {
		for _, option := range model.ReasoningOptions {
			if option.Type == "effort" && len(option.Values) > 0 {
				efforts[id] = normalizeReasoningEfforts(option.Values)
				break
			}
		}
	}
	return efforts
}
