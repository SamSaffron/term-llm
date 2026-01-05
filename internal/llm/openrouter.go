package llm

const openRouterBaseURL = "https://openrouter.ai/api/v1"

// NewOpenRouterProvider creates an OpenRouter provider using OpenAI-compatible APIs.
func NewOpenRouterProvider(apiKey, model, appURL, appTitle string) *OpenAICompatProvider {
	headers := map[string]string{}
	if appURL != "" {
		headers["HTTP-Referer"] = appURL
	}
	if appTitle != "" {
		headers["X-Title"] = appTitle
	}
	if len(headers) == 0 {
		headers = nil
	}
	return NewOpenAICompatProviderWithHeaders(openRouterBaseURL, apiKey, model, "OpenRouter", headers)
}
