package embedding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	ollamaDefaultModel = "nomic-embed-text"
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

func (p *OllamaProvider) Embed(req EmbedRequest) (*EmbeddingResult, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	// Ollama's /api/embed endpoint supports batch input
	ollamaReq := ollamaEmbedRequest{
		Model: model,
		Input: req.Texts,
	}

	jsonBody, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := p.baseURL + "/api/embed"
	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
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
		return nil, fmt.Errorf("Ollama API error (status %d): %s", resp.StatusCode, string(body))
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

// Ollama API types
type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
}
