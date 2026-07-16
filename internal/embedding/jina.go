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
	jinaEndpoint     = "https://api.jina.ai/v1/embeddings"
	jinaDefaultModel = config.DefaultEmbedJinaModel
	jinaEmbedTimeout = 2 * time.Minute
)

// JinaProvider implements EmbeddingProvider using Jina AI's embeddings API.
// Jina uses an OpenAI-compatible format at api.jina.ai/v1/embeddings and
// offers a free tier with 10M tokens (no credit card required).
type JinaProvider struct {
	apiKey string
	model  string
}

func NewJinaProvider(apiKey string) *JinaProvider {
	return &JinaProvider{
		apiKey: apiKey,
		model:  jinaDefaultModel,
	}
}

func (p *JinaProvider) Name() string {
	return "Jina"
}

func (p *JinaProvider) DefaultModel() string {
	return jinaDefaultModel
}

func (p *JinaProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	jinaReq := jinaEmbedRequest{
		Model:      model,
		Input:      req.Texts,
		Normalized: true,
	}

	if req.Dimensions > 0 {
		jinaReq.Dimensions = &req.Dimensions
	}

	// Map generic task types to Jina's task parameter
	if req.TaskType != "" {
		jinaReq.Task = mapTaskType(req.TaskType)
	}

	jsonBody, err := json.Marshal(jinaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, jinaEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	client := &http.Client{Timeout: jinaEmbedTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Jina request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, providerhttp.NewStatusError("Jina", resp, body)
	}

	var jinaResp jinaEmbedResponse
	if err := json.Unmarshal(body, &jinaResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	result := &EmbeddingResult{
		Model:      jinaResp.Model,
		Embeddings: make([]Embedding, len(jinaResp.Data)),
	}

	if jinaResp.Usage.TotalTokens > 0 || jinaResp.Usage.PromptTokens > 0 {
		result.Usage = &UsageInfo{
			PromptTokens: int64(jinaResp.Usage.PromptTokens),
			TotalTokens:  int64(jinaResp.Usage.TotalTokens),
		}
	}

	for i, emb := range jinaResp.Data {
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

// mapTaskType maps generic/Gemini-style task types to Jina's task parameter.
// Jina v3 supports: retrieval.query, retrieval.passage, text-matching,
// classification, separation.
func mapTaskType(taskType string) string {
	switch taskType {
	case "RETRIEVAL_QUERY", "retrieval.query":
		return "retrieval.query"
	case "RETRIEVAL_DOCUMENT", "retrieval.passage":
		return "retrieval.passage"
	case "SEMANTIC_SIMILARITY", "text-matching":
		return "text-matching"
	case "CLASSIFICATION", "classification":
		return "classification"
	case "CLUSTERING", "separation":
		return "separation"
	default:
		// Pass through unknown values as-is
		return taskType
	}
}

// Jina API types (OpenAI-compatible format)
type jinaEmbedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Normalized bool     `json:"normalized,omitempty"`
	Task       string   `json:"task,omitempty"`
	Dimensions *int     `json:"dimensions,omitempty"`
}

type jinaEmbedResponse struct {
	Model string          `json:"model"`
	Data  []jinaEmbedding `json:"data"`
	Usage jinaUsage       `json:"usage"`
}

type jinaEmbedding struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type jinaUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
