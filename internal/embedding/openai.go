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
	"github.com/samsaffron/term-llm/internal/openaiapi"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const openaiEmbedTimeout = 2 * time.Minute

const openaiDefaultModel = config.DefaultEmbedOpenAIModel

// OpenAIProvider implements EmbeddingProvider using OpenAI's embeddings API
type OpenAIProvider struct {
	apiKey   string
	model    string
	endpoint openaiapi.Endpoint
	client   *http.Client
}

// NewOpenAIProvider uses the OPENAI_BASE_URL/OPENAI_ORG_ID/OPENAI_PROJECT_ID
// environment, falling back to the public API root.
func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return NewOpenAIProviderWithEndpoint(apiKey, openaiapi.Resolve(openaiapi.Endpoint{}))
}

// NewOpenAIProviderWithEndpoint uses an already-resolved endpoint.
func NewOpenAIProviderWithEndpoint(apiKey string, endpoint openaiapi.Endpoint) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey:   apiKey,
		model:    openaiDefaultModel,
		endpoint: endpoint,
		client:   &http.Client{Timeout: openaiEmbedTimeout},
	}
}

func (p *OpenAIProvider) Name() string {
	return "OpenAI"
}

func (p *OpenAIProvider) DefaultModel() string {
	return openaiDefaultModel
}

func (p *OpenAIProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbeddingResult, error) {
	model := p.model
	if req.Model != "" {
		model = req.Model
	}
	ctx, cancel := context.WithTimeout(ctx, openaiEmbedTimeout)
	defer cancel()
	return embedOpenAICompatible(ctx, openAICompatibleEmbedCall{
		label:   "OpenAI",
		client:  p.client,
		baseURL: p.endpoint.URL(""),
		apiKey:  p.apiKey,
		headers: p.endpoint.HeaderMap(),
		texts:   req.Texts,
		payload: openAICompatibleEmbedRequest{
			Model:          model,
			Input:          req.Texts,
			EncodingFormat: "float",
			Dimensions:     req.Dimensions,
		},
	})
}

// openAICompatibleEmbedCall describes one POST to an OpenAI-compatible
// /embeddings endpoint. texts are the caller's original inputs, recorded on
// each returned embedding; payload.Input may be a provider-prepared variant.
type openAICompatibleEmbedCall struct {
	label   string
	client  *http.Client
	baseURL string
	apiKey  string
	headers map[string]string
	texts   []string
	payload openAICompatibleEmbedRequest
}

type openAICompatibleEmbedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions,omitempty"`
}

type openAICompatibleEmbedResponse struct {
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

func embedOpenAICompatible(ctx context.Context, call openAICompatibleEmbedCall) (*EmbeddingResult, error) {
	body, err := json.Marshal(call.payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s embedding request: %w", call.label, err)
	}
	endpoint := strings.TrimRight(call.baseURL, "/") + "/embeddings"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create %s embedding request: %w", call.label, err)
	}
	for key, value := range call.headers {
		httpReq.Header.Set(key, value)
	}
	httpReq.Header.Set("Authorization", "Bearer "+call.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := call.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s embedding request: %w", call.label, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s embedding response: %w", call.label, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, providerhttp.NewStatusError(call.label+" embedding", resp, responseBody)
	}
	var apiResp openAICompatibleEmbedResponse
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode %s embedding response: %w", call.label, err)
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
		if i < len(call.texts) {
			result.Embeddings[i].Text = call.texts[i]
		}
	}
	if len(result.Embeddings) > 0 {
		result.Dimensions = len(result.Embeddings[0].Vector)
	}
	return result, nil
}
