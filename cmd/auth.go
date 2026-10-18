package cmd

import (
	authapp "github.com/samsaffron/term-llm/internal/app/auth"
	"github.com/spf13/cobra"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Sign in to OAuth providers (ChatGPT, Grok, GitHub Copilot)",
	Long: `Manage OAuth credentials for providers that require interactive sign-in.

API-key providers (Anthropic, OpenAI direct, Gemini, etc.) are configured
via environment variables or 'term-llm config set'; they do not appear here.

Examples:
  term-llm auth login                # interactively pick a provider
  term-llm auth login chatgpt
  term-llm auth login grok
  term-llm auth status
  term-llm auth logout copilot`,
}

var authLoginCmd = &cobra.Command{
	Use:               "login [provider]",
	Short:             "Sign in to an OAuth provider",
	Args:              cobra.MaximumNArgs(1),
	RunE:              authapp.RunLogin,
	ValidArgsFunction: authapp.ProviderCompletion,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show authentication status for OAuth providers",
	Args:  cobra.NoArgs,
	RunE:  authapp.RunStatus,
}

var authLogoutCmd = &cobra.Command{
	Use:               "logout [provider]",
	Short:             "Clear stored credentials for an OAuth provider",
	Args:              cobra.MaximumNArgs(1),
	RunE:              authapp.RunLogout,
	ValidArgsFunction: authapp.ProviderCompletion,
}

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authLogoutCmd)
}
