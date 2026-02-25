package llm

import (
	"fmt"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
)

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

	// Allow hidden debug provider (not in built-in list)
	if provider == "debug" {
		return provider, model, nil
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

// NewProviderByName creates a provider by name from the config, with an optional model override.
// This is useful for per-command provider overrides.
// If the provider is a built-in type but not explicitly configured,
// it will be created with default settings.
func NewProviderByName(cfg *config.Config, name string, model string) (Provider, error) {
	// Handle hidden debug provider first
	if name == "debug" {
		provider := NewDebugProvider(model)
		return WrapWithRetry(provider, DefaultRetryConfig()), nil
	}

	providerCfg, ok := cfg.Providers[name]
	if !ok {
		// Check if it's a built-in provider type that can work without config
		providerType := config.InferProviderType(name, "")
		switch providerType {
		case config.ProviderTypeAnthropic:
			// anthropic uses API key, env var, or OAuth token with interactive setup
			provider, err := NewAnthropicProvider("", model, "")
			if err != nil {
				return nil, fmt.Errorf("provider anthropic: %w", err)
			}
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeClaudeBin:
			// claude-bin doesn't need API key, can create directly
			provider := NewClaudeBinProvider(model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeZen:
			// zen can work without API key (free tier)
			provider := NewZenProvider("", model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeXAI:
			// xai can use XAI_API_KEY env var
			apiKey := os.Getenv("XAI_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires XAI_API_KEY environment variable or explicit config", name)
			}
			provider := NewXAIProvider(apiKey, model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeVenice:
			apiKey := os.Getenv("VENICE_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires VENICE_API_KEY or explicit config", name)
			}
			provider := NewVeniceProvider(apiKey, model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeGemini:
			// gemini can use GEMINI_API_KEY env var
			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires GEMINI_API_KEY environment variable or explicit config", name)
			}
			provider := NewGeminiProvider(apiKey, model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeChatGPT:
			// chatgpt uses native OAuth with interactive authentication
			provider, err := NewChatGPTProvider(model)
			if err != nil {
				return nil, fmt.Errorf("provider chatgpt: %w", err)
			}
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeCopilot:
			// copilot uses GitHub device code OAuth with interactive authentication
			provider, err := NewCopilotProvider(model)
			if err != nil {
				return nil, fmt.Errorf("provider copilot: %w", err)
			}
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeGeminiCLI:
			// gemini-cli uses OAuth credentials from ~/.gemini/oauth_creds.json
			creds, err := credentials.GetGeminiOAuthCredentials()
			if err != nil {
				return nil, fmt.Errorf("provider gemini-cli: %w", err)
			}
			provider := NewGeminiCLIProvider(creds, model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		default:
			return nil, fmt.Errorf("provider %q not configured", name)
		}
	}

	// Apply model override if provided
	if model != "" {
		providerCfg.Model = model
	}

	provider, err := createProviderFromConfig(name, &providerCfg)
	if err != nil {
		return nil, err
	}
	return WrapWithRetry(provider, DefaultRetryConfig()), nil
}

// newProviderInternal creates the underlying provider without retry wrapper.
func newProviderInternal(cfg *config.Config) (Provider, error) {
	// Handle hidden debug provider first
	if cfg.DefaultProvider == "debug" {
		return NewDebugProvider(""), nil
	}

	providerCfg, ok := cfg.Providers[cfg.DefaultProvider]
	if !ok {
		// Check if it's a built-in provider type that can work without config
		providerType := config.InferProviderType(cfg.DefaultProvider, "")
		switch providerType {
		case config.ProviderTypeAnthropic:
			// anthropic uses API key, env var, or OAuth token with interactive setup
			return NewAnthropicProvider("", "", "")
		case config.ProviderTypeClaudeBin:
			// claude-bin doesn't need API key, can create directly
			return NewClaudeBinProvider(""), nil
		case config.ProviderTypeZen:
			// zen can work without API key (free tier)
			return NewZenProvider("", ""), nil
		case config.ProviderTypeXAI:
			// xai can use XAI_API_KEY env var
			apiKey := os.Getenv("XAI_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires XAI_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewXAIProvider(apiKey, ""), nil
		case config.ProviderTypeVenice:
			apiKey := os.Getenv("VENICE_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires VENICE_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewVeniceProvider(apiKey, ""), nil
		case config.ProviderTypeChatGPT:
			// chatgpt uses native OAuth with interactive authentication
			return NewChatGPTProvider("")
		case config.ProviderTypeCopilot:
			// copilot uses GitHub device code OAuth with interactive authentication
			return NewCopilotProvider("")
		case config.ProviderTypeGemini:
			// gemini can use GEMINI_API_KEY env var
			apiKey := os.Getenv("GEMINI_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires GEMINI_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewGeminiProvider(apiKey, ""), nil
		case config.ProviderTypeGeminiCLI:
			// gemini-cli uses OAuth credentials from ~/.gemini/oauth_creds.json
			creds, err := credentials.GetGeminiOAuthCredentials()
			if err != nil {
				return nil, fmt.Errorf("provider gemini-cli: %w", err)
			}
			return NewGeminiCLIProvider(creds, ""), nil
		default:
			return nil, fmt.Errorf("provider %q not configured", cfg.DefaultProvider)
		}
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
		return NewAnthropicProvider(cfg.ResolvedAPIKey, cfg.Model, cfg.Credentials)

	case config.ProviderTypeOpenAI:
		return NewOpenAIProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeChatGPT:
		// ChatGPT uses native OAuth with interactive authentication
		return NewChatGPTProvider(cfg.Model)

	case config.ProviderTypeCopilot:
		// Copilot uses GitHub device code OAuth with interactive authentication
		return NewCopilotProvider(cfg.Model)

	case config.ProviderTypeOpenRouter:
		return NewOpenRouterProvider(cfg.ResolvedAPIKey, cfg.Model, cfg.AppURL, cfg.AppTitle), nil

	case config.ProviderTypeGemini:
		return NewGeminiProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeGeminiCLI:
		// Fetch credentials from ~/.gemini/oauth_creds.json if not explicitly configured
		oauthCreds := cfg.OAuthCreds
		if oauthCreds == nil {
			creds, err := credentials.GetGeminiOAuthCredentials()
			if err != nil {
				return nil, fmt.Errorf("gemini-cli: %w", err)
			}
			oauthCreds = creds
		}
		return NewGeminiCLIProvider(oauthCreds, cfg.Model), nil

	case config.ProviderTypeZen:
		return NewZenProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeXAI:
		apiKey := cfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("XAI_API_KEY")
		}
		return NewXAIProvider(apiKey, cfg.Model), nil

	case config.ProviderTypeVenice:
		apiKey := cfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("VENICE_API_KEY")
		}
		return NewVeniceProvider(apiKey, cfg.Model), nil

	case config.ProviderTypeClaudeBin:
		return NewClaudeBinProvider(cfg.Model), nil

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
