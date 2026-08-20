package llm

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

func removedProviderError(name string) error {
	if name == "gemini-cli" {
		return fmt.Errorf("provider %q is no longer supported; use provider %q with GEMINI_API_KEY", name, "gemini")
	}
	return nil
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
	if err := removedProviderError(provider); err != nil {
		return "", "", err
	}
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
		// Exact built-in names win over effort-prefix interpretation. This avoids
		// a configured provider such as "claude" shadowing "claude-bin".
		for _, name := range GetBuiltInProviderNames() {
			if provider == name {
				return provider, model, nil
			}
		}
		if len(parts) == 1 {
			if baseProvider, baseModel, effort, configured, supported := parseConfiguredProviderEffort(provider, cfg); configured {
				if !supported {
					return "", "", fmt.Errorf("provider %q does not support reasoning effort %q", baseProvider, effort)
				}
				return baseProvider, baseModel + "-" + effort, nil
			}
			baseProvider, effort := ParseModelEffort(provider)
			if effort != "" {
				if providerCfg, ok := cfg.Providers[baseProvider]; ok {
					baseModel, _ := ParseModelEffort(strings.TrimSpace(providerCfg.Model))
					if baseModel == "" {
						return "", "", fmt.Errorf("provider %q has no configured model for effort suffix %q", baseProvider, effort)
					}
					return baseProvider, baseModel + "-" + effort, nil
				}
			}
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

// parseConfiguredProviderEffort resolves an effort-suffixed configured provider
// name using the selected model's explicit capability metadata. The configured
// result distinguishes an unsupported declared suffix from an unrelated name so
// callers can reject hidden aliases instead of falling back to global defaults.
func parseConfiguredProviderEffort(value string, cfg *config.Config) (provider, model, effort string, configured, supported bool) {
	if cfg == nil {
		return "", "", "", false, false
	}
	var providerNames []string
	for name := range cfg.Providers {
		if strings.HasPrefix(value, name+"-") {
			providerNames = append(providerNames, name)
		}
	}
	// Prefer the longest configured provider name when names share a prefix.
	sort.Slice(providerNames, func(i, j int) bool { return len(providerNames[i]) > len(providerNames[j]) })
	for _, name := range providerNames {
		pc := cfg.Providers[name]
		entry, ok := config.ModelConfigForProviderModel(cfg, name, pc.Model)
		if !ok || len(entry.ReasoningEfforts) == 0 {
			continue
		}
		candidateEffort := strings.TrimPrefix(value, name+"-")
		baseModel := strings.TrimSpace(pc.Model)
		for _, declared := range entry.ReasoningEfforts {
			declared = strings.ToLower(strings.TrimSpace(declared))
			if declared == "" {
				continue
			}
			if strings.EqualFold(candidateEffort, declared) {
				baseModel = trimDeclaredModelEffort(baseModel, entry)
				if baseModel == "" {
					return name, "", declared, true, false
				}
				return name, baseModel, declared, true, true
			}
		}
		return name, baseModel, candidateEffort, true, false
	}
	return "", "", "", false, false
}

func trimDeclaredModelEffort(model string, entry config.ProviderModelConfig) string {
	model = strings.TrimSpace(model)
	for _, name := range []string{entry.ID, entry.Alias} {
		if strings.EqualFold(model, strings.TrimSpace(name)) {
			return model
		}
	}
	efforts := append([]string(nil), entry.ReasoningEfforts...)
	sort.SliceStable(efforts, func(i, j int) bool {
		return len(strings.TrimSpace(efforts[i])) > len(strings.TrimSpace(efforts[j]))
	})
	for _, effort := range efforts {
		effort = strings.TrimSpace(effort)
		if effort != "" && strings.HasSuffix(strings.ToLower(model), "-"+strings.ToLower(effort)) {
			return model[:len(model)-len(effort)-1]
		}
	}
	return model
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
	if err := removedProviderError(name); err != nil {
		return nil, err
	}

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
			if err := ValidateClaudeBinModel(model); err != nil {
				return nil, err
			}
			provider := NewClaudeBinProvider(model, nil)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeGrokBin:
			if err := ValidateGrokBinModel(model); err != nil {
				return nil, err
			}
			provider := NewGrokBinProvider(model, nil)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeCursorBin:
			if err := ValidateCursorBinModel(model); err != nil {
				return nil, err
			}
			provider := NewCursorBinProvider(model, nil)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeAgyBin:
			if err := ValidateAgyBinModel(model); err != nil {
				return nil, err
			}
			provider := NewAgyBinProvider(model, nil)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeZen:
			// zen can work without API key (free tier)
			provider := NewZenProvider("", model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeOpenCodeGo:
			apiKey := strings.TrimSpace(os.Getenv("OPENCODE_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires OPENCODE_API_KEY or explicit config", name)
			}
			return WrapWithRetry(NewOpenCodeGoProvider(apiKey, model), DefaultRetryConfig()), nil
		case config.ProviderTypeBedrock:
			provider, err := NewBedrockProvider(model, "", "", "", "", "", nil)
			if err != nil {
				return nil, fmt.Errorf("provider bedrock: %w", err)
			}
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
			apiKey := strings.TrimSpace(os.Getenv("VENICE_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires VENICE_API_KEY or explicit config", name)
			}
			provider := NewVeniceProvider(apiKey, model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeNearAI:
			apiKey := strings.TrimSpace(os.Getenv("NEARAI_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires NEARAI_API_KEY or explicit config", name)
			}
			provider := NewNearAIProvider(apiKey, model)
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		case config.ProviderTypeSambaNova:
			apiKey := strings.TrimSpace(os.Getenv("SAMBANOVA_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires SAMBANOVA_API_KEY or explicit config", name)
			}
			provider := NewSambaNovaProvider(apiKey, model)
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
		case config.ProviderTypeOllama:
			// ollama connects to a local server; no credentials needed
			provider := NewOllamaChatProvider("", model, OllamaOptions{})
			return WrapWithRetry(provider, DefaultRetryConfig()), nil
		default:
			return nil, fmt.Errorf("provider %q not configured", name)
		}
	}

	if err := cfg.ResolveProviderCredentials(name); err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}
	providerCfg = cfg.Providers[name]

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

// NewProviderByNameNoRetry creates the same provider as NewProviderByName but
// returns the underlying adapter without the production retry wrapper. It is
// intended for controlled callers such as benchmarks where an implicit retry
// would hide attempt boundaries and could reuse a now-cacheable payload.
func NewProviderByNameNoRetry(cfg *config.Config, name string, model string) (Provider, error) {
	provider, err := NewProviderByName(cfg, name, model)
	if err != nil {
		return nil, err
	}
	if retryProvider, ok := provider.(*RetryProvider); ok {
		return retryProvider.inner, nil
	}
	return provider, nil
}

// NewFastProvider creates a lightweight provider instance for the specified provider key.
// Resolution order:
// 1. providers.<name>.fast_provider + fast_model
// 2. providers.<name>.fast_model on the same provider key
// 3. built-in ProviderFastModels fallback for inferred provider type
// Returns nil, nil if no fast model can be resolved.
func NewFastProvider(cfg *config.Config, name string) (Provider, error) {
	if cfg == nil {
		return nil, nil
	}

	targetName := name
	targetModel := ""

	if pc, ok := cfg.Providers[name]; ok {
		if strings.TrimSpace(pc.FastProvider) != "" {
			targetName = strings.TrimSpace(pc.FastProvider)
		}
		targetModel = strings.TrimSpace(pc.FastModel)
	}

	if targetModel == "" {
		providerType := string(config.InferProviderType(targetName, ""))
		targetModel = ProviderFastModels[providerType]
	}

	if targetModel == "" {
		return nil, nil
	}

	return NewProviderByName(cfg, targetName, targetModel)
}

// newProviderInternal creates the underlying provider without retry wrapper.
func newProviderInternal(cfg *config.Config) (Provider, error) {
	if err := removedProviderError(cfg.DefaultProvider); err != nil {
		return nil, err
	}

	// Handle hidden debug provider first
	if cfg.DefaultProvider == "debug" {
		variant := ""
		if providerCfg, ok := cfg.Providers["debug"]; ok {
			variant = providerCfg.Model
		}
		return NewDebugProvider(variant), nil
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
			return NewClaudeBinProvider("", nil), nil
		case config.ProviderTypeGrokBin:
			return NewGrokBinProvider("", nil), nil
		case config.ProviderTypeCursorBin:
			return NewCursorBinProvider("", nil), nil
		case config.ProviderTypeAgyBin:
			return NewAgyBinProvider("", nil), nil
		case config.ProviderTypeZen:
			// zen can work without API key (free tier)
			return NewZenProvider("", ""), nil
		case config.ProviderTypeOpenCodeGo:
			apiKey := strings.TrimSpace(os.Getenv("OPENCODE_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires OPENCODE_API_KEY or explicit config", cfg.DefaultProvider)
			}
			return NewOpenCodeGoProvider(apiKey, ""), nil
		case config.ProviderTypeBedrock:
			// bedrock uses AWS credential chain (env vars, ~/.aws/credentials, instance roles)
			return NewBedrockProvider("", "", "", "", "", "", nil)
		case config.ProviderTypeXAI:
			// xai can use XAI_API_KEY env var
			apiKey := os.Getenv("XAI_API_KEY")
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires XAI_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewXAIProvider(apiKey, ""), nil
		case config.ProviderTypeVenice:
			apiKey := strings.TrimSpace(os.Getenv("VENICE_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires VENICE_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewVeniceProvider(apiKey, ""), nil
		case config.ProviderTypeNearAI:
			apiKey := strings.TrimSpace(os.Getenv("NEARAI_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires NEARAI_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewNearAIProvider(apiKey, ""), nil
		case config.ProviderTypeSambaNova:
			apiKey := strings.TrimSpace(os.Getenv("SAMBANOVA_API_KEY"))
			if apiKey == "" {
				return nil, fmt.Errorf("provider %q requires SAMBANOVA_API_KEY environment variable or explicit config", cfg.DefaultProvider)
			}
			return NewSambaNovaProvider(apiKey, ""), nil
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
		case config.ProviderTypeOllama:
			// ollama connects to a local server; no credentials needed
			return NewOllamaChatProvider("", "", OllamaOptions{}), nil
		default:
			return nil, fmt.Errorf("provider %q not configured", cfg.DefaultProvider)
		}
	}
	if err := cfg.ResolveProviderCredentials(cfg.DefaultProvider); err != nil {
		return nil, fmt.Errorf("provider %q: %w", cfg.DefaultProvider, err)
	}
	providerCfg = cfg.Providers[cfg.DefaultProvider]
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
		return NewAnthropicProviderWithBaseURL(cfg.ResolvedAPIKey, cfg.Model, cfg.Credentials, cfg.BaseURL)

	case config.ProviderTypeOpenAI:
		return NewOpenAIProviderWithOptions(cfg.ResolvedAPIKey, cfg.Model, OpenAIProviderOptions{UseWebSocket: cfg.UseWebSocket, ServiceTier: cfg.ServiceTier, FileUploadPolicy: FileUploadPolicyOverrideForProviderConfig(name, *cfg), Responses: responsesOptionsFromConfig(cfg.Responses)}), nil

	case config.ProviderTypeChatGPT:
		// ChatGPT uses native OAuth with interactive authentication
		return NewChatGPTProviderWithOptions(cfg.Model, ChatGPTProviderOptions{UseWebSocket: cfg.UseWebSocket, ServiceTier: cfg.ServiceTier, FileUploadPolicy: FileUploadPolicyOverrideForProviderConfig(name, *cfg), Responses: responsesOptionsFromConfig(cfg.Responses)})

	case config.ProviderTypeCopilot:
		// Copilot uses GitHub device code OAuth with interactive authentication
		provider, err := NewCopilotProvider(cfg.Model)
		if err != nil {
			return nil, err
		}
		provider.fileUploadPolicy = cloneFileUploadPolicy(FileUploadPolicyOverrideForProviderConfig(name, *cfg))
		return provider, nil

	case config.ProviderTypeOpenRouter:
		return NewOpenRouterProvider(cfg.ResolvedAPIKey, cfg.Model, cfg.AppURL, cfg.AppTitle), nil

	case config.ProviderTypeGemini:
		return NewGeminiProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeZen:
		return NewZenProvider(cfg.ResolvedAPIKey, cfg.Model), nil

	case config.ProviderTypeOpenCodeGo:
		if strings.TrimSpace(cfg.URL) != "" {
			return nil, fmt.Errorf("provider %q does not support url overrides; use base_url", name)
		}
		apiKey := strings.TrimSpace(cfg.ResolvedAPIKey)
		if apiKey == "" {
			return nil, fmt.Errorf("provider %q requires OPENCODE_API_KEY or explicit config", name)
		}
		return NewOpenCodeGoProviderWithBaseURL(apiKey, cfg.Model, cfg.BaseURL), nil

	case config.ProviderTypeXAI:
		apiKey := cfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("XAI_API_KEY")
		}
		return NewXAIProvider(apiKey, cfg.Model), nil

	case config.ProviderTypeVenice:
		apiKey := strings.TrimSpace(cfg.ResolvedAPIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("VENICE_API_KEY"))
		}
		if apiKey == "" {
			return nil, fmt.Errorf("provider %q requires VENICE_API_KEY or explicit config", name)
		}
		return NewVeniceProvider(apiKey, cfg.Model), nil

	case config.ProviderTypeNearAI:
		apiKey := strings.TrimSpace(cfg.ResolvedAPIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("NEARAI_API_KEY"))
		}
		if apiKey == "" {
			return nil, fmt.Errorf("provider %q requires NEARAI_API_KEY or explicit config", name)
		}
		return NewNearAIProvider(apiKey, cfg.Model), nil

	case config.ProviderTypeSambaNova:
		apiKey := strings.TrimSpace(cfg.ResolvedAPIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(os.Getenv("SAMBANOVA_API_KEY"))
		}
		if apiKey == "" {
			return nil, fmt.Errorf("provider %q requires SAMBANOVA_API_KEY or explicit config", name)
		}
		return NewSambaNovaProvider(apiKey, cfg.Model), nil

	case config.ProviderTypeBedrock:
		return NewBedrockProvider(cfg.Model, cfg.Region, cfg.Profile, cfg.AccessKey, cfg.SecretKey, cfg.SessionToken, cfg.ModelMap)

	case config.ProviderTypeClaudeBin:
		if err := ValidateClaudeBinModel(cfg.Model); err != nil {
			return nil, err
		}
		provider := NewClaudeBinProvider(cfg.Model, cfg.Env)
		provider.SetEnableHooks(cfg.EnableHooks)
		return provider, nil

	case config.ProviderTypeGrokBin:
		if err := ValidateGrokBinModel(cfg.Model); err != nil {
			return nil, err
		}
		return NewGrokBinProvider(cfg.Model, cfg.Env), nil

	case config.ProviderTypeCursorBin:
		if err := ValidateCursorBinModel(cfg.Model); err != nil {
			return nil, err
		}
		return NewCursorBinProvider(cfg.Model, cfg.Env), nil

	case config.ProviderTypeAgyBin:
		if err := ValidateAgyBinModel(cfg.Model); err != nil {
			return nil, err
		}
		return NewAgyBinProvider(cfg.Model, cfg.Env), nil

	case config.ProviderTypeOllama:
		thinkLevel, err := normalizeOllamaThinkLevel(cfg.ThinkLevel)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		opts := OllamaOptions{
			Think:           cfg.Think,
			ThinkLevel:      thinkLevel,
			TopK:            cfg.TopK,
			MinP:            cfg.MinP,
			PresencePenalty: cfg.PresencePenalty,
			NumCtx:          cfg.NumCtx,
			NumPredict:      cfg.NumPredict,
		}
		return NewOllamaChatProvider(cfg.BaseURL, cfg.Model, opts), nil

	case config.ProviderTypeOpenAICompat, config.ProviderTypeVLLM:
		// Use ResolvedURL if available (from srv:// or $() resolution), otherwise use config values
		baseURL := cfg.BaseURL
		chatURL := cfg.URL
		if cfg.ResolvedURL != "" {
			chatURL = cfg.ResolvedURL
		}
		if baseURL == "" && chatURL == "" {
			return nil, fmt.Errorf("provider %q requires base_url or url", name)
		}
		if name == "" {
			return nil, fmt.Errorf("openai-compatible provider requires a non-empty name")
		}
		// Use provider name as display name, with first letter capitalized
		displayName := strings.ToUpper(name[:1]) + name[1:]
		if providerType == config.ProviderTypeVLLM {
			p := NewVLLMProviderFull(baseURL, chatURL, cfg.ResolvedAPIKey, cfg.Model, displayName)
			p.noStreamOptions = cfg.NoStreamOptions
			p.vllmThinkingParam = cfg.VLLMThinkingParam
			p.SetModelConfigs(cfg.ModelConfigs)
			return p, nil
		}
		p := NewOpenAICompatProviderFull(baseURL, chatURL, cfg.ResolvedAPIKey, cfg.Model, displayName, nil)
		p.noStreamOptions = cfg.NoStreamOptions
		parseReasoning, includeReasoning, thinkingParam := openAICompatReasoningParserOptions(cfg)
		p.SetReasoningParser(parseReasoning, includeReasoning, thinkingParam)
		p.SetModelConfigs(cfg.ModelConfigs)
		return p, nil

	default:
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
}

func openAICompatReasoningParserOptions(cfg *config.ProviderConfig) (parseReasoning, includeReasoning *bool, thinkingParam string) {
	if cfg == nil {
		return nil, nil, ""
	}
	return cfg.ParseReasoning, cfg.IncludeReasoning, strings.TrimSpace(cfg.ThinkingParam)
}
