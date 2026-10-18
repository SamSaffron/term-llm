package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
	"github.com/samsaffron/term-llm/internal/llm/providers/ollama"
	"github.com/samsaffron/term-llm/internal/providerspec"
	"github.com/spf13/cobra"
)

var modelsProvider string
var modelsJSON bool
var modelsRefresh bool

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List available models from a provider",
	Long: `List available models from a provider.

This command queries the provider's models API to discover what models
are available. Useful for finding model names to configure.

Examples:
  term-llm models                       # list models from current provider
  term-llm models --provider anthropic  # list models from Anthropic
  term-llm models --provider openrouter # list models from OpenRouter
  term-llm models --provider nearai     # list models from NEAR AI Cloud
  term-llm models --provider sambanova  # list models from SambaNova
  term-llm models --provider venice     # list models from Venice
  term-llm models --provider cursor-bin # list/cache Cursor Agent models
  term-llm models --provider grok-bin   # list/cache Grok Build CLI models
  term-llm models --provider agy-bin    # list/cache Antigravity CLI models
  term-llm models --provider ollama     # list models from Ollama
  term-llm models --provider lmstudio   # list models from LM Studio
  term-llm models --provider chatgpt --refresh # fetch the latest ChatGPT catalog
  term-llm models --json                # output as JSON`,
	RunE: runModels,
}

func init() {
	rootCmd.AddCommand(modelsCmd)
	modelsCmd.Flags().StringVarP(&modelsProvider, "provider", "p", "", "Provider to list models from ("+strings.Join(supportedModelListProviderTypes(), ", ")+")")
	modelsCmd.Flags().BoolVar(&modelsJSON, "json", false, "Output as JSON")
	modelsCmd.Flags().BoolVar(&modelsRefresh, "refresh", false, "Force a fresh ChatGPT model catalog fetch (report fetch errors)")
	modelsCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion)
}

// ModelLister is an interface for providers that can list available models
type ModelLister interface {
	ListModels(ctx context.Context) ([]llm.ModelInfo, error)
}

// supportsModelListing reports whether providerType can list models upstream:
// built-ins per the provider registry, plus generic OpenAI-compatible servers.
func supportsModelListing(providerType config.ProviderType) bool {
	if providerType == config.ProviderTypeOpenAICompat {
		return true
	}
	spec, ok := config.BuiltinProviderOfType(providerType)
	return ok && spec.ListModels
}

func supportedModelListProviderTypes() []string {
	types := []string{string(config.ProviderTypeOpenAICompat)}
	for _, spec := range config.BuiltinProviders() {
		if spec.ListModels {
			types = append(types, string(spec.Type))
		}
	}
	sort.Strings(types)
	return types
}

func runModels(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfigWithSetup()
	if err != nil {
		return err
	}

	// Determine which provider to query
	providerName := modelsProvider
	if providerName == "" {
		providerName = cfg.DefaultProvider
	}
	if err := llm.ProviderUnavailableError(cfg, providerName); err != nil {
		return err
	}

	// Get provider config - handle built-in providers that may not be explicitly configured
	providerCfg, configured := cfg.Providers[providerName]
	if configured {
		if err := cfg.ResolveProviderCredentials(providerName); err != nil {
			return fmt.Errorf("provider %q: %w", providerName, err)
		}
		providerCfg = cfg.Providers[providerName]
		if err := providerCfg.ResolveForInference(); err != nil {
			return fmt.Errorf("provider %q: %w", providerName, err)
		}
	}

	// Infer provider type - only use config type if provider is configured
	var providerType config.ProviderType
	if configured {
		providerType = config.InferProviderType(providerName, providerCfg.Type)
	} else {
		providerType = config.InferProviderType(providerName, "")
	}
	if err := config.ValidateProviderCompatibility(providerName, &providerCfg); err != nil {
		return err
	}

	if modelsRefresh && providerType != config.ProviderTypeChatGPT {
		return fmt.Errorf("--refresh is only supported for ChatGPT model listings")
	}

	handled, err := handleUnsupportedModelListProvider(providerName, providerType, configured)
	if handled {
		return err
	}

	lister, err := newModelLister(providerName, providerType, providerCfg, configured)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelListTimeout(providerType))
	defer cancel()

	var models []llm.ModelInfo
	if modelsRefresh {
		models, err = lister.(interface {
			RefreshModelsForProvider(context.Context, string) ([]llm.ModelInfo, error)
		}).RefreshModelsForProvider(ctx, providerName)
	} else if scoped, ok := lister.(interface {
		ListModelsForProvider(context.Context, string) ([]llm.ModelInfo, error)
	}); ok {
		models, err = scoped.ListModelsForProvider(ctx, providerName)
	} else {
		models, err = lister.ListModels(ctx)
	}
	if err == nil {
		refreshModelListCaches(providerName, providerCfg, models)
	}
	if err != nil {
		// Provide helpful error messages for common issues
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("cannot connect to %s server.\n"+
				"Make sure the server is running and accessible.\n\n"+
				"For Ollama: run 'ollama serve'\n"+
				"For LM Studio: start LM Studio and enable the server", providerName)
		}
		return fmt.Errorf("failed to list models: %w", err)
	}

	if len(models) == 0 {
		fmt.Println("No models found.")
		return nil
	}

	if modelsJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(models)
	}

	return printModelList(providerName, providerType, models)
}

func handleUnsupportedModelListProvider(providerName string, providerType config.ProviderType, configured bool) (bool, error) {
	if supportsModelListing(providerType) {
		return false, nil
	}

	if staticModels := llm.ResolveProviderModelIDs(providerName); len(staticModels) > 0 {
		return true, printStaticModels(providerName, staticModels)
	}
	if !configured {
		return true, fmt.Errorf("provider '%s' is not configured", providerName)
	}
	return true, fmt.Errorf("provider '%s' (type: %s) does not support model listing.\n"+
		"Model listing is supported for: %s", providerName, providerType, strings.Join(supportedModelListProviderTypes(), ", "))
}

type modelListerConstructor func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error)

// modelListerConstructors binds model listing behavior to registry listing kinds.
var modelListerConstructors = map[providerspec.ModelListing]modelListerConstructor{
	providerspec.ModelListingAnthropic: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return newAnthropicModelLister(providerCfg)
	},
	providerspec.ModelListingAPIKey: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return newAPIKeyModelLister(providerType, providerCfg)
	},
	providerspec.ModelListingChatGPT: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return newChatGPTModelLister(providerCfg.Model)
	},
	providerspec.ModelListingCopilot: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return newCopilotModelLister(providerCfg.Model, configured)
	},
	providerspec.ModelListingCursorBin: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return llm.NewCursorBinProvider(providerCfg.Model, providerCfg.Env), nil
	},
	providerspec.ModelListingGrokBin: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return llm.NewGrokBinProvider(providerCfg.Model, providerCfg.Env), nil
	},
	providerspec.ModelListingAgyBin: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return llm.NewAgyBinProvider(providerCfg.Model, providerCfg.Env), nil
	},
	providerspec.ModelListingCompatible: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return newOpenAICompatibleModelLister(providerName, providerCfg)
	},
	providerspec.ModelListingOllama: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return ollama.New(providerCfg.BaseURL, providerCfg.Model, ollama.Options{}), nil
	},
	providerspec.ModelListingZen: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return llm.NewZenProvider(providerCfg.ResolvedAPIKey, ""), nil
	},
	providerspec.ModelListingOpenCodeGo: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		apiKey := modelListAPIKey(providerCfg.ResolvedAPIKey, "OPENCODE_API_KEY")
		// The Go model and metadata endpoints are public; a key is required only
		// when the returned provider is used for inference.
		return llm.NewOpenCodeGoProviderWithBaseURL(apiKey, providerCfg.Model, providerCfg.BaseURL), nil
	},
	providerspec.ModelListingGrok: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		return newGrokModelLister(providerCfg.Model)
	},
	providerspec.ModelListingNearAI: func(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
		apiKey := modelListAPIKey(providerCfg.ResolvedAPIKey, "NEARAI_API_KEY")
		return llm.NewNearAIProvider(apiKey, providerCfg.Model), nil
	},
}

func newModelLister(providerName string, providerType config.ProviderType, providerCfg config.ProviderConfig, configured bool) (ModelLister, error) {
	spec, registered := providerspec.ByType(providerType)
	if !registered {
		return nil, nil
	}
	constructor, ok := modelListerConstructors[spec.ModelListing]
	if !ok {
		return nil, nil
	}
	return constructor(providerName, providerType, providerCfg, configured)
}

func newAnthropicModelLister(providerCfg config.ProviderConfig) (ModelLister, error) {
	provider, err := anthropic.NewAnthropicProviderWithOptions(providerCfg.ResolvedAPIKey, providerCfg.Model, providerCfg.Credentials, anthropic.AnthropicProviderOptions{BaseURL: providerCfg.BaseURL, Headers: providerCfg.Headers})
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	return provider, nil
}

func newChatGPTModelLister(model string) (ModelLister, error) {
	provider, err := llm.NewChatGPTProvider(model)
	if err != nil {
		return nil, fmt.Errorf("chatgpt provider: %w", err)
	}
	return provider, nil
}

func newCopilotModelLister(configuredModel string, configured bool) (ModelLister, error) {
	// Copilot uses OAuth - create provider which will prompt for auth if needed
	model := ""
	if configured {
		model = configuredModel
	}
	provider, err := llm.NewCopilotProvider(model)
	if err != nil {
		return nil, fmt.Errorf("copilot provider: %w", err)
	}
	return provider, nil
}

func newOpenAICompatibleModelLister(providerName string, providerCfg config.ProviderConfig) (ModelLister, error) {
	if providerCfg.BaseURL == "" {
		return nil, fmt.Errorf("provider '%s' requires base_url to be configured", providerName)
	}
	return llm.NewOpenAICompatProvider(providerCfg.BaseURL, providerCfg.ResolvedAPIKey, "", providerName), nil
}

func newGrokModelLister(model string) (ModelLister, error) {
	provider, err := llm.NewGrokProvider(model)
	if err != nil {
		return nil, fmt.Errorf("grok provider: %w", err)
	}
	return provider, nil
}

// apiKeyModelListers retain provider-specific endpoint and error semantics.
var apiKeyModelListers = map[config.ProviderType]func(config.ProviderConfig) (ModelLister, error){
	config.ProviderTypeOpenAI: func(providerCfg config.ProviderConfig) (ModelLister, error) {
		apiKey, err := requiredModelListAPIKey(providerCfg.ResolvedAPIKey, "OPENAI_API_KEY", "openai")
		if err != nil {
			return nil, err
		}
		return llm.NewOpenAIProviderWithOptions(apiKey, providerCfg.Model, llm.OpenAIProviderOptions{Endpoint: providerCfg.OpenAIEndpoint()}), nil
	},
	config.ProviderTypeOpenRouter: func(providerCfg config.ProviderConfig) (ModelLister, error) {
		apiKey, err := requiredModelListAPIKey(providerCfg.ResolvedAPIKey, "OPENROUTER_API_KEY", "openrouter")
		if err != nil {
			return nil, err
		}
		return llm.NewOpenRouterProvider(apiKey, "", providerCfg.AppURL, providerCfg.AppTitle), nil
	},
	config.ProviderTypeXAI: func(providerCfg config.ProviderConfig) (ModelLister, error) {
		apiKey, err := requiredModelListAPIKey(providerCfg.ResolvedAPIKey, "XAI_API_KEY", "xAI")
		if err != nil {
			return nil, err
		}
		return llm.NewXAIProvider(apiKey, providerCfg.Model), nil
	},
	config.ProviderTypeVenice: func(providerCfg config.ProviderConfig) (ModelLister, error) {
		apiKey, err := requiredModelListAPIKey(providerCfg.ResolvedAPIKey, "VENICE_API_KEY", "Venice")
		if err != nil {
			return nil, err
		}
		return llm.NewVeniceProvider(apiKey, providerCfg.Model), nil
	},
	config.ProviderTypeSambaNova: func(providerCfg config.ProviderConfig) (ModelLister, error) {
		apiKey, err := requiredModelListAPIKey(providerCfg.ResolvedAPIKey, "SAMBANOVA_API_KEY", "SambaNova")
		if err != nil {
			return nil, err
		}
		return llm.NewSambaNovaProvider(apiKey, providerCfg.Model), nil
	},
}

func newAPIKeyModelLister(providerType config.ProviderType, providerCfg config.ProviderConfig) (ModelLister, error) {
	constructor := apiKeyModelListers[providerType]
	if constructor == nil {
		return nil, nil
	}
	return constructor(providerCfg)
}

func modelListAPIKey(configuredKey, envVar string) string {
	if configuredKey != "" {
		return configuredKey
	}
	return os.Getenv(envVar)
}

func requiredModelListAPIKey(configuredKey, envVar, label string) (string, error) {
	apiKey := modelListAPIKey(configuredKey, envVar)
	if apiKey == "" {
		return "", fmt.Errorf("%s API key not configured. Set %s or configure api_key", label, envVar)
	}
	return apiKey, nil
}

func modelListTimeout(providerType config.ProviderType) time.Duration {
	if spec, ok := providerspec.ByType(providerType); ok && spec.ModelListTimeoutSeconds > 0 {
		return time.Duration(spec.ModelListTimeoutSeconds) * time.Second
	}
	return 10 * time.Second
}

func refreshModelListCaches(providerName string, providerCfg config.ProviderConfig, models []llm.ModelInfo) {
	providerType := config.InferProviderType(providerName, providerCfg.Type)
	switch providerType {
	case config.ProviderTypeOpenRouter:
		llm.RefreshOpenRouterCacheSync(providerCfg.ResolvedAPIKey, models)
	case config.ProviderTypeCursorBin:
		llm.RefreshCursorBinCacheSync(models)
	case config.ProviderTypeGrokBin:
		llm.RefreshGrokBinCacheSync(models)
	case config.ProviderTypeAgyBin:
		llm.RefreshAgyBinCacheSync(models)
	default:
		// Endpoint-defined catalogs (custom OpenAI base URLs, generic
		// OpenAI-compatible servers) are cached per endpoint+credential
		// so shell completion can offer the same list offline.
		if llm.UsesEndpointModelCatalog(providerName, providerCfg) {
			llm.RefreshEndpointCacheSync(providerCfg, models)
		}
	}
}

func printModelList(providerName string, providerType config.ProviderType, models []llm.ModelInfo) error {
	// Pretty print
	fmt.Printf("Available models from %s:\n\n", providerName)

	// Only these providers return or have known pricing info
	providerHasPricing := providerType == config.ProviderTypeOpenRouter || providerType == config.ProviderTypeZen || providerType == config.ProviderTypeNearAI || providerType == config.ProviderTypeSambaNova

	for _, m := range models {
		if m.DisplayName != "" {
			fmt.Printf("  %s (%s)", m.ID, m.DisplayName)
		} else {
			fmt.Printf("  %s", m.ID)
		}

		// Show input limit if known
		if providerType == config.ProviderTypeChatGPT {
			fmt.Print(" " + chatGPTContextSummary(m))
		} else if ctxStr := llm.FormatTokenCount(m.InputLimit); ctxStr != "" {
			fmt.Printf(" [%s input]", ctxStr)
		}
		if len(m.ReasoningEfforts) > 0 {
			fmt.Printf(" (effort: %s)", strings.Join(m.ReasoningEfforts, ", "))
		}

		// Show pricing info only if provider returns it
		if providerHasPricing {
			if m.InputPrice < 0 || m.OutputPrice < 0 {
				fmt.Printf(" [pricing unknown]")
			} else if m.InputPrice == 0 && m.OutputPrice == 0 {
				fmt.Printf(" [FREE]")
			} else {
				fmt.Printf(" [$%.2f/$%.2f per 1M tokens]", m.InputPrice, m.OutputPrice)
			}
		}
		fmt.Println()
		if providerType == config.ProviderTypeChatGPT {
			if detail := chatGPTContextReserveSummary(m); detail != "" {
				fmt.Printf("    %s\n", detail)
			}
		}
	}

	fmt.Printf("\nTo use a model, add to your config:\n")
	fmt.Printf("  providers:\n    %s:\n", providerName)
	fmt.Printf("    model: <model-name>\n")

	return nil
}

// printStaticModels prints a static list of models for providers without a ListModels API
func printStaticModels(providerName string, models []string) error {
	if modelsJSON {
		type staticModel struct {
			ID         string `json:"id"`
			InputLimit int    `json:"input_limit,omitempty"`
		}
		var jsonModels []staticModel
		for _, m := range models {
			jsonModels = append(jsonModels, staticModel{
				ID:         m,
				InputLimit: llm.InputLimitForProviderModel(providerName, m),
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(jsonModels)
	}

	fmt.Printf("Available models for %s:\n\n", providerName)
	for _, m := range models {
		line := "  " + m
		if ctxStr := llm.FormatTokenCount(llm.InputLimitForProviderModel(providerName, m)); ctxStr != "" {
			line += fmt.Sprintf(" [%s input]", ctxStr)
		}
		if variants := llm.EffortVariantsFor(m); len(variants) > 0 {
			line += fmt.Sprintf(" (effort: %s)", strings.Join(variants, ", "))
		}
		fmt.Println(line)
	}
	fmt.Printf("\nTo use a model, add to your config:\n")
	fmt.Printf("  providers:\n    %s:\n", providerName)
	fmt.Printf("      model: <model-name>\n")
	return nil
}

func chatGPTContextSummary(model llm.ModelInfo) string {
	count := func(n int) string {
		if s := llm.FormatTokenCount(n); s != "" {
			return s
		}
		return "unknown"
	}
	return fmt.Sprintf("[context: %s selected, %s recommended, %s max]", count(model.ConfiguredContext), count(model.RecommendedContext), count(model.MaxContext))
}

func chatGPTContextReserveSummary(model llm.ModelInfo) string {
	if model.InputLimit <= 0 || model.ConfiguredContext <= model.InputLimit {
		return ""
	}
	return fmt.Sprintf("Input budget: %s · Output reserve: %s", llm.FormatTokenCount(model.InputLimit), llm.FormatTokenCount(model.ConfiguredContext-model.InputLimit))
}
