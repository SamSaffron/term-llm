package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const (
	geminiEmbedTimeout = 2 * time.Minute
	geminiDefaultModel = config.DefaultEmbedGeminiModel
	geminiEmbedBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"
)

// GeminiProvider implements EmbeddingProvider using Google's Gemini API.
type GeminiProvider struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

func NewGeminiProvider(apiKey string) *GeminiProvider {
	return &GeminiProvider{
		apiKey:  apiKey,
		model:   geminiDefaultModel,
		baseURL: geminiEmbedBaseURL,
		client:  newGeminiEmbedHTTPClient(http.DefaultTransport),
	}
}

func newGeminiEmbedHTTPClient(defaultTransport http.RoundTripper) *http.Client {
	var transport *http.Transport
	if base, ok := defaultTransport.(*http.Transport); ok && base != nil {
		transport = base.Clone()
	} else {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = geminiEmbedTimeout
	transport.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: transport}
}

func (p *GeminiProvider) Name() string {
	return "Gemini"
}

func (p *GeminiProvider) DefaultModel() string {
	return geminiDefaultModel
}

type geminiBatchEmbedRequest struct {
	Requests []geminiEmbedRequest `json:"requests"`
}

type geminiEmbedRequest struct {
	Model                string             `json:"model"`
	Content              geminiEmbedContent `json:"content"`
	TaskType             string             `json:"taskType,omitempty"`
	OutputDimensionality int                `json:"outputDimensionality,omitempty"`
}

type geminiEmbedContent struct {
	Role  string            `json:"role,omitempty"`
	Parts []geminiEmbedPart `json:"parts"`
}

type geminiEmbedPart struct {
	Text string `json:"text"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float64 `json:"values"`
	} `json:"embeddings"`
}

func (p *GeminiProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	ctx, cancel := context.WithTimeout(ctx, geminiEmbedTimeout)
	defer cancel()

	model := p.model
	if req.Model != "" {
		model = req.Model
	}
	model = strings.TrimPrefix(model, "models/")

	payload := geminiBatchEmbedRequest{Requests: make([]geminiEmbedRequest, len(req.Texts))}
	for i, text := range req.Texts {
		payload.Requests[i] = geminiEmbedRequest{
			Model:                "models/" + model,
			Content:              geminiEmbedContent{Role: "user", Parts: []geminiEmbedPart{{Text: text}}},
			TaskType:             req.TaskType,
			OutputDimensionality: req.Dimensions,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("Gemini embedding request error: %w", err)
	}
	endpoint := fmt.Sprintf("%s/%s:batchEmbedContents", strings.TrimRight(p.baseURL, "/"), model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("Gemini embedding request error: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Gemini embedding API error: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, providerhttp.NewStatusErrorFromResponse("Gemini embedding", resp)
	}
	defer resp.Body.Close()
	var apiResp geminiBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode Gemini embedding response: %w", err)
	}
	if len(apiResp.Embeddings) != len(req.Texts) {
		return nil, fmt.Errorf("Gemini embedding API returned %d embeddings for %d inputs", len(apiResp.Embeddings), len(req.Texts))
	}

	result := &EmbeddingResult{Model: model, Embeddings: make([]Embedding, len(apiResp.Embeddings))}
	for i, emb := range apiResp.Embeddings {
		result.Embeddings[i] = Embedding{Index: i, Vector: emb.Values, Text: req.Texts[i]}
	}
	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0].Vector)
	}
	return result, nil
}
