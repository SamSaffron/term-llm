package chat

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sahilm/fuzzy"
)

// Command represents a slash command
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
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
			Usage:       "/load <name>",
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
func FilterCommands(query string) []Command {
	commands := AllCommands()
	if query == "" {
		return commands
	}

	// Remove leading slash if present
	query = strings.TrimPrefix(query, "/")

	// First check for exact alias matches
	queryLower := strings.ToLower(query)
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
	default:
		return m.showSystemMessage(fmt.Sprintf("Command /%s is not yet implemented.", cmd.Name))
	}
}

// Command implementations

func (m *Model) showSystemMessage(content string) (tea.Model, tea.Cmd) {
	// In inline mode, print directly to scrollback rather than adding to session
	m.textarea.SetValue("")

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
	b.WriteString("- `Ctrl+P` - Command palette\n")
	b.WriteString("- `Ctrl+L` - Switch model\n")
	b.WriteString("- `Ctrl+N` - New session\n")
	b.WriteString("- `Esc` - Cancel streaming\n")

	return m.showSystemMessage(b.String())
}

func (m *Model) cmdClear() (tea.Model, tea.Cmd) {
	m.session.Messages = []ChatMessage{}
	m.scrollOffset = 0
	m.textarea.SetValue("")

	// Print confirmation and save session
	return m, tea.Batch(
		tea.Println("Conversation cleared.\n"),
		m.saveSessionCmd(),
	)
}

func (m *Model) cmdQuit() (tea.Model, tea.Cmd) {
	m.quitting = true
	return m, tea.Quit
}

func (m *Model) cmdModel(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// Show model picker dialog
		m.dialog.ShowModelPicker(m.modelName, GetAvailableProviders())
		m.textarea.SetValue("")
		return m, nil
	}

	// Switch to specified model (format: provider:model or just model/alias)
	modelArg := args[0]

	// If it already has provider prefix, use as-is
	if strings.Contains(modelArg, ":") {
		return m.switchModel(modelArg)
	}

	// Try fuzzy matching across all providers
	match := fuzzyMatchModel(modelArg)
	if match != "" {
		return m.switchModel(match)
	}

	// Fallback to current provider with exact name
	return m.switchModel(m.providerName + ":" + modelArg)
}

// fuzzyMatchModel finds the best matching model for a query
// Returns "provider:model" or empty string if no good match
func fuzzyMatchModel(query string) string {
	query = strings.ToLower(query)

	// Build list of all provider:model combinations
	type modelEntry struct {
		provider string
		model    string
		combined string
	}
	var allModels []modelEntry
	for _, provider := range GetAvailableProviders() {
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

func (m *Model) cmdSearch() (tea.Model, tea.Cmd) {
	m.searchEnabled = !m.searchEnabled
	m.textarea.SetValue("")
	status := "disabled"
	if m.searchEnabled {
		status = "enabled"
	}
	return m.showSystemMessage(fmt.Sprintf("Web search %s.", status))
}

func (m *Model) cmdNew() (tea.Model, tea.Cmd) {
	// Save current session first if it has messages
	if len(m.session.Messages) > 0 {
		_ = SaveCurrentSession(m.session)
	}

	// Create new session
	m.session = NewSession(m.providerName, m.modelName)
	m.scrollOffset = 0
	m.textarea.SetValue("")

	return m.showSystemMessage("Started new session.")
}

func (m *Model) cmdSave(args []string) (tea.Model, tea.Cmd) {
	name := ""
	if len(args) > 0 {
		name = strings.Join(args, "-")
	} else {
		// Generate name from first message or timestamp
		if len(m.session.Messages) > 0 {
			// Use first few words of first user message
			for _, msg := range m.session.Messages {
				if msg.Role == RoleUser {
					words := strings.Fields(msg.Content)
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

	m.session.Name = name
	if err := SaveSession(m.session, name+".json"); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to save session: %v", err))
	}

	m.textarea.SetValue("")
	return m.showSystemMessage(fmt.Sprintf("Session saved as '%s'.", name))
}

func (m *Model) cmdLoad(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		// Show session list dialog
		sessions, err := ListSessions()
		if err != nil || len(sessions) == 0 {
			return m.showSystemMessage("No saved sessions found.\nUse `/save [name]` to save the current session.")
		}
		m.dialog.ShowSessionList(sessions, m.session.Name)
		m.textarea.SetValue("")
		return m, nil
	}

	name := args[0]
	session, err := LoadSession(name + ".json")
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to load session: %v", err))
	}
	if session == nil {
		return m.showSystemMessage(fmt.Sprintf("Session '%s' not found.", name))
	}

	m.session = session
	m.scrollOffset = 0
	m.textarea.SetValue("")
	return m.showSystemMessage(fmt.Sprintf("Loaded session '%s' (%d messages).", name, len(session.Messages)))
}

func (m *Model) cmdSessions() (tea.Model, tea.Cmd) {
	sessions, err := ListSessions()
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to list sessions: %v", err))
	}

	if len(sessions) == 0 {
		return m.showSystemMessage("No saved sessions found.\nUse `/save [name]` to save the current session.")
	}

	var b strings.Builder
	b.WriteString("## Saved Sessions\n\n")
	for _, name := range sessions {
		b.WriteString(fmt.Sprintf("- `%s`\n", name))
	}
	b.WriteString("\nUse `/load <name>` to load a session.")

	m.textarea.SetValue("")
	return m.showSystemMessage(b.String())
}

func (m *Model) cmdExport(args []string) (tea.Model, tea.Cmd) {
	if len(m.session.Messages) == 0 {
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
	if m.session.Name != "" {
		b.WriteString(fmt.Sprintf("**Session:** %s\n", m.session.Name))
	}
	b.WriteString("\n---\n\n")

	// Messages
	for _, msg := range m.session.Messages {
		// Role header
		if msg.Role == RoleUser {
			b.WriteString("## ❯\n\n")
		} else {
			b.WriteString("## 🤖 Assistant")
			if msg.DurationMs > 0 {
				b.WriteString(fmt.Sprintf(" *(%.1fs", float64(msg.DurationMs)/1000))
				if msg.Tokens > 0 {
					b.WriteString(fmt.Sprintf(", %d tokens", msg.Tokens))
				}
				if msg.WebSearch {
					b.WriteString(", web search")
				}
				b.WriteString(")*")
			}
			b.WriteString("\n\n")
		}

		// Content - for user messages, extract just the text (not file contents)
		content := msg.Content
		if msg.Role == RoleUser && len(msg.Files) > 0 {
			if idx := strings.Index(content, "\n\n---\n**Attached files:**"); idx != -1 {
				content = strings.TrimSpace(content[:idx])
			}
			b.WriteString(content)
			b.WriteString("\n\n")
			b.WriteString("**Attached files:** ")
			b.WriteString(strings.Join(msg.Files, ", "))
			b.WriteString("\n")
		} else {
			b.WriteString(content)
		}

		b.WriteString("\n---\n\n")
	}

	// Write to file
	if err := os.WriteFile(outputPath, []byte(b.String()), 0644); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to export: %v", err))
	}

	m.textarea.SetValue("")
	return m.showSystemMessage(fmt.Sprintf("Exported %d messages to `%s`", len(m.session.Messages), outputPath))
}

func (m *Model) cmdSystem(args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 {
		if m.config.Ask.Instructions != "" {
			return m.showSystemMessage(fmt.Sprintf("Current system prompt:\n\n%s", m.config.Ask.Instructions))
		}
		return m.showSystemMessage("No system prompt set.\nUsage: `/system <prompt>`")
	}

	// Set custom system prompt (session-only, doesn't persist to config)
	prompt := strings.Join(args, " ")
	m.config.Ask.Instructions = prompt
	m.textarea.SetValue("")
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
		m.textarea.SetValue("")
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
		m.textarea.SetValue("")
		return m.showSystemMessage(fmt.Sprintf("Approved directory: `%s`", path))

	case "remove", "rm", "delete":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/dirs remove <path>`")
		}
		path := strings.Join(subArgs, " ")
		if err := m.approvedDirs.RemoveDirectory(path); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to remove directory: %v", err))
		}
		m.textarea.SetValue("")
		return m.showSystemMessage(fmt.Sprintf("Removed directory from approved list: `%s`", path))

	default:
		return m.showSystemMessage(fmt.Sprintf("Unknown subcommand: %s\n\nUsage:\n- `/dirs` - List approved directories\n- `/dirs add <path>` - Approve a directory\n- `/dirs remove <path>` - Revoke approval", subCmd))
	}
}
