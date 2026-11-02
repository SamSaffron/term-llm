package opencompat

import (
	"github.com/samsaffron/term-llm/internal/config"
)

const cheaperInferenceBaseURL = "https://api.cheaperinference.com/v1"

// CheaperInferenceProvider talks to Cheaper Inference's OpenAI-compatible
// gateway. Model IDs are bare (for example gpt-5.4-mini), and the /models
// listing needs the API key.
type CheaperInferenceProvider struct {
	*OpenAICompatProvider
}

func NewCheaperInferenceProvider(apiKey, model string) *CheaperInferenceProvider {
	apiKey = config.NormalizeVeniceAPIKey(apiKey)
	if model == "" {
		model = config.DefaultProviderModel("cheaperinference")
	}
	return &CheaperInferenceProvider{OpenAICompatProvider: NewOpenAICompatProvider(cheaperInferenceBaseURL, apiKey, model, "Cheaper Inference")}
}

func (p *CheaperInferenceProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}
