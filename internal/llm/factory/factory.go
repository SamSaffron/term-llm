// concrete inference constructors. It imports only leaf core and provider
// families, never the root llm package; root-only behaviour is injected
// through Options.
package factory

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
	"github.com/samsaffron/term-llm/internal/llm/providers/bedrock"
	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
	"github.com/samsaffron/term-llm/internal/llm/providers/gemini"
	"github.com/samsaffron/term-llm/internal/llm/providers/ollama"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

// Provider is the shared provider contract.
type Provider = core.Provider

// Options carries root-only hooks. A zero Options is valid; nil hooks behave as
// "no override". Options are copied by value and never mutated.
type Options struct {
	// FileUploadPolicyOverride returns the configured upload policy for a
	// provider, or nil to use the provider's built-in defaults.
	FileUploadPolicyOverride func(name string, cfg config.ProviderConfig) *responses.FileUploadPolicy
	// HTTPClient is the shared streaming client for providers that accept one.
	HTTPClient *http.Client
	// ClampOutputTokens caps the requested output budget for a model.
	ClampOutputTokens func(requested int, model string) int
}

func (o Options) uploadPolicy(name string, cfg *config.ProviderConfig) *responses.FileUploadPolicy {
	if o.FileUploadPolicyOverride == nil {
		return nil
	}
	return o.FileUploadPolicyOverride(name, *cfg)
}

// Constructor creates a provider of a registered type.
type Constructor func(name string, providerType config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error)

var constructors = buildConstructors()

// Has reports whether providerType has a registered constructor.
func Has(providerType config.ProviderType) bool { return constructors[providerType] != nil }

// Types returns every provider type with a registered constructor.
func Types() []config.ProviderType {
	out := make([]config.ProviderType, 0, len(constructors))
	for t := range constructors {
		out = append(out, t)
	}
	return out
}

// Create resolves lazy config values and creates the provider cfg describes.
func Create(name string, cfg *config.ProviderConfig, opts Options) (Provider, error) {
	// Resolve lazy config values (op://, srv://, $()) before creating provider
	if err := cfg.ResolveForInference(); err != nil {
		return nil, fmt.Errorf("provider %q: %w", name, err)
	}

	providerType := config.InferProviderType(name, cfg.Type)
	if err := config.ValidateProviderCompatibility(name, cfg); err != nil {
		return nil, err
	}

	constructor, ok := constructors[providerType]
	if !ok {
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
	return constructor(name, providerType, cfg, opts)
}

var binProviderConstructors = map[config.ProviderType]func(string, map[string]string, bool) (Provider, error){
	config.ProviderTypeClaudeBin: func(model string, env map[string]string, enableHooks bool) (Provider, error) {
		if err := clibin.ValidateClaudeBinModel(model); err != nil {
			return nil, err
		}
		provider := clibin.NewClaudeBinProvider(model, env)
		provider.SetEnableHooks(enableHooks)
		return provider, nil
	},
	config.ProviderTypeGrokBin: func(model string, env map[string]string, enableHooks bool) (Provider, error) {
		if err := clibin.ValidateGrokBinModel(model); err != nil {
			return nil, err
		}
		return clibin.NewGrokBinProvider(model, env), nil
	},
	config.ProviderTypeCursorBin: func(model string, env map[string]string, enableHooks bool) (Provider, error) {
		if err := clibin.ValidateCursorBinModel(model); err != nil {
			return nil, err
		}
		return clibin.NewCursorBinProvider(model, env), nil
	},
	config.ProviderTypeAgyBin: func(model string, env map[string]string, enableHooks bool) (Provider, error) {
		if err := clibin.ValidateAgyBinModel(model); err != nil {
			return nil, err
		}
		return clibin.NewAgyBinProvider(model, env), nil
	},
}

// NewBinProvider validates model overrides before creating a CLI-backed provider.
func NewBinProvider(providerType config.ProviderType, model string, env map[string]string, enableHooks bool) (Provider, error) {
	constructor := binProviderConstructors[providerType]
	if constructor == nil {
		return nil, fmt.Errorf("unsupported bin provider type %q", providerType)
	}
	return constructor(model, env, enableHooks)
}

var apiKeyProviderConstructors = map[config.ProviderType]func(string, string, *config.ProviderConfig, Options) (Provider, error){
	config.ProviderTypeOpenRouter: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return opencompat.NewOpenRouterProvider(key, cfg.Model, cfg.AppURL, cfg.AppTitle), nil
	},
	config.ProviderTypeGemini: func(name, key string, cfg *config.ProviderConfig, opts Options) (Provider, error) {
		return gemini.NewWithOptions(key, cfg.Model, gemini.Options{Client: opts.HTTPClient, ClampOutputTokens: opts.ClampOutputTokens}), nil
	},
	config.ProviderTypeXAI: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return opencompat.NewXAIProvider(key, cfg.Model), nil
	},
	config.ProviderTypeZen: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return rp.NewZenProvider(key, cfg.Model), nil
	},
	config.ProviderTypeVenice: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return opencompat.NewVeniceProvider(key, cfg.Model), nil
	},
	config.ProviderTypeNearAI: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return opencompat.NewNearAIProvider(key, cfg.Model), nil
	},
	config.ProviderTypeSambaNova: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return opencompat.NewSambaNovaProvider(key, cfg.Model), nil
	},
	config.ProviderTypeOpenCodeGo: func(name, key string, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		if strings.TrimSpace(cfg.URL) != "" {
			return nil, fmt.Errorf("provider %q does not support url overrides; use base_url", name)
		}
		return rp.NewOpenCodeGoProviderWithBaseURL(key, cfg.Model, cfg.BaseURL), nil
	},
}

// NewAPIKeyProvider creates a built-in provider that authenticates with an
// API key: the resolved config value, else the registry environment variable.
func NewAPIKeyProvider(name string, providerType config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error) {
	spec, _ := config.BuiltinProviderOfType(providerType)
	key := strings.TrimSpace(cfg.ResolvedAPIKey)
	if key == "" && spec.APIKeyEnv != "" {
		key = strings.TrimSpace(os.Getenv(spec.APIKeyEnv))
	}
	if key == "" {
		return nil, fmt.Errorf("provider %q requires %s or explicit config", name, spec.APIKeyEnv)
	}
	constructor := apiKeyProviderConstructors[providerType]
	if constructor == nil {
		return nil, fmt.Errorf("unsupported API-key provider type %q", providerType)
	}
	return constructor(name, key, cfg, opts)
}

// OpenAICompatReasoningParserOptions extracts reasoning-parser settings.
func OpenAICompatReasoningParserOptions(cfg *config.ProviderConfig) (parseReasoning, includeReasoning *bool, thinkingParam string) {
	if cfg == nil {
		return nil, nil, ""
	}
	return cfg.ParseReasoning, cfg.IncludeReasoning, strings.TrimSpace(cfg.ThinkingParam)
}

func buildConstructors() map[config.ProviderType]Constructor {
	m := make(map[config.ProviderType]Constructor)
	m[config.ProviderTypeAnthropic] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return anthropic.NewAnthropicProviderWithOptions(cfg.ResolvedAPIKey, cfg.Model, cfg.Credentials, anthropic.AnthropicProviderOptions{BaseURL: cfg.BaseURL, Headers: cfg.Headers})
	}
	m[config.ProviderTypeOpenAI] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error) {
		return rp.NewOpenAIProviderWithOptions(cfg.ResolvedAPIKey, cfg.Model, rp.OpenAIProviderOptions{ProviderName: name, UsePreviousResponseID: cfg.UsePreviousResponseID, RequestEncryptedReasoning: cfg.RequestEncryptedReasoning, Endpoint: cfg.OpenAIEndpoint(), UseWebSocket: cfg.UseWebSocket, ServiceTier: cfg.ServiceTier, FileUploadPolicy: opts.uploadPolicy(name, cfg), Responses: responses.OptionsFromConfig(cfg.Responses)}), nil
	}
	m[config.ProviderTypeChatGPT] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error) {
		// ChatGPT uses native OAuth with interactive authentication
		return rp.NewChatGPTProviderWithOptions(cfg.Model, rp.ChatGPTProviderOptions{UseWebSocket: cfg.UseWebSocket, ServiceTier: cfg.ServiceTier, FileUploadPolicy: opts.uploadPolicy(name, cfg), Responses: responses.OptionsFromConfig(cfg.Responses)})
	}
	m[config.ProviderTypeGrok] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error) {
		return rp.NewGrokProviderWithOptions(cfg.Model, rp.GrokProviderOptions{FileUploadPolicy: opts.uploadPolicy(name, cfg)})
	}
	m[config.ProviderTypeCopilot] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error) {
		// Copilot uses GitHub device code OAuth with interactive authentication
		provider, err := rp.NewCopilotProvider(cfg.Model)
		if err != nil {
			return nil, err
		}
		provider.SetFileUploadPolicy(opts.uploadPolicy(name, cfg))
		return provider, nil
	}
	apiKey := func(name string, providerType config.ProviderType, cfg *config.ProviderConfig, opts Options) (Provider, error) {
		return NewAPIKeyProvider(name, providerType, cfg, opts)
	}
	// Types served by the shared API-key and CLI-bin dispatch layers are
	// derived from those layers' tables, so each type is listed once.
	for t := range apiKeyProviderConstructors {
		m[t] = apiKey
	}
	m[config.ProviderTypeBedrock] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return bedrock.New(cfg.Model, cfg.Region, cfg.Profile, cfg.AccessKey, cfg.SecretKey, cfg.SessionToken, cfg.ModelMap, cfg.Headers)
	}
	bin := func(name string, providerType config.ProviderType, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		return NewBinProvider(providerType, cfg.Model, cfg.Env, cfg.EnableHooks)
	}
	for t := range binProviderConstructors {
		m[t] = bin
	}
	m[config.ProviderTypeOllama] = func(name string, _ config.ProviderType, cfg *config.ProviderConfig, _ Options) (Provider, error) {
		thinkLevel, err := ollama.NormalizeThinkLevel(cfg.ThinkLevel)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		return ollama.New(cfg.BaseURL, cfg.Model, ollama.Options{
			Think:           cfg.Think,
			ThinkLevel:      thinkLevel,
			TopK:            cfg.TopK,
			MinP:            cfg.MinP,
			PresencePenalty: cfg.PresencePenalty,
			NumCtx:          cfg.NumCtx,
			NumPredict:      cfg.NumPredict,
		}), nil
	}
	compat := func(name string, providerType config.ProviderType, cfg *config.ProviderConfig, _ Options) (Provider, error) {
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
			p := opencompat.NewVLLMProviderFull(baseURL, chatURL, cfg.ResolvedAPIKey, cfg.Model, displayName)
			p.SetNoStreamOptions(cfg.NoStreamOptions)
			p.SetVLLMThinkingParam(cfg.VLLMThinkingParam)
			p.SetModelConfigs(cfg.ModelConfigs)
			return p, nil
		}
		p := opencompat.NewOpenAICompatProviderFull(baseURL, chatURL, cfg.ResolvedAPIKey, cfg.Model, displayName, nil)
		p.SetNoStreamOptions(cfg.NoStreamOptions)
		parseReasoning, includeReasoning, thinkingParam := OpenAICompatReasoningParserOptions(cfg)
		p.SetReasoningParser(parseReasoning, includeReasoning, thinkingParam)
		p.SetModelConfigs(cfg.ModelConfigs)
		return p, nil
	}
	m[config.ProviderTypeOpenAICompat] = compat
	m[config.ProviderTypeVLLM] = compat
	return m
}
