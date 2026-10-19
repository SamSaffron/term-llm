package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/providers"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

var providersOptions providers.Options

// ProviderInfo describes a provider for external consumption.
type ProviderInfo = providers.Info

var providersCmd = &cobra.Command{
	Use:   "providers [name]",
	Short: "List available LLM providers",
	Long: `List available LLM providers and their configuration details.

This command shows built-in providers, their credential requirements,
and available models. Useful for scripting and third-party integrations.

Examples:
  term-llm providers                    # list all providers
  term-llm providers --json             # JSON output for scripting
  term-llm providers --builtin          # only built-in providers
  term-llm providers --configured       # only configured providers
  term-llm providers anthropic          # details for specific provider`,
	Args: cobra.MaximumNArgs(1),
	RunE: runProviders,
}

func init() {
	rootCmd.AddCommand(providersCmd)
	providersCmd.Flags().BoolVar(&providersOptions.JSON, "json", false, "Output as JSON")
	providersCmd.Flags().BoolVar(&providersOptions.Configured, "configured", false, "Show only configured providers")
	providersCmd.Flags().BoolVar(&providersOptions.Builtin, "builtin", false, "Show only built-in providers")
}

func runProviders(_ *cobra.Command, args []string) error {
	return providers.Run(providersOptions, args)
}

func buildProviderListWith(cfg *config.Config, hasLocalCredentials func(string) bool) []ProviderInfo {
	return providers.BuildListWith(cfg, hasLocalCredentials)
}
