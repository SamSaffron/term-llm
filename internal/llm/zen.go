package llm

const (
	zenBaseURL     = "https://opencode.ai/zen/v1"
	zenDisplayName = "OpenCode Zen"
)

// NewZenProvider creates an OpenAICompatProvider preconfigured for OpenCode Zen.
// Zen provides free access to models like GLM 4.7 via opencode.ai.
// API key is optional: empty for free tier, or set ZEN_API_KEY for paid models.
func NewZenProvider(apiKey, model string) *OpenAICompatProvider {
	return NewOpenAICompatProvider(zenBaseURL, apiKey, model, zenDisplayName)
}
