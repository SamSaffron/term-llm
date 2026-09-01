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
	ollamaDefaultModel = config.DefaultEmbedOllamaModel
	ollamaEmbedTimeout = 2 * time.Minute
)

// OllamaProvider implements EmbeddingProvider using Ollama's native API
type OllamaProvider struct {
	baseURL string
	model   string
}

func NewOllamaProvider(baseURL string) *OllamaProvider {
	return &OllamaProvider{
		baseURL: baseURL,
		model:   ollamaDefaultModel,
	}
}

func (p *OllamaProvider) Name() string {
	return "Ollama"
}

func (p *OllamaProvider) DefaultModel() string {
	return ollamaDefaultModel
}

func (p *OllamaProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	texts := prepareOllamaTexts(model, req.TaskType, req.Texts)

	// Ollama's /api/embed endpoint supports batch input.
	ollamaReq := ollamaEmbedRequest{
		Model: model,
		Input: texts,
	}

	jsonBody, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := p.baseURL + "/api/embed"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: ollamaEmbedTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Ollama request failed (is Ollama running at %s?): %w", p.baseURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, providerhttp.NewStatusError("Ollama", resp, body)
	}

	var ollamaResp ollamaEmbedResponse
	if err := json.Unmarshal(body, &ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	result := &EmbeddingResult{
		Model:      ollamaResp.Model,
		Embeddings: make([]Embedding, len(ollamaResp.Embeddings)),
	}

	for i, vec := range ollamaResp.Embeddings {
		result.Embeddings[i] = Embedding{
			Index:  i,
			Vector: vec,
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

func prepareOllamaTexts(model, taskType string, texts []string) []string {
	if taskType != "RETRIEVAL_QUERY" || !strings.HasPrefix(strings.ToLower(model), "qwen3-embedding") {
		return texts
	}
	prepared := make([]string, len(texts))
	for i, text := range texts {
		prepared[i] = "Instruct: Given a user question, retrieve relevant passages that answer it\nQuery: " + text
	}
	return prepared
}

// Ollama API types
type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
}
