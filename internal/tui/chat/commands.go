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
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/sahilm/fuzzy"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
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
			Name:        "stats",
			Aliases:     []string{"st"},
			Description: "Show current chat usage, cost, and context breakdown",
			Usage:       "/stats",
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
			Name:        "share",
			Description: "Share this session as a GitHub Gist",
			Usage:       "/share [new] [public]",
			Subcommands: []Subcommand{
				{Name: "new", Description: "Create a new gist"},
				{Name: "public", Description: "Make a new gist public"},
			},
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
			Name:        "effort",
			Description: "Switch reasoning effort for current model (Ctrl+R cycles)",
			Usage:       "/effort [none|minimal|low|medium|high|xhigh|max|ultra|default]",
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
			Description: "Toggle ChatGPT fast mode",
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
			Description: "Open your shell in this session's working directory",
			Usage:       "/shell [--no-rc]",
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
			Usage:       "/worktree [new|browse|switch|root|pwd|diff|merge|promote|rm]",
			Subcommands: []Subcommand{
				{Name: "new", Description: "Create and bind a new worktree"},
				{Name: "browse", Description: "Browse managed worktrees"},
				{Name: "switch", Description: "Bind this session to a worktree"},
				{Name: "root", Description: "Return to the root checkout"},
				{Name: "pwd", Description: "Show current bound directory"},
				{Name: "diff", Description: "Show worktree diff including untracked files"},
				{Name: "merge", Description: "Merge into root, remove the worktree, and rebind the session"},
				{Name: "promote", Description: "Check out a new worktree branch in the root checkout"},
				{Name: "rm", Description: "Remove a worktree"},
			},
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
		"thinking":  true,
		"reasoning": true,
		"help":      true,
		"h":         true,
		"?":         true,
		"stats":     true,
		"st":        true,
		"effort":    true,
		"pro":       true,
		"title":     true,
		"autotitle": true,
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
	args := parts[1:]
	rawArgs := rawCommandArgs(input)

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
	case "stats":
		return m.cmdStats()
	case "goal":
		return m.cmdGoal(args, rawArgs)
	case "clear":
		return m.cmdClear()
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
		return m.cmdShell(args)
	case "dirs":
		return m.cmdDirs(args)
	case "worktree":
		return m.cmdWorktree(args)
	case "mcp":
		return m.cmdMcp(args)
	case "skills":
		return m.cmdSkills()
	case "inspect":
		return m.cmdInspect()
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

const transientFooterMessageDuration = 3 * time.Second

var footerMessageSanitizer = strings.NewReplacer(
	"**", "",
	"__", "",
	"`", "",
)

func sanitizeFooterMessage(content string) string {
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
	return m, tea.Tick(duration, func(time.Time) tea.Msg {
		return footerMessageClearMsg{Seq: seq}
	})
}

func (m *Model) showFooterMutedWithCmd(content string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	_, footerCmd := m.showFooterMuted(content)
	return m, tea.Batch(footerCmd, cmd)
}

func (m *Model) showSystemMessageWithCmd(content string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	_, systemCmd := m.showSystemMessage(content)
	return m, tea.Batch(systemCmd, cmd)
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
	m.setTextareaValue("")
	return m.showHelpModal()
}

func (m *Model) showHelpShortcut() (tea.Model, tea.Cmd) {
	draft := m.textarea.Value()
	result, cmd := m.showHelpModal()
	if rm, ok := result.(*Model); ok {
		rm.setTextareaValue(draft)
		return rm, cmd
	}
	return result, cmd
}

func (m *Model) cmdGoal(args []string, rawArgs string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	if m.sess == nil {
		return m.showFooterError("No active session for /goal.")
	}
	if len(args) == 0 {
		return m.cmdGoalStatus()
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	switch action {
	case "status":
		return m.cmdGoalStatus()
	case "pause":
		return m.updateGoalStatus(session.GoalStatusPaused, "goal paused")
	case "resume":
		return m.updateGoalStatus(session.GoalStatusActive, "goal resumed")
	case "clear":
		if err := session.UpdateGoal(context.Background(), m.store, m.sess.ID, nil); err != nil {
			return m.showFooterError(fmt.Sprintf("Goal clear failed: %v", err))
		}
		m.sess.Goal = nil
		return m.showFooterSuccess("Goal cleared.")
	case "set":
		objective, budget, err := parseGoalObjectiveAndBudget(strings.TrimSpace(strings.TrimPrefix(rawArgs, args[0])))
		if err != nil {
			return m.showFooterError(err.Error())
		}
		if objective == "" {
			return m.showFooterError("Usage: /goal set <objective> [--budget N]")
		}
		return m.setGoal(objective, budget, false)
	case "edit":
		objective, budget, err := parseGoalObjectiveAndBudget(strings.TrimSpace(strings.TrimPrefix(rawArgs, args[0])))
		if err != nil {
			return m.showFooterError(err.Error())
		}
		if objective == "" {
			return m.showFooterError("Usage: /goal edit <objective> [--budget N]")
		}
		return m.setGoal(objective, budget, true)
	default:
		objective, budget, err := parseGoalObjectiveAndBudget(rawArgs)
		if err != nil {
			return m.showFooterError(err.Error())
		}
		if objective == "" {
			return m.showFooterError("Usage: /goal <objective> [--budget N]")
		}
		return m.setGoal(objective, budget, false)
	}
}

func (m *Model) cmdGoalStatus() (tea.Model, tea.Cmd) {
	if m.store != nil && m.sess != nil {
		if refreshed, err := m.store.Get(context.Background(), m.sess.ID); err == nil && refreshed != nil {
			m.sess = refreshed
		}
	}
	if m.sess == nil || m.sess.Goal == nil || !m.sess.Goal.Exists() {
		return m.showFooterMuted("No goal set.")
	}
	return m.showSystemMessage(formatGoalStatus(m.sess.Goal))
}

func (m *Model) setGoal(objective string, tokenBudget int, edited bool) (tea.Model, tea.Cmd) {
	if m.sess == nil {
		return m.showFooterError("No active session for /goal.")
	}
	now := time.Now()
	goal := m.sess.Goal.Clone()
	if goal == nil || !edited {
		goal = session.NewGoal(objective, tokenBudget, now)
	} else {
		goal.Objective = strings.TrimSpace(objective)
		if tokenBudget >= 0 {
			goal.TokenBudget = tokenBudget
		}
		goal.Status = session.GoalStatusActive
		goal.UpdatedAt = now
		goal.UpdatedNotice = true
		goal.PausedAt = time.Time{}
		goal.BlockedAt = time.Time{}
		goal.CompletedAt = time.Time{}
	}
	if goal.TokenBudget < 0 {
		goal.TokenBudget = 0
	}
	goal.Normalize(now)
	if err := session.UpdateGoal(context.Background(), m.store, m.sess.ID, goal); err != nil {
		return m.showFooterError(fmt.Sprintf("Goal save failed: %v", err))
	}
	m.sess.Goal = goal
	if edited {
		return m.showFooterSuccess("Goal updated.")
	}
	return m.showFooterSuccess("Goal set.")
}

func (m *Model) updateGoalStatus(status session.GoalStatus, message string) (tea.Model, tea.Cmd) {
	if m.sess == nil || m.sess.Goal == nil || !m.sess.Goal.Exists() {
		return m.showFooterMuted("No goal set.")
	}
	goal := m.sess.Goal.Clone()
	oldStatus := goal.Status
	if status == session.GoalStatusActive && oldStatus == session.GoalStatusBudgetLimited && goal.BudgetExhausted() {
		return m.showFooterError("Goal budget is exhausted; edit the goal with a higher --budget before resuming.")
	}
	goal.Status = status
	goal.UpdatedAt = time.Now()
	switch status {
	case session.GoalStatusPaused:
		goal.PausedAt = goal.UpdatedAt
	case session.GoalStatusActive:
		goal.PausedAt = time.Time{}
		goal.CompletedAt = time.Time{}
		goal.BlockedAt = time.Time{}
	}
	if err := session.UpdateGoal(context.Background(), m.store, m.sess.ID, goal); err != nil {
		return m.showFooterError(fmt.Sprintf("Goal update failed: %v", err))
	}
	m.sess.Goal = goal
	return m.showFooterSuccess(message + ".")
}

func parseGoalObjectiveAndBudget(raw string) (string, int, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "", 0, nil
	}
	budget := -1
	kept := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "--tokens" || field == "--token-budget" || field == "--budget" {
			if i+1 >= len(fields) {
				return "", 0, fmt.Errorf("%s requires a number", field)
			}
			value, err := strconv.Atoi(fields[i+1])
			if err != nil || value <= 0 {
				return "", 0, fmt.Errorf("%s requires a positive number", field)
			}
			budget = value
			i++
			continue
		}
		if strings.HasPrefix(field, "--tokens=") || strings.HasPrefix(field, "--budget=") || strings.HasPrefix(field, "--token-budget=") {
			flagName, valueText, _ := strings.Cut(field, "=")
			value, err := strconv.Atoi(valueText)
			if err != nil || value <= 0 {
				return "", 0, fmt.Errorf("%s requires a positive number", flagName)
			}
			budget = value
			continue
		}
		kept = append(kept, field)
	}
	return strings.TrimSpace(strings.Join(kept, " ")), budget, nil
}

func formatGoalStatus(goal *session.Goal) string {
	if goal == nil || !goal.Exists() {
		return "No goal set."
	}
	budget := "unlimited"
	if goal.TokenBudget > 0 {
		budget = fmt.Sprintf("%d/%d tokens", goal.TokensUsed, goal.TokenBudget)
	}
	return fmt.Sprintf("Goal: %s\nStatus: %s\nBudget: %s", goal.Objective, goal.Status, budget)
}

func (m *Model) pauseGoalForLocalAction(reason string) {
	if m == nil || m.sess == nil || m.sess.Goal == nil || !m.sess.Goal.IsActive() {
		return
	}
	goal := m.sess.Goal.Clone()
	goal.Status = session.GoalStatusPaused
	goal.PausedAt = time.Now()
	goal.UpdatedAt = goal.PausedAt
	goal.LastReason = strings.TrimSpace(reason)
	_ = session.UpdateGoal(context.Background(), m.store, m.sess.ID, goal)
	m.sess.Goal = goal
}

func (m *Model) showHelpModal() (tea.Model, tea.Cmd) {
	var b strings.Builder
	b.WriteString("Slash commands\n")
	for _, cmd := range AllCommands() {
		usage := cmd.Usage
		if len(cmd.Aliases) > 0 {
			usage += " (" + strings.Join(cmd.Aliases, ", ") + ")"
		}
		b.WriteString(fmt.Sprintf("  %-28s %s\n", usage, cmd.Description))
	}

	b.WriteString("\nKeys\n")
	keyGroups := []struct {
		title string
		rows  [][2]string
	}{
		{
			title: "Global",
			rows: [][2]string{
				{"Ctrl+/ or Ctrl+H", "Show help"},
				{"Ctrl+C", "Copy selection; cancel active response/tool; press twice when idle to quit"},
				{"Esc", "Cancel streaming / close modal / clear selection or input"},
				{"Ctrl+P", "Command palette"},
				{"Ctrl+K", "Clear conversation"},
				{"Ctrl+N", "New session"},
				{"Ctrl+L", "Switch model"},
				{"Ctrl+R", "Cycle reasoning effort"},
				{"Ctrl+S", "Toggle web search"},
				{"Shift+Tab", "Cycle approval mode"},
				{"Ctrl+T", "MCP servers (tools)"},
				{"Ctrl+O", "Inspect conversation context"},
				{"Ctrl+E", "Expand/collapse tool and reasoning details"},
			},
		},
		{
			title: "Composer",
			rows: [][2]string{
				{"Enter", "Send message; while streaming, queue interjection"},
				{"Ctrl+J / Alt+Enter / Shift+Enter", "Insert newline"},
				{"\\ + Enter", "Turn trailing backslash into a newline"},
				{"/", "Open slash-command completions from an empty composer"},
				{"Tab", "Complete command args / MCP server names where supported"},
				{"Ctrl+U", "Clear current line (textarea)"},
				{"Ctrl+W", "Delete previous word (textarea)"},
				{"Ctrl+V", "Paste; attaches clipboard image when available"},
				{"Ctrl+E", "Expand collapsed paste placeholder at cursor"},
			},
		},
		{
			title: "Navigation and selection",
			rows: [][2]string{
				{"PageUp / PageDown", "Scroll conversation"},
				{"Up / Down", "Scroll when composer is empty; select queued interjections while streaming"},
				{"Ctrl+Y", "Copy selected conversation text"},
			},
		},
		{
			title: "Pickers and completions",
			rows: [][2]string{
				{"Up/Down or Ctrl+P/Ctrl+N", "Move selection"},
				{"Enter", "Execute command / choose model / toggle MCP server"},
				{"Tab", "Fill selected completion or choose selected model"},
				{"Backspace", "Edit filter"},
				{"Esc or Ctrl+C", "Close picker"},
			},
		},
	}
	for _, group := range keyGroups {
		b.WriteString("\n" + group.title + "\n")
		for _, row := range group.rows {
			b.WriteString(fmt.Sprintf("  %-32s %s\n", row[0], row[1]))
		}
	}

	m.dialog.ShowContent("Help", b.String())
	return m, nil
}

func (m *Model) cmdClear() (tea.Model, tea.Cmd) {
	m.clearPendingStreamModelSwitch()
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
	m.compactionIdx = 0
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

	// Reset image renderer caches for this terminal session.
	ui.ClearRenderedImages()
	m.resetImageUploadState()

	// Invalidate view cache so stale content doesn't bleed through
	m.viewCache.historyValid = false
	m.viewCache.completedStream = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
	m.resetTitleGenerationStateForSession()

	updated, footerCmd := m.showFooterSuccess("Started a new session.")
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdQuit() (tea.Model, tea.Cmd) {
	hadActiveStream := m.streaming || m.streamCancelFunc != nil
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
	if !hadActiveStream {
		if summary := m.exitStatsSummary(); summary != "" {
			return m, m.quitCmd(tea.Println(summary))
		}
	}
	return m, m.quitCmd()
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
	return m, m.quitCmd()
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
	m.pauseGoalForLocalAction("paused for model switch")
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

func (m *Model) currentProviderAndModel() (provider, model string) {
	provider = strings.TrimSpace(m.providerKey)
	if provider == "" && m.sess != nil {
		provider = strings.TrimSpace(m.sess.ProviderKey)
	}
	if provider == "" {
		provider = strings.TrimSpace(m.providerName)
	}

	model = strings.TrimSpace(m.modelName)
	if model == "" && m.sess != nil {
		model = strings.TrimSpace(m.sess.Model)
	}
	return provider, model
}

func (m *Model) currentProviderAndModelForEffortCycle() (provider, model string) {
	provider, model = m.currentProviderAndModel()
	if !m.streaming || m.pendingStreamModelSwitch == nil {
		return provider, model
	}
	if pendingProvider := strings.TrimSpace(m.pendingStreamModelSwitch.provider); pendingProvider != "" {
		provider = pendingProvider
	}
	if pendingModel := strings.TrimSpace(m.pendingStreamModelSwitch.model); pendingModel != "" {
		model = pendingModel
	}
	return provider, model
}

func (m *Model) baseModelAndEffort(provider, model string) (string, string) {
	if base, effort, _, ok := m.configuredModelEffort(provider, model); ok {
		return base, effort
	}
	return llm.BaseModelAndEffortForProvider(provider, model)
}

func (m *Model) reasoningEffortsForModel(provider, model string) []string {
	if _, _, efforts, ok := m.configuredModelEffort(provider, model); ok {
		return efforts
	}
	base, _ := m.baseModelAndEffort(provider, model)
	for _, metadata := range m.modelMetadata {
		if strings.EqualFold(strings.TrimSpace(metadata.ID), strings.TrimSpace(base)) && len(metadata.ReasoningEfforts) > 0 {
			return normalizedModelReasoningEfforts(metadata.ReasoningEfforts)
		}
	}
	return llm.ReasoningEffortsForProviderModel(provider, model)
}

func (m *Model) configuredModelEffort(provider, model string) (base, effort string, efforts []string, ok bool) {
	if m == nil || m.config == nil {
		return "", "", nil, false
	}
	pc, exists := m.config.Providers[strings.TrimSpace(provider)]
	if !exists || len(pc.ModelConfigs) == 0 {
		return "", "", nil, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", nil, false
	}
	modelLower := strings.ToLower(model)
	for _, entry := range pc.ModelConfigs {
		efforts = normalizedModelReasoningEfforts(entry.ReasoningEfforts)
		if len(efforts) == 0 {
			continue
		}
		for _, name := range configuredModelNamesForEffort(entry) {
			nameLower := strings.ToLower(name)
			if modelLower == nameLower {
				return name, "", efforts, true
			}
			for _, candidateEffort := range efforts {
				suffix := "-" + strings.ToLower(candidateEffort)
				if strings.HasSuffix(modelLower, suffix) && strings.TrimSuffix(modelLower, suffix) == nameLower {
					return name, candidateEffort, efforts, true
				}
			}
		}
	}
	return "", "", nil, false
}

func configuredModelNamesForEffort(entry config.ProviderModelConfig) []string {
	seen := make(map[string]bool, 2)
	var names []string
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	appendName(entry.DisplayName())
	appendName(entry.ID)
	return names
}

func normalizedModelReasoningEfforts(efforts []string) []string {
	seen := make(map[string]bool, len(efforts))
	out := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

func (m *Model) pendingStreamEffortStatus() (label string, applied bool, ok bool) {
	if m == nil || m.pendingStreamModelSwitch == nil {
		return "", false, false
	}
	provider := strings.TrimSpace(m.pendingStreamModelSwitch.provider)
	model := strings.TrimSpace(m.pendingStreamModelSwitch.model)
	if provider == "" || model == "" {
		return "", false, false
	}
	_, effort := m.baseModelAndEffort(provider, model)
	if effort == "" {
		effort = "default"
	}
	return effort, m.pendingStreamModelSwitch.applied, true
}

func (m *Model) markPendingStreamModelSwitchApplied(model string) bool {
	if m == nil || m.pendingStreamModelSwitch == nil {
		return false
	}
	if strings.TrimSpace(m.pendingStreamModelSwitch.model) != strings.TrimSpace(model) {
		return false
	}
	m.pendingStreamModelSwitch.applied = true
	return true
}

func (m *Model) clearPendingStreamModelSwitch() {
	m.pendingStreamModelSwitch = nil
	if m.engine != nil {
		m.engine.ClearPendingRequestModelSwitch()
	}
}

func (m *Model) queuePendingStreamModelSwitch(provider, model string) {
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: provider, model: model}
	currentProvider, _ := m.currentProviderAndModel()
	if m.engine != nil && provider == currentProvider {
		m.engine.QueueRequestModelSwitch(model)
	}
}

func (m *Model) cmdPro(args []string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	provider, model := m.currentProviderAndModel()
	supported := llm.SupportsReasoningMode(provider, model)
	current := "off"
	if m.sess != nil && strings.EqualFold(m.sess.ReasoningMode, "pro") {
		current = "on"
	}
	requested := "status"
	if len(args) > 0 {
		requested = strings.ToLower(strings.TrimSpace(args[0]))
	}
	if requested == "status" {
		if !supported && current == "on" {
			m.sess.ReasoningMode = ""
			if m.store != nil {
				_ = m.store.Update(context.Background(), m.sess)
			}
			return m.showFooterWarning("Pro mode was disabled because the current provider/model does not support it.")
		}
		return m.showFooterMuted(fmt.Sprintf("Pro mode: %s (OpenAI API GPT-5.6 only)", current))
	}
	if requested != "on" && requested != "off" {
		return m.showFooterWarning("Usage: /pro [on|off|status]")
	}
	if requested == "on" && !supported {
		return m.showFooterWarning(fmt.Sprintf("Pro mode is not supported by %s:%s; use ChatGPT /effort ultra where available.", provider, model))
	}
	if m.sess == nil {
		return m.showFooterWarning("Cannot change Pro mode: no active session.")
	}
	if requested == "on" {
		m.sess.ReasoningMode = "pro"
	} else {
		m.sess.ReasoningMode = ""
	}
	if m.store != nil {
		_ = m.store.Update(context.Background(), m.sess)
	}
	return m.showFooterMuted("Pro mode: " + requested)
}

func (m *Model) cmdEffort(args []string) (tea.Model, tea.Cmd) {
	provider, model := m.currentProviderAndModel()

	m.setTextareaValue("")
	if provider == "" || model == "" {
		return m.showFooterWarning("Cannot switch effort: no current provider/model is active.")
	}
	if m.streaming && len(args) == 0 {
		if queued, applied, ok := m.pendingStreamEffortStatus(); ok {
			_, currentEffort := m.baseModelAndEffort(provider, model)
			if applied {
				currentEffort = queued
			}
			if currentEffort == "" {
				currentEffort = "default"
			}
			efforts := m.reasoningEffortsForModel(provider, model)
			available := append(cloneStrings(efforts), "default")
			queuedText := fmt.Sprintf("Queued: %s (applies at next model turn)", queued)
			if applied {
				queuedText = fmt.Sprintf("Active for current run: %s (persists after response)", queued)
			}
			return m.showFooterMuted(fmt.Sprintf("Current effort: %s. %s. Available: %s. Usage: /effort <value>", currentEffort, queuedText, strings.Join(available, ", ")))
		}
	}

	resolved := m.resolveEffortSwitch(provider, model, args)
	if m.streaming && resolved.already {
		if m.pendingStreamModelSwitch != nil {
			m.clearPendingStreamModelSwitch()
			return m.showFooterMuted(fmt.Sprintf("Effort %s already active; cleared queued effort change.", resolved.label))
		}
		return m.showEffortResolutionMessage(resolved)
	}
	if !resolved.ok {
		return m.showEffortResolutionMessage(resolved)
	}

	if m.streaming {
		m.queuePendingStreamModelSwitch(provider, resolved.targetModel)
		return m.showFooterMuted(fmt.Sprintf("Effort %s queued; will apply at the next model turn.", resolved.label))
	}

	return m.switchEffortResolved(resolved, false)
}

func (m *Model) cycleEffort() (tea.Model, tea.Cmd) {
	provider, model := m.currentProviderAndModelForEffortCycle()
	if provider == "" || model == "" {
		return m.showFooterWarning("Cannot switch effort: no current provider/model is active.")
	}

	_, currentEffort := m.baseModelAndEffort(provider, model)
	efforts := m.reasoningEffortsForModel(provider, model)
	if len(efforts) == 0 {
		return m.showFooterWarning(fmt.Sprintf("Model %s:%s does not expose switchable reasoning efforts.", provider, model))
	}

	next := efforts[0]
	if currentEffort != "" {
		if idx := slices.Index(efforts, currentEffort); idx >= 0 {
			if idx == len(efforts)-1 {
				next = "default"
			} else {
				next = efforts[idx+1]
			}
		}
	}

	draft := m.textarea.Value()
	if m.streaming {
		resolved := m.resolveEffortSwitch(provider, model, []string{next})
		if !resolved.ok {
			return m.showEffortResolutionMessage(resolved)
		}
		m.queuePendingStreamModelSwitch(provider, resolved.targetModel)
		_, cmd := m.showFooterMuted(fmt.Sprintf("Effort %s queued; will apply at the next model turn.", resolved.label))
		m.setTextareaValue(draft)
		return m, cmd
	}

	result, cmd := m.switchEffort(provider, model, []string{next}, true)
	if rm, ok := result.(*Model); ok {
		rm.setTextareaValue(draft)
		return rm, cmd
	}
	return result, cmd
}

type effortSwitchResolution struct {
	provider    string
	targetModel string
	label       string
	message     string
	tone        string
	ok          bool
	already     bool
}

func (m *Model) resolveEffortSwitch(provider, model string, args []string) effortSwitchResolution {
	base, currentEffort := m.baseModelAndEffort(provider, model)
	efforts := m.reasoningEffortsForModel(provider, model)
	if len(args) == 0 {
		if len(efforts) == 0 {
			return effortSwitchResolution{message: fmt.Sprintf("Model %s:%s does not expose switchable reasoning efforts.", provider, model), tone: "warning"}
		}
		current := currentEffort
		if current == "" {
			current = "default"
		}
		available := append(cloneStrings(efforts), "default")
		return effortSwitchResolution{message: fmt.Sprintf("Current effort: %s. Available: %s. Usage: /effort <value>", current, strings.Join(available, ", ")), tone: "muted"}
	}

	requested := strings.ToLower(strings.TrimSpace(args[0]))
	if requested == "" {
		return effortSwitchResolution{message: "Usage: /effort <value>", tone: "warning"}
	}

	label := requested
	var targetModel string
	switch requested {
	case "default", "auto", "none", "off":
		targetModel = base
		label = "default"
	default:
		if len(efforts) == 0 || !slices.Contains(efforts, requested) {
			allowed := append(cloneStrings(efforts), "default")
			allowedText := strings.Join(allowed, ", ")
			if allowedText == "" {
				allowedText = "none"
			}
			return effortSwitchResolution{message: fmt.Sprintf("Unsupported effort %q for %s:%s. Available: %s.", requested, provider, base, allowedText), tone: "warning"}
		}
		targetModel = base + "-" + requested
	}

	if targetModel == model {
		return effortSwitchResolution{
			provider:    provider,
			targetModel: targetModel,
			label:       label,
			message:     fmt.Sprintf("Effort already %s for %s:%s.", label, provider, model),
			tone:        "muted",
			already:     true,
		}
	}

	return effortSwitchResolution{
		provider:    provider,
		targetModel: targetModel,
		label:       label,
		ok:          true,
	}
}

func (m *Model) showEffortResolutionMessage(resolved effortSwitchResolution) (tea.Model, tea.Cmd) {
	if resolved.tone == "warning" {
		return m.showFooterWarning(resolved.message)
	}
	return m.showFooterMuted(resolved.message)
}

func (m *Model) switchEffort(provider, model string, args []string, deferMarker bool) (tea.Model, tea.Cmd) {
	resolved := m.resolveEffortSwitch(provider, model, args)
	if !resolved.ok {
		return m.showEffortResolutionMessage(resolved)
	}
	return m.switchEffortResolved(resolved, deferMarker)
}

func (m *Model) switchEffortResolved(resolved effortSwitchResolution, deferMarker bool) (tea.Model, tea.Cmd) {
	if m.config == nil {
		m.config = &config.Config{}
	}
	currentProvider, _ := m.currentProviderAndModel()
	if strings.TrimSpace(currentProvider) == strings.TrimSpace(resolved.provider) {
		return m.switchEffortStateOnly(resolved, deferMarker)
	}
	return m.switchModelWithOptions(resolved.provider+":"+resolved.targetModel, switchModelOptions{deferMarker: deferMarker})
}

func (m *Model) switchEffortStateOnly(resolved effortSwitchResolution, deferMarker bool) (tea.Model, tea.Cmd) {
	oldProvider := strings.TrimSpace(m.providerKey)
	if oldProvider == "" {
		oldProvider = strings.TrimSpace(m.providerName)
	}
	oldModel := strings.TrimSpace(m.modelName)

	m.clearPendingStreamModelSwitch()
	if !deferMarker {
		m.pendingModelSwitch = nil
	}

	m.providerKey = resolved.provider
	m.modelName = resolved.targetModel
	m.refreshEffectiveFastMode()

	if m.sess != nil {
		m.sess.ProviderKey = m.providerKey
		m.sess.Model = resolved.targetModel
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}

	m.recordCurrentModelUse()
	m.setTextareaValue("")
	guardianErr := m.refreshGuardianReviewer(resolved.provider, resolved.targetModel)

	if m.sess != nil && len(m.messages) > 0 {
		marker := llm.ModelSwapMarker{
			FromProvider: oldProvider,
			FromModel:    oldModel,
			ToProvider:   resolved.provider,
			ToModel:      resolved.targetModel,
			Status:       "started",
		}
		if deferMarker {
			m.deferModelSwitchMarker(marker)
		} else {
			m.appendModelSwitchMarker(marker)
		}
	}

	if guardianErr != nil {
		return m.showFooterWarning(fmt.Sprintf("Switched effort to %s, but guardian auto-approval was disabled: %v", resolved.label, guardianErr))
	}
	return m.showFooterMuted(fmt.Sprintf("Switched effort to %s for %s:%s. Next response will use the selected reasoning effort.", resolved.label, resolved.provider, resolved.targetModel))
}

func (m *Model) refreshGuardianReviewer(providerKey, modelName string) error {
	if m.guardianReviewerRefresh == nil {
		return nil
	}
	if err := m.guardianReviewerRefresh(providerKey, modelName); err != nil {
		if m.approvalMgr != nil {
			m.approvalMgr.PolicyReviewFunc = nil
			if m.approvalMgr.ApprovalMode() == tools.ModeAuto {
				m.approvalMgr.SetApprovalMode(tools.ModePrompt)
			}
		}
		return err
	}
	return nil
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
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
	provider, model := m.currentProviderAndModel()
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

func (m *Model) effortCompletionItems(commandPrefix, partial string) []Command {
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
		return nil
	}

	base, _ := m.baseModelAndEffort(provider, model)
	efforts := m.reasoningEffortsForModel(provider, model)
	if len(efforts) == 0 {
		return nil
	}

	partial = strings.ToLower(strings.TrimSpace(partial))
	items := make([]Command, 0, len(efforts)+2)
	appendOption := func(option, target string) {
		if partial != "" && !strings.HasPrefix(option, partial) {
			return
		}
		desc := "switch to " + target
		if target == model {
			desc = "current"
		}
		items = append(items, Command{Name: commandPrefix + option, Description: desc})
	}
	for _, effort := range efforts {
		appendOption(effort, base+"-"+effort)
	}
	appendOption("default", base)
	appendOption("auto", base)
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
	return m.showFooterMuted(fmt.Sprintf("Web search %s.", status))
}

func (m *Model) cmdFast() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	if m.streaming {
		return m.showFooterWarning("Fast mode can be toggled after the current response finishes.")
	}
	if !m.supportsServiceTierToggle() {
		return m.showFooterError(fmt.Sprintf("Fast mode is not supported for %s:%s.", m.providerKey, m.modelName))
	}

	// If fast is currently effective (from provider config or a prior /fast), /fast
	// explicitly clears the provider default for this chat.
	m.refreshEffectiveFastMode()
	if m.fastMode {
		m.fastOverride = serviceTierClear
		m.refreshEffectiveFastMode()
		return m.showFooterMuted("Fast mode disabled.")
	}

	// OpenAI supports service_tier at the request layer, but term-llm does not
	// currently have model metadata for it. Let users opt in and let the API reject
	// unsupported models, just as provider-level config does.
	if m.providerType() == config.ProviderTypeOpenAI {
		m.fastOverride = serviceTierFast
		m.refreshEffectiveFastMode()
		return m.showFooterSuccess("Fast mode enabled.")
	}

	if !m.fastMetadataLoaded {
		m.pendingFastToggle = true
		if m.fastMetadataLoading {
			return m.showFooterMuted("Loading model metadata…")
		}
		if cmd := m.loadChatGPTModelsCmd(); cmd != nil {
			return m.showFooterMutedWithCmd("Loading model metadata…", cmd)
		}
		return m.showFooterError("Could not load model metadata; fast support unknown.")
	}
	if !m.currentModelSupportsFast() {
		return m.showFooterError(fmt.Sprintf("Fast mode is not supported for %s:%s.", m.providerKey, m.modelName))
	}
	m.fastOverride = serviceTierFast
	m.refreshEffectiveFastMode()
	return m.showFooterSuccess("Fast mode enabled.")
}

func (m *Model) cmdNew() (tea.Model, tea.Cmd) {
	m.pauseGoalForLocalAction("paused because a new session was started")
	m.clearPendingStreamModelSwitch()
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
	m.compactionIdx = 0
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

	// Reset image renderer caches for this terminal session.
	ui.ClearRenderedImages()
	m.resetImageUploadState()

	// Invalidate view cache so stale content doesn't bleed through
	m.viewCache.historyValid = false
	m.viewCache.completedStream = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
	m.resetTitleGenerationStateForSession()

	updated, footerCmd := m.showFooterSuccess("Started a new session.")
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdTitleRaw(rawName string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	name := strings.TrimSpace(rawName)
	if name == "" {
		return m.showFooterError("Usage: /title <name>")
	}
	if m.sess == nil {
		return m.showFooterError("No active session to title.")
	}
	if m.store == nil {
		return m.showFooterError("Session storage is disabled. Enable it in config with `sessions.enabled: true`.")
	}

	m.sess.Name = name
	m.sess.TitleSource = session.TitleSourceUser
	m.titleManualEditVersion++
	if err := m.store.Update(context.Background(), m.sess); err != nil {
		return m.showFooterError(fmt.Sprintf("Failed to set session title: %v", err))
	}

	updated, footerCmd := m.showFooterSuccess(fmt.Sprintf("Session title set to '%s'.", name))
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdAutotitle() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	if m.sess == nil {
		return m.showFooterError("No active session to title.")
	}
	sessionID := strings.TrimSpace(m.sess.ID)
	if sessionID == "" {
		return m.showFooterError("No active session to title.")
	}
	if m.titleGenerationSessionID != sessionID {
		m.resetTitleGenerationStateForSession()
	}
	if m.titleGenerationInFlight {
		return m.showFooterError("Title generation is already running.")
	}
	if m.fastProvider == nil {
		return m.showFooterError("Fast title generation is unavailable.")
	}
	if m.store == nil {
		return m.showFooterError("Session storage is disabled. Enable it in config with `sessions.enabled: true`.")
	}

	m.messagesMu.Lock()
	hasMessages := len(m.messages) > 0
	m.messagesMu.Unlock()
	if !hasMessages {
		return m.showFooterError("No conversation messages to title yet.")
	}

	clearManualName := strings.TrimSpace(m.sess.Name) != ""
	cmd := m.generateSessionTitleCmd(true, clearManualName, m.titleManualEditVersion)
	if cmd == nil {
		return m.showFooterError("No conversation messages to title yet.")
	}
	return m, cmd
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
	m.titleManualEditVersion++
	if err := m.store.Update(context.Background(), m.sess); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to save session: %v", err))
	}

	m.setTextareaValue("")
	updated, footerCmd := m.showFooterSuccess(fmt.Sprintf("Saved session as '%s'.", name))
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
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

func (m *Model) cmdThinking(args []string) (tea.Model, tea.Cmd) {
	valid := map[string]bool{
		config.ReasoningDisplayOff:       true,
		config.ReasoningDisplayStatus:    true,
		config.ReasoningDisplayCollapsed: true,
		config.ReasoningDisplayExpanded:  true,
		config.ReasoningDisplayRaw:       true,
	}

	mode := ""
	if len(args) > 0 {
		mode = strings.ToLower(strings.TrimSpace(args[0]))
		if mode == config.ReasoningDisplayAuto {
			mode = config.ReasoningDisplayCollapsed
		}
		if !valid[mode] {
			return m.showFooterWarning("Usage: /thinking [off|status|collapsed|expanded|raw]")
		}
	} else {
		current := m.reasoningModeOverride
		if current == "" {
			current = internalreasoning.EffectiveDisplay(m.effectiveReasoningConfig())
		}
		switch current {
		case config.ReasoningDisplayExpanded:
			mode = config.ReasoningDisplayOff
		case config.ReasoningDisplayOff:
			mode = config.ReasoningDisplayCollapsed
		default:
			mode = config.ReasoningDisplayExpanded
		}
	}

	cfg := m.effectiveReasoningConfig()
	cfg.Display = mode
	if mode == config.ReasoningDisplayRaw && !cfg.Raw {
		return m.showFooterWarning("Raw reasoning is disabled. Set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
	}
	m.reasoningModeOverride = mode
	m.reasoningConfig.Display = mode
	if m.chatRenderer != nil {
		m.chatRenderer.SetReasoningConfig(m.effectiveReasoningConfig())
	}
	m.clearReasoningSegmentExpansionOverrides()
	m.forceHistoryRerender()
	m.applyReasoningPhase()
	m.setTextareaValue("")

	label := mode
	if mode == config.ReasoningDisplayRaw {
		label = "raw visible"
	}
	return m.showFooterSuccess(fmt.Sprintf("Reasoning display: %s", label))
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
	exportReasoningCfg := m.effectiveReasoningConfig()
	includeReasoningSummaries := internalreasoning.ExportSummaries(exportReasoningCfg)
	includeRawReasoning := internalreasoning.ExportRaw(exportReasoningCfg)
	rawReasoningOmitted := strings.EqualFold(strings.TrimSpace(exportReasoningCfg.Export), config.ReasoningExportRaw) && !exportReasoningCfg.Raw
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

		// Content - for user messages, extract just the text (not file contents).
		if msg.Role == llm.RoleUser {
			content := llm.StripEmbeddedFileText(msg.TextContent)
			b.WriteString(content)
			b.WriteString("\n---\n\n")
			continue
		}
		if msg.Role == llm.RoleAssistant {
			for _, part := range msg.Parts {
				if part.Type != llm.PartText {
					continue
				}
				if rendered := session.RenderExportReasoning(part, session.ExportOptions{
					IncludeReasoningSummaries: includeReasoningSummaries,
					IncludeRawReasoning:       includeRawReasoning,
				}); rendered != "" {
					b.WriteString(rendered)
				}
				if part.Text != "" {
					b.WriteString(part.Text)
					b.WriteString("\n\n")
				}
			}
			if len(msg.Parts) == 0 && msg.TextContent != "" {
				b.WriteString(msg.TextContent)
				b.WriteString("\n\n")
			}
		}
		b.WriteString("---\n\n")
	}

	// Write to file
	if err := os.WriteFile(outputPath, []byte(b.String()), 0644); err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to export: %v", err))
	}

	m.setTextareaValue("")
	message := fmt.Sprintf("Exported %d messages to %s.", len(m.messages), outputPath)
	if rawReasoningOmitted {
		return m.showFooterWarning(message + " Raw reasoning omitted; set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
	}
	return m.showFooterSuccess(message)
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
	m.systemPromptOverridden = true
	m.systemPromptOverride = prompt
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
		return m.showFooterSuccess(fmt.Sprintf("Cleared %d attached file(s).", count))
	}

	// Join all args in case path has spaces
	path := strings.Join(args, " ")
	m.setTextareaValue("")

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
		return m.showFooterSuccess(fmt.Sprintf("Approved directory: %s", path))

	case "remove", "rm", "delete":
		if len(subArgs) == 0 {
			return m.showSystemMessage("Usage: `/dirs remove <path>`")
		}
		path := strings.Join(subArgs, " ")
		if err := m.approvedDirs.RemoveDirectory(path); err != nil {
			return m.showSystemMessage(fmt.Sprintf("Failed to remove directory: %v", err))
		}
		m.setTextareaValue("")
		return m.showFooterSuccess(fmt.Sprintf("Removed approved directory: %s", path))

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
	return m.showFooterMuted(fmt.Sprintf("Starting %s… tools will be available shortly.", name))
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
	return m.showFooterSuccess(fmt.Sprintf("Stopped %s.", name))
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
	b.WriteString("- Skills are loaded from: `~/.config/term-llm/skills/`, `.skills/`, `.agents/skills/`, and ecosystem/project directories\n\n")

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

func (m *Model) newInspectorConfig() *inspector.Config {
	var toolSpecs []llm.ToolSpec
	if m.mcpManager != nil {
		for _, t := range m.mcpManager.AllTools() {
			toolSpecs = append(toolSpecs, llm.ToolSpec{
				Name:        t.Name,
				Description: t.Description,
				Schema:      t.Schema,
			})
		}
	}
	if len(m.localTools) > 0 && m.engine != nil {
		for _, specName := range m.localTools {
			if tool, ok := m.engine.Tools().Get(specName); ok {
				toolSpecs = append(toolSpecs, tool.Spec())
			}
		}
	}

	cfg := &inspector.Config{
		ProviderName:            m.providerName,
		ModelName:               m.modelName,
		ToolSpecs:               toolSpecs,
		ReasoningConfig:         m.effectiveReasoningConfig(),
		HasCompactionBoundary:   m.compactionIdx > 0 || session.HasCompactionBoundary(m.sess),
		CompactionBoundaryIndex: -1,
		CompactionBoundarySeq:   -1,
		CompactionCount:         0,
	}
	if m.compactionIdx > 0 {
		cfg.CompactionBoundaryIndex = m.compactionIdx
	}
	if m.sess != nil {
		cfg.CompactionBoundarySeq = m.sess.CompactionSeq
		cfg.CompactionCount = m.sess.CompactionCount
	}
	return cfg
}

func (m *Model) cmdInspect() (tea.Model, tea.Cmd) {
	m.setTextareaValue("")

	if len(m.messages) == 0 {
		return m.showSystemMessage("No messages to inspect. Send a message first.")
	}

	m.inspectorMode = true
	m.inspectorModel = inspector.NewWithConfig(m.messages, m.width, m.height, m.styles, m.store, m.newInspectorConfig())
	return m, nil
}

func (m *Model) cmdCompress(args ...string) (tea.Model, tea.Cmd) {
	m.pauseGoalForLocalAction("paused for context compaction")
	m.setTextareaValue("")

	mode := "soft"
	if len(args) > 1 {
		return m.showSystemMessage("Usage: /compact [hard]")
	}
	if len(args) == 1 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "", "soft":
			mode = "soft"
		case "hard":
			mode = "hard"
		default:
			return m.showSystemMessage("Usage: /compact [hard]")
		}
	}

	if m.streaming {
		return m.showSystemMessage("Cannot compress while streaming. Wait for the response to finish.")
	}

	// Need at least a couple of messages to make compaction worthwhile
	m.messagesMu.Lock()
	snapshot := make([]session.Message, len(m.messages))
	copy(snapshot, m.messages)
	compIdx := m.compactionIdx
	m.messagesMu.Unlock()

	messagesStart := compIdx
	if messagesStart > 0 {
		if messagesStart < len(snapshot) {
			snapshot = snapshot[messagesStart:]
		} else {
			snapshot = nil
		}
	}

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
	if m.engine != nil && m.engine.InputLimit() > 0 {
		compactConfig.InputLimit = m.engine.InputLimit()
	} else if limit := llm.InputLimitForProviderModel(m.providerKey, m.modelName); limit > 0 {
		compactConfig.InputLimit = limit
	}
	model := m.modelName
	provider := m.provider
	phase := llm.PhaseCompacting
	if mode == "hard" {
		phase = llm.PhaseCompactingSummarizeHistory
	}

	m.clearFooterMessage()
	m.streaming = true
	// Drop the retained previous-turn tracker so it is not re-rendered as
	// streaming content while compaction runs.
	m.resetRetainedStreamTracker()
	m.phase = phase
	m.streamStartTime = time.Now()
	if m.altScreen {
		m.scrollToBottom = true
	}

	ctx, cancel := context.WithCancel(m.rootContext())
	m.streamCancelFunc = cancel

	return m, tea.Batch(
		func() tea.Msg {
			var (
				result *llm.CompactionResult
				err    error
			)
			if mode == "hard" {
				result, err = llm.Compact(ctx, provider, model, systemPrompt, llmMessages, compactConfig)
			} else {
				result, err = llm.SoftCompact(ctx, provider, model, systemPrompt, llmMessages, compactConfig)
			}
			return compactDoneMsg{result: result, err: err}
		},
		m.spinner.Tick,
		m.tickEvery(),
	)
}

type switchModelOptions struct {
	deferMarker bool
}

// switchModel switches to a new provider:model
func (m *Model) switchModel(providerModel string) (tea.Model, tea.Cmd) {
	return m.switchModelWithOptions(providerModel, switchModelOptions{})
}

func (m *Model) switchModelWithOptions(providerModel string, opts switchModelOptions) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(providerModel, ":", 2)
	if len(parts) != 2 {
		return m.showSystemMessage(fmt.Sprintf("Invalid model format: %s", providerModel))
	}

	providerName := parts[0]
	modelName := parts[1]

	oldProvider := strings.TrimSpace(m.providerKey)
	if oldProvider == "" {
		oldProvider = strings.TrimSpace(m.providerName)
	}
	oldModel := strings.TrimSpace(m.modelName)

	// Create new provider using the centralized factory
	provider, err := llm.NewProviderByName(m.config, providerName, modelName)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to switch model: %v", err))
	}
	m.clearPendingStreamModelSwitch()
	if !opts.deferMarker {
		m.pendingModelSwitch = nil
	}

	// Update model state
	m.provider = provider
	// Preserve existing tool registry when creating new engine
	m.engine = llm.NewEngine(provider, m.engine.Tools())
	m.providerName = provider.Name()
	m.providerKey = providerName
	m.modelName = modelName

	// Recompute fast/service-tier state for the new provider. /fast overrides are
	// per-provider-session state, so switching models/providers returns to config.
	var fastMetadataCmd tea.Cmd
	m.fastOverride = serviceTierInherit
	m.fastProviderDefault = m.configuredFastDefault()
	m.pendingFastToggle = false
	m.refreshEffectiveFastMode()
	if m.canUseFastMetadata() && (!m.fastMetadataLoaded || m.fastMetadataStale) {
		fastMetadataCmd = m.loadChatGPTModelsCmd()
	} else if !m.supportsServiceTierToggle() {
		m.fastMode = false
	}

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
	m.setTextareaValue("")
	guardianErr := m.refreshGuardianReviewer(providerName, modelName)

	if m.sess != nil && len(m.messages) > 0 {
		marker := llm.ModelSwapMarker{
			FromProvider: oldProvider,
			FromModel:    oldModel,
			ToProvider:   providerName,
			ToModel:      modelName,
			Status:       "started",
		}
		if opts.deferMarker {
			m.deferModelSwitchMarker(marker)
		} else {
			m.appendModelSwitchMarker(marker)
		}
	}

	msg := fmt.Sprintf("Switched model to %s:%s. Next response will try the existing context; if incompatible, use /handover to prepare a compact handoff.", providerName, modelName)
	if guardianErr != nil {
		msg += fmt.Sprintf(" Guardian auto-approval was disabled: %v", guardianErr)
		if fastMetadataCmd != nil {
			_, footerCmd := m.showFooterWarning(msg)
			return m, tea.Batch(footerCmd, fastMetadataCmd)
		}
		return m.showFooterWarning(msg)
	}
	if fastMetadataCmd != nil {
		_, footerCmd := m.showFooterMuted(msg)
		return m, tea.Batch(footerCmd, fastMetadataCmd)
	}
	return m.showFooterMuted(msg)
}

func (m *Model) applyPendingStreamModelSwitch() tea.Cmd {
	if m.pendingStreamModelSwitch == nil {
		return nil
	}
	pending := *m.pendingStreamModelSwitch
	m.clearPendingStreamModelSwitch()
	if strings.TrimSpace(pending.provider) == "" || strings.TrimSpace(pending.model) == "" {
		return nil
	}
	provider, model := m.currentProviderAndModel()
	if provider == pending.provider && model == pending.model {
		return nil
	}

	// Preserve any still-queued interjections across the engine replacement. A
	// text-only stream can finish with queued interjections that were never
	// committed; applying a deferred effort switch must not strand them on the old
	// engine before restorePendingInterjectionDraft has a chance to recover them.
	var queuedInterjections []llm.QueuedInterjection
	if m.engine != nil {
		queuedInterjections = m.engine.ListPendingInterjections()
	}

	// switchModelWithOptions clears the composer because most model switches are
	// explicit slash commands. A queued in-stream effort switch is applied
	// asynchronously, so preserve any draft the user typed while waiting for the
	// current response to finish.
	draft := m.textarea.Value()
	if m.config == nil {
		m.config = &config.Config{}
	}
	_, cmd := m.switchModelWithOptions(pending.provider+":"+pending.model, switchModelOptions{deferMarker: true})
	if m.engine != nil && m.providerKey == pending.provider && m.modelName == pending.model {
		for _, entry := range queuedInterjections {
			m.engine.QueueInterjection(entry)
		}
	}
	m.setTextareaValue(draft)
	return cmd
}

func (m *Model) deferModelSwitchMarker(marker llm.ModelSwapMarker) {
	if m.pendingModelSwitch != nil {
		m.pendingModelSwitch.ToProvider = marker.ToProvider
		m.pendingModelSwitch.ToModel = marker.ToModel
		m.pendingModelSwitch.Status = marker.Status
		return
	}
	pending := marker
	m.pendingModelSwitch = &pending
}

func (m *Model) appendPendingModelSwitchMarker() {
	if m.pendingModelSwitch == nil {
		return
	}
	marker := *m.pendingModelSwitch
	m.pendingModelSwitch = nil
	m.appendModelSwitchMarker(marker)
}

func (m *Model) appendModelSwitchMarker(marker llm.ModelSwapMarker) {
	if m.sess == nil {
		return
	}
	msg := llm.ModelSwapEventMessage(marker)
	sm := *session.NewMessage(m.sess.ID, msg, -1)
	m.messagesMu.Lock()
	m.messages = append(m.messages, sm)
	m.messagesMu.Unlock()
	if m.store != nil {
		_ = m.store.AddMessage(context.Background(), m.sess.ID, &sm)
	}
	// completedStream is an alt-screen-only cache of the response that was just
	// streamed. Once we append a model-switch marker after that assistant turn,
	// the cache is no longer a tail replacement; leaving it in place renders the
	// persisted assistant from history plus the cached stream again.
	m.invalidateViewCache()
}

// transientHandoverSystemPrompt intentionally keeps generated handover results
// free of target-agent system prompts. executeHandover resolves the target prompt
// through the normal chat startup pipeline and prepends it when persisting the
// new session, so these intermediate results should carry handover context only.
const transientHandoverSystemPrompt = ""

// cmdHandover handles /handover @agent [provider:model]
func (m *Model) cmdHandover(args []string) (tea.Model, tea.Cmd) {
	m.pauseGoalForLocalAction("paused for handover")
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

	// Handover results are intermediate context documents. The target agent's
	// actual system prompt is resolved during executeHandover via the same pipeline
	// used to start a new chat session.

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
		result := llm.HandoverFromFile(preview, transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
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
		result := llm.HandoverFromFile(doc, transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
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

	// File mode: use this session's pinned handover document. Legacy prompts
	// without a pinned assignment retain the latest-file fallback.
	if mode == "file" || (mode == "" && m.currentAgent != nil && m.currentAgent.EnableHandover) {
		handoverDir := ""
		pinnedPath := ""
		pathPinned := false
		if m.currentAgent != nil && m.currentAgent.EnableHandover {
			if path, dir, pinned, err := m.resolveHandoverPath(m.currentSystemPromptText()); err == nil {
				pinnedPath, handoverDir, pathPinned = path, dir, pinned
			}
		}
		if handoverDir != "" {
			// When the prompt assigns a path, never fall back to scanning — a
			// newer .md from a concurrent session must not shadow this session's
			// plan, even while ours is empty, missing, or ambiguous.
			var latestFile string
			var latestInfo os.FileInfo
			if pathPinned {
				if pinnedPath != "" {
					if info, err := os.Stat(pinnedPath); err == nil {
						latestFile, latestInfo = pinnedPath, info
					}
				}
			} else {
				latestFile, latestInfo = findLatestHandoverFile(handoverDir)
			}
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
						result := llm.HandoverFromFile(string(content), transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
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
		result := llm.HandoverFromFile("(No prior conversation to hand over.)", transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
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
		result := llm.HandoverFromFile("(No prior conversation to hand over.)", transientHandoverSystemPrompt, sourceAgent, targetAgent.Name)
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

	m.clearFooterMessage()
	m.streaming = true
	// A tool-initiated handover continues the current engine stream, so leave
	// its tracker intact; only a manual /handover starts fresh and should drop
	// the retained previous-turn tracker to avoid re-rendering it.
	if m.handoverToolDoneCh == nil {
		m.resetRetainedStreamTracker()
	}
	m.phase = "Handover"
	m.streamStartTime = time.Now()
	if m.altScreen {
		m.scrollToBottom = true
	}

	ctx, cancel := context.WithCancel(m.rootContext())
	m.streamCancelFunc = cancel

	return m, tea.Batch(
		func() tea.Msg {
			result, err := llm.Handover(ctx, provider, model, currentSystemPrompt, transientHandoverSystemPrompt, llmMessages, sourceAgent, targetAgent.Name, compactConfig)
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

// currentSystemPromptText returns the text of this session's persisted system
// message, or "" if none exists.
func (m *Model) currentSystemPromptText() string {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()
	for _, msg := range m.messages {
		if msg.Role != llm.RoleSystem {
			continue
		}
		for _, p := range msg.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
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

var runHandoverScriptForCmd = runHandoverScript

func handoverScriptCmd(ctx context.Context, approvalMgr *tools.ApprovalManager, scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, provider string, confirmed bool, instructions string) tea.Cmd {
	agentName := ""
	if targetAgent != nil {
		agentName = targetAgent.Name
	}
	return func() tea.Msg {
		result, err := buildScriptBackedHandover(ctx, approvalMgr, scriptAgent, sourceAgent, targetAgent, provider, instructions)
		return handoverDoneMsg{
			result:       result,
			err:          err,
			agentName:    agentName,
			providerStr:  provider,
			confirmed:    confirmed,
			instructions: instructions,
		}
	}
}

func buildScriptBackedHandover(ctx context.Context, approvalMgr *tools.ApprovalManager, scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, provider string, instructions string) (*llm.HandoverResult, error) {
	if scriptAgent == nil {
		return nil, fmt.Errorf("handover script agent is not configured")
	}
	if targetAgent == nil {
		return nil, fmt.Errorf("target agent is not configured")
	}
	doc, err := runHandoverScriptForCmd(ctx, approvalMgr, scriptAgent, scriptAgent.HandoverScript, handoverApprovalTranscript(sourceAgent, provider, instructions))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(doc) == "" {
		return nil, fmt.Errorf("handover script produced no output")
	}
	return llm.HandoverFromFile(doc, transientHandoverSystemPrompt, sourceAgent, targetAgent.Name), nil
}

func (m *Model) startHandoverScriptHandover(scriptAgent *agents.Agent, sourceAgent string, targetAgent *agents.Agent, providerStr string, confirmed bool, instructions string) (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(m.rootContext())
	m.streamCancelFunc = cancel
	m.clearFooterMessage()
	m.streaming = true
	// As with the LLM handover path, only drop the retained tracker for a
	// manual handover; a tool-initiated handover continues the engine stream.
	if m.handoverToolDoneCh == nil {
		m.resetRetainedStreamTracker()
	}
	m.phase = "Handover"
	m.streamStartTime = time.Now()
	if m.altScreen {
		m.scrollToBottom = true
	}

	return m, tea.Batch(
		handoverScriptCmd(ctx, m.handoverApprovalMgr, scriptAgent, sourceAgent, targetAgent, providerStr, confirmed, instructions),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

// runHandoverScript executes a handover command without invoking a shell and returns its stdout.
func runHandoverScript(ctx context.Context, approvalMgr *tools.ApprovalManager, agent *agents.Agent, script string, transcript []tools.TranscriptEntry) (string, error) {
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
	outcome, err := approvalMgr.CheckShellApprovalWithContext(ctx, script, workDir, transcript)
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

func handoverMessagesToPersist(messages []llm.Message, systemPrompt string) []llm.Message {
	start := 0
	for start < len(messages) && messages[start].Role == llm.RoleSystem {
		start++
	}

	if strings.TrimSpace(systemPrompt) == "" {
		return messages[start:]
	}

	out := make([]llm.Message, 0, len(messages)-start+1)
	out = append(out, llm.SystemText(systemPrompt))
	out = append(out, messages[start:]...)
	return out
}

// executeHandover performs the actual agent switch after user confirmation.
// It creates a brand-new session containing only the reconstructed handover
// context, then triggers a TUI restart so the target agent's full runtime
// configuration (tools, permissions, MCP, shell allowlists) is applied via the
// normal startup path. The source session is left intact to avoid cross-talk.
func (m *Model) executeHandover() (tea.Model, tea.Cmd) {
	if m.pendingHandover == nil {
		return m, nil
	}

	pending := m.pendingHandover

	// Step 1: Resolve target agent before any mutations.
	targetAgent, resolveErr := m.agentResolver(pending.agentName, m.config)
	if resolveErr != nil || targetAgent == nil {
		m.cancelHandoverTool()
		msg := "unknown error"
		if resolveErr != nil {
			msg = resolveErr.Error()
		}
		return m.showFooterError(fmt.Sprintf("Handover failed to resolve target agent: %s", msg))
	}

	result := applyHandoverInstructions(pending.result, pending.instructions)
	if result == nil {
		m.cancelHandoverTool()
		return m.showFooterError("Handover failed: no result returned.")
	}
	if m.store == nil {
		m.cancelHandoverTool()
		return m.showFooterError("Handover failed to persist: session storage is not configured")
	}

	ctx := context.Background()

	// Step 2: Build a fresh target session. Handover must never compact or mutate
	// the current session: the new agent gets a clean DB session whose history is
	// only the reconstructed handover context.
	newSess := m.buildHandoverSession(pending, targetAgent)

	var targetSystemPrompt string
	var err error
	targetDir := strings.TrimSpace(newSess.WorktreeDir)
	if targetDir == "" {
		targetDir = strings.TrimSpace(newSess.CWD)
	}
	if m.runtimeSystemContextResolver != nil {
		resolved, resolveErr := m.runtimeSystemContextResolver(targetAgent, newSess.ProviderKey, newSess.Model, targetDir)
		err = resolveErr
		targetSystemPrompt = resolved.SystemPrompt
	} else if m.handoverSystemPromptResolver != nil {
		targetSystemPrompt, err = m.handoverSystemPromptResolver(targetAgent, newSess.ProviderKey, newSess.Model)
	} else {
		m.cancelHandoverTool()
		return m.showFooterError("Handover failed to resolve target system prompt: resolver is not configured")
	}
	if err != nil {
		m.cancelHandoverTool()
		return m.showFooterError(fmt.Sprintf("Handover failed to resolve target system prompt: %v", err))
	}

	if err := m.store.Create(ctx, newSess); err != nil {
		m.cancelHandoverTool()
		return m.showFooterError(fmt.Sprintf("Handover failed to persist: %v", err))
	}
	cleanupNewSession := func() {
		_ = m.store.Delete(context.Background(), newSess.ID)
	}

	// Persist the target agent's resolved system prompt first, followed by the
	// conversational handover context. Keeping the system prompt at sequence 0 is
	// required so every resume/reload sees the target agent's prompt before the
	// handover document and does not inject duplicates later.
	for _, msg := range handoverMessagesToPersist(result.NewMessages, targetSystemPrompt) {
		newMsg := session.NewMessage(newSess.ID, msg, -1)
		if err := m.store.AddMessage(ctx, newSess.ID, newMsg); err != nil {
			cleanupNewSession()
			m.cancelHandoverTool()
			return m.showFooterError(fmt.Sprintf("Handover failed to persist: %v", err))
		}
	}

	if err := m.store.SetCurrent(ctx, newSess.ID); err != nil {
		cleanupNewSession()
		m.cancelHandoverTool()
		return m.showFooterError(fmt.Sprintf("Handover failed to set current session: %v", err))
	}

	// Mark the source session complete only after the target session is fully
	// committed and current. Failure here is best-effort and should not undo the
	// successful handover.
	if m.sess != nil && m.sess.ID != "" {
		_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusComplete)
	}
	m.pendingHandover = nil

	// Step 3: Trigger TUI restart via the same resume mechanism used by /resume.
	// This causes runChatOnce to exit and the outer loop to re-enter with the new
	// session, running the full agent setup path. Do not append handover messages
	// into the source session's in-memory scrollback; the dying model should not
	// show or later persist target-agent context under the source session.
	m.pendingResumeSessionID = newSess.ID
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
	// Cancel the engine stream now that the tool is unblocked.
	if m.streamCancelFunc != nil {
		m.streamCancelFunc()
		m.streamCancelFunc = nil
	}
	m.quitting = true
	return m, m.quitCmd()
}

func (m *Model) buildHandoverSession(pending *handoverDoneMsg, targetAgent *agents.Agent) *session.Session {
	agentName := strings.TrimSpace(pending.agentName)
	if agentName == "" && targetAgent != nil {
		agentName = targetAgent.Name
	}

	providerKey := m.providerKey
	modelName := m.modelName
	if pending.providerStr != "" {
		parts := strings.SplitN(pending.providerStr, ":", 2)
		if len(parts) == 2 {
			providerKey = parts[0]
			modelName = parts[1]
		}
	} else if targetAgent != nil {
		// Apply agent provider and model independently (matching ResolveSettings behavior).
		if targetAgent.Provider != "" {
			providerKey = targetAgent.Provider
		}
		if targetAgent.Model != "" {
			modelName = targetAgent.Model
		}
	}

	providerLabel := m.providerName
	if providerKey != m.providerKey || modelName != m.modelName {
		providerLabel = providerKey
		if modelName != "" {
			providerLabel = fmt.Sprintf("%s (%s)", providerKey, modelName)
		}
	}
	if providerLabel == "" {
		providerLabel = providerKey
	}

	toolsStr := ""
	searchEnabled := false
	mcpStr := ""
	if targetAgent != nil {
		searchEnabled = targetAgent.Search
		toolsStr = resolveAgentTools(targetAgent)
		mcpNames := targetAgent.GetMCPServerNames()
		if len(mcpNames) > 0 {
			mcpStr = strings.Join(mcpNames, ",")
		}
	}

	now := time.Now()
	newSess := &session.Session{
		ID:            session.NewID(),
		Provider:      providerLabel,
		ProviderKey:   providerKey,
		Model:         modelName,
		Mode:          session.ModeChat,
		Origin:        session.OriginTUI,
		Agent:         agentName,
		CreatedAt:     now,
		UpdatedAt:     now,
		Search:        searchEnabled,
		Tools:         toolsStr,
		MCP:           mcpStr,
		Status:        session.StatusActive,
		CompactionSeq: -1,
	}
	if m.sess != nil {
		if worktreeDir := strings.TrimSpace(m.sess.WorktreeDir); worktreeDir != "" {
			newSess.WorktreeDir = worktreeDir
			newSess.CWD = worktreeDir
			return newSess
		}
		if cwd := strings.TrimSpace(m.sess.CWD); cwd != "" {
			newSess.CWD = cwd
			return newSess
		}
	}
	if m.toolMgr != nil {
		if baseDir := strings.TrimSpace(m.toolMgr.BaseDir()); baseDir != "" {
			newSess.CWD = baseDir
			return newSess
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		newSess.CWD = cwd
	}
	return newSess
}

func handoverApprovalTranscript(sourceAgent, targetAgent, instructions string) []tools.TranscriptEntry {
	var b strings.Builder
	b.WriteString("The operator initiated a handover")
	if strings.TrimSpace(sourceAgent) != "" || strings.TrimSpace(targetAgent) != "" {
		fmt.Fprintf(&b, " from agent %q to agent %q", strings.TrimSpace(sourceAgent), strings.TrimSpace(targetAgent))
	}
	b.WriteString(". Running the configured handover_script is the narrow handover preparation step.")
	if strings.TrimSpace(instructions) != "" {
		fmt.Fprintf(&b, " Operator handover instructions: %s", strings.TrimSpace(instructions))
	}
	return []tools.TranscriptEntry{{Role: "user", Text: b.String()}}
}
