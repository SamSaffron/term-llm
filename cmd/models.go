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
	modelsCmd.Flags().StringVarP(&modelsProvider, "provider", "p", "", "Provider to list models from (anthropic, openrouter, ollama, lmstudio, openai-compat)")
	modelsCmd.Flags().BoolVar(&modelsJSON, "json", false, "Output as JSON")
}

// ModelLister is an interface for providers that can list available models
type ModelLister interface {
	ListModels(ctx context.Context) ([]llm.ModelInfo, error)
}

func runModels(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Determine which provider to query
	providerName := modelsProvider
	if providerName == "" {
		providerName = cfg.DefaultProvider
	}

	// Get provider config
	providerCfg, ok := cfg.Providers[providerName]
	if !ok {
		return fmt.Errorf("provider '%s' is not configured", providerName)
	}

	providerType := config.InferProviderType(providerName, providerCfg.Type)

	// Validate provider supports model listing
	supportedTypes := map[config.ProviderType]bool{
		config.ProviderTypeAnthropic:    true,
		config.ProviderTypeOpenRouter:   true,
		config.ProviderTypeOpenAICompat: true,
	}

	if !supportedTypes[providerType] {
		return fmt.Errorf("provider '%s' (type: %s) does not support model listing.\n"+
			"Model listing is supported for: anthropic, openrouter, and openai_compatible providers", providerName, providerType)
	}

	// Create provider to query models
	var lister ModelLister
	switch providerType {
	case config.ProviderTypeAnthropic:
		if providerCfg.ResolvedAPIKey == "" {
			return fmt.Errorf("anthropic API key not configured. Set ANTHROPIC_API_KEY or configure credentials")
		}
		lister = llm.NewAnthropicProvider(providerCfg.ResolvedAPIKey, providerCfg.Model)
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
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	models, err := lister.ListModels(ctx)
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
	for _, m := range models {
		if m.DisplayName != "" {
			fmt.Printf("  %s (%s)\n", m.ID, m.DisplayName)
		} else {
			fmt.Printf("  %s\n", m.ID)
		}
		if m.Created > 0 {
			t := time.Unix(m.Created, 0)
			fmt.Printf("    Released: %s\n", t.Format("2006-01-02"))
		}
	}

	fmt.Printf("\nTo use a model, add to your config:\n")
	fmt.Printf("  providers:\n    %s:\n", providerName)
	fmt.Printf("    model: <model-name>\n")

	return nil
}
