package opencompat

import (
	"github.com/samsaffron/term-llm/internal/config"
)

const obscurifyBaseURL = "https://obscurify.ai/v1"

// ObscurifyProvider talks to Obscurify.ai's OpenAI-compatible API. Its public
// /models listing carries context_length and per-token pricing, which the
// shared OpenAI-compatible ListModels already parses.
type ObscurifyProvider struct {
	*OpenAICompatProvider
}

func NewObscurifyProvider(apiKey, model string) *ObscurifyProvider {
	apiKey = config.NormalizeVeniceAPIKey(apiKey)
	if model == "" {
		model = config.DefaultProviderModel("obscurify")
	}
	return &ObscurifyProvider{OpenAICompatProvider: NewOpenAICompatProvider(obscurifyBaseURL, apiKey, model, "Obscurify.ai")}
}

func (p *ObscurifyProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch:    false,
		NativeWebFetch:     false,
		ToolCalls:          true,
		SupportsToolChoice: true,
	}
}
