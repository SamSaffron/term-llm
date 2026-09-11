package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

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
	draft := m.captureComposerSnapshot()
	result, cmd := m.showHelpModal()
	if rm, ok := result.(*Model); ok {
		rm.restoreComposerSnapshot(draft)
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
				{"Ctrl+C", "Copy selection; cancel active response/tool/shell; press twice when idle to quit"},
				{"Esc", "Cancel streaming or a running ! command / close modal / clear selection or input"},
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
				{"Enter", "Send message; while streaming, queue steering"},
				{"Esc (pending steering)", "Interrupt safely, then start a new run with all pending guidance"},
				{"Delete (selected steering)", "Remove the selected pending message"},
				{"! command", "Run directly in the session directory, then ask the model to respond"},
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
				{"Ctrl+Up / Ctrl+Down", "Jump between user prompts"},
				{"Up / Down", "Scroll when composer is empty; select queued steering while streaming"},
				{"Ctrl+Y", "Copy selection, or latest assistant response"},
			},
		},
		{
			title: "Pickers and completions",
			rows: [][2]string{
				{"@query", "Find permitted agents and project files/directories"},
				{"@agent:name", "Request delegation through authorized spawn_agent"},
				{"@path", "Attach a project file/directory when submitted"},
				{"Up/Down or Ctrl+P/Ctrl+N", "Move selection"},
				{"Enter", "Select mention / execute command / choose item"},
				{"Tab", "Select mention or fill selected completion"},
				{"Backspace", "Edit filter"},
				{"Esc", "Close picker"},
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

type transcriptMutationDoneMsg struct {
	sessionID     string
	redo          bool
	result        session.TranscriptMutationResult
	sess          *session.Session
	messages      []session.Message
	compactionIdx int
	mutationErr   error
	refreshErr    error
}

func (m *Model) transcriptMutationBusy() bool {
	return m.transcriptMutationInFlight || m.branchContextInFlight() || m.streaming || m.activeSkillRunCount() > 0 || m.sideQuestion.Running || m.titleGenerationInFlight ||
		m.worktreeOperationBusy() || m.shareInFlight || m.externalProcessActive || m.pausedForExternalUI
}

func transcriptMutationCmd(ctx context.Context, store session.Store, mutationStore session.TranscriptUndoRedoStore, sessionID string, redo bool) tea.Cmd {
	return func() tea.Msg {
		expected, err := mutationStore.TranscriptMutationState(ctx, sessionID)
		if err != nil {
			return transcriptMutationDoneMsg{sessionID: sessionID, redo: redo, mutationErr: err}
		}
		var result session.TranscriptMutationResult
		if redo {
			result, err = mutationStore.RedoLastUserTurn(ctx, sessionID, expected)
		} else {
			result, err = mutationStore.UndoLastUserTurn(ctx, sessionID, expected)
		}
		if err != nil {
			return transcriptMutationDoneMsg{sessionID: sessionID, redo: redo, mutationErr: err}
		}
		refreshed, err := store.Get(ctx, sessionID)
		if err != nil {
			return transcriptMutationDoneMsg{sessionID: sessionID, redo: redo, result: result, refreshErr: err}
		}
		messages, compactionIdx, err := loadSessionMessagesForScrollback(ctx, store, refreshed)
		return transcriptMutationDoneMsg{
			sessionID: sessionID, redo: redo, result: result, sess: refreshed,
			messages: messages, compactionIdx: compactionIdx, refreshErr: err,
		}
	}
}

func (m *Model) handleTranscriptMutationDone(msg transcriptMutationDoneMsg) (tea.Model, tea.Cmd) {
	m.transcriptMutationInFlight = false
	command := "undo"
	if msg.redo {
		command = "redo"
	}
	label := strings.ToUpper(command[:1]) + command[1:]
	if msg.mutationErr != nil {
		switch {
		case errors.Is(msg.mutationErr, context.Canceled), errors.Is(msg.mutationErr, context.DeadlineExceeded):
			return m.showFooterMuted(label + " cancelled.")
		case errors.Is(msg.mutationErr, session.ErrNothingToUndo):
			return m.showFooterMuted("Nothing to undo.")
		case errors.Is(msg.mutationErr, session.ErrNothingToRedo):
			return m.showFooterMuted("Nothing to redo.")
		case errors.Is(msg.mutationErr, session.ErrTranscriptConflict):
			return m.showFooterWarning("Conversation changed in another client; try again.")
		default:
			return m.showFooterError(fmt.Sprintf("%s failed: %v", label, msg.mutationErr))
		}
	}
	if m.sess == nil || m.sess.ID != msg.sessionID {
		return m.showFooterWarning(label + " completed for a session that is no longer active.")
	}
	if msg.refreshErr != nil || msg.sess == nil {
		return m.showFooterError(fmt.Sprintf("%s succeeded, but refresh failed: %v", label, msg.refreshErr))
	}
	m.sess = msg.sess
	m.messagesMu.Lock()
	m.messages = msg.messages
	m.compactionIdx = msg.compactionIdx
	m.messagesMu.Unlock()
	m.olderScrollbackLoaded = true
	m.scrollOffset = 0
	m.scrollToBottom = true
	// A completed response may still be held separately from persisted history for
	// the streaming fast path. Transcript replacement makes that snapshot stale.
	m.viewCache.completedStream = ""
	m.invalidateAltScreenStreamingViewportCache()
	m.invalidateHistoryCache()
	m.resetContextEstimateBaseline(m.rootContext())
	m.resetTitleGenerationStateForSession()
	if m.engine != nil {
		m.engine.ResetSessionState(m.sess.ID)
		m.engine.SetContextEstimateBaseline(0, 0)
	}
	m.currentResponse.Reset()
	m.currentTokens = 0
	m.retryStatus = ""
	m.clearPendingStreamModelSwitch()
	if m.tracker != nil {
		m.resetTracker()
	}
	hydrate := m.loadPersistedSubagentsCmd()
	if msg.redo {
		m.setTextareaValue("")
		updated, footer := m.showFooterSuccess("Restored the undone turn.")
		return updated, tea.Batch(footer, m.terminalTitleCmd(), hydrate)
	}
	m.setTextareaValue(msg.result.UserText)
	if msg.result.AttachmentsOmitted {
		updated, footer := m.showFooterWarning("Removed the latest turn; attachments were not restored.")
		return updated, tea.Batch(footer, m.terminalTitleCmd(), hydrate)
	}
	updated, footer := m.showFooterSuccess("Removed the latest turn.")
	return updated, tea.Batch(footer, m.terminalTitleCmd(), hydrate)
}

func (m *Model) cmdUndoRedo(redo bool, args []string) (tea.Model, tea.Cmd) {
	command := "undo"
	if redo {
		command = "redo"
	}
	if len(args) != 0 {
		m.setTextareaValue("")
		return m.showFooterError("Usage: /" + command)
	}
	if m.transcriptMutationBusy() {
		return m.showFooterWarning("Cannot " + command + " while work is active.")
	}
	if m.store == nil || m.sess == nil || strings.TrimSpace(m.sess.ID) == "" {
		m.setTextareaValue("")
		return m.showFooterError("No stored session to " + command + ".")
	}
	mutationStore, ok := m.store.(session.TranscriptUndoRedoStore)
	if !ok {
		m.setTextareaValue("")
		return m.showFooterError("Session storage does not support /" + command + ".")
	}
	m.setTextareaValue("")
	m.transcriptMutationInFlight = true
	return m, transcriptMutationCmd(m.rootContext(), m.store, mutationStore, m.sess.ID, redo)
}

func (m *Model) cmdClear() (tea.Model, tea.Cmd) {
	m.clearSideQuestionHistory()
	m.clearPendingStreamModelSwitch()
	m.resetMainRunSessionBinding()
	// Mark the old session as complete before creating a new one
	if m.store != nil && m.sess != nil {
		_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusComplete)
	}

	// Create a new session to clear the conversation
	// This preserves the old session in history while starting fresh
	m.sess = &session.Session{
		ID:           session.NewID(),
		Provider:     m.providerName,
		ProviderKey:  m.providerKey,
		Model:        m.modelName,
		Mode:         session.ModeChat,
		Agent:        m.agentName,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		Search:       m.searchEnabled,
		Tools:        m.toolsStr,
		MCP:          m.mcpStr,
		ApprovalMode: sessionApprovalModeFromTools(m.requestedApprovalMode),
	}
	if cwd, err := os.Getwd(); err == nil {
		m.sess.CWD = cwd
		m.pendingTerminalDirectory = cwd
	}

	// Persist new session and infer its registered project from the CWD.
	persistNewTUISession(context.Background(), m.store, m.sess)
	m.notifySessionInputs()

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
	m.attachVisibleMainRunUISink()

	updated, footerCmd := m.showFooterSuccess("Started a new session.")
	return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
}

func (m *Model) cmdQuit() (tea.Model, tea.Cmd) {
	m.cancelSideQuestion()
	if m.branchOperationCancel != nil {
		m.branchOperationCancel()
		m.branchOperationCancel = nil
	}
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
	m.cancelActiveSkillRuns()
	m.quitting = true
	if m.activeSkillRunCount() > 0 {
		m.quitAfterSkillRuns = true
		return m, nil
	}
	if !hadActiveStream {
		if summary := m.exitStatsSummary(); summary != "" {
			return m, m.quitCmd(tea.Println(summary))
		}
	}
	return m, m.quitCmd()
}
