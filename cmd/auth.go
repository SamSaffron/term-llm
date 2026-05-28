package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// authProvider describes an OAuth provider we can drive a sign-in flow for.
// Each provider exposes the four hooks the auth subcommands need; we keep
// the registry small (just ChatGPT + Copilot) because API-key providers
// are configured via env vars or `term-llm config set`.
type authProvider struct {
	name     string // user-facing label
	id       string // CLI slug
	exists   func() bool
	clear    func() error
	login    func() error
	describe func() (string, error)
}

func authProviders() []authProvider {
	return []authProvider{
		{
			name:   "ChatGPT (Codex)",
			id:     "chatgpt",
			exists: credentials.ChatGPTCredentialsExist,
			clear:  credentials.ClearChatGPTCredentials,
			login: func() error {
				_, err := llm.PromptForChatGPTAuth()
				return err
			},
			describe: chatgptAuthStatus,
		},
		{
			name:   "GitHub Copilot",
			id:     "copilot",
			exists: credentials.CopilotCredentialsExist,
			clear:  credentials.ClearCopilotCredentials,
			login: func() error {
				_, err := llm.PromptForCopilotAuth()
				return err
			},
			describe: copilotAuthStatus,
		},
	}
}

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Sign in to OAuth providers (ChatGPT, GitHub Copilot)",
	Long: `Manage OAuth credentials for providers that require interactive sign-in.

API-key providers (Anthropic, OpenAI direct, Gemini, etc.) are configured
via environment variables or 'term-llm config set'; they do not appear here.

Examples:
  term-llm auth login                # interactively pick a provider
  term-llm auth login chatgpt
  term-llm auth status
  term-llm auth logout copilot`,
}

var authLoginCmd = &cobra.Command{
	Use:               "login [provider]",
	Short:             "Sign in to an OAuth provider",
	Args:              cobra.MaximumNArgs(1),
	RunE:              runAuthLogin,
	ValidArgsFunction: authProviderCompletion,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show authentication status for OAuth providers",
	Args:  cobra.NoArgs,
	RunE:  runAuthStatus,
}

var authLogoutCmd = &cobra.Command{
	Use:               "logout [provider]",
	Short:             "Clear stored credentials for an OAuth provider",
	Args:              cobra.MaximumNArgs(1),
	RunE:              runAuthLogout,
	ValidArgsFunction: authProviderCompletion,
}

func init() {
	rootCmd.AddCommand(authCmd)
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authLogoutCmd)
}

func runAuthLogin(cmd *cobra.Command, args []string) error {
	p, err := resolveAuthProvider(args, "Sign in to which provider?")
	if err != nil {
		return err
	}
	return p.login()
}

func runAuthLogout(cmd *cobra.Command, args []string) error {
	p, err := resolveAuthProvider(args, "Sign out of which provider?")
	if err != nil {
		return err
	}
	if !p.exists() {
		fmt.Fprintf(cmd.OutOrStdout(), "No %s credentials stored.\n", p.name)
		return nil
	}
	if err := p.clear(); err != nil {
		return fmt.Errorf("clear %s credentials: %w", p.id, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Cleared %s credentials.\n", p.name)
	return nil
}

func runAuthStatus(cmd *cobra.Command, args []string) error {
	for _, p := range authProviders() {
		line, err := p.describe()
		if err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%-18s ERROR: %v\n", p.name, err)
			continue
		}
		fmt.Fprintln(cmd.OutOrStdout(), line)
	}
	return nil
}

// resolveAuthProvider picks a provider from the arg list, falling back to
// an interactive huh picker that shows current sign-in state next to each
// provider. Returns an error if no provider is given and stdin isn't a TTY
// — there's no sensible default since each provider is for a different
// account.
func resolveAuthProvider(args []string, prompt string) (authProvider, error) {
	providers := authProviders()
	if len(args) == 1 {
		id := strings.ToLower(args[0])
		for _, p := range providers {
			if p.id == id {
				return p, nil
			}
		}
		return authProvider{}, fmt.Errorf("unknown provider %q (valid: %s)", args[0], providerIDsCSV(providers))
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return authProvider{}, fmt.Errorf("no provider given and stdin is not a terminal; pass one of: %s", providerIDsCSV(providers))
	}

	options := make([]huh.Option[string], 0, len(providers))
	for _, p := range providers {
		label := p.name
		if p.exists() {
			label += " ✓"
		} else {
			label += " (not signed in)"
		}
		options = append(options, huh.NewOption(label, p.id))
	}
	var chosen string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(prompt).
				Options(options...).
				Value(&chosen),
		),
	)
	if err := form.Run(); err != nil {
		return authProvider{}, err
	}
	for _, p := range providers {
		if p.id == chosen {
			return p, nil
		}
	}
	return authProvider{}, fmt.Errorf("no provider selected")
}

func providerIDsCSV(providers []authProvider) string {
	ids := make([]string, len(providers))
	for i, p := range providers {
		ids[i] = p.id
	}
	return strings.Join(ids, ", ")
}

func authProviderCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) >= 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	providers := authProviders()
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.id)
	}
	return ids, cobra.ShellCompDirectiveNoFileComp
}

func chatgptAuthStatus() (string, error) {
	const label = "ChatGPT (Codex)"
	if !credentials.ChatGPTCredentialsExist() {
		return formatAuthStatusLine(label, "not signed in", ""), nil
	}
	creds, err := credentials.GetChatGPTCredentials()
	if err != nil {
		return "", err
	}
	detail := ""
	if creds.AccountID != "" {
		detail = "account " + creds.AccountID
	}
	return formatAuthStatusLine(label, formatExpiry(creds.ExpiresAt, creds.IsExpired()), detail), nil
}

func copilotAuthStatus() (string, error) {
	const label = "GitHub Copilot"
	if !credentials.CopilotCredentialsExist() {
		return formatAuthStatusLine(label, "not signed in", ""), nil
	}
	creds, err := credentials.GetCopilotCredentials()
	if err != nil {
		return "", err
	}
	return formatAuthStatusLine(label, formatExpiry(creds.ExpiresAt, creds.IsExpired()), ""), nil
}

// formatExpiry renders a human-friendly state string given a Unix expiry
// timestamp. A zero expiry means "no expiry set" (Copilot tokens are
// long-lived). Expired tokens are still considered "signed in" because the
// next API call will refresh them transparently.
func formatExpiry(unix int64, expired bool) string {
	if unix == 0 {
		return "signed in (no expiry)"
	}
	if expired {
		return "expired (will refresh on next use)"
	}
	return "signed in; expires " + time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func formatAuthStatusLine(name, state, detail string) string {
	line := fmt.Sprintf("%-18s %s", name, state)
	if detail != "" {
		line += " (" + detail + ")"
	}
	return line
}
