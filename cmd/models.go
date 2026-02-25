package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
)

var modelsProvider string
var modelsJSON bool

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
  term-llm models --provider ollama     # list models from Ollama
  term-llm models --provider lmstudio   # list models from LM Studio
  term-llm models --json                # output as JSON`,
	RunE: runModels,
}

func init() {
	rootCmd.AddCommand(modelsCmd)
	modelsCmd.Flags().StringVarP(&modelsProvider, "provider", "p", "", "Provider to list models from (anthropic, copilot, openrouter, xai, zen, ollama, lmstudio, openai-compat)")
	modelsCmd.Flags().BoolVar(&modelsJSON, "json", false, "Output as JSON")
	modelsCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion)
}

// ModelLister is an interface for providers that can list available models
type ModelLister interface {
	ListModels(ctx context.Context) ([]llm.ModelInfo, error)
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

	// Get provider config - handle built-in providers that may not be explicitly configured
	providerCfg, ok := cfg.Providers[providerName]

	// Infer provider type - only use config type if provider is configured
	var providerType config.ProviderType
	if ok {
		providerType = config.InferProviderType(providerName, providerCfg.Type)
	} else {
		providerType = config.InferProviderType(providerName, "")
	}

	// Providers that support dynamic model listing
	supportedTypes := map[config.ProviderType]bool{
		config.ProviderTypeAnthropic:    true,
		config.ProviderTypeOpenAI:       true,
		config.ProviderTypeCopilot:      true,
		config.ProviderTypeOpenRouter:   true,
		config.ProviderTypeOpenAICompat: true,
		config.ProviderTypeZen:          true,
		config.ProviderTypeXAI:          true,
		config.ProviderTypeVenice:       true,
	}

	// For built-in providers without explicit config, check if they support dynamic listing
	// or fall back to static model list
	if !ok {
		// Copilot can work without config (uses OAuth)
		if providerType == config.ProviderTypeCopilot {
			// Continue to dynamic listing below
		} else if staticModels, hasStatic := llm.ProviderModels[providerName]; hasStatic {
			return printStaticModels(providerName, staticModels)
		} else {
			return fmt.Errorf("provider '%s' is not configured", providerName)
		}
	}

	if !supportedTypes[providerType] {
		// Fall back to static model list if available
		if staticModels, hasStatic := llm.ProviderModels[providerName]; hasStatic {
			return printStaticModels(providerName, staticModels)
		}
		return fmt.Errorf("provider '%s' (type: %s) does not support model listing.\n"+
			"Model listing is supported for: anthropic, openrouter, xai, zen, copilot, and openai_compatible providers", providerName, providerType)
	}

	// Create provider to query models
	var lister ModelLister
	switch providerType {
	case config.ProviderTypeAnthropic:
		provider, err := llm.NewAnthropicProvider(providerCfg.ResolvedAPIKey, providerCfg.Model, providerCfg.Credentials)
		if err != nil {
			return fmt.Errorf("anthropic: %w", err)
		}
		lister = provider
	case config.ProviderTypeOpenAI:
		if providerCfg.ResolvedAPIKey == "" {
			return fmt.Errorf("openai API key not configured. Set OPENAI_API_KEY or configure api_key")
		}
		lister = llm.NewOpenAIProvider(providerCfg.ResolvedAPIKey, providerCfg.Model)
	case config.ProviderTypeCopilot:
		// Copilot uses OAuth - create provider which will prompt for auth if needed
		model := ""
		if ok {
			model = providerCfg.Model
		}
		provider, err := llm.NewCopilotProvider(model)
		if err != nil {
			return fmt.Errorf("copilot provider: %w", err)
		}
		lister = provider
	case config.ProviderTypeOpenRouter:
		if providerCfg.ResolvedAPIKey == "" {
			return fmt.Errorf("openrouter API key not configured. Set OPENROUTER_API_KEY or configure api_key")
		}
		lister = llm.NewOpenRouterProvider(providerCfg.ResolvedAPIKey, "", providerCfg.AppURL, providerCfg.AppTitle)
	case config.ProviderTypeOpenAICompat:
		if providerCfg.BaseURL == "" {
			return fmt.Errorf("provider '%s' requires base_url to be configured", providerName)
		}
		lister = llm.NewOpenAICompatProvider(providerCfg.BaseURL, providerCfg.ResolvedAPIKey, "", providerName)
	case config.ProviderTypeZen:
		lister = llm.NewZenProvider(providerCfg.ResolvedAPIKey, "")
	case config.ProviderTypeXAI:
		apiKey := providerCfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("XAI_API_KEY")
		}
		if apiKey == "" {
			return fmt.Errorf("xAI API key not configured. Set XAI_API_KEY or configure api_key")
		}
		lister = llm.NewXAIProvider(apiKey, providerCfg.Model)
	case config.ProviderTypeVenice:
		apiKey := providerCfg.ResolvedAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("VENICE_API_KEY")
		}
		if apiKey == "" {
			return fmt.Errorf("Venice API key not configured. Set VENICE_API_KEY or configure api_key")
		}
		lister = llm.NewVeniceProvider(apiKey, providerCfg.Model)
	}

	// Copilot may need interactive device auth (up to 5 minutes)
	timeout := 10 * time.Second
	if providerType == config.ProviderTypeCopilot {
		timeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	models, err := lister.ListModels(ctx)
	if err == nil && providerType == config.ProviderTypeOpenRouter {
		llm.RefreshOpenRouterCacheSync(providerCfg.ResolvedAPIKey, models)
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

	// Pretty print
	fmt.Printf("Available models from %s:\n\n", providerName)

	// Only these providers return actual pricing info
	providerHasPricing := providerType == config.ProviderTypeOpenRouter || providerType == config.ProviderTypeZen

	for _, m := range models {
		if m.DisplayName != "" {
			fmt.Printf("  %s (%s)", m.ID, m.DisplayName)
		} else {
			fmt.Printf("  %s", m.ID)
		}

		// Show input limit if known
		if ctxStr := llm.FormatTokenCount(m.InputLimit); ctxStr != "" {
			fmt.Printf(" [%s input]", ctxStr)
		}

		// Show pricing info only if provider returns it
		if providerHasPricing {
			if m.InputPrice == 0 && m.OutputPrice == 0 {
				fmt.Printf(" [FREE]")
			} else {
				fmt.Printf(" [$%.2f/$%.2f per 1M tokens]", m.InputPrice, m.OutputPrice)
			}
		}
		fmt.Println()
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
