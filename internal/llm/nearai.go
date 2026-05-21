package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

const nearAIBaseURL = "https://cloud-api.near.ai/v1"

type NearAIProvider struct {
	*OpenAICompatProvider
}

func NewNearAIProvider(apiKey, model string) *NearAIProvider {
	apiKey = config.NormalizeVeniceAPIKey(apiKey)
	if model == "" {
		model = "zai-org/GLM-5.1-FP8"
	}
	return &NearAIProvider{OpenAICompatProvider: NewOpenAICompatProvider(nearAIBaseURL, apiKey, model, "NEAR AI")}
}

func (p *NearAIProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *NearAIProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	resp, err := p.makeRequest(ctx, "GET", "/model/list", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list NEAR AI models: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read NEAR AI models response: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("NEAR AI API error (status %d): %s", resp.StatusCode, string(body))
	}

	var catalog nearAIModelCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("failed to parse NEAR AI models response: %w", err)
	}

	models := make([]ModelInfo, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		if !nearAIModelIsChat(m) {
			continue
		}
		info := ModelInfo{
			ID:          m.ModelID,
			DisplayName: m.Metadata.ModelDisplayName,
			OwnedBy:     m.Metadata.OwnedBy,
			InputLimit:  m.Metadata.ContextLength,
			InputPrice:  m.InputCostPerToken.perMillionTokens(),
			OutputPrice: m.OutputCostPerToken.perMillionTokens(),
		}
		if info.InputLimit == 0 {
			info.InputLimit = InputLimitForProviderModel("nearai", info.ID)
		}
		if inputPrice, outputPrice, ok := PricingForProviderModel("nearai", info.ID); ok {
			if info.InputPrice < 0 {
				info.InputPrice = inputPrice
			}
			if info.OutputPrice < 0 {
				info.OutputPrice = outputPrice
			}
		}
		models = append(models, info)
	}
	return models, nil
}

type nearAIModelCatalog struct {
	Models []nearAIModel `json:"models"`
}

type nearAIModel struct {
	ModelID            string              `json:"modelId"`
	InputCostPerToken  *nearAITokenPrice   `json:"inputCostPerToken"`
	OutputCostPerToken *nearAITokenPrice   `json:"outputCostPerToken"`
	Metadata           nearAIModelMetadata `json:"metadata"`
}

type nearAITokenPrice struct {
	Amount   float64 `json:"amount"`
	Scale    int     `json:"scale"`
	Currency string  `json:"currency"`
}

func (p *nearAITokenPrice) perMillionTokens() float64 {
	if p == nil || (p.Amount == 0 && p.Scale == 0 && p.Currency == "") {
		return -1
	}
	if p.Currency != "" && p.Currency != "USD" {
		return -1
	}
	value := p.Amount * math.Pow10(6-p.Scale)
	return math.Round(value*1_000_000) / 1_000_000
}

type nearAIModelMetadata struct {
	ContextLength    int                     `json:"contextLength"`
	ModelDisplayName string                  `json:"modelDisplayName"`
	OwnedBy          string                  `json:"ownedBy"`
	Architecture     nearAIModelArchitecture `json:"architecture"`
}

type nearAIModelArchitecture struct {
	InputModalities  []string `json:"inputModalities"`
	OutputModalities []string `json:"outputModalities"`
}

func nearAIModelIsChat(m nearAIModel) bool {
	modelID := strings.TrimSpace(m.ModelID)
	if modelID == "" {
		return false
	}
	lowerID := strings.ToLower(modelID)
	if strings.Contains(lowerID, "privacy-filter") || strings.Contains(lowerID, "reranker") {
		return false
	}

	input := lowerStringSet(m.Metadata.Architecture.InputModalities)
	output := lowerStringSet(m.Metadata.Architecture.OutputModalities)
	if input["audio"] || output["embedding"] || output["image"] || output["score"] {
		return false
	}
	if len(output) > 0 && !output["text"] {
		return false
	}
	return true
}

func lowerStringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			out[value] = true
		}
	}
	return out
}
