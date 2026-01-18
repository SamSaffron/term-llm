package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/debuglog"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var debugLogCmd = &cobra.Command{
	Use:   "debug-log",
	Short: "Manage debug logs",
	Long: `View, search, and manage LLM debug logs.

Debug logs capture all LLM requests and responses for debugging purposes.
Enable debug logging with: term-llm debug-log enable

Examples:
  term-llm debug-log              # list recent sessions
  term-llm debug-log show 1       # view most recent session
  term-llm debug-log tail         # live tail of current session
  term-llm debug-log search "error"
  term-llm debug-log clean --days 3`,
	RunE: debugLogList, // Default to list
}

var (
	debugLogDays      int
	debugLogShowTools bool
	debugLogRaw       bool
	debugLogJSON      bool
	debugLogFollow    bool
	debugLogRedact    bool
	debugLogMarkdown  bool
	debugLogDryRun    bool
	debugLogAll       bool
	debugLogProvider  string
	debugLogToolName  string
	debugLogErrors    bool
)

func init() {
	rootCmd.AddCommand(debugLogCmd)

	// List command (default)
	debugLogCmd.Flags().IntVar(&debugLogDays, "days", 7, "Show sessions from last N days")

	// Subcommands
	debugLogCmd.AddCommand(debugLogListCmd)
	debugLogCmd.AddCommand(debugLogShowCmd)
	debugLogCmd.AddCommand(debugLogTailCmd)
	debugLogCmd.AddCommand(debugLogSearchCmd)
	debugLogCmd.AddCommand(debugLogCleanCmd)
	debugLogCmd.AddCommand(debugLogExportCmd)
	debugLogCmd.AddCommand(debugLogPathCmd)
	debugLogCmd.AddCommand(debugLogEnableCmd)
	debugLogCmd.AddCommand(debugLogDisableCmd)
	debugLogCmd.AddCommand(debugLogStatusCmd)

	// List flags
	debugLogListCmd.Flags().IntVar(&debugLogDays, "days", 7, "Show sessions from last N days")

	// Show flags
	debugLogShowCmd.Flags().BoolVar(&debugLogRaw, "raw", false, "Output raw JSONL")
	debugLogShowCmd.Flags().BoolVar(&debugLogShowTools, "tools", false, "Highlight tool calls and arguments")
	debugLogShowCmd.Flags().BoolVar(&debugLogJSON, "json", false, "Output as pretty-printed JSON")

	// Tail flags
	debugLogTailCmd.Flags().BoolVarP(&debugLogFollow, "follow", "f", true, "Follow for new entries")

	// Search flags
	debugLogSearchCmd.Flags().StringVar(&debugLogToolName, "tool", "", "Filter by tool name")
	debugLogSearchCmd.Flags().StringVar(&debugLogProvider, "provider", "", "Filter by provider")
	debugLogSearchCmd.Flags().BoolVar(&debugLogErrors, "errors", false, "Show only errors")
	debugLogSearchCmd.Flags().IntVar(&debugLogDays, "days", 7, "Search sessions from last N days")

	// Clean flags
	debugLogCleanCmd.Flags().IntVar(&debugLogDays, "days", 7, "Remove logs older than N days")
	debugLogCleanCmd.Flags().BoolVar(&debugLogAll, "all", false, "Remove all logs")
	debugLogCleanCmd.Flags().BoolVar(&debugLogDryRun, "dry-run", false, "Show what would be deleted")

	// Export flags
	debugLogExportCmd.Flags().BoolVar(&debugLogRedact, "redact", false, "Redact sensitive content")
	debugLogExportCmd.Flags().BoolVar(&debugLogMarkdown, "markdown", false, "Export as markdown")
	debugLogExportCmd.Flags().BoolVar(&debugLogJSON, "json", false, "Export as JSON (default)")
	debugLogExportCmd.Flags().BoolVar(&debugLogRaw, "raw", false, "Export raw JSONL")
}

var debugLogListCmd = &cobra.Command{
	Use:   "list",
	Short: "List recent debug sessions",
	Long: `List recent debug sessions with summary information.

Examples:
  term-llm debug-log list           # list sessions from last 7 days
  term-llm debug-log list --days 3  # list sessions from last 3 days`,
	RunE: debugLogList,
}

var debugLogShowCmd = &cobra.Command{
	Use:   "show [session]",
	Short: "View a debug session",
	Long: `View a debug session in human-readable format.

The session can be specified by:
  - Number (1 = most recent, 2 = second most recent, etc.)
  - Session ID (e.g., 2025-01-18T14-35-22-f7a3)

If no session is specified, shows the most recent session.

Examples:
  term-llm debug-log show           # most recent
  term-llm debug-log show 1         # by number
  term-llm debug-log show --tools   # with tool details
  term-llm debug-log show --raw     # raw JSONL output`,
	Args: cobra.MaximumNArgs(1),
	RunE: debugLogShow,
}

var debugLogTailCmd = &cobra.Command{
	Use:   "tail [session]",
	Short: "Live tail of a debug session",
	Long: `Watch a debug session in real-time, showing new entries as they arrive.

Great for watching what's happening in another terminal while running a command.

Press Ctrl+C to stop.

Examples:
  term-llm debug-log tail           # tail most recent session
  term-llm debug-log tail -f        # follow for new entries (default)
  term-llm debug-log tail --no-follow  # show current content and exit`,
	Args: cobra.MaximumNArgs(1),
	RunE: debugLogTail,
}

var debugLogSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search across debug sessions",
	Long: `Search for text across all debug sessions.

Examples:
  term-llm debug-log search "connection refused"
  term-llm debug-log search --tool read_file
  term-llm debug-log search --errors
  term-llm debug-log search --provider anthropic`,
	RunE: debugLogSearch,
}

var debugLogCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Remove old debug logs",
	Long: `Remove old debug logs to free up disk space.

By default, removes logs older than 7 days.

Examples:
  term-llm debug-log clean             # remove logs > 7 days old
  term-llm debug-log clean --days 3    # remove logs > 3 days old
  term-llm debug-log clean --all       # remove all logs
  term-llm debug-log clean --dry-run   # preview what would be deleted`,
	RunE: debugLogClean,
}

var debugLogExportCmd = &cobra.Command{
	Use:   "export [session]",
	Short: "Export a session for sharing",
	Long: `Export a debug session for sharing or bug reports.

Examples:
  term-llm debug-log export 1 > session.json
  term-llm debug-log export 1 --redact       # redact sensitive content
  term-llm debug-log export 1 --markdown     # export as markdown
  term-llm debug-log export 1 --raw          # export raw JSONL`,
	Args: cobra.MaximumNArgs(1),
	RunE: debugLogExport,
}

var debugLogPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Show debug logs directory path",
	Long: `Print the debug logs directory path.

Useful for scripting:
  ls $(term-llm debug-log path)`,
	RunE: debugLogPath,
}

var debugLogEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable debug logging",
	Long:  `Enable debug logging without editing the config file.`,
	RunE:  debugLogEnable,
}

var debugLogDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable debug logging",
	Long:  `Disable debug logging without editing the config file.`,
	RunE:  debugLogDisable,
}

var debugLogStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show debug logging status",
	Long:  `Show the current status of debug logging.`,
	RunE:  debugLogStatus,
}

// debugLogList lists recent debug sessions
func debugLogList(cmd *cobra.Command, args []string) error {
	dir := getDebugLogDir()

	sessions, err := debuglog.ListSessions(dir)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	// Filter by days
	if debugLogDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -debugLogDays)
		var filtered []debuglog.SessionSummary
		for _, s := range sessions {
			if s.StartTime.After(cutoff) {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	debuglog.FormatSessionList(os.Stdout, sessions, debugLogDays)
	return nil
}

// debugLogShow shows a specific session
func debugLogShow(cmd *cobra.Command, args []string) error {
	dir := getDebugLogDir()

	// Determine session to show
	identifier := ""
	if len(args) > 0 {
		identifier = args[0]
	}

	var summary *debuglog.SessionSummary
	var err error

	if identifier == "" {
		summary, err = debuglog.GetMostRecentSession(dir)
	} else {
		summary, err = debuglog.ResolveSession(dir, identifier)
	}
	if err != nil {
		return fmt.Errorf("failed to find session: %w", err)
	}
	if summary == nil {
		return fmt.Errorf("session not found")
	}

	// Raw output
	if debugLogRaw {
		return debuglog.ExportRawByID(dir, summary.ID, os.Stdout)
	}

	// JSON output
	if debugLogJSON {
		return debuglog.ExportSessionByID(dir, summary.ID, os.Stdout, debuglog.ExportOptions{
			Format: debuglog.FormatJSON,
		})
	}

	// Human-readable output
	session, err := debuglog.ParseSession(summary.FilePath)
	if err != nil {
		return fmt.Errorf("failed to parse session: %w", err)
	}

	debuglog.FormatSession(os.Stdout, session, debuglog.FormatOptions{
		ShowTools:     debugLogShowTools,
		ShowTimestamp: true,
	})

	return nil
}

// debugLogTail tails a session file
func debugLogTail(cmd *cobra.Command, args []string) error {
	dir := getDebugLogDir()

	// Determine which session to tail
	var filePath string
	if len(args) > 0 {
		path, err := debuglog.GetSessionFilePath(dir, args[0])
		if err != nil {
			return err
		}
		filePath = path
	} else {
		summary, err := debuglog.GetMostRecentSession(dir)
		if err != nil {
			return err
		}
		if summary == nil {
			return fmt.Errorf("no debug sessions found")
		}
		filePath = summary.FilePath
	}

	fmt.Fprintf(os.Stderr, "Watching: %s\n\n", filepath.Base(filePath))

	// Set up signal handling for graceful exit
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	err := debuglog.Tail(ctx, filePath, os.Stdout, debuglog.TailOptions{
		Follow: debugLogFollow,
	})

	if err == context.Canceled {
		return nil
	}
	return err
}

// debugLogSearch searches across sessions
func debugLogSearch(cmd *cobra.Command, args []string) error {
	dir := getDebugLogDir()

	query := ""
	if len(args) > 0 {
		query = args[0]
	}

	opts := debuglog.SearchOptions{
		Query:      query,
		ToolName:   debugLogToolName,
		Provider:   debugLogProvider,
		ErrorsOnly: debugLogErrors,
		Days:       debugLogDays,
	}

	// Require at least one search criterion
	if opts.Query == "" && opts.ToolName == "" && !opts.ErrorsOnly && opts.Provider == "" {
		return fmt.Errorf("specify a search query, --tool, --provider, or --errors")
	}

	results, err := debuglog.Search(dir, opts)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No matches found.")
		return nil
	}

	fmt.Printf("Found %d matches:\n\n", len(results))

	for _, r := range results {
		fmt.Printf("[%s] %s %s\n", r.Timestamp.Local().Format("2006-01-02 15:04:05"), r.SessionID, r.Context)
		if r.Match != "" {
			fmt.Printf("  %s\n", r.Match)
		}
		fmt.Println()
	}

	return nil
}

// debugLogClean removes old log files
func debugLogClean(cmd *cobra.Command, args []string) error {
	dir := getDebugLogDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No debug logs found.")
			return nil
		}
		return err
	}

	var toDelete []string
	var totalSize int64
	cutoff := time.Now().AddDate(0, 0, -debugLogDays)

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		shouldDelete := debugLogAll || info.ModTime().Before(cutoff)
		if shouldDelete {
			toDelete = append(toDelete, entry.Name())
			totalSize += info.Size()
		}
	}

	if len(toDelete) == 0 {
		fmt.Println("No logs to clean up.")
		return nil
	}

	if debugLogDryRun {
		fmt.Printf("Would delete %d files (%s):\n", len(toDelete), formatBytes(totalSize))
		for _, name := range toDelete {
			fmt.Printf("  %s\n", name)
		}
		return nil
	}

	// Delete files
	var deleted int
	for _, name := range toDelete {
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			deleted++
		}
	}

	fmt.Printf("Removed %d log files (%s)\n", deleted, formatBytes(totalSize))
	return nil
}

// debugLogExport exports a session
func debugLogExport(cmd *cobra.Command, args []string) error {
	dir := getDebugLogDir()

	identifier := ""
	if len(args) > 0 {
		identifier = args[0]
	} else {
		// Default to most recent
		summary, err := debuglog.GetMostRecentSession(dir)
		if err != nil {
			return err
		}
		if summary == nil {
			return fmt.Errorf("no debug sessions found")
		}
		identifier = summary.ID
	}

	// Determine format
	format := debuglog.FormatJSON
	if debugLogMarkdown {
		format = debuglog.FormatMarkdown
	}
	if debugLogRaw {
		format = debuglog.FormatRaw
	}

	return debuglog.ExportSessionByID(dir, identifier, os.Stdout, debuglog.ExportOptions{
		Format: format,
		Redact: debugLogRedact,
	})
}

// debugLogPath prints the debug log directory
func debugLogPath(cmd *cobra.Command, args []string) error {
	fmt.Println(getDebugLogDir())
	return nil
}

// debugLogEnable enables debug logging
func debugLogEnable(cmd *cobra.Command, args []string) error {
	if err := setDebugLogEnabled(true); err != nil {
		return err
	}
	dir := getDebugLogDir()
	fmt.Printf("Debug logging enabled. Logs will be saved to %s\n", dir)
	return nil
}

// debugLogDisable disables debug logging
func debugLogDisable(cmd *cobra.Command, args []string) error {
	if err := setDebugLogEnabled(false); err != nil {
		return err
	}
	fmt.Println("Debug logging disabled.")
	return nil
}

// debugLogStatus shows the current status
func debugLogStatus(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	dir := getDebugLogDir()
	sessions, _ := debuglog.ListSessions(dir)
	size, _ := debuglog.DirSize(dir)

	enabledStr := "disabled"
	if cfg.DebugLogs.Enabled {
		enabledStr = "enabled"
	}

	fmt.Printf("Debug logging: %s\n", enabledStr)
	fmt.Printf("Log directory: %s\n", dir)
	fmt.Printf("Sessions: %d\n", len(sessions))
	fmt.Printf("Total size: %s\n", formatBytes(size))

	return nil
}

// getDebugLogDir returns the debug log directory
func getDebugLogDir() string {
	cfg, err := config.Load()
	if err == nil && cfg.DebugLogs.Dir != "" {
		return cfg.DebugLogs.Dir
	}
	return config.GetDebugLogsDir()
}

// setDebugLogEnabled sets the debug_logs.enabled config value
func setDebugLogEnabled(enabled bool) error {
	configPath, err := config.GetConfigPath()
	if err != nil {
		return fmt.Errorf("failed to get config path: %w", err)
	}

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Read existing file or create empty document
	var root yaml.Node
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create new document with empty mapping
			root = yaml.Node{
				Kind: yaml.DocumentNode,
				Content: []*yaml.Node{{
					Kind: yaml.MappingNode,
				}},
			}
		} else {
			return fmt.Errorf("failed to read config: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("failed to parse config: %w", err)
		}
	}

	// Set debug_logs.enabled
	value := "false"
	if enabled {
		value = "true"
	}

	if err := setYAMLValue(&root, []string{"debug_logs", "enabled"}, value); err != nil {
		return fmt.Errorf("failed to set value: %w", err)
	}

	// Write back
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	encoder.Close()

	if err := os.WriteFile(configPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	return nil
}

// formatBytes formats a byte count as a human-readable string
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
