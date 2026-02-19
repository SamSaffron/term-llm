package embedding

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

const (
	openaiDefaultModel = "text-embedding-3-small"
	openaiEmbedTimeout = 2 * time.Minute
)

// OpenAIProvider implements EmbeddingProvider using OpenAI's embeddings API
type OpenAIProvider struct {
	apiKey string
	model  string
}

func NewOpenAIProvider(apiKey string) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey: apiKey,
		model:  openaiDefaultModel,
	}
}

func (p *OpenAIProvider) Name() string {
	return "OpenAI"
}

func (p *OpenAIProvider) DefaultModel() string {
	return openaiDefaultModel
}

func (p *OpenAIProvider) Embed(req EmbedRequest) (*EmbeddingResult, error) {
	client := openai.NewClient(option.WithAPIKey(p.apiKey))

	model := p.model
	if req.Model != "" {
		model = req.Model
	}

	params := openai.EmbeddingNewParams{
		Model: model,
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: req.Texts,
		},
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	}

	if req.Dimensions > 0 {
		params.Dimensions = param.NewOpt(int64(req.Dimensions))
	}

	ctx, cancel := context.WithTimeout(context.Background(), openaiEmbedTimeout)
	defer cancel()

	resp, err := client.Embeddings.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("OpenAI embedding API error: %w", err)
	}

	result := &EmbeddingResult{
		Model:      resp.Model,
		Embeddings: make([]Embedding, len(resp.Data)),
	}

	if resp.Usage.PromptTokens > 0 || resp.Usage.TotalTokens > 0 {
		result.Usage = &UsageInfo{
			PromptTokens: resp.Usage.PromptTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}

	for i, emb := range resp.Data {
		result.Embeddings[i] = Embedding{
			Index:  int(emb.Index),
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
