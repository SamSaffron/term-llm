package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	voyageEndpoint     = "https://api.voyageai.com/v1/embeddings"
	voyageDefaultModel = config.DefaultEmbedVoyageModel
	voyageEmbedTimeout = 2 * time.Minute
)

// VoyageProvider implements EmbeddingProvider using Voyage AI's embeddings API.
// Voyage AI (acquired by MongoDB, Feb 2025) is Anthropic's recommended
// embedding provider. The API remains fully available at api.voyageai.com.
type VoyageProvider struct {
	apiKey string
	model  string
}

func NewVoyageProvider(apiKey string) *VoyageProvider {
	return &VoyageProvider{
		apiKey: apiKey,
		model:  voyageDefaultModel,
	}
}

func (p *VoyageProvider) Name() string {
	return "Voyage"
}

func (p *VoyageProvider) DefaultModel() string {
	return voyageDefaultModel
}

func (p *VoyageProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	voyageReq := voyageEmbedRequest{
		Model: model,
		Input: req.Texts,
	}

	if req.Dimensions > 0 {
		voyageReq.OutputDimension = &req.Dimensions
	}

	// Map generic task types to Voyage's input_type parameter.
	// Voyage supports: "query", "document", or null.
	if req.TaskType != "" {
		voyageReq.InputType = mapVoyageInputType(req.TaskType)
	}

	jsonBody, err := json.Marshal(voyageReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, voyageEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := &http.Client{Timeout: voyageEmbedTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Voyage request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, providerhttp.NewStatusError("Voyage", resp, body)
	}

	var voyageResp voyageEmbedResponse
	if err := json.Unmarshal(body, &voyageResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	result := &EmbeddingResult{
		Model:      voyageResp.Model,
		Embeddings: make([]Embedding, len(voyageResp.Data)),
	}

	if voyageResp.Usage.TotalTokens > 0 {
		result.Usage = &UsageInfo{
			TotalTokens: int64(voyageResp.Usage.TotalTokens),
		}
	}

	for i, emb := range voyageResp.Data {
		result.Embeddings[i] = Embedding{
			Index:  emb.Index,
			Vector: emb.Embedding,
		}
		if i < len(req.Texts) {
			result.Embeddings[i].Text = req.Texts[i]
		}
	}

	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0].Vector)
	}

	return result, nil
}

// mapVoyageInputType maps generic/Gemini-style task types to Voyage's input_type.
// Voyage supports: "query", "document", or empty (null).
func mapVoyageInputType(taskType string) string {
	switch taskType {
	case "RETRIEVAL_QUERY", "query":
		return "query"
	case "RETRIEVAL_DOCUMENT", "document":
		return "document"
	default:
		// Voyage only supports query/document, so pass-through for those
		// and ignore other task types
		return ""
	}
}

// Voyage API types (OpenAI-compatible format)
type voyageEmbedRequest struct {
	Model           string   `json:"model"`
	Input           []string `json:"input"`
	InputType       string   `json:"input_type,omitempty"`
	OutputDimension *int     `json:"output_dimension,omitempty"`
}

type voyageEmbedResponse struct {
	Model string            `json:"model"`
	Data  []voyageEmbedding `json:"data"`
	Usage voyageUsage       `json:"usage"`
}

type voyageEmbedding struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type voyageUsage struct {
	TotalTokens int `json:"total_tokens"`
}
