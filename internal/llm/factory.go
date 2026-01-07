package llm

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// IsCodexModel returns true if the model name indicates a Codex model.
func IsCodexModel(model string) bool {
	model = strings.ToLower(model)
	return strings.Contains(model, "codex")
}

// ParseProviderModel parses "provider:model" or just "provider" from a flag value.
// Returns (provider, model, error). Model will be empty if not specified.
func ParseProviderModel(s string) (string, string, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return "", "", fmt.Errorf("invalid provider format: %q", s)
	}
	provider := strings.TrimSpace(parts[0])
	model := ""
	if len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}
	valid := false
	for _, name := range GetProviderNames() {
		if provider == name {
			valid = true
			break
		}
	}
	if !valid {
		return "", "", fmt.Errorf("unknown provider: %s", provider)
	}
	return provider, model, nil
}

// NewProvider creates a new LLM provider based on the config.
// Providers are wrapped with automatic retry for rate limits (429) and transient errors.
func NewProvider(cfg *config.Config) (Provider, error) {
	provider, err := newProviderInternal(cfg)
	if err != nil {
		return nil, err
	}
	// Wrap with retry logic (enabled by default)
	return WrapWithRetry(provider, DefaultRetryConfig()), nil
}

// newProviderInternal creates the underlying provider without retry wrapper.
func newProviderInternal(cfg *config.Config) (Provider, error) {
	switch cfg.Provider {
	case "anthropic":
		return NewAnthropicProvider(cfg.Anthropic.APIKey, cfg.Anthropic.Model), nil
	case "openai":
		// Use CodexProvider when using Codex OAuth credentials (has account ID)
		if cfg.OpenAI.AccountID != "" {
			return NewCodexProvider(cfg.OpenAI.APIKey, cfg.OpenAI.Model, cfg.OpenAI.AccountID), nil
		}
		return NewOpenAIProvider(cfg.OpenAI.APIKey, cfg.OpenAI.Model), nil
	case "openrouter":
		return NewOpenRouterProvider(cfg.OpenRouter.APIKey, cfg.OpenRouter.Model, cfg.OpenRouter.AppURL, cfg.OpenRouter.AppTitle), nil
	case "gemini":
		// Use CodeAssistProvider when using gemini-cli OAuth credentials
		if cfg.Gemini.Credentials == "gemini-cli" && cfg.Gemini.OAuthCreds != nil {
			return NewCodeAssistProvider(cfg.Gemini.OAuthCreds, cfg.Gemini.Model), nil
		}
		return NewGeminiProvider(cfg.Gemini.APIKey, cfg.Gemini.Model), nil
	case "zen":
		return NewZenProvider(cfg.Zen.APIKey, cfg.Zen.Model), nil
	case "ollama":
		baseURL := cfg.Ollama.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		return NewOpenAICompatProvider(baseURL, cfg.Ollama.APIKey, cfg.Ollama.Model, "Ollama"), nil
	case "lmstudio":
		baseURL := cfg.LMStudio.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:1234/v1"
		}
		return NewOpenAICompatProvider(baseURL, cfg.LMStudio.APIKey, cfg.LMStudio.Model, "LM Studio"), nil
	case "openai-compat":
		if cfg.OpenAICompat.BaseURL == "" {
			return nil, fmt.Errorf("openai-compat requires base_url")
		}
		return NewOpenAICompatProvider(cfg.OpenAICompat.BaseURL, cfg.OpenAICompat.APIKey, cfg.OpenAICompat.Model, "OpenAI-Compatible"), nil
	default:
		return nil, fmt.Errorf("unknown provider: %s", cfg.Provider)
	}
}
