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
	provider := modelsProvider
	if provider == "" {
		provider = cfg.Provider
	}

	// Validate provider supports model listing
	supportedProviders := map[string]bool{
		"anthropic":     true,
		"openrouter":    true,
		"ollama":        true,
		"lmstudio":      true,
		"openai-compat": true,
	}

	if !supportedProviders[provider] {
		return fmt.Errorf("provider '%s' does not support model listing.\n"+
			"Model listing is supported for: anthropic, openrouter, ollama, lmstudio, openai-compat", provider)
	}

	// Create provider to query models
	var lister ModelLister
	switch provider {
	case "anthropic":
		if cfg.Anthropic.APIKey == "" {
			return fmt.Errorf("anthropic API key not configured. Set ANTHROPIC_API_KEY or configure credentials")
		}
		lister = llm.NewAnthropicProvider(cfg.Anthropic.APIKey, cfg.Anthropic.Model)
	case "openrouter":
		if cfg.OpenRouter.APIKey == "" {
			return fmt.Errorf("openrouter API key not configured. Set OPENROUTER_API_KEY or configure api_key")
		}
		lister = llm.NewOpenRouterProvider(cfg.OpenRouter.APIKey, "", cfg.OpenRouter.AppURL, cfg.OpenRouter.AppTitle)
	case "ollama":
		baseURL := cfg.Ollama.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434/v1"
		}
		lister = llm.NewOpenAICompatProvider(baseURL, cfg.Ollama.APIKey, "", "Ollama")
	case "lmstudio":
		baseURL := cfg.LMStudio.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:1234/v1"
		}
		lister = llm.NewOpenAICompatProvider(baseURL, cfg.LMStudio.APIKey, "", "LM Studio")
	case "openai-compat":
		if cfg.OpenAICompat.BaseURL == "" {
			return fmt.Errorf("openai-compat requires base_url to be configured")
		}
		lister = llm.NewOpenAICompatProvider(cfg.OpenAICompat.BaseURL, cfg.OpenAICompat.APIKey, "", "OpenAI-Compatible")
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
				"For LM Studio: start LM Studio and enable the server", provider)
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
	fmt.Printf("Available models from %s:\n\n", provider)
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
	fmt.Printf("  %s:\n", provider)
	fmt.Printf("    model: <model-name>\n")

	return nil
}
