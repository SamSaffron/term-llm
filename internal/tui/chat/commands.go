package chat

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
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
			Name:        "load",
			Description: "Load a saved session",
			Usage:       "/load <number|id>",
		},
		{
			Name:        "sessions",
			Aliases:     []string{"ls"},
			Description: "List saved sessions",
			Usage:       "/sessions",
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
	case "load":
		return m.cmdLoad(args)
	case "sessions":
		return m.cmdSessions()
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
	default:
		return m.showSystemMessage(fmt.Sprintf("Command /%s is not yet implemented.", cmd.Name))
	}
}

// Command implementations

func (m *Model) showSystemMessage(content string) (tea.Model, tea.Cmd) {
	// In inline mode, print directly to scrollback rather than adding to session
	m.setTextareaValue("")

	// Render the message content with markdown
	rendered := m.renderMarkdown(content)

	return m, tea.Println(rendered + "\n")
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
		ID:        session.NewID(),
		Provider:  m.providerName,
		Model:     m.modelName,
		Mode:      session.ModeChat,
		Agent:     m.agentName,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Search:    m.searchEnabled,
		Tools:     m.toolsStr,
		MCP:       m.mcpStr,
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
		m.tracker = ui.NewToolTracker()
		m.tracker.TextMode = m.textMode
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

	return m, tea.Println("Conversation cleared (new session started).")
}

func (m *Model) cmdQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	return m, tea.Quit
}

func (m *Model) cmdModel(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// Show model picker dialog
		m.dialog.ShowModelPicker(m.modelName, GetAvailableProviders(m.config))
		m.setTextareaValue("")
		return m, nil
	}

	// Switch to specified model (format: provider:model or just model/alias)
	modelArg := args[0]

	// If it already has provider prefix, use as-is
	if strings.Contains(modelArg, ":") {
		return m.switchModel(modelArg)
	}

	// Try fuzzy matching across all providers
	match := fuzzyMatchModel(modelArg, m.config)
	if match != "" {
		return m.switchModel(match)
	}

	// Fallback to current provider with exact name
	return m.switchModel(m.providerName + ":" + modelArg)
}

// fuzzyMatchModel finds the best matching model for a query
// Returns "provider:model" or empty string if no good match
func fuzzyMatchModel(query string, cfg *config.Config) string {
	query = strings.ToLower(query)

	// Build list of all provider:model combinations
	type modelEntry struct {
		provider string
		model    string
		combined string
	}
	var allModels []modelEntry
	for _, provider := range GetAvailableProviders(cfg) {
		for _, model := range provider.Models {
			allModels = append(allModels, modelEntry{
				provider: provider.Name,
				model:    model,
				combined: provider.Name + ":" + model,
			})
		}
	}

	// First try exact substring match on model name
	// Collect all matches and prefer shorter model names
	var substringMatches []modelEntry
	for _, entry := range allModels {
		if strings.Contains(strings.ToLower(entry.model), query) {
			substringMatches = append(substringMatches, entry)
		}
	}
	if len(substringMatches) > 0 {
		// Prefer shorter model names (more specific matches)
		best := substringMatches[0]
		for _, entry := range substringMatches[1:] {
			if len(entry.model) < len(best.model) {
				best = entry
			}
		}
		return best.combined
	}

	// Try fuzzy match using the fuzzy package
	modelNames := make([]string, len(allModels))
	for i, entry := range allModels {
		modelNames[i] = entry.model
	}
	matches := fuzzy.Find(query, modelNames)
	if len(matches) > 0 {
		return allModels[matches[0].Index].combined
	}

	return ""
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
		ID:        session.NewID(),
		Provider:  m.providerName,
		Model:     m.modelName,
		Mode:      session.ModeChat,
		Agent:     m.agentName,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Search:    m.searchEnabled,
		Tools:     m.toolsStr,
		MCP:       m.mcpStr,
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
		m.tracker = ui.NewToolTracker()
		m.tracker.TextMode = m.textMode
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

func (m *Model) cmdLoad(args []string) (tea.Model, tea.Cmd) {
	if m.store == nil {
		return m.showSystemMessage("Session storage is disabled.")
	}

	ctx := context.Background()
	if len(args) == 0 {
		// Show session list dialog
		summaries, err := m.store.List(ctx, session.ListOptions{Limit: 20})
		if err != nil || len(summaries) == 0 {
			return m.showSystemMessage("No saved sessions found.\nUse `/save [name]` to save the current session.")
		}
		// Convert to DialogItem slice with full IDs
		var items []DialogItem
		for _, s := range summaries {
			label := s.Name
			if label == "" {
				label = fmt.Sprintf("#%d", s.Number)
			}
			items = append(items, DialogItem{
				ID:    s.ID, // Full session ID for lookup
				Label: label,
			})
		}
		currentID := ""
		if m.sess != nil {
			currentID = m.sess.ID
		}
		m.dialog.ShowSessionList(items, currentID)
		m.setTextareaValue("")
		return m, nil
	}

	// Load by session ID or name
	sessionID := args[0]
	sess, err := m.store.Get(ctx, sessionID)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to load session: %v", err))
	}
	if sess == nil {
		return m.showSystemMessage(fmt.Sprintf("Session '%s' not found.", sessionID))
	}

	messages, _ := m.store.GetMessages(ctx, sess.ID, 0, 0)
	m.sess = sess
	m.messages = messages
	m.scrollOffset = 0
	m.setTextareaValue("")
	_ = m.store.SetCurrent(ctx, sess.ID)

	name := sess.Name
	if name == "" {
		name = fmt.Sprintf("#%d", sess.Number)
	}
	return m.showSystemMessage(fmt.Sprintf("Loaded session '%s' (%d messages).", name, len(messages)))
}

func (m *Model) cmdSessions() (tea.Model, tea.Cmd) {
	if m.store == nil {
		return m.showSystemMessage("Session storage is disabled.")
	}

	summaries, err := m.store.List(context.Background(), session.ListOptions{Limit: 20})
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to list sessions: %v", err))
	}

	if len(summaries) == 0 {
		return m.showSystemMessage("No saved sessions found.\nUse `/save [name]` to save the current session.")
	}

	var b strings.Builder
	b.WriteString("## Recent Sessions\n\n")
	for _, s := range summaries {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("#%d", s.Number)
		}
		summary := s.Summary
		if len(summary) > 50 {
			summary = summary[:47] + "..."
		}
		b.WriteString(fmt.Sprintf("- `%s` (%s) - %d msgs - %s\n", name, s.Provider, s.MessageCount, summary))
	}
	b.WriteString("\nUse `/load <number>` to load a session.")

	m.setTextareaValue("")
	return m.showSystemMessage(b.String())
}

func (m *Model) cmdResume(args []string) (tea.Model, tea.Cmd) {
	if m.store == nil {
		return m.showSystemMessage("Session storage is disabled.")
	}

	ctx := context.Background()

	// /resume <number|id> — direct resume without the picker
	if len(args) > 0 {
		sess, err := m.store.GetByPrefix(ctx, args[0])
		if err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to find session: %v", err))
		}
		if sess == nil {
			return m.showSystemMessage(fmt.Sprintf("Session '%s' not found.", args[0]))
		}

		messages, _ := m.store.GetMessages(ctx, sess.ID, 0, 0)
		m.sess = sess
		m.messages = messages
		m.scrollOffset = 0
		m.setTextareaValue("")
		_ = m.store.SetCurrent(ctx, sess.ID)

		name := sess.Name
		if name == "" {
			name = fmt.Sprintf("#%d", sess.Number)
		}
		return m.showSystemMessage(fmt.Sprintf("Resumed session '%s' (%d messages).", name, len(messages)))
	}

	// /resume with no args — show the session picker dialog with richer labels
	summaries, err := m.store.List(ctx, session.ListOptions{Limit: 30})
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to list sessions: %v", err))
	}
	if len(summaries) == 0 {
		return m.showSystemMessage("No saved sessions found.")
	}

	var items []DialogItem
	for _, s := range summaries {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("#%d", s.Number)
		}

		// Age
		age := resumeFormatAge(s.UpdatedAt)

		// Summary snippet
		snippet := s.Summary
		if len(snippet) > 45 {
			snippet = snippet[:42] + "..."
		}

		var label string
		if snippet != "" {
			label = fmt.Sprintf("%s  %s · %dm · %s", name, snippet, s.MessageCount, age)
		} else {
			label = fmt.Sprintf("%s  %dm · %s", name, s.MessageCount, age)
		}

		items = append(items, DialogItem{
			ID:    s.ID,
			Label: label,
		})
	}

	currentID := ""
	if m.sess != nil {
		currentID = m.sess.ID
	}
	m.dialog.ShowSessionList(items, currentID)
	m.setTextareaValue("")
	return m, nil
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
	// Only enter alt screen if chat isn't already in alt screen mode
	if !m.altScreen {
		return m, tea.EnterAltScreen
	}
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
	m.providerName = providerName
	m.modelName = modelName

	return m.showSystemMessage(fmt.Sprintf("Switched to %s:%s", providerName, modelName))
}
