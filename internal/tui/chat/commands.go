package chat

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/sahilm/fuzzy"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

// SlashEntryKind distinguishes fixed built-ins from dynamically discovered
// user-invocable skills.
type SlashEntryKind int

const (
	SlashEntryBuiltIn SlashEntryKind = iota
	SlashEntrySkill
)

// Command represents a slash completion/resolution entry.
type Command struct {
	Name         string
	Aliases      []string
	Description  string
	Usage        string
	Subcommands  []Subcommand // Optional subcommands
	Kind         SlashEntryKind
	ArgumentHint string
	Source       string
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
			Name:        "stats",
			Aliases:     []string{"st"},
			Description: "Show current chat usage, cost, and context breakdown",
			Usage:       "/stats",
		},
		{
			Name:        "usage",
			Description: "Show live account usage for the current provider",
			Usage:       "/usage",
		},
		{
			Name:        "goal",
			Aliases:     []string{"g"},
			Description: "Set or manage a persistent objective",
			Usage:       "/goal [set] <objective> [--budget N] | status|pause|resume|clear|edit",
			Subcommands: []Subcommand{
				{Name: "set", Description: "Set a new goal"},
				{Name: "status", Description: "Show current goal"},
				{Name: "pause", Description: "Pause the active goal"},
				{Name: "resume", Description: "Resume a paused or blocked goal"},
				{Name: "clear", Description: "Clear the goal"},
				{Name: "edit", Description: "Edit the goal objective"},
			},
		},
		{
			Name:        "side",
			Description: "Ask a private one-turn question about this conversation",
			Usage:       "/side <question>",
		},
		{
			Name:         "copy",
			Description:  "Copy one assistant response as source Markdown",
			Usage:        "/copy [N]",
			ArgumentHint: "[N]",
		},
		{
			Name:        "share",
			Description: "Share this complete session (unlisted is not private)",
			Usage:       "/share [new] [raw] [public|unlisted|private]",
			Subcommands: []Subcommand{
				{Name: "new", Description: "Create a new share"},
				{Name: "raw", Description: "Explicitly include privacy-sensitive raw model reasoning"},
				{Name: "public", Description: "Request public visibility"},
				{Name: "unlisted", Description: "Request unlisted visibility"},
				{Name: "private", Description: "Request private visibility"},
			},
		},
		{
			Name:        "clear",
			Aliases:     []string{"c"},
			Description: "Clear conversation history",
			Usage:       "/clear",
		},
		{
			Name:        "tree",
			Description: "Browse conversation paths or branch from an earlier message",
			Usage:       "/tree",
		},
		{
			Name:         "thread",
			Description:  "Start a related conversation with optional context",
			Usage:        "/thread [message]",
			ArgumentHint: "[message]",
		},
		{
			Name:         "fork",
			Description:  "Create a parallel continuation from the last safe point",
			Usage:        "/fork [message]",
			ArgumentHint: "[message]",
		},
		{
			Name:        "undo",
			Description: "Remove the latest user turn and everything after it",
			Usage:       "/undo",
		},
		{
			Name:        "redo",
			Description: "Restore the turn removed by /undo",
			Usage:       "/redo",
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
			Name:        "effort",
			Description: "Switch reasoning effort for current model (Ctrl+R cycles)",
			Usage:       "/effort [none|minimal|low|medium|high|xhigh|max|default]",
		},
		{
			Name:        "pro",
			Description: "Toggle GPT-5.6 Pro reasoning mode (OpenAI API only)",
			Usage:       "/pro [on|off|status]",
		},
		{
			Name:        "search",
			Aliases:     []string{"web", "s"},
			Description: "Toggle web search on/off",
			Usage:       "/search",
		},
		{
			Name:        "fast",
			Description: "Toggle fast mode (OpenAI/ChatGPT priority, Cursor -fast)",
			Usage:       "/fast",
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
			Name:        "title",
			Description: "Set the session title (or /autotitle to regenerate)",
			Usage:       "/title <name>",
		},
		{
			Name:        "autotitle",
			Description: "Regenerate the session title with the fast model",
			Usage:       "/autotitle",
		},
		{
			Name:        "export",
			Description: "Export conversation as markdown",
			Usage:       "/export [path]",
		},
		{
			Name:        "thinking",
			Aliases:     []string{"reasoning"},
			Description: "Toggle reasoning summary display for this session",
			Usage:       "/thinking [off|status|collapsed|expanded|raw]",
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
			Name:        "shell",
			Aliases:     []string{"sh"},
			Description: "Open your shell or run a command in the session directory",
			Usage:       "/shell [--no-rc] [command ...]",
		},
		{
			Name:        "dirs",
			Description: "Manage approved directories",
			Usage:       "/dirs [add|remove <path>]",
		},
		{
			Name:        "worktree",
			Aliases:     []string{"wt"},
			Description: "Manage git worktrees for this chat session",
			Usage:       "/worktree [new|browse|switch|root|pwd|diff|promote|rm]",
			Subcommands: []Subcommand{
				{Name: "new", Description: "Create and bind a new worktree"},
				{Name: "browse", Description: "Browse managed worktrees"},
				{Name: "switch", Description: "Bind this session to a worktree"},
				{Name: "root", Description: "Return to the root checkout"},
				{Name: "pwd", Description: "Show current bound directory"},
				{Name: "diff", Description: "Show worktree diff including untracked files"},
				{Name: "promote", Description: "Promote the current worktree into root"},
				{Name: "rm", Description: "Remove a worktree"},
			},
		},
		{
			Name:        "mcp",
			Description: "MCP servers (browser, database, git tools)",
			Usage:       "/mcp [start|stop|login|logout|add|list|status [tools]]",
			Subcommands: []Subcommand{
				{Name: "start", Description: "Start a configured server"},
				{Name: "stop", Description: "Stop a running server"},
				{Name: "login", Description: "Sign in to a remote server"},
				{Name: "logout", Description: "Sign out of a remote server"},
				{Name: "add", Description: "Add a new server"},
				{Name: "list", Description: "Show available servers"},
				{Name: "status", Description: "Show server status"},
			},
		},
		{
			Name:        "skills",
			Aliases:     []string{"sk"},
			Description: "List, inspect, invoke, and cancel skills",
			Usage:       "/skills [list|show|run|active|cancel]",
			Subcommands: []Subcommand{
				{Name: "list", Description: "List user-invocable skills"},
				{Name: "show", Description: "Show skill metadata and resources"},
				{Name: "run", Description: "Invoke a skill by exact name"},
				{Name: "active", Description: "List active isolated skill runs"},
				{Name: "cancel", Description: "Cancel an isolated skill run"},
			},
		},
		{
			Name:        "inspect",
			Aliases:     []string{"debug", "i"},
			Description: "View full conversation with tool details",
			Usage:       "/inspect",
		},
		{
			Name:         "commit",
			Description:  "Review, stage, draft, and create a Git commit",
			Usage:        "/commit [commit intent]",
			ArgumentHint: "[commit intent]",
		},
		{
			Name:        "compact",
			Description: "Compact context (soft brief by default; hard = full summary)",
			Usage:       "/compact [hard]",
			Subcommands: []Subcommand{
				{Name: "soft", Description: "Write a compact continuation brief (default)"},
				{Name: "hard", Description: "Create a full summary of conversation history"},
			},
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

// FilterCommands returns built-in commands matching the query using fuzzy search.
func FilterCommands(query string) []Command {
	return filterCommandEntries(AllCommands(), query)
}

// filterCommandEntries filters a supplied static/dynamic slash catalog. If query
// contains a space (for example, "mcp "), it returns subcommands for the parent.
func filterCommandEntries(commands []Command, query string) []Command {
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

	// Exact command names are definitive. Aliases are not: a short alias may
	// also be a useful prefix for another command (for example, /sh should show
	// both /shell and /share).
	queryLower := strings.ToLower(query)
	if len(query) > 1 {
		for _, cmd := range commands {
			if cmd.Name == queryLower {
				return []Command{cmd}
			}
		}
	}

	var result []Command
	seen := make(map[string]bool)
	appendCommand := func(cmd Command) {
		if !seen[cmd.Name] {
			seen[cmd.Name] = true
			result = append(result, cmd)
		}
	}

	// Put direct name-prefix matches first, followed by exact aliases and fuzzy
	// matches. This keeps likely completions visible without an alias hiding a
	// longer command.
	for _, cmd := range commands {
		if strings.HasPrefix(cmd.Name, queryLower) {
			appendCommand(cmd)
		}
	}
	if len(query) > 1 {
		for _, cmd := range commands {
			if slices.Contains(cmd.Aliases, queryLower) {
				appendCommand(cmd)
			}
		}
	}

	source := CommandSource(commands)
	for _, match := range fuzzy.FindFrom(query, source) {
		appendCommand(commands[match.Index])
	}

	return result
}

// isSlashCommandLike reports whether input begins with a known slash command
// (including aliases and unambiguous/ambiguous command prefixes). Chat prompts
// often start with absolute paths like /tmp/foo; those should be submitted to
// the model rather than treated as unknown commands.
func isSlashCommandLike(input string) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	cmdName := strings.ToLower(strings.TrimPrefix(parts[0], "/"))

	if cmdName == "" {
		return true
	}
	for _, c := range AllCommands() {
		if c.Name == cmdName || strings.HasPrefix(c.Name, cmdName) {
			return true
		}
		for _, alias := range c.Aliases {
			if alias == cmdName {
				return true
			}
		}
	}
	return false
}

func isStreamingLocalSlashCommand(input string) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {
		return false
	}
	name := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if name == "" {
		return false
	}
	// During a live turn only handle UI-local commands here. Commands that would
	// replace the active provider/engine must either be blocked or explicitly
	// defer their side effects until the current stream has ended.
	localCommands := map[string]bool{
		"copy":      true,
		"side":      true,
		"thinking":  true,
		"reasoning": true,
		"help":      true,
		"h":         true,
		"?":         true,
		"stats":     true,
		"st":        true,
		"usage":     true,
		"effort":    true,
		"fast":      true,
		"pro":       true,
		"title":     true,
		"autotitle": true,
		"tree":      true,
		"thread":    true,
		"fork":      true,
	}
	return localCommands[name]
}

// rawCommandArgs returns everything after the command token while preserving
// interior whitespace. This is used by commands like /title where spaces are
// part of the user-provided value rather than mere argument separators.
func rawCommandArgs(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	idx := strings.IndexFunc(trimmed, unicode.IsSpace)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(trimmed[idx:])
}

// ExecuteCommand handles slash command execution
func (m *Model) ExecuteCommand(input string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return m, nil
	}

	cmdName := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if m.steeringHandoff != "" && (cmdName == "stop" || cmdName == "cancel") && m.mainRunManager != nil {
		m.mainRunManager.Cancel(m.SessionID())
		m.setTextareaValue("")
		return m.showFooterMuted("Stopping steered handoff…")
	}
	args := parts[1:]
	rawArgs := rawCommandArgs(input)

	// Resolution order is fixed built-in canonical, built-in alias, exact
	// user-invocable skill, then the existing built-in unique-prefix behavior.
	var cmd *Command
	for _, candidate := range AllCommands() {
		if candidate.Name == cmdName {
			copy := candidate
			cmd = &copy
			break
		}
	}
	if cmd == nil {
		for _, candidate := range AllCommands() {
			for _, alias := range candidate.Aliases {
				if alias == cmdName {
					copy := candidate
					cmd = &copy
					break
				}
			}
			if cmd != nil {
				break
			}
		}
	}
	if cmd == nil {
		if _, _, ok := m.userInvocableSkill(cmdName); ok {
			return m.executeSkill(cmdName, rawArgs, strings.TrimSpace(input))
		}
	}

	// If no exact match, try built-in prefix matching.
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
	case "side":
		return m.cmdSide(rawArgs)
	case "copy":
		return m.cmdCopy(args)
	case "stats":
		return m.cmdStats()
	case "usage":
		return m.cmdProviderUsage(args)
	case "goal":
		return m.cmdGoal(args, rawArgs)
	case "clear":
		return m.cmdClear()
	case "tree":
		return m.cmdTree(args)
	case "thread":
		return m.cmdThread(rawArgs)
	case "fork":
		return m.cmdFork(rawArgs)
	case "undo":
		return m.cmdUndoRedo(false, args)
	case "redo":
		return m.cmdUndoRedo(true, args)
	case "quit":
		return m.cmdQuit()
	case "model":
		return m.cmdModel(args)
	case "effort":
		return m.cmdEffort(args)
	case "pro":
		return m.cmdPro(args)
	case "search":
		return m.cmdSearch()
	case "fast":
		return m.cmdFast()
	case "new":
		return m.cmdNew()
	case "save":
		return m.cmdSave(args)
	case "title":
		return m.cmdTitleRaw(rawArgs)
	case "autotitle":
		return m.cmdAutotitle()
	case "export":
		return m.cmdExport(args)
	case "share":
		return m.cmdShare(args)
	case "thinking":
		return m.cmdThinking(args)
	case "system":
		return m.cmdSystem(args)
	case "file":
		return m.cmdFile(args)
	case "shell":
		return m.cmdShell(rawArgs)
	case "dirs":
		return m.cmdDirs(args)
	case "worktree":
		return m.cmdWorktree(args)
	case "mcp":
		return m.cmdMcp(args)
	case "skills":
		return m.cmdSkills(args, rawArgs)
	case "inspect":
		return m.cmdInspect()
	case "commit":
		return m.cmdCommit(rawArgs)
	case "compact":
		return m.cmdCompress(args...)
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

const (
	transientFooterMessageDuration = 3 * time.Second
	mcpFailureFooterDuration       = 15 * time.Second
)

var footerMessageSanitizer = strings.NewReplacer(
	"**", "",
	"__", "",
	"`", "",
)

func sanitizeFooterMessage(content string) string {
	content = terminaltext.SanitizeSingleLine(content)
	return strings.TrimSpace(footerMessageSanitizer.Replace(content))
}

func (m *Model) clearFooterMessage() {
	m.footerMessage = ""
	m.footerMessageTone = ""
}

func (m *Model) showSystemMessage(content string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")

	// If a tool-initiated handover is pending but we're showing an error,
	// signal the tool so it doesn't hang forever.
	m.cancelHandoverTool()

	trimmed := strings.TrimSpace(content)
	if trimmed != "" && !strings.Contains(trimmed, "\n") {
		return m.showFooterMessage(trimmed)
	}

	m.clearFooterMessage()

	// Fall back to scrollback for rich / multiline output.
	rendered := m.renderMarkdown(content)
	return m, tea.Println(rendered + "\n")
}

func (m *Model) showFooterMessage(content string) (tea.Model, tea.Cmd) {
	return m.showFooterMessageWithTone(content, "")
}

func (m *Model) showFooterMuted(content string) (tea.Model, tea.Cmd) {
	return m.showFooterMessageWithTone(content, "muted")
}

func (m *Model) showFooterSuccess(content string) (tea.Model, tea.Cmd) {
	return m.showFooterMessageWithTone(content, "success")
}

func (m *Model) showFooterWarning(content string) (tea.Model, tea.Cmd) {
	return m.showFooterMessageWithTone(content, "warning")
}

// SetFooterWarning sets a warning notice in the chat footer. It is intended for
// startup/configuration notices emitted before the Bubble Tea program is running.
func (m *Model) SetFooterWarning(content string) {
	m.footerMessage = sanitizeFooterMessage(content)
	if m.footerMessage == "" {
		m.footerMessageTone = ""
		return
	}
	m.footerMessageTone = "warning"
	m.footerMessageSeq++
}

func (m *Model) showFooterError(content string) (tea.Model, tea.Cmd) {
	return m.showFooterMessageWithTone(content, "error")
}

func (m *Model) showFooterMessageWithTone(content string, tone string) (tea.Model, tea.Cmd) {
	return m.showFooterMessageWithToneFor(content, tone, transientFooterMessageDuration)
}

func (m *Model) showFooterMessageWithToneFor(content string, tone string, duration time.Duration) (tea.Model, tea.Cmd) {
	m.footerMessage = sanitizeFooterMessage(content)
	m.footerMessageTone = tone
	if m.footerMessage == "" {
		m.footerMessageTone = ""
		return m, nil
	}
	m.footerMessageSeq++
	seq := m.footerMessageSeq
	return m, m.presentationTick(duration, func(time.Time) tea.Msg {
		return footerMessageClearMsg{Seq: seq}
	})
}

func (m *Model) showFooterPersistent(content string, tone string) (tea.Model, tea.Cmd) {
	m.footerMessage = sanitizeFooterMessage(content)
	m.footerMessageTone = tone
	m.footerMessageSeq++ // Invalidate any older transient clear timer.
	if m.footerMessage == "" {
		m.footerMessageTone = ""
	}
	return m, nil
}

func (m *Model) showFooterMutedWithCmd(content string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	_, footerCmd := m.showFooterMuted(content)
	// Batch commands run concurrently, so their order is not observable by the
	// application. Put the operation first so synchronous command consumers do
	// not wait for the transient footer's expiration before reaching it.
	return m, tea.Batch(cmd, footerCmd)
}

func (m *Model) showSystemMessageWithCmd(content string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	_, systemCmd := m.showSystemMessage(content)
	return m, tea.Batch(cmd, systemCmd)
}

// cancelHandoverTool signals false on the tool-initiated handover channel
// and clears it. This is a no-op if no tool handover is pending.
