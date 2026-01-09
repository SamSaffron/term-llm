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
// For the new config format, we validate against configured providers or built-in types.
func ParseProviderModel(s string, cfg *config.Config) (string, string, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return "", "", fmt.Errorf("invalid provider format: %q", s)
	}
	provider := strings.TrimSpace(parts[0])
	model := ""
	if len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}

	// Check if provider is configured or is a built-in type
	if cfg != nil {
		if _, ok := cfg.Providers[provider]; ok {
			return provider, model, nil
		}
	}

	// Also accept built-in provider type names
	for _, name := range GetBuiltInProviderNames() {
		if provider == name {
			return provider, model, nil
		}
	}

	return "", "", fmt.Errorf("unknown provider: %s", provider)
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

// NewProviderByName creates a provider by name from the config.
// This is useful for per-command provider overrides.
func NewProviderByName(cfg *config.Config, name string) (Provider, error) {
	providerCfg, ok := cfg.Providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %q not configured", name)
	}
	provider, err := createProviderFromConfig(name, &providerCfg)
	if err != nil {
		return nil, err
	}
	return WrapWithRetry(provider, DefaultRetryConfig()), nil
}

// newProviderInternal creates the underlying provider without retry wrapper.
func newProviderInternal(cfg *config.Config) (Provider, error) {
	providerCfg, ok := cfg.Providers[cfg.DefaultProvider]
	if !ok {
		return nil, fmt.Errorf("provider %q not configured", cfg.DefaultProvider)
	}
	return createProviderFromConfig(cfg.DefaultProvider, &providerCfg)
}

// createProviderFromConfig creates a provider from a ProviderConfig.
func createProviderFromConfig(name string, cfg *config.ProviderConfig) (Provider, error) {
	// Resolve lazy config values (op://, srv://, $()) before creating provider
	if err := cfg.ResolveForInference(); err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}

	providerType := config.InferProviderType(name, cfg.Type)

	switch providerType {
	case config.ProviderTypeAnthropic:
		return NewAnthropicProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeOpenAI:
		// Use CodexProvider when using Codex OAuth credentials (has account ID)
		if cfg.AccountID != "" {
			return NewCodexProvider(cfg.ResolvedAPIKey, cfg.Model, cfg.AccountID), nil
		}
		return NewOpenAIProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeOpenRouter:
		return NewOpenRouterProvider(cfg.ResolvedAPIKey, cfg.Model, cfg.AppURL, cfg.AppTitle), nil

	case config.ProviderTypeGemini:
		// Use CodeAssistProvider when using gemini-cli OAuth credentials
		if cfg.Credentials == "gemini-cli" && cfg.OAuthCreds != nil {
			return NewCodeAssistProvider(cfg.OAuthCreds, cfg.Model), nil
		}
		return NewGeminiProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeZen:
		return NewZenProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeOpenAICompat:
		// Use ResolvedURL if available (from srv:// or $() resolution), otherwise use config values
		baseURL := cfg.BaseURL
		chatURL := cfg.URL
		if cfg.ResolvedURL != "" {
			chatURL = cfg.ResolvedURL
		}
		if baseURL == "" && chatURL == "" {
			return nil, fmt.Errorf("provider %q requires base_url or url", name)
		}
		// Use provider name as display name, with first letter capitalized
		displayName := strings.ToUpper(name[:1]) + name[1:]
		return NewOpenAICompatProviderFull(baseURL, chatURL, cfg.ResolvedAPIKey, cfg.Model, displayName, nil), nil

	default:
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
}
