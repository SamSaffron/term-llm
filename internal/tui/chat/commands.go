package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/sahilm/fuzzy"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	"github.com/samsaffron/term-llm/internal/ui"
)

// Command represents a slash command
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Subcommands []Subcommand // Optional subcommands
}

// Subcommand represents a subcommand of a slash command
type Subcommand struct {
	Name        string
	Description string
}

// AllCommands returns all available slash commands
func AllCommands() []Command {
	return []Command{
		{
			Name:        "help",
			Aliases:     []string{"h", "?"},
			Description: "Show help and available commands",
			Usage:       "/help",
		},
		{
			Name:        "clear",
			Aliases:     []string{"c"},
			Description: "Clear conversation history",
			Usage:       "/clear",
		},
		{
			Name:        "quit",
			Aliases:     []string{"q", "exit"},
			Description: "Exit chat",
			Usage:       "/quit",
		},
		{
			Name:        "model",
			Aliases:     []string{"m"},
			Description: "Switch provider/model",
			Usage:       "/model [name]",
		},
		{
			Name:        "search",
			Aliases:     []string{"web", "s"},
			Description: "Toggle web search on/off",
			Usage:       "/search",
		},
		{
			Name:        "new",
			Aliases:     []string{"n"},
			Description: "Start a new session (saves current)",
			Usage:       "/new",
		},
		{
			Name:        "save",
			Description: "Save session with a name",
			Usage:       "/save [name]",
		},
		{
			Name:        "export",
			Description: "Export conversation as markdown",
			Usage:       "/export [path]",
		},
		{
			Name:        "system",
			Description: "Set custom system prompt",
			Usage:       "/system <prompt>",
		},
		{
			Name:        "file",
			Aliases:     []string{"f"},
			Description: "Attach file(s) to next message",
			Usage:       "/file <path>",
		},
		{
			Name:        "dirs",
			Description: "Manage approved directories",
			Usage:       "/dirs [add|remove <path>]",
		},
		{
			Name:        "mcp",
			Description: "MCP servers (browser, database, git tools)",
			Usage:       "/mcp [start|stop|add|list]",
			Subcommands: []Subcommand{
				{Name: "start", Description: "Start a configured server"},
				{Name: "stop", Description: "Stop a running server"},
				{Name: "add", Description: "Add a new server"},
				{Name: "list", Description: "Show available servers"},
				{Name: "status", Description: "Show server status"},
			},
		},
		{
			Name:        "skills",
			Aliases:     []string{"sk"},
			Description: "Show available skills",
			Usage:       "/skills",
		},
		{
			Name:        "inspect",
			Aliases:     []string{"debug", "i"},
			Description: "View full conversation with tool details",
			Usage:       "/inspect",
		},
		{
			Name:        "compact",
			Description: "Compact conversation context into a summary",
			Usage:       "/compact",
		},
		{
			Name:        "resume",
			Aliases:     []string{"r"},
			Description: "Browse and resume a previous session",
			Usage:       "/resume [number|id]",
		},
		{
			Name:        "reload",
			Description: "Re-exec under the current binary, resuming this session (useful after upgrades)",
			Usage:       "/reload",
		},
		{
			Name:        "handover",
			Aliases:     []string{"ho"},
			Description: "Hand conversation to another agent",
			Usage:       "/handover @agent [provider:model]",
		},
	}
}

// CommandSource implements fuzzy.Source for command searching
type CommandSource []Command

func (c CommandSource) String(i int) string {
	return c[i].Name
}

func (c CommandSource) Len() int {
	return len(c)
}

// FilterCommands returns commands matching the query using fuzzy search
// If query contains a space (e.g., "mcp "), it returns subcommands for that command
func FilterCommands(query string) []Command {
	commands := AllCommands()
	if query == "" {
		return commands
	}

	// Remove leading slash if present
	query = strings.TrimPrefix(query, "/")

	// Check for subcommand completion (query contains space)
	if idx := strings.Index(query, " "); idx != -1 {
		cmdName := strings.ToLower(query[:idx])
		subQuery := strings.ToLower(strings.TrimSpace(query[idx+1:]))

		// Find the parent command
		for _, cmd := range commands {
			if cmd.Name == cmdName || slices.Contains(cmd.Aliases, cmdName) {
				if len(cmd.Subcommands) == 0 {
					return nil // No subcommands for this command
				}
				// Return subcommands as pseudo-commands
				var result []Command
				for _, sub := range cmd.Subcommands {
					// Filter by subquery if present
					if subQuery == "" || strings.HasPrefix(sub.Name, subQuery) {
						result = append(result, Command{
							Name:        cmd.Name + " " + sub.Name,
							Description: sub.Description,
						})
					}
				}
				return result
			}
		}
		return nil
	}

	// First check for exact name/alias matches, but only short-circuit
	// for multi-character queries (so "/m" shows both "model" and "mcp")
	queryLower := strings.ToLower(query)
	if len(query) > 1 {
		for _, cmd := range commands {
			if cmd.Name == queryLower {
				return []Command{cmd}
			}
			for _, alias := range cmd.Aliases {
				if alias == queryLower {
					return []Command{cmd}
				}
			}
		}
	}

	// Fuzzy search on command names
	source := CommandSource(commands)
	matches := fuzzy.FindFrom(query, source)

	var result []Command
	for _, match := range matches {
		result = append(result, commands[match.Index])
	}

	// If no fuzzy matches, also check if query is prefix of any command
	if len(result) == 0 {
		for _, cmd := range commands {
			if strings.HasPrefix(cmd.Name, queryLower) {
				result = append(result, cmd)
			}
		}
	}

	return result
}

// ExecuteCommand handles slash command execution
func (m *Model) ExecuteCommand(input string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return m, nil
	}

	cmdName := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	args := parts[1:]

	// Find matching command - first try exact match
	var cmd *Command
	for _, c := range AllCommands() {
		if c.Name == cmdName {
			cmd = &c
			break
		}
		for _, alias := range c.Aliases {
			if alias == cmdName {
				cmd = &c
				break
			}
		}
		if cmd != nil {
			break
		}
	}

	// If no exact match, try prefix matching
	if cmd == nil {
		var prefixMatches []Command
		for _, c := range AllCommands() {
			if strings.HasPrefix(c.Name, cmdName) {
				prefixMatches = append(prefixMatches, c)
			}
		}

		switch len(prefixMatches) {
		case 0:
			// No matches at all
			return m.showSystemMessage(fmt.Sprintf("Unknown command: /%s\nType /help for available commands.", cmdName))
		case 1:
			// Unique prefix match - use it
			cmd = &prefixMatches[0]
		default:
			// Multiple matches - show them
			var names []string
			for _, c := range prefixMatches {
				names = append(names, "/"+c.Name)
			}
			return m.showSystemMessage(fmt.Sprintf("Ambiguous command: /%s\nDid you mean: %s?", cmdName, strings.Join(names, ", ")))
		}
	}

	if cmd == nil {
		return m.showSystemMessage(fmt.Sprintf("Unknown command: /%s\nType /help for available commands.", cmdName))
	}

	// Execute the command
	switch cmd.Name {
	case "help":
		return m.cmdHelp()
	case "clear":
		return m.cmdClear()
	case "quit":
		return m.cmdQuit()
	case "model":
		return m.cmdModel(args)
	case "search":
		return m.cmdSearch()
	case "new":
		return m.cmdNew()
	case "save":
		return m.cmdSave(args)
	case "export":
		return m.cmdExport(args)
	case "system":
		return m.cmdSystem(args)
	case "file":
		return m.cmdFile(args)
	case "dirs":
		return m.cmdDirs(args)
	case "mcp":
		return m.cmdMcp(args)
	case "skills":
		return m.cmdSkills()
	case "inspect":
		return m.cmdInspect()
	case "compact":
		return m.cmdCompress()
	case "resume":
		return m.cmdResume(args)
	case "reload":
		return m.cmdReload()
	case "handover":
		return m.cmdHandover(args)
	default:
		return m.showSystemMessage(fmt.Sprintf("Command /%s is not yet implemented.", cmd.Name))
	}
}

// Command implementations

func (m *Model) showSystemMessage(content string) (tea.Model, tea.Cmd) {
	// In inline mode, print directly to scrollback rather than adding to session
	m.setTextareaValue("")

	// If a tool-initiated handover is pending but we're showing an error,
	// signal the tool so it doesn't hang forever.
	m.cancelHandoverTool()

	// Render the message content with markdown
	rendered := m.renderMarkdown(content)

	return m, tea.Println(rendered + "\n")
}

// cancelHandoverTool signals false on the tool-initiated handover channel
// and clears it. This is a no-op if no tool handover is pending.
func (m *Model) cancelHandoverTool() {
	if m.handoverToolDoneCh != nil {
		m.handoverToolDoneCh <- false
		m.handoverToolDoneCh = nil
	}
}

func (m *Model) cmdHelp() (tea.Model, tea.Cmd) {
	var b strings.Builder
	b.WriteString("## Available Commands\n\n")

	for _, cmd := range AllCommands() {
		b.WriteString(fmt.Sprintf("**%s**", cmd.Usage))
		if len(cmd.Aliases) > 0 {
			b.WriteString(fmt.Sprintf(" (aliases: %s)", strings.Join(cmd.Aliases, ", ")))
		}
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  %s\n\n", cmd.Description))
	}

	b.WriteString("## Keyboard Shortcuts\n\n")
	b.WriteString("- `Enter` - Send message\n")
	b.WriteString("- `Ctrl+J` or `Alt+Enter` - Insert newline\n")
	b.WriteString("- `Ctrl+C` - Quit\n")
	b.WriteString("- `Ctrl+K` - Clear conversation\n")
	b.WriteString("- `Ctrl+S` - Toggle web search\n")
	b.WriteString("- `Ctrl+T` - MCP servers (tools)\n")
	b.WriteString("- `Ctrl+P` - Command palette\n")
	b.WriteString("- `Ctrl+L` - Switch model\n")
	b.WriteString("- `Ctrl+N` - New session\n")
	b.WriteString("- `Esc` - Cancel streaming\n")

	return m.showSystemMessage(b.String())
}

func (m *Model) cmdClear() (tea.Model, tea.Cmd) {
	// Mark the old session as complete before creating a new one
	if m.store != nil && m.sess != nil {
		_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusComplete)
	}

	// Create a new session to clear the conversation
	// This preserves the old session in history while starting fresh
	m.sess = &session.Session{
		ID:          session.NewID(),
		Provider:    m.providerName,
		ProviderKey: m.providerKey,
		Model:       m.modelName,
		Mode:        session.ModeChat,
		Agent:       m.agentName,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Search:      m.searchEnabled,
		Tools:       m.toolsStr,
		MCP:         m.mcpStr,
	}
	if cwd, err := os.Getwd(); err == nil {
		m.sess.CWD = cwd
	}

	// Persist new session
	if m.store != nil {
		ctx := context.Background()
		_ = m.store.Create(ctx, m.sess)
		_ = m.store.SetCurrent(ctx, m.sess.ID)
	}

	// Clear conversation messages and input
	m.messages = nil
	m.scrollOffset = 0
	m.setTextareaValue("")
	m.clearFiles()
	m.pasteChunks = nil

	// Reset engine state (compaction tracking, provider conversation IDs)
	if m.engine != nil {
		m.engine.ResetConversation()
	}

	// Reset streaming and rendering state
	m.currentResponse.Reset()
	m.currentTokens = 0
	m.webSearchUsed = false
	m.retryStatus = ""
	if m.tracker != nil {
		m.resetTracker()
	}
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.smoothTickPending = false
	m.deferredStreamRead = false
	m.streamRenderTickPending = false

	// Reset stats for new session
	if m.stats != nil {
		m.stats = ui.NewSessionStats()
	}

	// Invalidate view cache so stale content doesn't bleed through
	m.viewCache.historyValid = false
	m.viewCache.completedStream = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()

	return m, tea.Println("Conversation cleared (new session started).")
}

func (m *Model) cmdQuit() (tea.Model, tea.Cmd) {
	// Signal tool-initiated handover (if any) right before quitting.
	// The session is about to restart so the tool result is moot,
	// but we unblock the goroutine to avoid a leak.
	if m.handoverToolDoneCh != nil {
		m.handoverToolDoneCh <- true
		m.handoverToolDoneCh = nil
	}
	// Cancel the engine stream now that the tool is unblocked
	if m.streamCancelFunc != nil {
		m.streamCancelFunc()
		m.streamCancelFunc = nil
	}
	m.quitting = true
	return m, tea.Quit
}

func (m *Model) cmdReload() (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showSystemMessage("Cannot reload while streaming. Cancel first (Esc).")
	}
	m.setTextareaValue("")
	m.quitting = true
	m.reloadRequested = true
	if m.sess != nil {
		m.reloadSessionID = m.sess.ID
	}
	return m, tea.Batch(
		tea.Println("Reloading..."),
		tea.Quit,
	)
}

func (m *Model) cmdModel(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// Show model picker dialog with MRU ordering
		history, _ := config.LoadModelHistory()
		m.dialog.ShowModelPicker(m.providerKey+":"+m.modelName, GetAvailableProviders(m.config), config.ModelHistoryOrder(history))
		m.setTextareaValue("")
		return m, nil
	}

	// Switch to specified model (format: provider:model or just model/alias)
	modelArg := args[0]
	fallbackProvider := strings.TrimSpace(m.providerKey)
	if fallbackProvider == "" {
		fallbackProvider = strings.TrimSpace(m.providerName)
	}
	resolved, ok := resolveProviderModelArg(modelArg, m.config, fallbackProvider)
	if !ok {
		return m.showSystemMessage(fmt.Sprintf("Invalid model format: %s", modelArg))
	}
	return m.switchModel(resolved)
}

// providerModelEntry captures a concrete provider:model combination.
type providerModelEntry struct {
	provider string
	model    string
	combined string
}

func availableProviderModels(cfg *config.Config) []providerModelEntry {
	var entries []providerModelEntry
	for _, provider := range GetAvailableProviders(cfg) {
		for _, model := range provider.Models {
			entries = append(entries, providerModelEntry{
				provider: provider.Name,
				model:    model,
				combined: provider.Name + ":" + model,
			})
		}
	}
	return entries
}

func matchProviderModels(arg string, cfg *config.Config) []providerModelEntry {
	query := strings.ToLower(strings.TrimSpace(arg))
	entries := availableProviderModels(cfg)
	if query == "" {
		return reorderProviderModelsByRecency(entries, recentProviderModels())
	}

	var direct []providerModelEntry
	for _, entry := range entries {
		providerLower := strings.ToLower(entry.provider)
		modelLower := strings.ToLower(entry.model)
		combinedLower := strings.ToLower(entry.combined)

		match := false
		if strings.Contains(query, ":") {
			match = strings.HasPrefix(combinedLower, query)
		} else {
			match = strings.HasPrefix(providerLower, query) ||
				strings.HasPrefix(modelLower, query) ||
				strings.Contains(modelLower, query) ||
				strings.HasPrefix(combinedLower, query)
		}
		if match {
			direct = append(direct, entry)
		}
	}
	if len(direct) > 0 {
		return reorderProviderModelsByRecency(direct, recentProviderModels())
	}

	return reorderProviderModelsByRecency(fuzzyProviderModelMatches(query, entries), recentProviderModels())
}

func (m *Model) currentProviderModel() string {
	provider := strings.TrimSpace(m.providerKey)
	if provider == "" && m.sess != nil {
		provider = strings.TrimSpace(m.sess.ProviderKey)
	}
	if provider == "" {
		provider = strings.TrimSpace(m.providerName)
	}

	model := strings.TrimSpace(m.modelName)
	if model == "" && m.sess != nil {
		model = strings.TrimSpace(m.sess.Model)
	}

	if provider == "" || model == "" {
		return ""
	}
	return provider + ":" + model
}

func (m *Model) recordCurrentModelUse() {
	if providerModel := m.currentProviderModel(); providerModel != "" {
		config.RecordModelUseAsync(providerModel)
	}
}

func recentProviderModels() []string {
	history, err := config.LoadModelHistory()
	if err != nil {
		return nil
	}
	return config.ModelHistoryOrder(history)
}

func reorderProviderModelsByRecency(entries []providerModelEntry, recent []string) []providerModelEntry {
	if len(entries) <= 1 || len(recent) == 0 {
		return entries
	}

	byCombined := make(map[string]providerModelEntry, len(entries))
	for _, entry := range entries {
		byCombined[entry.combined] = entry
	}

	ordered := make([]providerModelEntry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, combined := range recent {
		entry, ok := byCombined[combined]
		if !ok {
			continue
		}
		ordered = append(ordered, entry)
		seen[combined] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := seen[entry.combined]; ok {
			continue
		}
		ordered = append(ordered, entry)
	}
	return ordered
}

func fuzzyProviderModelMatches(query string, entries []providerModelEntry) []providerModelEntry {
	if query == "" || len(entries) == 0 {
		return nil
	}

	rawCandidates := make([]string, len(entries))
	normalizedCandidates := make([]string, len(entries))
	for i, entry := range entries {
		rawCandidates[i] = strings.ToLower(entry.combined)
		normalizedCandidates[i] = normalizeProviderModelMatcher(entry.provider + entry.model)
	}

	orderedIndexes := fuzzyMatchIndexes(query, rawCandidates)
	normalizedQuery := normalizeProviderModelMatcher(query)
	if normalizedQuery != "" && normalizedQuery != query {
		orderedIndexes = appendUniqueIndexes(orderedIndexes, fuzzyMatchIndexes(normalizedQuery, normalizedCandidates)...)
	}

	matches := make([]providerModelEntry, 0, len(orderedIndexes))
	for _, idx := range orderedIndexes {
		matches = append(matches, entries[idx])
	}
	return matches
}

func fuzzyMatchIndexes(query string, candidates []string) []int {
	results := fuzzy.Find(query, candidates)
	indexes := make([]int, 0, len(results))
	for _, match := range results {
		indexes = append(indexes, match.Index)
	}
	return indexes
}

func appendUniqueIndexes(existing []int, additional ...int) []int {
	seen := make(map[int]struct{}, len(existing))
	for _, idx := range existing {
		seen[idx] = struct{}{}
	}
	for _, idx := range additional {
		if _, ok := seen[idx]; ok {
			continue
		}
		existing = append(existing, idx)
		seen[idx] = struct{}{}
	}
	return existing
}

func normalizeProviderModelMatcher(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func providerModelCompletionItems(commandPrefix, arg string, cfg *config.Config) []Command {
	entries := matchProviderModels(arg, cfg)
	items := make([]Command, 0, len(entries))
	for _, entry := range entries {
		items = append(items, Command{
			Name:        commandPrefix + entry.combined,
			Description: entry.provider,
		})
	}
	return items
}

func resolveProviderModelArg(arg string, cfg *config.Config, fallbackProvider string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", false
	}
	if strings.Contains(arg, ":") {
		return arg, true
	}
	if matches := matchProviderModels(arg, cfg); len(matches) > 0 {
		return matches[0].combined, true
	}
	if fallbackProvider != "" {
		return fallbackProvider + ":" + arg, true
	}
	return "", false
}

// toggleSearch toggles web search and persists to session.
// Called by both Ctrl+S and /search command.
func (m *Model) toggleSearch() {
	m.searchEnabled = !m.searchEnabled

	// Persist to session
	if m.sess != nil {
		m.sess.Search = m.searchEnabled
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
}

func (m *Model) cmdSearch() (tea.Model, tea.Cmd) {
	m.toggleSearch()
	m.setTextareaValue("")

	status := "disabled"
	if m.searchEnabled {
		status = "enabled"
	}
	return m.showSystemMessage(fmt.Sprintf("Web search %s.", status))
}

func (m *Model) cmdNew() (tea.Model, tea.Cmd) {
	// Mark the old session as complete before creating a new one
	if m.store != nil && m.sess != nil {
		_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusComplete)
	}

	// Create new session with current settings
	m.sess = &session.Session{
		ID:          session.NewID(),
		Provider:    m.providerName,
		ProviderKey: m.providerKey,
		Model:       m.modelName,
		Mode:        session.ModeChat,
		Agent:       m.agentName,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Search:      m.searchEnabled,
		Tools:       m.toolsStr,
		MCP:         m.mcpStr,
	}
	if cwd, err := os.Getwd(); err == nil {
		m.sess.CWD = cwd
	}

	// Persist to store
	if m.store != nil {
		ctx := context.Background()
		_ = m.store.Create(ctx, m.sess)
		_ = m.store.SetCurrent(ctx, m.sess.ID)
	}

	// Clear conversation messages and input
	m.messages = nil
	m.scrollOffset = 0
	m.setTextareaValue("")
	m.clearFiles()
	m.pasteChunks = nil

	// Reset engine state (compaction tracking, provider conversation IDs)
	if m.engine != nil {
		m.engine.ResetConversation()
	}

	// Reset streaming and rendering state
	m.currentResponse.Reset()
	m.currentTokens = 0
	m.webSearchUsed = false
	m.retryStatus = ""
	if m.tracker != nil {
		m.resetTracker()
	}
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.smoothTickPending = false
	m.streamRenderTickPending = false

	// Reset stats for new session
	if m.stats != nil {
		m.stats = ui.NewSessionStats()
	}

	// Invalidate view cache so stale content doesn't bleed through
	m.viewCache.historyValid = false
	m.viewCache.completedStream = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()

	return m.showSystemMessage("Started new session.")
}

func (m *Model) cmdSave(args []string) (tea.Model, tea.Cmd) {
	name := ""
	if len(args) > 0 {
		name = strings.Join(args, "-")
	} else {
		// Generate name from first message or timestamp
		if len(m.messages) > 0 {
			// Use first few words of first user message
			for _, msg := range m.messages {
				if msg.Role == llm.RoleUser {
					words := strings.Fields(msg.TextContent)
					if len(words) > 5 {
						words = words[:5]
					}
					name = strings.Join(words, "-")
					name = strings.ToLower(name)
					// Remove special characters
					name = strings.Map(func(r rune) rune {
						if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
							return r
						}
						return -1
					}, name)
					break
				}
			}
		}
		if name == "" {
			name = fmt.Sprintf("session-%d", time.Now().Unix())
		}
	}

	if m.store == nil {
		m.setTextareaValue("")
		return m.showSystemMessage("Session storage is disabled. Enable it in config with `sessions.enabled: true`.")
	}

	m.sess.Name = name
	if err := m.store.Update(context.Background(), m.sess); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to save session: %v", err))
	}

	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("Session saved as '%s'.", name))
}

func (m *Model) cmdResume(args []string) (tea.Model, tea.Cmd) {
	if m.store == nil {
		return m.showSystemMessage("Session storage is disabled.")
	}

	ctx := context.Background()

	// /resume <number|id> — direct resume without the browser.
	if len(args) > 0 {
		sess, err := m.store.GetByPrefix(ctx, args[0])
		if err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to find session: %v", err))
		}
		if sess == nil {
			return m.showSystemMessage(fmt.Sprintf("Session '%s' not found.", args[0]))
		}

		m.setTextareaValue("")
		return m.requestResumeSession(sess.ID)
	}

	// /resume with no args — open the dedicated embedded sessions browser.
	summaries, err := m.store.List(ctx, session.ListOptions{Limit: 1})
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to list sessions: %v", err))
	}
	if len(summaries) == 0 {
		return m.showSystemMessage("No saved sessions found.")
	}

	return m.openResumeBrowser()
}

// resumeFormatAge returns a compact human-readable age string.
func resumeFormatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// resumeShortenModel returns a compact model name for display in the session picker.
// It strips the "claude-" prefix and any trailing 8-digit date suffix, then truncates.
func resumeShortenModel(model string) string {
	s := strings.TrimPrefix(model, "claude-")
	// Strip trailing date suffix of the form -YYYYMMDD
	if len(s) > 9 {
		suffix := s[len(s)-9:]
		if suffix[0] == '-' {
			allDigits := true
			for _, c := range suffix[1:] {
				if c < '0' || c > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				s = s[:len(s)-9]
			}
		}
	}
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

func (m *Model) cmdExport(args []string) (tea.Model, tea.Cmd) {
	if len(m.messages) == 0 {
		return m.showSystemMessage("No messages to export.")
	}

	// Determine output path
	var outputPath string
	if len(args) > 0 {
		outputPath = strings.Join(args, " ")
	} else {
		// Generate default filename
		timestamp := time.Now().Format("2006-01-02_15-04-05")
		outputPath = fmt.Sprintf("chat-export-%s.md", timestamp)
	}

	// Build markdown content
	var b strings.Builder

	// Header
	b.WriteString("# Chat Export\n\n")
	b.WriteString(fmt.Sprintf("**Model:** %s (%s)\n", m.modelName, m.providerName))
	b.WriteString(fmt.Sprintf("**Exported:** %s\n", time.Now().Format("2006-01-02 15:04:05")))
	if m.sess.Name != "" {
		b.WriteString(fmt.Sprintf("**Session:** %s\n", m.sess.Name))
	}
	b.WriteString("\n---\n\n")

	// Messages
	for _, msg := range m.messages {
		// Role header
		if msg.Role == llm.RoleUser {
			b.WriteString("## ❯\n\n")
		} else {
			b.WriteString("## 🤖 Assistant")
			if msg.DurationMs > 0 {
				b.WriteString(fmt.Sprintf(" *(%.1fs)*", float64(msg.DurationMs)/1000))
			}
			b.WriteString("\n\n")
		}

		// Content - for user messages, extract just the text (not file contents)
		content := msg.TextContent
		if msg.Role == llm.RoleUser {
			if idx := strings.Index(content, "\n\n---\n**Attached files:**"); idx != -1 {
				content = strings.TrimSpace(content[:idx])
			}
		}
		b.WriteString(content)
		b.WriteString("\n---\n\n")
	}

	// Write to file
	if err := os.WriteFile(outputPath, []byte(b.String()), 0644); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to export: %v", err))
	}

	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("Exported %d messages to `%s`", len(m.messages), outputPath))
}

func (m *Model) cmdSystem(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		if m.config.Chat.Instructions != "" {
			return m.showSystemMessage(fmt.Sprintf("Current system prompt:\n\n%s", m.config.Chat.Instructions))
		}
		return m.showSystemMessage("No system prompt set.\nUsage: `/system <prompt>`")
	}

	// Set custom system prompt (session-only, doesn't persist to config)
	prompt := strings.Join(args, " ")
	m.config.Chat.Instructions = prompt
	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("System prompt set for this session:\n\n%s", prompt))
}

func (m *Model) cmdFile(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		if len(m.files) == 0 {
			return m.showSystemMessage("No files attached.\nUsage: `/file <path>` or `/file clear`")
		}
		// Show attached files
		var b strings.Builder
		b.WriteString("## Attached Files\n\n")
		var totalSize int64
		for _, f := range m.files {
			b.WriteString(fmt.Sprintf("- `%s` (%s)\n", f.Name, FormatFileSize(f.Size)))
			totalSize += f.Size
		}
		b.WriteString(fmt.Sprintf("\nTotal: %d file(s), %s", len(m.files), FormatFileSize(totalSize)))
		b.WriteString("\n\nUse `/file clear` to remove all attachments.")
		return m.showSystemMessage(b.String())
	}

	// Handle clear command
	if args[0] == "clear" {
		count := len(m.files)
		m.clearFiles()
		m.setTextareaValue("")
		if count == 0 {
			return m.showSystemMessage("No files were attached.")
		}
		return m.showSystemMessage(fmt.Sprintf("Cleared %d attached file(s).", count))
	}

	// Join all args in case path has spaces
	path := strings.Join(args, " ")

	// Check if it's a glob pattern
	if strings.ContainsAny(path, "*?[") {
		return m.attachFiles(path)
	}

	// Single file attachment
	return m.attachFile(path)
}

func (m *Model) cmdDirs(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// List approved directories
		if len(m.approvedDirs.Directories) == 0 {
			return m.showSystemMessage("No approved directories.\n\nUse `/dirs add <path>` to approve a directory,\nor attach a file to be prompted for approval.")
		}

		var b strings.Builder
		b.WriteString("## Approved Directories\n\n")
		for _, dir := range m.approvedDirs.Directories {
			b.WriteString(fmt.Sprintf("- `%s`\n", dir))
		}
		b.WriteString("\n**Commands:**\n")
		b.WriteString("- `/dirs add <path>` - Approve a directory\n")
		b.WriteString("- `/dirs remove <path>` - Revoke approval")
		return m.showSystemMessage(b.String())
	}

	subCmd := strings.ToLower(args[0])
	subArgs := args[1:]

	switch subCmd {
	case "add":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/dirs add <path>`")
		}
		path := strings.Join(subArgs, " ")
		if err := m.approvedDirs.AddDirectory(path); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to add directory: %v", err))
		}
		m.setTextareaValue("")
		return m.showSystemMessage(fmt.Sprintf("Approved directory: `%s`", path))

	case "remove", "rm", "delete":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/dirs remove <path>`")
		}
		path := strings.Join(subArgs, " ")
		if err := m.approvedDirs.RemoveDirectory(path); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to remove directory: %v", err))
		}
		m.setTextareaValue("")
		return m.showSystemMessage(fmt.Sprintf("Removed directory from approved list: `%s`", path))

	default:
		return m.showSystemMessage(fmt.Sprintf("Unknown subcommand: %s\n\nUsage:\n- `/dirs` - List approved directories\n- `/dirs add <path>` - Approve a directory\n- `/dirs remove <path>` - Revoke approval", subCmd))
	}
}

func (m *Model) cmdMcp(args []string) (tea.Model, tea.Cmd) {
	// No args - open the MCP picker dialog
	if len(args) == 0 {
		if m.mcpManager == nil || len(m.mcpManager.AvailableServers()) == 0 {
			return m.showMCPQuickStart()
		}
		m.dialog.ShowMCPPicker(m.mcpManager)
		m.setTextareaValue("")
		return m, nil
	}

	subCmd := strings.ToLower(args[0])
	subArgs := args[1:]

	switch subCmd {
	case "list":
		return m.showBundledServersList()

	case "add":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp add <server>`\n\nUse `/mcp list` to see available servers.")
		}
		return m.quickAddMCP(strings.Join(subArgs, " "))

	case "start":
		if m.mcpManager == nil {
			return m.showMCPQuickStart()
		}
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp start <server>`\n\nUse `/mcp` to see configured servers.")
		}
		return m.mcpStartServer(strings.Join(subArgs, " "))

	case "stop":
		if m.mcpManager == nil {
			return m.showSystemMessage("No MCP servers configured.")
		}
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp stop <server>`\n\nUse `/mcp` to see running servers.")
		}
		return m.mcpStopServer(strings.Join(subArgs, " "))

	case "restart":
		if m.mcpManager == nil {
			return m.showSystemMessage("No MCP servers configured.")
		}
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/mcp restart <server>`")
		}
		name, err := m.mcpFindServer(subArgs[0])
		if err != nil {
			return m.showSystemMessage(err.Error())
		}
		if err := m.mcpManager.Restart(context.Background(), name); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to restart %s: %v", name, err))
		}
		m.setTextareaValue("")
		return m.showSystemMessage(fmt.Sprintf("Restarting MCP server: %s", name))

	case "status":
		if m.mcpManager == nil {
			return m.showMCPQuickStart()
		}
		return m.mcpShowStatus()

	default:
		return m.showSystemMessage(fmt.Sprintf("Unknown subcommand: %s\n\n**Commands:**\n- `/mcp start <server>` - Start a server\n- `/mcp stop <server>` - Stop a server\n- `/mcp add <server>` - Add a new server\n- `/mcp list` - Show available servers\n- `/mcp status` - Show current status", subCmd))
	}
}

// mcpFindServer finds a server by fuzzy matching
func (m *Model) mcpFindServer(query string) (string, error) {
	available := m.mcpManager.AvailableServers()
	if len(available) == 0 {
		return "", fmt.Errorf("no MCP servers configured\n\nUse `/mcp add %s` to add it", query)
	}

	queryLower := strings.ToLower(query)
	var exactMatch string
	var prefixMatches []string
	var containsMatches []string

	for _, s := range available {
		sLower := strings.ToLower(s)
		if sLower == queryLower {
			exactMatch = s
			break
		}
		if strings.HasPrefix(sLower, queryLower) {
			prefixMatches = append(prefixMatches, s)
		} else if strings.Contains(sLower, queryLower) {
			containsMatches = append(containsMatches, s)
		}
	}

	if exactMatch != "" {
		return exactMatch, nil
	}
	if len(prefixMatches) == 1 {
		return prefixMatches[0], nil
	}
	if len(prefixMatches) > 1 {
		return "", fmt.Errorf("multiple servers match '%s':\n\n- %s\n\nBe more specific", query, strings.Join(prefixMatches, "\n- "))
	}
	if len(containsMatches) == 1 {
		return containsMatches[0], nil
	}
	if len(containsMatches) > 1 {
		return "", fmt.Errorf("multiple servers match '%s':\n\n- %s\n\nBe more specific", query, strings.Join(containsMatches, "\n- "))
	}
	return "", fmt.Errorf("no server matches '%s'\n\nConfigured: %s", query, strings.Join(available, ", "))
}

// mcpStartServer starts a server by name (with fuzzy matching)
func (m *Model) mcpStartServer(query string) (tea.Model, tea.Cmd) {
	name, err := m.mcpFindServer(query)
	if err != nil {
		return m.showSystemMessage(err.Error())
	}

	status, _ := m.mcpManager.ServerStatus(name)
	if status == "ready" {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is already running.", name))
	}
	if status == "starting" {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is already starting.", name))
	}

	if err := m.mcpManager.Enable(context.Background(), name); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to start %s: %v", name, err))
	}
	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("Starting **%s**... tools will be available shortly.", name))
}

// mcpStopServer stops a server by name (with fuzzy matching)
func (m *Model) mcpStopServer(query string) (tea.Model, tea.Cmd) {
	name, err := m.mcpFindServer(query)
	if err != nil {
		return m.showSystemMessage(err.Error())
	}

	status, _ := m.mcpManager.ServerStatus(name)
	if status == "stopped" || status == "" {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is not running.", name))
	}

	if err := m.mcpManager.Disable(name); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to stop %s: %v", name, err))
	}
	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf("Stopped **%s**", name))
}

func (m *Model) mcpShowStatus() (tea.Model, tea.Cmd) {
	var b strings.Builder

	available := m.mcpManager.AvailableServers()
	states := m.mcpManager.GetAllStates()

	if len(available) == 0 {
		b.WriteString("## MCP Servers\n\n")
		b.WriteString("No MCP servers configured.\n\n")
		b.WriteString("**Quick start:**\n")
		b.WriteString("- `/mcp add playwright` - Browser automation\n")
		b.WriteString("- `/mcp add github` - GitHub integration\n")
		b.WriteString("- `/mcp add filesystem` - File operations\n")
		b.WriteString("- `/mcp list` - See all available servers\n")
		return m.showSystemMessage(b.String())
	}

	b.WriteString("## MCP Servers\n\n")

	// Build status map
	statusMap := make(map[string]string)
	for _, state := range states {
		switch state.Status {
		case "starting":
			statusMap[state.Name] = "starting..."
		case "ready":
			statusMap[state.Name] = "running"
		case "failed":
			errMsg := "failed"
			if state.Error != nil {
				errMsg = fmt.Sprintf("failed: %v", state.Error)
			}
			statusMap[state.Name] = errMsg
		default:
			statusMap[state.Name] = "stopped"
		}
	}

	hasStoppedServers := false
	for _, name := range available {
		status := statusMap[name]
		if status == "" {
			status = "stopped"
		}
		if status == "stopped" {
			hasStoppedServers = true
		}

		icon := "  "
		if status == "running" {
			icon = "* "
		} else if status == "starting..." {
			icon = ". "
		}

		b.WriteString(fmt.Sprintf("%s**%s** - %s\n", icon, name, status))
	}

	// Add hint for stopped servers
	if hasStoppedServers {
		b.WriteString("\n`/mcp start <name>` to start a server\n")
	}

	// Show tools from running servers
	tools := m.mcpManager.AllTools()
	if len(tools) > 0 {
		b.WriteString(fmt.Sprintf("\n**Available tools (%d):**\n", len(tools)))
		for _, t := range tools {
			// Tool name is prefixed with "servername__toolname"
			parts := strings.SplitN(t.Name, "__", 2)
			if len(parts) == 2 {
				b.WriteString(fmt.Sprintf("- `%s` (%s)\n", parts[1], parts[0]))
			} else {
				b.WriteString(fmt.Sprintf("- `%s`\n", t.Name))
			}
		}
	}

	b.WriteString("\n**Commands:**\n")
	b.WriteString("- `/mcp start <server>` - Start a server\n")
	b.WriteString("- `/mcp stop <server>` - Stop a server\n")
	b.WriteString("- `/mcp add <name>` - Add a new server\n")
	b.WriteString("- `/mcp list` - Show available servers\n")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

// showMCPQuickStart shows helpful info when user presses Ctrl+M with no MCPs configured
func (m *Model) showMCPQuickStart() (tea.Model, tea.Cmd) {
	var b strings.Builder
	b.WriteString("## MCP Quick Start\n\n")
	b.WriteString("MCP servers give the LLM tools like browser automation, database access, and more.\n\n")
	b.WriteString("**Popular servers:**\n")
	b.WriteString("- `playwright` - Browser automation with accessibility snapshots\n")
	b.WriteString("- `filesystem` - Secure file operations\n")
	b.WriteString("- `git` - Git repository operations\n")
	b.WriteString("- `github` - GitHub API integration\n")
	b.WriteString("- `fetch` - Web content fetching\n")
	b.WriteString("\n**Get started:**\n")
	b.WriteString("- `/mcp add playwright` - Add a server\n")
	b.WriteString("- `/mcp list` - See all available servers\n")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

// quickAddMCP adds an MCP server from bundled servers
func (m *Model) quickAddMCP(query string) (tea.Model, tea.Cmd) {
	bundled := mcp.GetBundledServers()
	queryLower := strings.ToLower(query)

	// First try exact name match
	var match *mcp.BundledServer
	for i, s := range bundled {
		if strings.ToLower(s.Name) == queryLower {
			match = &bundled[i]
			break
		}
	}

	// Then try fuzzy match on name or description
	if match == nil {
		for i, s := range bundled {
			if strings.Contains(strings.ToLower(s.Name), queryLower) ||
				strings.Contains(strings.ToLower(s.Description), queryLower) {
				match = &bundled[i]
				break
			}
		}
	}

	if match == nil {
		return m.showSystemMessage(fmt.Sprintf(
			"No server found matching '%s'.\n\nUse `/mcp list` to see available servers.",
			query))
	}

	// Load config
	cfg, err := mcp.LoadConfig()
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to load MCP config: %v", err))
	}

	// Check if already configured
	if _, exists := cfg.Servers[match.Name]; exists {
		return m.showSystemMessage(fmt.Sprintf("Server '%s' is already configured.\n\nUse `/mcp %s` to enable it.", match.Name, match.Name))
	}

	// Add to config
	serverConfig := match.ToServerConfig()
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]mcp.ServerConfig)
	}
	cfg.Servers[match.Name] = serverConfig

	if err := cfg.Save(); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to save MCP config: %v", err))
	}

	// Reload manager config and auto-enable the server
	if m.mcpManager != nil {
		if err := m.mcpManager.LoadConfig(); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Added '%s' but failed to reload config: %v\n\nRestart chat to use it.", match.Name, err))
		}
		// Auto-enable the newly added server
		if err := m.mcpManager.Enable(context.Background(), match.Name); err != nil {
			m.setTextareaValue("")
			return m.showSystemMessage(fmt.Sprintf(
				"Added **%s** but failed to start: %v\n\nUse `/mcp %s` to try again.",
				match.Name, err, match.Name))
		}
	}

	m.setTextareaValue("")
	return m.showSystemMessage(fmt.Sprintf(
		"Enabled **%s**\n\n%s\n\nTools will be available shortly.",
		match.Name, match.Description))
}

// showBundledServersList shows available bundled servers
func (m *Model) showBundledServersList() (tea.Model, tea.Cmd) {
	bundled := mcp.GetBundledServers()

	// Group by category
	byCategory := make(map[string][]mcp.BundledServer)
	for _, s := range bundled {
		byCategory[s.Category] = append(byCategory[s.Category], s)
	}

	// Define category order
	categoryOrder := []string{"Reference", "Browser", "DevTools", "Database", "Cloud", "Productivity", "Search", "Data", "Finance", "Communication", "Other"}

	var b strings.Builder
	b.WriteString("## Available MCP Servers\n\n")

	for _, cat := range categoryOrder {
		servers, ok := byCategory[cat]
		if !ok || len(servers) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("**%s:**\n", cat))
		for _, s := range servers {
			b.WriteString(fmt.Sprintf("- `%s` - %s\n", s.Name, s.Description))
		}
		b.WriteString("\n")
	}

	b.WriteString("Use `/mcp add <name>` to add a server.\n")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

func (m *Model) cmdSkills() (tea.Model, tea.Cmd) {
	var b strings.Builder
	b.WriteString("## Skills System\n\n")
	b.WriteString("Skills are reusable prompt templates that can be activated to help with specific tasks.\n\n")

	b.WriteString("**How to use skills:**\n")
	b.WriteString("- Ask the AI to use a specific skill: \"use the code-review skill\"\n")
	b.WriteString("- The AI can also activate skills automatically when relevant\n")
	b.WriteString("- Skills are loaded from: `~/.config/term-llm/skills/`, `.skills/`, and project directories\n\n")

	b.WriteString("**Manage skills with CLI:**\n")
	b.WriteString("- `term-llm skills list` - List available skills\n")
	b.WriteString("- `term-llm skills new <name>` - Create a new skill\n")
	b.WriteString("- `term-llm skills show <name>` - View skill details\n")
	b.WriteString("- `term-llm skills edit <name>` - Edit an existing skill\n\n")

	b.WriteString("**Create skills:**\n")
	b.WriteString("Skills are defined in SKILL.md files with YAML frontmatter:\n\n")
	b.WriteString("```yaml\n")
	b.WriteString("---\n")
	b.WriteString("name: my-skill\n")
	b.WriteString("description: A helpful skill\n")
	b.WriteString("---\n")
	b.WriteString("Instructions for the AI when this skill is activated...\n")
	b.WriteString("```\n")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

func (m *Model) cmdInspect() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")

	if len(m.messages) == 0 {
		return m.showSystemMessage("No messages to inspect. Send a message first.")
	}

	m.inspectorMode = true
	m.inspectorModel = inspector.NewWithStore(m.messages, m.width, m.height, m.styles, m.store)
	return m, nil
}

func (m *Model) cmdCompress() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")

	if m.streaming {
		return m.showSystemMessage("Cannot compress while streaming. Wait for the response to finish.")
	}

	// Need at least a couple of messages to make compaction worthwhile
	m.messagesMu.Lock()
	snapshot := make([]session.Message, len(m.messages))
	copy(snapshot, m.messages)
	m.messagesMu.Unlock()

	if len(snapshot) < 2 {
		return m.showSystemMessage("Not enough conversation history to compress.")
	}

	// Build llm.Message slice from session messages, separating system prompt
	var llmMessages []llm.Message
	var systemPrompt string
	for _, msg := range snapshot {
		if msg.Role == llm.RoleSystem {
			for _, p := range msg.Parts {
				if p.Text != "" {
					systemPrompt = p.Text
					break
				}
			}
			continue
		}
		llmMessages = append(llmMessages, msg.ToLLMMessage())
	}

	if len(llmMessages) == 0 {
		return m.showSystemMessage("No conversation messages to compress.")
	}

	compactConfig := llm.DefaultCompactionConfig()
	model := m.modelName
	provider := m.provider

	m.streaming = true
	m.phase = "Compacting"
	m.streamStartTime = time.Now()

	theme := m.styles.Theme()
	muted := lipgloss.NewStyle().Foreground(theme.Muted)
	statusLine := muted.Render(fmt.Sprintf("⠋ Compacting %d messages...", len(llmMessages)))

	return m, tea.Batch(
		tea.Println(statusLine),
		func() tea.Msg {
			ctx, cancel := context.WithCancel(context.Background())
			m.streamCancelFunc = cancel
			result, err := llm.Compact(ctx, provider, model, systemPrompt, llmMessages, compactConfig)
			return compactDoneMsg{result: result, err: err}
		},
		m.spinner.Tick,
		m.tickEvery(),
	)
}

// switchModel switches to a new provider:model
func (m *Model) switchModel(providerModel string) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(providerModel, ":", 2)
	if len(parts) != 2 {
		return m.showSystemMessage(fmt.Sprintf("Invalid model format: %s", providerModel))
	}

	providerName := parts[0]
	modelName := parts[1]

	// Create new provider using the centralized factory
	provider, err := llm.NewProviderByName(m.config, providerName, modelName)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to switch model: %v", err))
	}

	// Update model state
	m.provider = provider
	// Preserve existing tool registry when creating new engine
	m.engine = llm.NewEngine(provider, m.engine.Tools())
	m.providerName = provider.Name()
	m.providerKey = providerName
	m.modelName = modelName

	// Keep session metadata aligned so future resume restores correct runtime.
	if m.sess != nil {
		m.sess.Provider = m.providerName
		m.sess.ProviderKey = m.providerKey
		m.sess.Model = modelName
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}

	// Record model usage for MRU ordering in the picker
	m.recordCurrentModelUse()

	return m.showSystemMessage(fmt.Sprintf("Switched to %s:%s", providerName, modelName))
}

// cmdHandover handles /handover @agent [provider:model]
func (m *Model) cmdHandover(args []string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")

	if m.streaming {
		return m.showSystemMessage("Cannot handover while streaming. Wait for the response to finish.")
	}

	if len(args) == 0 {
		return m.showSystemMessage("Usage: /handover @agent [provider:model]\nExample: /handover @developer anthropic:claude-sonnet-4-5")
	}

	if m.agentResolver == nil {
		return m.showSystemMessage("Agent resolver not configured.")
	}

	if m.store == nil {
		return m.showSystemMessage("Handover requires session storage. Enable sessions to use /handover.")
	}

	// Parse agent name (strip @ prefix if present)
	agentName := strings.TrimPrefix(args[0], "@")

	// Optional provider:model override
	var providerStr string
	if len(args) > 1 {
		resolved, ok := resolveProviderModelArg(args[1], m.config, "")
		if !ok {
			return m.showSystemMessage(fmt.Sprintf("Invalid provider format: %s (expected provider:model)", args[1]))
		}
		providerStr = resolved
	}

	// Resolve target agent
	targetAgent, err := m.agentResolver(agentName, m.config)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to load agent @%s: %v", agentName, err))
	}
	if targetAgent == nil {
		return m.showSystemMessage(fmt.Sprintf("Agent @%s not found.", agentName))
	}

	// Determine the new system prompt from the target agent.
	// Fall back to empty string — the restart path will resolve it properly.
	newSystemPrompt := targetAgent.SystemPrompt

	// Determine handover mode from the current (source) agent
	mode := ""
	if m.currentAgent != nil {
		mode = m.currentAgent.HandoverMode
	}

	sourceAgent := m.agentName
	if sourceAgent == "" {
		sourceAgent = "default"
	}

	// Target-declared script takes precedence over source's handover mode, but it
	// only runs after the user explicitly confirms the handover.
	if strings.TrimSpace(targetAgent.HandoverScript) != "" {
		preview := pendingTargetScriptPreview(targetAgent)
		result := llm.HandoverFromFile(preview, newSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{result: result, agentName: agentName, providerStr: providerStr}
		}
	}

	// Light mode: use last assistant message as handover context
	if mode == "light" {
		doc := m.lastAssistantMessage()
		if doc == "" {
			doc = "(No assistant response to hand over.)"
		}
		result := llm.HandoverFromFile(doc, newSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{
				result:      result,
				agentName:   agentName,
				providerStr: providerStr,
			}
		}
	}

	// Script mode (source-side): run source agent's handover_script
	if mode == "script" {
		script := ""
		if m.currentAgent != nil {
			script = m.currentAgent.HandoverScript
		}
		if script == "" {
			return m.showSystemMessage("Agent has handover_mode: script but no handover_script configured.")
		}
		return m.startHandoverScriptHandover(m.currentAgent, sourceAgent, targetAgent, providerStr, false, "")
	}

	// File mode: scan handover directory for latest .md file
	if mode == "file" || (mode == "" && m.currentAgent != nil && m.currentAgent.EnableHandover) {
		handoverDir := ""
		if m.currentAgent != nil && m.currentAgent.EnableHandover {
			if dir, err := session.GetHandoverDir("."); err == nil {
				handoverDir = dir
			}
		}
		if handoverDir != "" {
			latestFile, latestInfo := findLatestHandoverFile(handoverDir)
			if latestFile != "" && latestInfo.Size() > 0 {
				// Check freshness: file must have been modified after the session started
				sessionStart := time.Time{}
				if m.sess != nil {
					sessionStart = m.sess.CreatedAt
				}
				stale := !sessionStart.IsZero() && latestInfo.ModTime().Before(sessionStart)
				if stale && mode == "file" {
					// Explicit file mode — hard fail on stale file
					return m.showSystemMessage(fmt.Sprintf(
						"Handover file %s exists but predates this session (last modified %s).\n"+
							"Ask the agent to update it, or remove it to use LLM compression.",
						latestFile, latestInfo.ModTime().Format("2006-01-02 15:04")))
				}
				if !stale {
					content, readErr := os.ReadFile(latestFile)
					if readErr == nil && len(content) > 0 {
						result := llm.HandoverFromFile(string(content), newSystemPrompt, sourceAgent, targetAgent.Name)
						return m, func() tea.Msg {
							return handoverDoneMsg{
								result:      result,
								agentName:   agentName,
								providerStr: providerStr,
							}
						}
					}
				}
				// Auto mode with stale file: fall through to LLM compression
			}
			// File mode was explicitly set but no handover file found — warn
			if mode == "file" {
				return m.showSystemMessage(fmt.Sprintf(
					"Agent @%s has handover_mode: file but no .md files found in %s.\n"+
						"Ask the agent to write the handover document first.",
					sourceAgent, handoverDir))
			}
		}
	}

	// Compress mode (or auto fallback): LLM compression
	m.messagesMu.Lock()
	snapshot := make([]session.Message, len(m.messages))
	copy(snapshot, m.messages)
	m.messagesMu.Unlock()

	if len(snapshot) < 2 {
		result := llm.HandoverFromFile("(No prior conversation to hand over.)", newSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{
				result:      result,
				agentName:   agentName,
				providerStr: providerStr,
			}
		}
	}

	// Build llm.Message slice, extract system prompt
	var llmMessages []llm.Message
	var currentSystemPrompt string
	for _, msg := range snapshot {
		if msg.Role == llm.RoleSystem {
			for _, p := range msg.Parts {
				if p.Text != "" {
					currentSystemPrompt = p.Text
					break
				}
			}
			continue
		}
		llmMessages = append(llmMessages, msg.ToLLMMessage())
	}

	if len(llmMessages) == 0 {
		result := llm.HandoverFromFile("(No prior conversation to hand over.)", newSystemPrompt, sourceAgent, targetAgent.Name)
		return m, func() tea.Msg {
			return handoverDoneMsg{
				result:      result,
				agentName:   agentName,
				providerStr: providerStr,
			}
		}
	}

	compactConfig := llm.DefaultCompactionConfig()
	model := m.modelName
	provider := m.provider

	m.streaming = true
	m.phase = "Handover"
	m.streamStartTime = time.Now()

	theme := m.styles.Theme()
	muted := lipgloss.NewStyle().Foreground(theme.Muted)
	statusLine := muted.Render(fmt.Sprintf("⠋ Generating handover to @%s...", agentName))

	return m, tea.Batch(
		tea.Println(statusLine),
		func() tea.Msg {
			ctx, cancel := context.WithCancel(context.Background())
			m.streamCancelFunc = cancel
			result, err := llm.Handover(ctx, provider, model, currentSystemPrompt, newSystemPrompt, llmMessages, sourceAgent, targetAgent.Name, compactConfig)
			return handoverDoneMsg{result: result, err: err, agentName: agentName, providerStr: providerStr}
		},
		m.spinner.Tick,
		m.tickEvery(),
	)
}

// resolveAgentTools derives the comma-separated tool list from an agent's config.
func resolveAgentTools(agent *agents.Agent) string {
	if agent.HasEnabledList() {
		return strings.Join(agent.Tools.Enabled, ",")
	}
	if agent.HasDisabledList() {
		allTools := tools.AllToolNames()
		enabled := agent.GetEnabledTools(allTools)
		return strings.Join(enabled, ",")
	}
	return "" // No tool restrictions — use defaults
}

// lastAssistantMessage returns the text of the most recent assistant message.
func (m *Model) lastAssistantMessage() string {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()

	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == llm.RoleAssistant {
			var text strings.Builder
			for _, p := range m.messages[i].Parts {
				if p.Text != "" {
					text.WriteString(p.Text)
				}
			}
			return text.String()
		}
	}
	return ""
}

// pendingTargetScriptPreview describes a deferred target-side handover script.
func pendingTargetScriptPreview(agent *agents.Agent) string {
	var b strings.Builder
	b.WriteString("## Pending Target Handover Script\n\n")
	b.WriteString(fmt.Sprintf("@%s declares a handover script. It will run only after you confirm this handover and approve the command if required.\n", agent.Name))
	if script := strings.TrimSpace(agent.HandoverScript); script != "" {
		b.WriteString("\n```sh\n")
		b.WriteString(script)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// findLatestHandoverFile scans dir for .md files and returns the path and
// os.FileInfo of the most recently modified one. Returns ("", nil) if none found.
func findLatestHandoverFile(dir string) (string, os.FileInfo) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil
	}
	var latestPath string
	var latestInfo os.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if latestInfo == nil || info.ModTime().After(latestInfo.ModTime()) {
			latestPath = filepath.Join(dir, e.Name())
			latestInfo = info
		}
	}
	return latestPath, latestInfo
}

func handoverSourceAgent(pending *handoverDoneMsg, fallback string) string {
	if pending != nil && pending.result != nil && strings.TrimSpace(pending.result.SourceAgent) != "" {
		return strings.TrimSpace(pending.result.SourceAgent)
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "default"
	}
	return fallback
}

func handoverScriptCmd(ctx context.Context, approvalMgr *tools.ApprovalManager, scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, providerStr string, confirmed bool, instructions string) tea.Cmd {
	agentName := ""
	if targetAgent != nil {
		agentName = targetAgent.Name
	}
	return func() tea.Msg {
		result, err := buildScriptBackedHandover(ctx, approvalMgr, scriptAgent, sourceAgent, targetAgent)
		return handoverDoneMsg{
			result:       result,
			err:          err,
			agentName:    agentName,
			providerStr:  providerStr,
			confirmed:    confirmed,
			instructions: instructions,
		}
	}
}

func buildScriptBackedHandover(ctx context.Context, approvalMgr *tools.ApprovalManager, scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent) (*llm.HandoverResult, error) {
	if scriptAgent == nil {
		return nil, fmt.Errorf("handover script agent is not configured")
	}
	if targetAgent == nil {
		return nil, fmt.Errorf("target agent is not configured")
	}
	doc, err := runHandoverScript(ctx, approvalMgr, scriptAgent, scriptAgent.HandoverScript)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(doc) == "" {
		return nil, fmt.Errorf("handover script produced no output")
	}
	return llm.HandoverFromFile(doc, targetAgent.SystemPrompt, sourceAgent, targetAgent.Name), nil
}

func (m *Model) startHandoverScriptHandover(scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, providerStr string, confirmed bool, instructions string) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(m.rootContext())
	m.streamCancelFunc = cancel
	m.streaming = true
	m.phase = "Handover"
	m.streamStartTime = time.Now()

	theme := m.styles.Theme()
	muted := lipgloss.NewStyle().Foreground(theme.Muted)
	statusLine := muted.Render(fmt.Sprintf("⠋ Generating handover to @%s...", targetAgent.Name))

	return m, tea.Batch(
		tea.Println(statusLine),
		handoverScriptCmd(ctx, m.handoverApprovalMgr, scriptAgent, sourceAgent, targetAgent, providerStr, confirmed, instructions),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

// runHandoverScript executes a handover command without invoking a shell and returns its stdout.
func runHandoverScript(ctx context.Context, approvalMgr *tools.ApprovalManager, agent *agents.Agent, script string) (string, error) {
	script = strings.TrimSpace(script)
	if script == "" {
		return "", fmt.Errorf("handover_script is empty")
	}
	if tools.HasUnsafeShellSyntax(script) {
		return "", fmt.Errorf("handover_script must be a single executable plus arguments; shell operators like |, &&, or redirection are not supported")
	}

	argv, err := tools.SplitShellWords(script)
	if err != nil {
		return "", fmt.Errorf("invalid handover_script: %w", err)
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("handover_script is empty")
	}

	execPath, workDir, err := resolveHandoverScriptCommand(agent, argv)
	if err != nil {
		return "", err
	}

	if approvalMgr == nil {
		return "", fmt.Errorf("handover script approval is not configured")
	}
	outcome, err := approvalMgr.CheckShellApproval(script, workDir)
	if err != nil {
		return "", err
	}
	if outcome == tools.Cancel {
		return "", fmt.Errorf("handover script not approved")
	}

	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, execPath, argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	cleanup, prepErr := tools.PrepareCommand(cmd)
	if prepErr != nil {
		return "", fmt.Errorf("handover script setup failed: %w", prepErr)
	}
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("handover script timed out after 30s")
	}
	if errors.Is(execCtx.Err(), context.Canceled) {
		return "", execCtx.Err()
	}
	if err != nil {
		if output := strings.TrimSpace(stderr.String()); output != "" {
			return "", fmt.Errorf("handover script failed: %w: %s", err, output)
		}
		return "", fmt.Errorf("handover script failed: %w", err)
	}
	return stdout.String(), nil
}

func resolveHandoverScriptCommand(agent *agents.Agent, argv []string) (string, string, error) {
	workDir, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("resolve working directory: %w", err)
	}
	if gitInfo := tools.DetectGitRepo(workDir); gitInfo.IsRepo {
		workDir = gitInfo.Root
	}

	execPath := argv[0]
	if isRelativeCommandPath(execPath) {
		if agent == nil || !filepath.IsAbs(agent.SourcePath) || agent.Source == agents.SourceBuiltin {
			return "", "", fmt.Errorf("relative handover_script path %q requires a filesystem-backed agent source", execPath)
		}
		workDir = agent.SourcePath
		execPath = filepath.Join(agent.SourcePath, execPath)
	}

	return execPath, workDir, nil
}

func isRelativeCommandPath(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	return strings.Contains(path, "/") || strings.Contains(path, "\\") || strings.HasPrefix(path, ".")
}

func applyHandoverInstructions(result *llm.HandoverResult, instructions string) *llm.HandoverResult {
	instructions = strings.TrimSpace(instructions)
	if result == nil || instructions == "" {
		return result
	}
	document := "## Additional Instructions\n\n" + instructions + "\n\n" + result.Document
	return &llm.HandoverResult{
		Document:    document,
		NewMessages: llm.ReconstructHandoverHistory(handoverSystemPrompt(result.NewMessages), document, result.SourceAgent, result.TargetAgent),
		SourceAgent: result.SourceAgent,
		TargetAgent: result.TargetAgent,
	}
}

func handoverSystemPrompt(messages []llm.Message) string {
	if len(messages) == 0 || messages[0].Role != llm.RoleSystem {
		return ""
	}
	for _, p := range messages[0].Parts {
		if p.Text != "" {
			return p.Text
		}
	}
	return ""
}

// executeHandover performs the actual agent switch after user confirmation.
// It persists the handover messages and session metadata, then triggers a TUI
// restart so the target agent's full runtime configuration (tools, permissions,
// MCP, shell allowlists) is applied via the normal startup path.
func (m *Model) executeHandover() (tea.Model, tea.Cmd) {
	if m.pendingHandover == nil {
		return m, nil
	}

	pending := m.pendingHandover
	m.pendingHandover = nil

	theme := m.styles.Theme()
	errStyle := lipgloss.NewStyle().Foreground(theme.Error)

	// Step 1: Resolve target agent before any mutations
	targetAgent, resolveErr := m.agentResolver(pending.agentName, m.config)
	if resolveErr != nil || targetAgent == nil {
		m.cancelHandoverTool()
		msg := "unknown error"
		if resolveErr != nil {
			msg = resolveErr.Error()
		}
		return m, tea.Println(errStyle.Render(fmt.Sprintf("Handover failed to resolve target agent: %s", msg)))
	}

	result := applyHandoverInstructions(pending.result, pending.instructions)
	if result == nil {
		m.cancelHandoverTool()
		return m, tea.Println(errStyle.Render("Handover failed: no result returned."))
	}

	// Step 2: Persist handover messages
	var newSessionMsgs []session.Message
	for _, msg := range result.NewMessages {
		newSessionMsgs = append(newSessionMsgs, *session.NewMessage(m.sess.ID, msg, -1))
	}

	if err := m.store.CompactMessages(context.Background(), m.sess.ID, newSessionMsgs); err != nil {
		m.cancelHandoverTool()
		return m, tea.Println(errStyle.Render(fmt.Sprintf("Handover failed to persist: %v", err)))
	}

	// Step 3: Update session metadata with target agent's runtime settings.
	// On restart, cmd/chat.go:205-211 overwrites resolved settings with session
	// values (Tools, Search, MCP, Provider, Model), so we must persist the
	// target agent's values here — otherwise the old agent's config survives.
	m.sess.Agent = pending.agentName
	m.sess.Search = targetAgent.Search
	m.sess.Tools = resolveAgentTools(targetAgent)

	mcpNames := targetAgent.GetMCPServerNames()
	if len(mcpNames) > 0 {
		m.sess.MCP = strings.Join(mcpNames, ",")
	} else {
		m.sess.MCP = ""
	}

	// Provider/model: explicit override > target agent > keep current
	if pending.providerStr != "" {
		parts := strings.SplitN(pending.providerStr, ":", 2)
		if len(parts) == 2 {
			m.sess.ProviderKey = parts[0]
			m.sess.Model = parts[1]
		}
	} else {
		// Apply agent provider and model independently (matching ResolveSettings behavior)
		if targetAgent.Provider != "" {
			m.sess.ProviderKey = targetAgent.Provider
		}
		if targetAgent.Model != "" {
			m.sess.Model = targetAgent.Model
		}
	}

	if err := m.store.Update(context.Background(), m.sess); err != nil {
		m.cancelHandoverTool()
		return m, tea.Println(errStyle.Render(fmt.Sprintf("Handover failed to persist session metadata: %v", err)))
	}

	// Step 4: Update in-memory state only after both writes succeed
	m.messagesMu.Lock()
	m.compactionIdx = len(m.messages)
	m.messages = append(m.messages, newSessionMsgs...)
	m.messagesMu.Unlock()
	m.invalidateHistoryCache()

	// Step 5: Trigger TUI restart via the same resume mechanism used by /resume.
	// This causes runChatOnce to exit and the outer loop to re-enter with
	// the updated session, running the full agent setup path.
	m.pendingResumeSessionID = m.sess.ID
	if prompt := strings.TrimSpace(targetAgent.DefaultPrompt); prompt != "" {
		m.pendingHandoverAutoSend = prompt
	} else if pending != nil && pending.result != nil {
		m.pendingHandoverAutoSend = "Execute the pending tasks from the handover."
	} else {
		m.pendingHandoverAutoSend = ""
	}
	// Signal tool-initiated handover (if any) now that the handover is committed.
	// The session is about to restart so the tool result is moot,
	// but we unblock the goroutine to avoid a leak.
	if m.handoverToolDoneCh != nil {
		m.handoverToolDoneCh <- true
		m.handoverToolDoneCh = nil
	}
	// Cancel the engine stream now that the tool is unblocked
	if m.streamCancelFunc != nil {
		m.streamCancelFunc()
		m.streamCancelFunc = nil
	}
	m.quitting = true
	return m, tea.Quit
}
