package cmd

import (
	"strings"

	modelsapp "github.com/samsaffron/term-llm/internal/app/models"
	"github.com/spf13/cobra"
)

var modelsOptions = modelsapp.Options{LoadConfig: loadConfigWithSetup}

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
	RunE: modelsOptions.Run,
}

func init() {
	rootCmd.AddCommand(modelsCmd)
	modelsCmd.Flags().StringVarP(&modelsOptions.Provider, "provider", "p", "", "Provider to list models from ("+strings.Join(modelsapp.SupportedModelListProviderTypes(), ", ")+")")
	modelsCmd.Flags().BoolVar(&modelsOptions.JSON, "json", false, "Output as JSON")
	modelsCmd.Flags().BoolVar(&modelsOptions.Refresh, "refresh", false, "Force a fresh ChatGPT model catalog fetch (report fetch errors)")
	modelsCmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion)
}
