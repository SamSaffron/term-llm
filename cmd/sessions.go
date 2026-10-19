package cmd

import (
	"fmt"
	"time"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	sessionsapp "github.com/samsaffron/term-llm/internal/app/sessions"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage chat sessions",
	Long: `List, search, show, delete, and export chat sessions.

Sessions are identified by sequential numbers (#1, #2, #3...) shown in listings.
You can also use the full session ID or a unique prefix.

Examples:
  term-llm sessions                       # List recent sessions
  term-llm sessions list --provider anthropic
  term-llm sessions search "kubernetes"
  term-llm sessions show 42               # By number
  term-llm sessions show #42              # By number (explicit)
  term-llm sessions delete 42
  term-llm sessions share 42 --visibility unlisted
  term-llm sessions export 42 [path.md]`,
	RunE: runSessionsList, // Default to list
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions",
	RunE:  runSessionsList,
}

var sessionsSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search sessions",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runSessionsSearch,
}

var sessionsShowCmd = &cobra.Command{
	Use:   "show <number|id>",
	Short: "Show session details",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsShow,
}

var sessionsDeleteCmd = &cobra.Command{
	Use:   "delete <number|id>",
	Short: "Delete a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsDelete,
}

var sessionsExportCmd = &cobra.Command{
	Use:   "export <number|id> [path]",
	Short: "Export session as Markdown or HTML (.html output filename)",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runSessionsExport,
}

var sessionsShareCmd = &cobra.Command{
	Use:   "share <number|id>",
	Short: "Share a complete saved session",
	Long: `Share a complete saved session through the configured provider.

Raw model reasoning is excluded unless --include-raw-reasoning is explicitly
supplied. GitHub's default unlisted Gists are not private: anyone with the link
can view them. GitHub sharing requires an authenticated gh CLI account.`,
	Args:         cobra.ExactArgs(1),
	RunE:         runSessionsShare,
	SilenceUsage: true,
}

var sessionsResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Delete all sessions (requires confirmation)",
	Long: `Delete the sessions database entirely. This cannot be undone.

You must type 'yes' to confirm, or pass --yes for non-interactive use.`,
	RunE: runSessionsReset,
}

var sessionsNameCmd = &cobra.Command{
	Use:   "name <number|id> <name>",
	Short: "Set a custom name for a session",
	Long: `Set a custom name for a session. The name is displayed instead of
the auto-generated summary in listings.

Example:
  term-llm sessions name 42 "Auth refactor"`,
	Args: cobra.ExactArgs(2),
	RunE: runSessionsName,
}

var sessionsTagCmd = &cobra.Command{
	Use:   "tag <number|id> <tags...>",
	Short: "Add tags to a session",
	Long: `Add one or more tags to a session. Tags can be used to organize
and filter sessions.

Example:
  term-llm sessions tag 42 bug feature`,
	Args: cobra.MinimumNArgs(2),
	RunE: runSessionsTag,
}

var sessionsUntagCmd = &cobra.Command{
	Use:   "untag <number|id> <tags...>",
	Short: "Remove tags from a session",
	Long: `Remove one or more tags from a session.

Example:
  term-llm sessions untag 42 bug`,
	Args: cobra.MinimumNArgs(2),
	RunE: runSessionsUntag,
}

var sessionsBrowseCmd = &cobra.Command{
	Use:   "browse",
	Short: "Browse sessions interactively",
	Long: `Open an interactive TUI for browsing, searching, and inspecting sessions.

Key bindings:
  j/k or arrows  Navigate up/down
  enter          Open session inspector
  /              Search sessions
  ctrl+f         Toggle FTS (full-text search)
  s              Cycle sort order
  f              Cycle status filter
  d              Delete session (with confirmation)
  q/esc          Quit`,
	RunE: runSessionsBrowse,
}

var sessionsExportGistCmd = &cobra.Command{
	Use:   "gist <number|id>",
	Short: "Export session to GitHub Gist",
	Long: `Export a session to a GitHub Gist for sharing.

Requires the gh CLI to be installed and authenticated.
Creates index.html and session.md in a new secret Gist by default. Secret Gists
are unlisted, not private: anyone with the URL can view them. The primary rich
transcript link is rendered through https://gisthost.github.io/.

Examples:
  term-llm sessions export gist 42
  term-llm sessions export gist 42 --public
  term-llm sessions export gist 42 --include-system`,
	Args:         cobra.ExactArgs(1),
	RunE:         runSessionsExportGist,
	SilenceUsage: true,
}

// Flags
var (
	sessionsProvider                  string
	sessionsLimit                     int
	sessionsJSON                      bool
	sessionsStatus                    string
	sessionsMode                      string
	sessionsTag                       string
	sessionsExportIncludeSystem       bool
	sessionsExportIncludeAudio        bool
	sessionsExportIncludeReasoning    bool
	sessionsExportIncludeRawReasoning bool
	sessionsGistPublic                bool
	sessionsGistIncludeSystem         bool
	sessionsGistIncludeReasoning      bool
	sessionsGistIncludeRawReasoning   bool
	sessionsShareVisibility           string
	sessionsShareNew                  bool
	sessionsShareJSON                 bool
	sessionsShareIncludeRawReasoning  bool
	sessionsShareIncludeAudio         bool
	sessionsShareNoImages             bool
	sessionsShareNoTools              bool
	sessionsResetYes                  bool
)

func init() {
	// List flags
	sessionsListCmd.Flags().StringVar(&sessionsProvider, "provider", "", "Filter by provider")
	sessionsListCmd.Flags().IntVar(&sessionsLimit, "limit", 20, "Maximum number of sessions to list")
	sessionsListCmd.Flags().StringVar(&sessionsStatus, "status", "", "Filter by status (active, complete, error, interrupted)")
	sessionsListCmd.Flags().StringVar(&sessionsMode, "mode", "", "Filter by mode (chat, ask, plan, exec)")
	sessionsListCmd.Flags().StringVar(&sessionsTag, "tag", "", "Filter by tag")

	// Show flags
	sessionsShowCmd.Flags().BoolVar(&sessionsJSON, "json", false, "Output as JSON")

	// Local transcript export flags
	sessionsExportCmd.Flags().BoolVar(&sessionsExportIncludeSystem, "include-system", false, "Include system prompt in export")
	sessionsExportCmd.Flags().BoolVar(&sessionsExportIncludeAudio, "include-audio", false, "Include short audio in HTML exports (recordings and metadata copied as-is)")
	sessionsExportCmd.Flags().BoolVar(&sessionsExportIncludeReasoning, "include-reasoning", false, "Include provider reasoning summaries in export")
	sessionsExportCmd.Flags().BoolVar(&sessionsExportIncludeRawReasoning, "include-raw-reasoning", false, "Include raw reasoning when reasoning.raw is enabled")

	// Share flags
	sessionsShareCmd.Flags().StringVar(&sessionsShareVisibility, "visibility", "", "Share visibility: public, unlisted, or private (default: provider default)")
	sessionsShareCmd.Flags().BoolVar(&sessionsShareNew, "new", false, "Create a new share instead of updating a compatible session share")
	sessionsShareCmd.Flags().BoolVar(&sessionsShareJSON, "json", false, "Output the share result as JSON")
	sessionsShareCmd.Flags().BoolVar(&sessionsShareIncludeRawReasoning, "include-raw-reasoning", false, "Include raw model reasoning (privacy-sensitive; requires reasoning.raw/source permission)")
	sessionsShareCmd.Flags().BoolVar(&sessionsShareIncludeAudio, "include-audio", false, "Include short audio (privacy-sensitive; recordings and metadata copied as-is)")
	sessionsShareCmd.Flags().BoolVar(&sessionsShareNoImages, "no-images", false, "Omit images (uploads, generated images, and shown images) from the share")
	sessionsShareCmd.Flags().BoolVar(&sessionsShareNoTools, "no-tools", false, "Omit tool calls and tool output (which can contain secrets) from the share")

	// Reset flags
	sessionsResetCmd.Flags().BoolVarP(&sessionsResetYes, "yes", "y", false, "Delete without interactive confirmation")

	// Gist export flags
	sessionsExportGistCmd.Flags().BoolVar(&sessionsGistPublic, "public", false, "Create a public Gist (default: secret/unlisted, not private)")
	sessionsExportGistCmd.Flags().BoolVar(&sessionsGistIncludeSystem, "include-system", false, "Include system prompt in export")
	sessionsExportGistCmd.Flags().BoolVar(&sessionsGistIncludeReasoning, "include-reasoning", false, "Include provider reasoning summaries in export")
	sessionsExportGistCmd.Flags().BoolVar(&sessionsGistIncludeRawReasoning, "include-raw-reasoning", false, "Include raw reasoning when reasoning.raw is enabled")

	// Add subcommands
	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsSearchCmd)
	sessionsCmd.AddCommand(sessionsShowCmd)
	sessionsCmd.AddCommand(sessionsDeleteCmd)
	sessionsCmd.AddCommand(sessionsExportCmd)
	sessionsCmd.AddCommand(sessionsShareCmd)
	sessionsCmd.AddCommand(sessionsResetCmd)
	sessionsCmd.AddCommand(sessionsNameCmd)
	sessionsCmd.AddCommand(sessionsTagCmd)
	sessionsCmd.AddCommand(sessionsUntagCmd)
	sessionsCmd.AddCommand(sessionsBrowseCmd)

	// Add gist subcommand to export
	sessionsExportCmd.AddCommand(sessionsExportGistCmd)

	rootCmd.AddCommand(sessionsCmd)
}

// sessionsApp binds the flag variables and cmd-level helpers to the
// implementation in internal/app/sessions.
func sessionsApp() sessionsapp.App {
	return sessionsapp.App{
		Options: sessionsapp.Options{
			Provider:                  sessionsProvider,
			Limit:                     sessionsLimit,
			Status:                    sessionsStatus,
			Mode:                      sessionsMode,
			TagFilter:                 sessionsTag,
			JSON:                      sessionsJSON,
			ExportIncludeSystem:       sessionsExportIncludeSystem,
			ExportIncludeAudio:        sessionsExportIncludeAudio,
			ExportIncludeReasoning:    sessionsExportIncludeReasoning,
			ExportIncludeRawReasoning: sessionsExportIncludeRawReasoning,
			GistPublic:                sessionsGistPublic,
			GistIncludeSystem:         sessionsGistIncludeSystem,
			GistIncludeReasoning:      sessionsGistIncludeReasoning,
			GistIncludeRawReasoning:   sessionsGistIncludeRawReasoning,
			ShareVisibility:           sessionsShareVisibility,
			ShareNew:                  sessionsShareNew,
			ShareJSON:                 sessionsShareJSON,
			ShareIncludeRawReasoning:  sessionsShareIncludeRawReasoning,
			ShareIncludeAudio:         sessionsShareIncludeAudio,
			ShareNoImages:             sessionsShareNoImages,
			ShareNoTools:              sessionsShareNoTools,
			ResetYes:                  sessionsResetYes,
			AutotitleDryRun:           sessionsAutotitleDryRun,
			AutotitleForce:            sessionsAutotitleForce,
			AutotitleMinAge:           sessionsAutotitleMinAge,
			AutotitleVerbose:          sessionsAutotitleVerbose,
		},
		Deps: sessionsapp.Deps{
			LoadConfig: loadConfig,
			SessionStoreConfig: func(cfg *config.Config) session.Config {
				return appruntime.SessionStoreConfigWith(cfg, appruntime.SessionStoreOverrides{NoSession: noSession, DBPath: sessionDBPath})
			},
			StartInteractiveDiagnostics: startInteractiveDiagnostics,
			ResumeChat:                  resumeChatSession,
			NewSharePublisher:           newSessionSharePublisher,
		},
	}
}

// resumeChatSession runs `chat --resume <id>` after the session browser exits.
func resumeChatSession(sessionID string) error {
	chatResume = sessionID
	// Mark the flag as changed so runChat processes it
	if err := chatCmd.Flags().Set("resume", chatResume); err != nil {
		return fmt.Errorf("failed to set resume flag: %w", err)
	}
	return runChat(chatCmd, nil)
}

func runSessionsList(cmd *cobra.Command, args []string) error {
	return sessionsApp().List(cmd, args)
}
func runSessionsSearch(cmd *cobra.Command, args []string) error {
	return sessionsApp().Search(cmd, args)
}
func runSessionsShow(cmd *cobra.Command, args []string) error {
	return sessionsApp().Show(cmd, args)
}
func runSessionsDelete(cmd *cobra.Command, args []string) error {
	return sessionsApp().Delete(cmd, args)
}
func runSessionsExport(cmd *cobra.Command, args []string) error {
	return sessionsApp().Export(cmd, args)
}
func runSessionsReset(cmd *cobra.Command, args []string) error {
	return sessionsApp().Reset(cmd, args)
}
func runSessionsName(cmd *cobra.Command, args []string) error {
	return sessionsApp().Name(cmd, args)
}
func runSessionsTag(cmd *cobra.Command, args []string) error {
	return sessionsApp().Tag(cmd, args)
}
func runSessionsUntag(cmd *cobra.Command, args []string) error {
	return sessionsApp().Untag(cmd, args)
}
func runSessionsBrowse(cmd *cobra.Command, args []string) error {
	return sessionsApp().Browse(cmd, args)
}
func runSessionsExportGist(cmd *cobra.Command, args []string) error {
	return sessionsApp().ExportGist(cmd, args)
}

// Helpers shared with other cmd files.
func parseTags(tags string) []string        { return sessionsapp.ParseTags(tags) }
func formatRelativeTime(t time.Time) string { return sessionsapp.FormatRelativeTime(t) }
