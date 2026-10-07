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

// ProviderUnavailableError reports providers that cannot be created: removed
// ones, ones disabled with providers.<name>.enabled: false, and ones that
// provider_discovery does not enable.
func ProviderUnavailableError(cfg *config.Config, name string) error {
	if err := removedProviderError(name); err != nil {
		return err
	}
	if cfg.ProviderDisabled(name) {
		return fmt.Errorf("provider %q is disabled in config (providers.%s.enabled: false)", name, name)
	}
	return cfg.ProviderNotEnabledError(name)
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
	return NewProviderByName(cfg, cfg.DefaultProvider, "")
}

// NewProviderByName creates a provider by name from the config, with an optional model override.
// This is useful for per-command provider overrides.
// A built-in provider without a providers.<name> block runs with an empty one.
func NewProviderByName(cfg *config.Config, name string, model string) (Provider, error) {
	provider, err := newNamedProvider(cfg, name, model)
	if err != nil {
		return nil, err
	}
	return WrapWithRetry(provider, DefaultRetryConfig()), nil
}

// newNamedProvider creates provider name without the retry wrapper.
func newNamedProvider(cfg *config.Config, name string, model string) (Provider, error) {
	if err := ProviderUnavailableError(cfg, name); err != nil {
		return nil, err
	}
	if name == "debug" {
		if model == "" {
			model = cfg.Providers["debug"].Model
		}
		return NewDebugProvider(model), nil
	}
	providerCfg, err := resolvedProviderConfig(cfg, name)
	if err != nil {
		return nil, err
	}
	if model != "" {
		providerCfg.Model = model
	}
	return createProviderFromConfig(name, &providerCfg)
}

// resolvedProviderConfig returns provider name's config with credentials
// resolved. A built-in provider without a providers.<name> block gets an empty
// one, so its API key comes from the registry environment variable.
func resolvedProviderConfig(cfg *config.Config, name string) (config.ProviderConfig, error) {
	if _, ok := cfg.Providers[name]; ok {
		if err := cfg.ResolveProviderCredentials(name); err != nil {
			return config.ProviderConfig{}, fmt.Errorf("provider %q: %w", name, err)
		}
		return cfg.Providers[name], nil
	}
	spec, ok := config.BuiltinProvider(name)
	if !ok {
		return config.ProviderConfig{}, fmt.Errorf("provider %q not configured", name)
	}
	providerCfg := config.ProviderConfig{Type: spec.Type}
	if err := providerCfg.ResolveCredentials(name); err != nil {
		return config.ProviderConfig{}, fmt.Errorf("provider %q: %w", name, err)
	}
	return providerCfg, nil
}

// newBinProvider validates model overrides before creating a CLI-backed provider.
func newBinProvider(providerType config.ProviderType, model string, env map[string]string, enableHooks bool) (Provider, error) {
	switch providerType {
	case config.ProviderTypeClaudeBin:
		if err := ValidateClaudeBinModel(model); err != nil {
			return nil, err
		}
		provider := NewClaudeBinProvider(model, env)
		provider.SetEnableHooks(enableHooks)
		return provider, nil
	case config.ProviderTypeGrokBin:
		if err := ValidateGrokBinModel(model); err != nil {
			return nil, err
		}
		return NewGrokBinProvider(model, env), nil
	case config.ProviderTypeCursorBin:
		if err := ValidateCursorBinModel(model); err != nil {
			return nil, err
		}
		return NewCursorBinProvider(model, env), nil
	case config.ProviderTypeAgyBin:
		if err := ValidateAgyBinModel(model); err != nil {
			return nil, err
		}
		return NewAgyBinProvider(model, env), nil
	default:
		return nil, fmt.Errorf("unsupported bin provider type %q", providerType)
	}
}

// NewProviderByNameNoRetry creates the same provider as NewProviderByName but
// returns the underlying adapter without the production retry wrapper. It is
// intended for controlled callers such as benchmarks where an implicit retry
// would hide attempt boundaries and could reuse a now-cacheable payload.
func NewProviderByNameNoRetry(cfg *config.Config, name string, model string) (Provider, error) {
	return newNamedProvider(cfg, name, model)
}

// NewFastProvider creates a lightweight provider instance for the specified provider key.
// Resolution order:
// 1. providers.<name>.fast_provider + fast_model
// 2. providers.<name>.fast_model on the same provider key
// 3. built-in ProviderFastModels fallback for inferred provider type
// Returns nil, nil if no fast model can be resolved.
func ResolveFastTarget(cfg *config.Config, name string) (string, string, bool) {
	if cfg == nil {
		return "", "", false
	}
	targetName, targetModel := name, ""
	if pc, ok := cfg.Providers[name]; ok {
		if strings.TrimSpace(pc.FastProvider) != "" {
			targetName = strings.TrimSpace(pc.FastProvider)
		}
		targetModel = strings.TrimSpace(pc.FastModel)
	}
	if targetModel == "" {
		var explicitType config.ProviderType
		if pc, ok := cfg.Providers[targetName]; ok {
			explicitType = pc.Type
		}
		targetModel = ProviderFastModels[string(config.InferProviderType(targetName, explicitType))]
	}
	return targetName, targetModel, targetModel != ""
}

func NewFastProvider(cfg *config.Config, name string) (Provider, error) {
	target, model, ok := ResolveFastTarget(cfg, name)
	if !ok {
		return nil, nil
	}
	return NewProviderByName(cfg, target, model)
}

// newAPIKeyProvider creates a built-in provider that authenticates with an
// API key: the resolved config value, else the registry environment variable.
func newAPIKeyProvider(name string, providerType config.ProviderType, cfg *config.ProviderConfig) (Provider, error) {
	spec, _ := config.BuiltinProviderOfType(providerType)
	key := strings.TrimSpace(cfg.ResolvedAPIKey)
	if key == "" && spec.APIKeyEnv != "" {
		key = strings.TrimSpace(os.Getenv(spec.APIKeyEnv))
	}
	if key == "" {
		return nil, fmt.Errorf("provider %q requires %s or explicit config", name, spec.APIKeyEnv)
	}
	switch providerType {
	case config.ProviderTypeOpenRouter:
		return NewOpenRouterProvider(key, cfg.Model, cfg.AppURL, cfg.AppTitle), nil
	case config.ProviderTypeGemini:
		return NewGeminiProvider(key, cfg.Model), nil
	case config.ProviderTypeXAI:
		return NewXAIProvider(key, cfg.Model), nil
	case config.ProviderTypeZen:
		return NewZenProvider(key, cfg.Model), nil
	case config.ProviderTypeVenice:
		return NewVeniceProvider(key, cfg.Model), nil
	case config.ProviderTypeNearAI:
		return NewNearAIProvider(key, cfg.Model), nil
	case config.ProviderTypeSambaNova:
		return NewSambaNovaProvider(key, cfg.Model), nil
	case config.ProviderTypeOpenCodeGo:
		if strings.TrimSpace(cfg.URL) != "" {
			return nil, fmt.Errorf("provider %q does not support url overrides; use base_url", name)
		}
		return NewOpenCodeGoProviderWithBaseURL(key, cfg.Model, cfg.BaseURL), nil
	}
	return nil, fmt.Errorf("unsupported API-key provider type %q", providerType)
}

// createProviderFromConfig creates a provider from a ProviderConfig.
func createProviderFromConfig(name string, cfg *config.ProviderConfig) (Provider, error) {
	// Resolve lazy config values (op://, srv://, $()) before creating provider
	if err := cfg.ResolveForInference(); err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}

	providerType := config.InferProviderType(name, cfg.Type)
	if err := config.ValidateProviderCompatibility(name, cfg); err != nil {
		return nil, err
	}

	switch providerType {
	case config.ProviderTypeAnthropic:
		return NewAnthropicProviderWithBaseURL(cfg.ResolvedAPIKey, cfg.Model, cfg.Credentials, cfg.BaseURL)

	case config.ProviderTypeOpenAI:
		return NewOpenAIProviderWithOptions(cfg.ResolvedAPIKey, cfg.Model, OpenAIProviderOptions{UseWebSocket: cfg.UseWebSocket, ServiceTier: cfg.ServiceTier, FileUploadPolicy: FileUploadPolicyOverrideForProviderConfig(name, *cfg), Responses: responsesOptionsFromConfig(cfg.Responses)}), nil

	case config.ProviderTypeChatGPT:
		// ChatGPT uses native OAuth with interactive authentication
		return NewChatGPTProviderWithOptions(cfg.Model, ChatGPTProviderOptions{UseWebSocket: cfg.UseWebSocket, ServiceTier: cfg.ServiceTier, FileUploadPolicy: FileUploadPolicyOverrideForProviderConfig(name, *cfg), Responses: responsesOptionsFromConfig(cfg.Responses)})

	case config.ProviderTypeGrok:
		return NewGrokProviderWithOptions(cfg.Model, GrokProviderOptions{FileUploadPolicy: FileUploadPolicyOverrideForProviderConfig(name, *cfg)})

	case config.ProviderTypeCopilot:
		// Copilot uses GitHub device code OAuth with interactive authentication
		provider, err := NewCopilotProvider(cfg.Model)
		if err != nil {
			return nil, err
		}
		provider.fileUploadPolicy = cloneFileUploadPolicy(FileUploadPolicyOverrideForProviderConfig(name, *cfg))
		return provider, nil

	case config.ProviderTypeOpenRouter, config.ProviderTypeGemini, config.ProviderTypeXAI,
		config.ProviderTypeZen, config.ProviderTypeVenice, config.ProviderTypeNearAI,
		config.ProviderTypeSambaNova, config.ProviderTypeOpenCodeGo:
		return newAPIKeyProvider(name, providerType, cfg)

	case config.ProviderTypeBedrock:
		return NewBedrockProvider(cfg.Model, cfg.Region, cfg.Profile, cfg.AccessKey, cfg.SecretKey, cfg.SessionToken, cfg.ModelMap)

	case config.ProviderTypeClaudeBin, config.ProviderTypeGrokBin, config.ProviderTypeCursorBin, config.ProviderTypeAgyBin:
		return newBinProvider(providerType, cfg.Model, cfg.Env, cfg.EnableHooks)

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
