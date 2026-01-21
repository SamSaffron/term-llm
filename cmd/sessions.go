package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage chat sessions",
	Long: `List, search, show, delete, and export chat sessions.

Examples:
  term-llm sessions                       # List recent sessions
  term-llm sessions list --provider anthropic
  term-llm sessions search "kubernetes"
  term-llm sessions show <id>
  term-llm sessions delete <id>
  term-llm sessions export <id> [path.md]`,
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
	Use:   "show <id>",
	Short: "Show session details",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsShow,
}

var sessionsDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a session",
	Args:  cobra.ExactArgs(1),
	RunE:  runSessionsDelete,
}

var sessionsExportCmd = &cobra.Command{
	Use:   "export <id> [path]",
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

// Flags
var (
	sessionsProvider string
	sessionsLimit    int
	sessionsJSON     bool
)

func init() {
	// List flags
	sessionsListCmd.Flags().StringVar(&sessionsProvider, "provider", "", "Filter by provider")
	sessionsListCmd.Flags().IntVar(&sessionsLimit, "limit", 20, "Maximum number of sessions to list")

	// Show flags
	sessionsShowCmd.Flags().BoolVar(&sessionsJSON, "json", false, "Output as JSON")

	// Add subcommands
	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsSearchCmd)
	sessionsCmd.AddCommand(sessionsShowCmd)
	sessionsCmd.AddCommand(sessionsDeleteCmd)
	sessionsCmd.AddCommand(sessionsExportCmd)
	sessionsCmd.AddCommand(sessionsResetCmd)

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
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	summaries, err := store.List(ctx, session.ListOptions{
		Provider: sessionsProvider,
		Limit:    sessionsLimit,
	})
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(summaries) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	fmt.Printf("%-20s %-12s %-20s %s\n", "ID", "Provider", "Updated", "Summary")
	fmt.Println(strings.Repeat("-", 80))

	for _, s := range summaries {
		id := s.ID
		if len(id) > 20 {
			id = id[:17] + "..."
		}
		summary := s.Summary
		if s.Name != "" {
			summary = s.Name
		}
		if len(summary) > 35 {
			summary = summary[:32] + "..."
		}
		updated := formatRelativeTime(s.UpdatedAt)
		fmt.Printf("%-20s %-12s %-20s %s\n", id, s.Provider, updated, summary)
	}

	return nil
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
			name = session.ShortID(r.SessionID)
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
	sess, err := store.Get(ctx, args[0])
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
	fmt.Printf("Created: %s\n", sess.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Updated: %s\n", sess.UpdatedAt.Format(time.RFC3339))
	if sess.CWD != "" {
		fmt.Printf("CWD: %s\n", sess.CWD)
	}
	fmt.Printf("Messages: %d\n", len(messages))
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
	if err := store.Delete(ctx, args[0]); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	fmt.Printf("Deleted session: %s\n", args[0])
	return nil
}

func runSessionsExport(cmd *cobra.Command, args []string) error {
	store, err := getSessionStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.Get(ctx, args[0])
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
			name = session.ShortID(sess.ID)
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
