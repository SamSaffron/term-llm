package llm

import (
	"context"

	"github.com/samsaffron/term-llm/internal/config"
)

const sambaNovaBaseURL = "https://api.sambanova.ai/v1"

type SambaNovaProvider struct {
	*OpenAICompatProvider
}

func NewSambaNovaProvider(apiKey, model string) *SambaNovaProvider {
	apiKey = config.NormalizeVeniceAPIKey(apiKey)
	if model == "" {
		model = "gpt-oss-120b"
	}
	return &SambaNovaProvider{OpenAICompatProvider: NewOpenAICompatProvider(sambaNovaBaseURL, apiKey, model, "SambaNova")}
}

func (p *SambaNovaProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}

func (p *SambaNovaProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	models, err := p.OpenAICompatProvider.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for i := range models {
		models[i].InputLimit = InputLimitForProviderModel("sambanova", models[i].ID)
	}
	return models, nil
}
