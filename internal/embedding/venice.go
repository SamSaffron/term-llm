package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	veniceEmbedBaseURL = "https://api.venice.ai/api/v1"
	veniceEmbedTimeout = 2 * time.Minute
)

// VeniceProvider implements Venice's OpenAI-compatible embeddings endpoint.
type VeniceProvider struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

func NewVeniceProvider(apiKey, baseURL string) *VeniceProvider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = veniceEmbedBaseURL
	}
	return &VeniceProvider{
		apiKey:  apiKey,
		model:   config.DefaultEmbedVeniceModel,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: veniceEmbedTimeout},
	}
}

func (p *VeniceProvider) Name() string {
	return "Venice"
}

func (p *VeniceProvider) DefaultModel() string {
	return config.DefaultEmbedVeniceModel
}

func (p *VeniceProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}
	payload := veniceEmbedRequest{
		Model:          model,
		Input:          prepareQwen3EmbeddingTexts(model, req.TaskType, req.Texts),
		EncodingFormat: "float",
		Dimensions:     req.Dimensions,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal Venice embedding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Venice embedding request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Venice embedding request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Venice embedding response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, providerhttp.NewStatusError("Venice embedding", resp, responseBody)
	}
	var apiResp veniceEmbedResponse
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode Venice embedding response: %w", err)
	}
	result := &EmbeddingResult{
		Model:      apiResp.Model,
		Embeddings: make([]Embedding, len(apiResp.Data)),
	}
	if apiResp.Usage.PromptTokens > 0 || apiResp.Usage.TotalTokens > 0 {
		result.Usage = &UsageInfo{PromptTokens: apiResp.Usage.PromptTokens, TotalTokens: apiResp.Usage.TotalTokens}
	}
	for i, item := range apiResp.Data {
		result.Embeddings[i] = Embedding{Index: item.Index, Vector: item.Embedding}
		if i < len(req.Texts) {
			result.Embeddings[i].Text = req.Texts[i]
		}
	}
	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0].Vector)
	}
	return result, nil
}

type veniceEmbedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

type veniceEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}
