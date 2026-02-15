package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const (
	zenBaseURL     = "https://opencode.ai/zen/v1"
	zenDisplayName = "OpenCode Zen"
	modelsDevURL   = "https://models.dev/api.json"
)

// ZenProvider wraps OpenAICompatProvider with models.dev pricing data.
type ZenProvider struct {
	*OpenAICompatProvider
}

// NewZenProvider creates a ZenProvider preconfigured for OpenCode Zen.
// Zen provides free access to models like GLM 4.7 via opencode.ai.
// API key is optional: empty for free tier, or set ZEN_API_KEY for paid models.
func NewZenProvider(apiKey, model string) *ZenProvider {
	return &ZenProvider{
		OpenAICompatProvider: NewOpenAICompatProvider(zenBaseURL, apiKey, model, zenDisplayName),
	}
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

// ListModels returns available models with pricing from models.dev.
func (p *ZenProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	// Fetch models.dev data for pricing info
	pricing, err := fetchModelsDevPricing(ctx)
	if err != nil {
		// Fall back to basic listing if models.dev fails
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
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
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

	// Sort: free models first, then by input price
	sort.Slice(models, func(i, j int) bool {
		isFreeI := models[i].InputPrice == 0 && models[i].OutputPrice == 0
		isFreeJ := models[j].InputPrice == 0 && models[j].OutputPrice == 0
		if isFreeI != isFreeJ {
			return isFreeI // Free models come first
		}
		return models[i].InputPrice < models[j].InputPrice
	})

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
