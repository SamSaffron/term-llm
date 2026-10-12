package embedding

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
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
	return embedOpenAICompatible(ctx, openAICompatibleEmbedCall{
		label:   "Venice",
		client:  p.client,
		baseURL: p.baseURL,
		apiKey:  p.apiKey,
		texts:   req.Texts,
		payload: openAICompatibleEmbedRequest{
			Model:          model,
			Input:          prepareQwen3EmbeddingTexts(model, req.TaskType, req.Texts),
			EncodingFormat: "float",
			Dimensions:     req.Dimensions,
		},
	})
}
