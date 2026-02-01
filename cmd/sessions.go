package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/sessions"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	Short: "Export session as markdown",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runSessionsExport,
}

var sessionsResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Delete all sessions (requires confirmation)",
	Long: `Delete the sessions database entirely. This cannot be undone.

You must type 'yes' to confirm.`,
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
Creates a new gist with the session exported as pretty markdown.

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
	sessionsProvider          string
	sessionsLimit             int
	sessionsJSON              bool
	sessionsStatus            string
	sessionsMode              string
	sessionsTag               string
	sessionsGistPublic        bool
	sessionsGistIncludeSystem bool
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

	// Gist export flags
	sessionsExportGistCmd.Flags().BoolVar(&sessionsGistPublic, "public", false, "Create a public gist (default: private)")
	sessionsExportGistCmd.Flags().BoolVar(&sessionsGistIncludeSystem, "include-system", false, "Include system prompt in export")

	// Add subcommands
	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsSearchCmd)
	sessionsCmd.AddCommand(sessionsShowCmd)
	sessionsCmd.AddCommand(sessionsDeleteCmd)
	sessionsCmd.AddCommand(sessionsExportCmd)
	sessionsCmd.AddCommand(sessionsResetCmd)
	sessionsCmd.AddCommand(sessionsNameCmd)
	sessionsCmd.AddCommand(sessionsTagCmd)
	sessionsCmd.AddCommand(sessionsUntagCmd)
	sessionsCmd.AddCommand(sessionsBrowseCmd)

	// Add gist subcommand to export
	sessionsExportCmd.AddCommand(sessionsExportGistCmd)

	rootCmd.AddCommand(sessionsCmd)
}

func getSessionStore() (session.Store, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	if !cfg.Sessions.Enabled {
		return nil, fmt.Errorf("session storage is disabled in config")
	}

	return session.NewStore(session.Config{
		Enabled:    cfg.Sessions.Enabled,
		MaxAgeDays: cfg.Sessions.MaxAgeDays,
		MaxCount:   cfg.Sessions.MaxCount,
	})
}

func runSessionsList(cmd *cobra.Command, args []string) error {
	// Validate status if provided
	if sessionsStatus != "" {
		validStatuses := []string{"active", "complete", "error", "interrupted"}
		if !slices.Contains(validStatuses, sessionsStatus) {
			return fmt.Errorf("invalid status %q: must be one of %v", sessionsStatus, validStatuses)
		}
	}

	// Validate mode if provided
	if sessionsMode != "" {
		validModes := []string{"chat", "ask", "plan", "exec"}
		if !slices.Contains(validModes, sessionsMode) {
			return fmt.Errorf("invalid mode %q: must be one of %v", sessionsMode, validModes)
		}
	}

	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	summaries, err := store.List(ctx, session.ListOptions{
		Provider: sessionsProvider,
		Mode:     session.SessionMode(sessionsMode),
		Status:   session.SessionStatus(sessionsStatus),
		Tag:      sessionsTag,
		Limit:    sessionsLimit,
	})
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	// Header line
	fmt.Printf("%4s %-25s %4s %5s %5s %-11s %-8s %s\n",
		"#", "SUMMARY", "MSGS", "TURNS", "TOOLS", "TOKENS", "STATUS", "AGE")
	fmt.Println(strings.Repeat("-", 100))

	for _, s := range summaries {
		summary := s.Summary
		if s.Name != "" {
			summary = s.Name
		}
		if len(summary) > 25 {
			summary = summary[:22] + "..."
		}

		// Format tokens as "input/output" in k format
		tokens := formatSessionTokens(s.InputTokens, s.OutputTokens)

		// Format status
		status := string(s.Status)
		if status == "" {
			status = "active"
		}

		age := formatRelativeTime(s.UpdatedAt)

		// MSGS shows actual message count (MessageCount), TURNS shows LLM API round-trips
		fmt.Printf("%4d %-25s %4d %5d %5d %-11s %-8s %s\n",
			s.Number, summary, s.MessageCount, s.LLMTurns, s.ToolCalls, tokens, status, age)
	}

	return nil
}

// formatSessionTokens formats input/output tokens in compact form
func formatSessionTokens(input, output int) string {
	if input == 0 && output == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", formatSessionCount(input), formatSessionCount(output))
}

// formatSessionCount formats a number in compact form (e.g., 1k, 1.2k, 3.4M)
func formatSessionCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		val := float64(n) / 1000
		if val == float64(int(val)) {
			return fmt.Sprintf("%dk", int(val))
		}
		return fmt.Sprintf("%.1fk", val)
	}
	val := float64(n) / 1000000
	if val == float64(int(val)) {
		return fmt.Sprintf("%dM", int(val))
	}
	return fmt.Sprintf("%.1fM", val)
}

func runSessionsSearch(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	query := strings.Join(args, " ")
	ctx := context.Background()
	results, err := store.Search(ctx, query, 20)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Printf("No results found for '%s'\n", query)
		return nil
	}

	fmt.Printf("Found %d matches for '%s':\n\n", len(results), query)
	for _, r := range results {
		name := r.SessionName
		if name == "" {
			name = fmt.Sprintf("#%d", r.SessionNumber)
		}
		fmt.Printf("**%s** (%s)\n", name, r.Provider)
		fmt.Printf("  %s\n\n", r.Snippet)
	}

	return nil
}

func runSessionsShow(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}

	if sessionsJSON {
		data := struct {
			Session  *session.Session  `json:"session"`
			Messages []session.Message `json:"messages"`
		}{
			Session:  sess,
			Messages: messages,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	// Text output
	fmt.Printf("Session: %s\n", sess.ID)
	if sess.Name != "" {
		fmt.Printf("Name: %s\n", sess.Name)
	}
	fmt.Printf("Provider: %s\n", sess.Provider)
	fmt.Printf("Model: %s\n", sess.Model)
	mode := string(sess.Mode)
	if mode == "" {
		mode = "chat"
	}
	fmt.Printf("Mode: %s\n", mode)
	fmt.Printf("Created: %s\n", sess.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Updated: %s\n", sess.UpdatedAt.Format(time.RFC3339))
	if sess.CWD != "" {
		fmt.Printf("CWD: %s\n", sess.CWD)
	}
	fmt.Printf("Messages: %d\n", len(messages))

	// Show metrics
	status := string(sess.Status)
	if status == "" {
		status = "active"
	}
	fmt.Printf("Status: %s\n", status)
	fmt.Printf("User Turns: %d\n", sess.UserTurns)
	fmt.Printf("LLM Turns: %d\n", sess.LLMTurns)
	fmt.Printf("Tool Calls: %d\n", sess.ToolCalls)
	fmt.Printf("Tokens: %s (input: %d, output: %d)\n",
		formatSessionTokens(sess.InputTokens, sess.OutputTokens),
		sess.InputTokens, sess.OutputTokens)
	if sess.Tags != "" {
		fmt.Printf("Tags: %s\n", sess.Tags)
	}
	fmt.Println()

	for _, msg := range messages {
		role := string(msg.Role)
		if msg.Role == "user" {
			role = "❯"
		} else if msg.Role == "assistant" {
			role = "🤖"
		}
		content := msg.TextContent
		if len(content) > 200 {
			content = content[:197] + "..."
		}
		fmt.Printf("%s %s\n\n", role, content)
	}

	return nil
}

func runSessionsDelete(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	// Look up session by prefix first to get full ID
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	if err := store.Delete(ctx, sess.ID); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	fmt.Printf("Deleted session: #%d\n", sess.Number)
	return nil
}

func runSessionsExport(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}

	// Determine output path
	var outputPath string
	if len(args) > 1 {
		outputPath = args[1]
	} else {
		name := sess.Name
		if name == "" {
			name = fmt.Sprintf("session-%d", sess.Number)
		}
		outputPath = fmt.Sprintf("%s.md", name)
	}

	// Build markdown
	var b strings.Builder
	b.WriteString("# Chat Export\n\n")
	b.WriteString(fmt.Sprintf("**Session:** %s\n", sess.ID))
	if sess.Name != "" {
		b.WriteString(fmt.Sprintf("**Name:** %s\n", sess.Name))
	}
	b.WriteString(fmt.Sprintf("**Provider:** %s\n", sess.Provider))
	b.WriteString(fmt.Sprintf("**Model:** %s\n", sess.Model))
	b.WriteString(fmt.Sprintf("**Created:** %s\n", sess.CreatedAt.Format(time.RFC3339)))
	b.WriteString("\n---\n\n")

	for _, msg := range messages {
		if msg.Role == "user" {
			b.WriteString("## ❯\n\n")
		} else {
			b.WriteString("## 🤖 Assistant\n\n")
		}
		b.WriteString(msg.TextContent)
		b.WriteString("\n\n---\n\n")
	}

	if err := os.WriteFile(outputPath, []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	fmt.Printf("Exported %d messages to %s\n", len(messages), outputPath)
	return nil
}

// formatRelativeTime returns a human-readable relative time string
func formatRelativeTime(t time.Time) string {
	dur := time.Since(t)
	switch {
	case dur < time.Minute:
		return "just now"
	case dur < time.Hour:
		return fmt.Sprintf("%dm ago", int(dur.Minutes()))
	case dur < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(dur.Hours()))
	case dur < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(dur.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

func runSessionsReset(cmd *cobra.Command, args []string) error {
	dbPath, err := session.GetDBPath()
	if err != nil {
		return fmt.Errorf("failed to get database path: %w", err)
	}

	// Check if database exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Println("No sessions database found.")
		return nil
	}

	// Require confirmation
	fmt.Printf("This will delete ALL sessions at:\n  %s\n\n", dbPath)
	fmt.Print("Type 'yes' to confirm: ")

	var response string
	if _, err := fmt.Scanln(&response); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if response != "yes" {
		fmt.Println("Aborted.")
		return nil
	}

	// Delete the database file and WAL files
	filesToDelete := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
	}

	for _, f := range filesToDelete {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to delete %s: %w", f, err)
		}
	}

	fmt.Println("Sessions database deleted.")
	return nil
}

func runSessionsName(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	sess.Name = args[1]
	if err := store.Update(ctx, sess); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	fmt.Printf("Session #%d named: %s\n", sess.Number, sess.Name)
	return nil
}

func runSessionsTag(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	// Parse existing tags
	existingTags := parseTags(sess.Tags)

	// Add new tags (normalize and avoid duplicates)
	addedTags := []string{}
	for _, tag := range args[1:] {
		normalized := normalizeTag(tag)
		if normalized == "" {
			continue // Skip empty tags
		}
		if !containsTag(existingTags, normalized) {
			existingTags = append(existingTags, normalized)
			addedTags = append(addedTags, normalized)
		}
	}

	if len(addedTags) == 0 {
		fmt.Println("No new tags to add (all already present or empty).")
		return nil
	}

	sess.Tags = strings.Join(existingTags, ",")
	if err := store.Update(ctx, sess); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	fmt.Printf("Added tags to session #%d: %s\n", sess.Number, strings.Join(addedTags, ", "))
	return nil
}

func runSessionsUntag(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	// Parse existing tags (already normalized)
	existingTags := parseTags(sess.Tags)

	// Normalize tags to remove
	tagsToRemove := make([]string, 0, len(args[1:]))
	for _, t := range args[1:] {
		if nt := normalizeTag(t); nt != "" {
			tagsToRemove = append(tagsToRemove, nt)
		}
	}

	// Remove specified tags
	removedTags := []string{}
	newTags := []string{}
	for _, tag := range existingTags {
		if slices.Contains(tagsToRemove, tag) {
			removedTags = append(removedTags, tag)
		} else {
			newTags = append(newTags, tag)
		}
	}

	if len(removedTags) == 0 {
		fmt.Println("No tags to remove (none present).")
		return nil
	}

	sess.Tags = strings.Join(newTags, ",")
	if err := store.Update(ctx, sess); err != nil {
		return fmt.Errorf("failed to update session: %w", err)
	}

	fmt.Printf("Removed tags from session #%d: %s\n", sess.Number, strings.Join(removedTags, ", "))
	return nil
}

// parseTags parses a comma-separated tag string into a slice of normalized tags.
func parseTags(tags string) []string {
	if tags == "" {
		return nil
	}
	parts := strings.Split(tags, ",")
	var result []string
	for _, tag := range parts {
		normalized := normalizeTag(tag)
		if normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

// normalizeTag normalizes a tag: trim whitespace, lowercase.
func normalizeTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// containsTag checks if a slice contains a tag (already normalized)
func containsTag(tags []string, tag string) bool {
	return slices.Contains(tags, normalizeTag(tag))
}

func runSessionsBrowse(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	// Get terminal size
	width, height := 80, 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width, height = w, h
	}

	styles := ui.DefaultStyles()
	model := sessions.New(store, width, height, styles)

	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("failed to run browser: %w", err)
	}

	return nil
}

func runSessionsExportGist(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()

	// Get session
	sess, err := store.GetByPrefix(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session '%s' not found", args[0])
	}

	// Get messages
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return fmt.Errorf("failed to get messages: %w", err)
	}

	// Generate markdown
	markdown := session.ExportToMarkdown(sess, messages, session.ExportOptions{
		IncludeSystem: sessionsGistIncludeSystem,
	})

	// Initialize gist client
	client, err := gist.NewClient()
	if err != nil {
		return err
	}

	// Build description
	name := sess.Name
	if name == "" {
		name = fmt.Sprintf("#%d", sess.Number)
	}
	description := fmt.Sprintf("term-llm session: %s", name)

	// Create gist
	fmt.Println("Creating gist...")
	files := map[string]string{
		"session.md": markdown,
	}

	g, err := client.Create(description, sessionsGistPublic, files)
	if err != nil {
		return fmt.Errorf("create gist: %w", err)
	}

	fmt.Printf("Created: %s\n", g.URL)
	fmt.Println()
	fmt.Println("Share with:")
	fmt.Printf("  %s\n", g.URL)

	return nil
}
