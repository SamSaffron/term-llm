package chat

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
)

// pasteCollapseThreshold is the minimum length (in characters) of a paste
// before it collapses into a placeholder instead of inline text.
const pasteCollapseThreshold = 100

func (m *Model) streamCancelTimeoutCmd() tea.Cmd {
	done := m.streamDone
	generation := m.streamGeneration
	return func() tea.Msg {
		if done == nil {
			return nil
		}
		timer := time.NewTimer(streamCancelMaxWait)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
			return streamCancelTimeoutMsg{done: done, generation: generation}
		}
	}
}

// updateResumeBrowserMode handles updates while the embedded resume browser is active.
func (m *Model) updateResumeBrowserMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.applyWindowSize(msg)
		if m.resumeBrowserModel != nil {
			var cmd tea.Cmd
			updated, next := m.resumeBrowserModel.Update(msg)
			if browser, ok := updated.(*sessionsui.Model); ok {
				m.resumeBrowserModel = browser
			}
			cmd = next
			return m, cmd
		}
		return m, nil

	case sessionsui.ChatMsg:
		m.resumeBrowserMode = false
		m.resumeBrowserModel = nil
		return m.requestResumeSession(msg.SessionID)

	case sessionsui.CloseMsg:
		return m.closeResumeBrowser()

	default:
		if m.resumeBrowserModel != nil {
			var cmd tea.Cmd
			updated, next := m.resumeBrowserModel.Update(msg)
			if browser, ok := updated.(*sessionsui.Model); ok {
				m.resumeBrowserModel = browser
			}
			cmd = next
			return m, cmd
		}
	}

	return m, nil
}

// updateInspectorMode handles updates while in inspector mode
func (m *Model) updateInspectorMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.applyWindowSize(msg)
		// Pass to inspector
		if m.inspectorModel != nil {
			m.inspectorModel, _ = m.inspectorModel.Update(msg)
		}
		return m, nil

	case tea.KeyPressMsg:
		// Pass to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
		return m, nil

	case inspector.CloseMsg:
		// Exit inspector mode
		m.inspectorMode = false
		m.inspectorModel = nil
		m.textarea.Focus()
		return m, nil

	default:
		// Pass through to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m *Model) isYoloToggleKey(msg tea.KeyPressMsg) bool {
	return key.Matches(msg, m.keyMap.ToggleYolo)
}

func (m *Model) isHelpKey(msg tea.KeyPressMsg) bool {
	if key.Matches(msg, m.keyMap.Help) {
		return true
	}

	matches := func(value string) bool {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "ctrl+h", "ctrl+?", "ctrl+shift+?", "ctrl+/", "ctrl+shift+/", "ctrl+_", "ctrl+shift+_":
			return true
		default:
			return false
		}
	}
	if matches(msg.String()) || matches(msg.Keystroke()) {
		return true
	}

	k := msg.Key()
	if !k.Mod.Contains(tea.ModCtrl) {
		return false
	}
	if k.Text == "?" {
		return true
	}
	for _, code := range []rune{k.Code, k.ShiftedCode, k.BaseCode} {
		switch code {
		case 'h', '?', '/', '_':
			return true
		}
	}
	return false
}

func (m *Model) currentApprovalMode() tools.ApprovalMode {
	if m.approvalMgr != nil {
		return m.approvalMgr.ApprovalMode()
	}
	if m.handoverApprovalMgr != nil {
		return m.handoverApprovalMgr.ApprovalMode()
	}
	if m.yolo {
		return tools.ModeYolo
	}
	return tools.ModePrompt
}

func (m *Model) isYoloModeActive() bool {
	return m.currentApprovalMode() == tools.ModeYolo
}

func isChaosMonkeyKey(msg tea.KeyPressMsg) bool {
	return os.Getenv("TERM_LLM_CHAOS_MONKEY") != "" && key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+m", "ctrl+g")))
}

func (m *Model) setApprovalMode(mode tools.ApprovalMode) {
	m.yolo = mode == tools.ModeYolo
	if m.approvalMgr != nil {
		m.approvalMgr.SetApprovalMode(mode)
	}
	if m.handoverApprovalMgr != nil && m.handoverApprovalMgr != m.approvalMgr {
		m.handoverApprovalMgr.SetApprovalMode(mode)
	}
	if m.mcpManager != nil {
		m.mcpManager.SetSamplingYoloMode(mode == tools.ModeYolo)
	}
	m.persistApprovalMode(mode)
}

func (m *Model) setApprovalYoloMode(enabled bool) {
	if enabled {
		m.setApprovalMode(tools.ModeYolo)
	} else {
		m.setApprovalMode(tools.ModePrompt)
	}
}

func (m *Model) autoApprovalAvailable() bool {
	if m.approvalMgr != nil && m.approvalMgr.GuardianReviewerAvailable() {
		return true
	}
	if m.handoverApprovalMgr != nil && m.handoverApprovalMgr != m.approvalMgr && m.handoverApprovalMgr.GuardianReviewerAvailable() {
		return true
	}
	return false
}

func (m *Model) toggleYoloMode() (tea.Model, tea.Cmd) {
	current := m.currentApprovalMode()
	next := tools.ModeAuto
	autoUnavailable := false
	switch current {
	case tools.ModePrompt:
		if m.autoApprovalAvailable() {
			next = tools.ModeAuto
		} else {
			next = tools.ModeYolo
			autoUnavailable = true
		}
	case tools.ModeAuto:
		next = tools.ModeYolo
	case tools.ModeYolo:
		next = tools.ModePrompt
	}
	m.setApprovalMode(next)

	var cmds []tea.Cmd
	if next == tools.ModeYolo && m.approvalModel != nil && m.approvalDoneCh != nil {
		m.approvalDoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceOnce}
		m.approvalModel = nil
		m.approvalDoneCh = nil
		m.pausedForExternalUI = false
		cmds = append(cmds, m.spinner.Tick)
	}

	message := "Prompt approval mode enabled. Tool approvals will prompt."
	tone := "muted"
	if autoUnavailable {
		message = "Auto approval mode unavailable: no guardian reviewer configured. Skipping to yolo mode."
		tone = "warning"
	}
	switch next {
	case tools.ModeAuto:
		message = "Auto approval mode enabled. Unmatched shell commands will be reviewed by guardian. Other approvals will still prompt."
		tone = "success"
	case tools.ModeYolo:
		if !autoUnavailable {
			message = "Yolo mode enabled. Tool approvals will auto-approve."
			tone = "success"
		}
	}
	_, footerCmd := m.showFooterMessageWithTone(message, tone)
	if footerCmd != nil {
		cmds = append(cmds, footerCmd)
	}
	m.appendTerminalTitleCmd(&cmds)
	return m, tea.Batch(cmds...)
}

const ctrlCExitConfirmWindow = 2 * time.Second

func (m *Model) cancelActiveForInterrupt() (bool, tea.Cmd) {
	cancelled := false
	var cmds []tea.Cmd

	if m.approvalDoneCh != nil || m.approvalModel != nil {
		if m.approvalDoneCh != nil {
			select {
			case m.approvalDoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}:
			default:
			}
		}
		m.approvalDoneCh = nil
		m.approvalModel = nil
		m.pausedForExternalUI = false
		cancelled = true
	}

	if m.askUserDoneCh != nil || m.askUserModel != nil {
		if m.askUserDoneCh != nil {
			select {
			case m.askUserDoneCh <- nil:
			default:
			}
		}
		m.askUserDoneCh = nil
		m.askUserModel = nil
		m.pausedForExternalUI = false
		cancelled = true
	}

	if m.handoverPreview != nil || m.pendingHandover != nil || m.handoverToolDoneCh != nil {
		m.cancelHandoverTool()
		m.pendingHandover = nil
		m.handoverPreview = nil
		cancelled = true
	}

	if (m.streaming || m.streamCancelFunc != nil) && !m.isStreamCancelRequested() {
		m.phase = "Stopping..."
		if m.streamCancelFunc != nil {
			m.setStreamCancelRequested(true)
			m.streamCancelFunc()
			m.streamCancelFunc = nil
		}
		if m.engine != nil {
			_ = m.engine.DrainInterjection()
		}
		m.clearPendingInterjectionState()
		cmds = append(cmds, m.streamCancelTimeoutCmd())
		cancelled = true
	}

	return cancelled, tea.Batch(cmds...)
}

func (m *Model) quitFromInterrupt() (tea.Model, tea.Cmd) {
	m.quitting = true
	m.phase = "Stopping..."
	m.selection = Selection{}
	m.ctrlCExitArmedUntil = time.Time{}
	if m.completions != nil {
		m.completions.Hide()
	}
	if m.dialog.IsOpen() {
		m.dialog.Close()
	}

	hadActiveStream := m.streaming || m.streamCancelFunc != nil
	_, _ = m.cancelActiveForInterrupt()
	m.setShellTerminalHandoff(false)
	if m.program != nil {
		p := m.program
		go func() {
			time.Sleep(2 * time.Second)
			p.Kill()
		}()
	}

	if !hadActiveStream {
		if summary := m.exitStatsSummary(); summary != "" {
			return m, m.quitCmd(tea.Println(summary))
		}
	}
	return m, m.quitCmd()
}

func (m *Model) handleCtrlC() (tea.Model, tea.Cmd) {
	if cancelled, cancelCmd := m.cancelActiveForInterrupt(); cancelled {
		m.ctrlCExitArmedUntil = time.Time{}
		_, footerCmd := m.showFooterWarning("Interrupted current response/tool.")
		return m, tea.Batch(cancelCmd, footerCmd, m.terminalTitleCmd())
	}

	now := time.Now()
	if !m.ctrlCExitArmedUntil.IsZero() && now.Before(m.ctrlCExitArmedUntil) {
		return m.quitFromInterrupt()
	}

	m.ctrlCExitArmedUntil = now.Add(ctrlCExitConfirmWindow)
	if m.completions != nil {
		m.completions.Hide()
	}
	if m.dialog.IsOpen() {
		if m.dialog.Type() == DialogWorktreeRecovery {
			m.ctrlCExitArmedUntil = time.Time{}
			return m.resolveWorktreeRecoveryPrompt(false)
		}
		m.dialog.Close()
	}
	_, footerCmd := m.showFooterMessageWithToneFor("Press Ctrl-C again to exit.", "warning", ctrlCExitConfirmWindow)
	return m, footerCmd
}

func (m *Model) handleKeyMsg(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if isChaosMonkeyKey(msg) {
		if m.streaming && m.engine != nil {
			m.engine.TriggerChaosFailure()
			return m.showFooterMessageWithTone("Chaos monkey armed: simulating stream failure...", "warning")
		}
		return m.showFooterMessageWithTone("Chaos monkey is enabled; start streaming, then press ctrl+m/ctrl+g to fail the stream.", "muted")
	}

	// Ctrl+C copies an active text selection, matching the status-line hint and
	// preserving the long-standing selection workflow.
	if m.selection.Active {
		cmd := m.copySelectionToClipboard()
		m.selection = Selection{}
		return m, cmd
	}

	// Ctrl+C always does something safe: first it interrupts active work; once
	// idle, it requires a second Ctrl+C within a short confirmation window to
	// exit the TUI.
	if key.Matches(msg, m.keyMap.Quit) {
		return m.handleCtrlC()
	}
	m.ctrlCExitArmedUntil = time.Time{}

	// Shift+Tab toggles yolo mode globally, including while a reply is streaming
	// or an inline approval prompt is visible.
	if m.isYoloToggleKey(msg) {
		return m.toggleYoloMode()
	}

	// Handle embedded approval UI first if active
	if m.approvalModel != nil {
		done := m.approvalModel.UpdateEmbedded(msg)
		if done {
			result := m.approvalModel.Result()
			// Add summary to tracker for display
			if m.tracker != nil && !result.Cancelled {
				m.tracker.AddExternalUIResult(m.approvalModel.RenderSummary())
			}
			// Send result and clean up
			m.approvalDoneCh <- result
			m.approvalModel = nil
			m.approvalDoneCh = nil
			m.pausedForExternalUI = false
			return m, m.withTerminalTitleCmd(m.spinner.Tick)
		}
		return m, nil
	}

	// Handle embedded ask_user UI first if active
	if m.askUserModel != nil {
		cmd := m.askUserModel.UpdateEmbedded(msg)
		if m.askUserModel.IsDone() || m.askUserModel.IsCancelled() {
			// Add summary to tracker for display
			if m.tracker != nil && !m.askUserModel.IsCancelled() {
				m.tracker.AddExternalUIResult(m.askUserModel.RenderPlainSummary())
			}
			// Send result and clean up
			if m.askUserModel.IsCancelled() {
				m.askUserDoneCh <- nil
			} else {
				m.askUserDoneCh <- m.askUserModel.Answers()
			}
			m.askUserModel = nil
			m.askUserDoneCh = nil
			m.pausedForExternalUI = false
			return m, m.withTerminalTitleCmd(m.spinner.Tick)
		}
		if cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	// Handle pending handover confirmation via inline preview
	if m.handoverPreview != nil {
		done, handled := m.handoverPreview.UpdateEmbedded(msg)
		if done {
			if m.handoverPreview.confirmed {
				return m.Update(handoverConfirmMsg{})
			}
			return m.Update(handoverCancelMsg{})
		}
		if handled {
			return m, nil
		}
		// Allow viewport scroll keys to pass through; block everything else
		if m.altScreen && (key.Matches(msg, m.keyMap.PageUp) || key.Matches(msg, m.keyMap.PageDown)) {
			if key.Matches(msg, m.keyMap.PageUp) {
				m.loadOlderScrollbackPrefix(context.Background())
			}
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	// Ctrl+? / Ctrl+Shift+/ opens help globally in the normal chat UI and
	// preserves the current composer draft. Handle this before dialogs,
	// completions, and the textarea so terminal-specific encodings don't leak
	// into filters or render at the prompt.
	if m.isHelpKey(msg) {
		return m.showHelpShortcut()
	}

	// Bracketed paste and Ctrl+V image attach support for the composer.
	if m.maybeAttachImageFromPaste(msg) {
		return m, nil
	}

	// When image chips are present, allow keyboard selection/removal with arrows + backspace/delete.
	if m.handleImageAttachmentKeys(msg) {
		return m, nil
	}

	// While streaming, pending interjections form a cancellable stack. With an
	// empty composer, up/down selects and delete/backspace cancels the selected
	// not-yet-incorporated interjection.
	if m.streaming && len(m.pendingInterjections) > 0 && strings.TrimSpace(m.textarea.Value()) == "" {
		switch msg.String() {
		case "up":
			if m.selectedInterjection < 0 {
				m.selectedInterjection = len(m.pendingInterjections) - 1
			} else if m.selectedInterjection > 0 {
				m.selectedInterjection--
			}
			return m, nil
		case "down":
			if m.selectedInterjection >= 0 && m.selectedInterjection < len(m.pendingInterjections)-1 {
				m.selectedInterjection++
			}
			return m, nil
		case "delete", "backspace":
			if m.cancelSelectedPendingInterjection() {
				m.interruptNotice = "removed queued interjection"
			} else {
				m.interruptNotice = "interjection already incorporated"
			}
			return m, nil
		}
	}

	// Handle dialog first if open
	if m.dialog.IsOpen() {
		if m.dialog.Type() == DialogContent {
			m.dialog.Update(msg)
			return m, nil
		}
		// Model picker supports typing to filter (like completions)
		if m.dialog.Type() == DialogModelPicker {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
				selected := m.dialog.Selected()
				if selected != nil {
					m.dialog.Close()
					return m.switchModel(selected.ID)
				}
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
				m.dialog.Close()
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				// Update query on backspace
				query := m.dialog.Query()
				if len(query) > 0 {
					m.dialog.SetQuery(query[:len(query)-1])
				}
				return m, nil
			default:
				// Type to filter
				if len(msg.String()) == 1 {
					m.dialog.SetQuery(m.dialog.Query() + msg.String())
					return m, nil
				}
			}
			return m, nil
		}

		// MCP picker supports typing to filter and toggle without closing
		if m.dialog.Type() == DialogMCPPicker {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
				selected := m.dialog.Selected()
				if selected != nil {
					// Toggle the selected MCP server
					name := selected.ID
					status, _ := m.mcpManager.ServerStatus(name)
					if status == "ready" || status == "starting" {
						m.mcpManager.Disable(name)
					} else {
						m.mcpManager.Enable(context.Background(), name)
					}
					m.refreshMCPPickerIfOpen()
				}
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
				m.dialog.Close()
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k", "ctrl+p"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j", "ctrl+n"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				// Update query on backspace
				query := m.dialog.Query()
				if len(query) > 0 {
					m.dialog.SetQuery(query[:len(query)-1])
				}
				return m, nil
			default:
				// Type to filter
				if len(msg.String()) == 1 {
					m.dialog.SetQuery(m.dialog.Query() + msg.String())
					return m, nil
				}
			}
			return m, nil
		}

		if m.dialog.Type() == DialogWorktreeRecovery {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
				selected := m.dialog.Selected()
				if selected == nil {
					return m.resolveWorktreeRecoveryPrompt(false)
				}
				return m.resolveWorktreeRecoveryPrompt(selected.ID == "yes")
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
				return m.resolveWorktreeRecoveryPrompt(false)
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k", "down", "j"))):
				m.dialog.Update(msg)
				return m, nil
			default:
				return m, nil
			}
		}

		// Other dialogs (SessionList, DirApproval) use standard handling
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
			selected := m.dialog.Selected()
			if selected != nil {
				switch m.dialog.Type() {
				case DialogSessionList:
					m.dialog.Close()
					return m.cmdResume([]string{selected.ID})
				case DialogShareChoice:
					req := m.pendingShare
					m.pendingShare = nil
					m.dialog.Close()
					if req == nil || selected.ID == "cancel" {
						return m, nil
					}
					if selected.ID == "new" {
						req.forceNew = true
						return m.startShare(*req, false)
					}
					return m.startShare(*req, true)
				case DialogDirApproval:
					if selected.ID == "__deny__" {
						m.pendingFilePath = ""
						m.dialog.Close()
						return m.showSystemMessage("File access denied.")
					}
					// Approve the directory
					if err := m.approvedDirs.AddDirectory(selected.ID); err != nil {
						m.dialog.Close()
						return m.showSystemMessage("Failed to approve directory: " + err.Error())
					}
					// Now try to attach the file again
					filePath := m.pendingFilePath
					m.pendingFilePath = ""
					m.dialog.Close()
					return m.attachFile(filePath)
				}
			}
			m.dialog.Close()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
			m.pendingFilePath = ""
			m.pendingShare = nil
			if m.dialog.Type() == DialogWorktreeRecovery {
				return m.resolveWorktreeRecoveryPrompt(false)
			}
			m.dialog.Close()
			return m, nil
		default:
			m.dialog.Update(msg)
			return m, nil
		}
	}

	// Handle completions if visible
	if m.completions.IsVisible() {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			// Enter executes immediately with the selected command
			selected := m.completions.Selected()
			if selected != nil {
				// Capture typed input before clearing. Worktree commands clear their own
				// composer only after validation succeeds so failed commands remain editable.
				input := m.textarea.Value()
				m.completions.Hide()
				clearBeforeExecute := !strings.HasPrefix(selected.Name, "worktree")
				if clearBeforeExecute {
					m.setTextareaValue("")
				}

				// Multi-word completion items (e.g., "handover @developer",
				// "mcp start server") already contain the selected arg.
				// Preserve any typed suffix beyond what the completion covers.
				if strings.Contains(selected.Name, " ") {
					// Count words in selected name to find where extra args start
					selectedParts := strings.Fields(selected.Name)
					inputParts := strings.Fields(strings.TrimPrefix(input, "/"))
					// If user typed more words than the selection, keep the extra
					if len(inputParts) > len(selectedParts) {
						extra := strings.Join(inputParts[len(selectedParts):], " ")
						return m.ExecuteCommand("/" + selected.Name + " " + extra)
					}
					return m.ExecuteCommand("/" + selected.Name)
				}
				// Single-word command: extract any args the user typed
				args := ""
				if idx := strings.Index(input, " "); idx != -1 {
					args = strings.TrimSpace(input[idx+1:])
				}
				if args != "" {
					return m.ExecuteCommand("/" + selected.Name + " " + args)
				}
				return m.ExecuteCommand("/" + selected.Name)
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
			// Tab completes but doesn't execute (for adding args)
			selected := m.completions.Selected()
			if selected != nil {
				m.setTextareaValue("/" + selected.Name + " ")
				// Re-run completions — commands like /handover may show
				// argument completions (e.g., agent names) at this point
				m.updateCompletions()
				if !m.completions.IsVisible() {
					m.completions.Hide()
				}
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.completions.Hide()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
			m.completions.Update(msg)
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
			m.completions.Update(msg)
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
			// Update query on backspace
			value := m.textarea.Value()
			if len(value) > 1 {
				m.setTextareaValue(value[:len(value)-1])
				m.updateCompletions()
			} else if len(value) == 1 {
				m.setTextareaValue("")
				m.completions.Hide()
			}
			return m, nil
		default:
			// Add character to query
			if len(msg.String()) == 1 {
				m.setTextareaValue(m.textarea.Value() + msg.String())
				m.updateCompletions()
				return m, nil
			}
		}
	}

	// Shell-style prompt history: Up/Down first move within the composer, then
	// recall cross-session persisted user prompts at the visual boundaries. This
	// runs before viewport scrolling so the streaming interjection composer gets
	// the same history behavior as the normal composer.
	if handled, cmd := m.handlePromptHistoryKey(msg); handled {
		return m, cmd
	}

	// Clear stale copy status on any keypress
	m.copyStatus = ""

	// Copy selection on Ctrl+Y
	if key.Matches(msg, m.keyMap.Copy) && m.selection.Active {
		cmd := m.copySelectionToClipboard()
		m.selection = Selection{}
		return m, cmd
	}

	// Handle cancel during streaming (takes priority over clearing selection)
	if key.Matches(msg, m.keyMap.Cancel) {
		if m.streaming && m.streamCancelFunc != nil {
			m.setStreamCancelRequested(true)
			m.phase = "Stopping..."
			m.streamCancelFunc()

			// Recover pending interjection text into textarea
			if residual := m.engine.DrainInterjection(); residual != "" {
				m.setTextareaValue(residual)
			}
			m.clearPendingInterjectionState()

			m.textarea.Focus()
			return m, tea.Batch(m.applyPendingStreamModelSwitch(), m.streamCancelTimeoutCmd())
		}
		// Clear selection if active (before clearing textarea)
		if m.selection.Active {
			m.selection = Selection{}
			return m, nil
		}
		// Clear input if not empty
		if m.textarea.Value() != "" {
			m.setTextareaValue("")
			m.pasteChunks = nil
		}
		return m, nil
	}

	// Handle inspector view (Ctrl+O) - works even during streaming
	if key.Matches(msg, m.keyMap.Inspector) {
		// Only open inspector if we have messages
		if len(m.messages) > 0 {
			m.inspectorMode = true
			m.inspectorModel = inspector.NewWithConfig(m.messages, m.width, m.height, m.styles, m.store, m.newInspectorConfig())
			return m, nil
		}
		return m, nil
	}

	// Toggle expanded tool display (Ctrl+E) - works even during streaming.
	// If the cursor is on a collapsed paste placeholder in the composer, expand
	// that placeholder instead and don't bubble through to the global tool toggle.
	if key.Matches(msg, m.keyMap.ExpandTools) {
		if m.expandPastePlaceholderAtCursor() {
			return m, nil
		}
		wasAtBottom := m.altScreen && m.viewport.AtBottom()
		oldYOffset := 0
		if m.altScreen {
			oldYOffset = m.viewport.YOffset()
		}
		m.toolsExpanded = !m.toolsExpanded
		m.setReasoningDetailsExpanded(m.toolsExpanded)
		if m.chatRenderer != nil {
			m.chatRenderer.SetToolsExpanded(m.toolsExpanded)
		}
		if m.tracker != nil {
			m.tracker.SetExpanded(m.toolsExpanded)
			m.rerenderCommittedReasoningSegments()
			m.rerenderCompletedStreamFromTracker()
		}
		m.viewCache.cachedCompletedContent = ""
		m.viewCache.cachedTrackerVersion = 0
		m.viewCache.lastTrackerVersion = 0
		m.viewCache.lastSetContentAt = time.Time{}
		m.resetAltScreenStreamingAppendCache()
		m.bumpContentVersion()
		if m.altScreen {
			if wasAtBottom {
				m.scrollToBottom = true
			} else {
				m.viewport.SetYOffset(oldYOffset)
			}
		}
		return m, nil
	}

	// Allow viewport scrolling even while streaming (in alt screen mode)
	if m.altScreen {
		if key.Matches(msg, m.keyMap.PageUp) {
			m.loadOlderScrollbackPrefix(context.Background())
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		if key.Matches(msg, m.keyMap.PageDown) {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		// Arrow keys/j/k scroll viewport when:
		// - Textarea is empty (normal vim mode), OR
		// - Streaming is active (always allow scrolling during stream)
		if m.textarea.Value() == "" || m.streaming {
			// Scroll faster during streaming when content is out of view
			scrollAmount := 1
			if m.streaming && !m.viewport.AtBottom() {
				scrollAmount = 5
			}
			if key.Matches(msg, m.keyMap.HistoryUp) {
				m.loadOlderScrollbackPrefix(context.Background())
				m.viewport.ScrollUp(scrollAmount)
				return m, nil
			}
			if key.Matches(msg, m.keyMap.HistoryDown) {
				m.viewport.ScrollDown(scrollAmount)
				return m, nil
			}
		}
	}

	// Newline insertion (ctrl+j, alt+enter, shift+enter) — works in both the
	// streaming interjection composer and the normal composer. Must precede
	// any Send handler so shift+enter is caught before a plain "enter" match.
	if key.Matches(msg, m.keyMap.Newline) || key.Matches(msg, m.keyMap.NewlineAlt) {
		m.textarea.InsertString("\n")
		m.updateTextareaHeight()
		return m, nil
	}

	// Streaming-local shortcuts that affect the interjection composer or queue
	// deferred state must run before the generic streaming textarea handler.
	if m.streaming {
		if key.Matches(msg, m.keyMap.Commands) {
			m.setTextareaValue("/")
			m.completions.Show()
			m.updateCompletions()
			return m, nil
		}
		if key.Matches(msg, m.keyMap.CycleEffort) {
			return m.cycleEffort()
		}
	}

	// During streaming: allow local slash commands, typing, image attachments,
	// and interjection (send queues message for next turn). Commands like
	// /thinking should update the UI immediately rather than being shipped as an
	// interrupt/interjection to the model.
	if m.streaming {
		if key.Matches(msg, m.keyMap.Send) {
			raw := strings.TrimSpace(m.textarea.Value())
			if strings.HasSuffix(raw, "\\") {
				m.setTextareaValue(strings.TrimSuffix(raw, "\\") + "\n")
				return m, nil
			}
			if strings.HasPrefix(raw, "/") && isStreamingLocalSlashCommand(raw) {
				m.setTextareaValue("")
				m.completions.Hide()
				m.invalidateAltScreenStreamingViewportCache()
				updated, cmd := m.handleSlashCommand(raw)
				// Rebuild the cleared composer over a freshly rendered alt-screen
				// background before the command opens a panel or waits for an
				// asynchronous result. ClearScreen remains ordered after term-llm's
				// own viewport/append cache has been invalidated.
				return updated, tea.Sequence(tea.ClearScreen, cmd)
			}
			content := m.expandPastePlaceholders(raw)
			parts := m.imagePartList()
			if content == "" && len(parts) == 0 {
				m.phase = "Type to interject, attach an image, or press Esc to cancel"
				return m, nil
			}

			interjectionID := m.nextPendingInterjectionID()
			if action, ok := llm.ClassifyInterruptImmediate(content); ok && len(parts) == 0 {
				m.applyInterruptActionWithParts(interjectionID, content, parts, action)
				return m, nil
			}
			if m.fastProvider == nil {
				m.applyInterruptActionWithParts(interjectionID, content, parts, llm.InterruptInterject)
				return m, nil
			}

			return m, m.queueInterruptClassification(interjectionID, content, parts)
		}
		// Allow textarea to receive input
		old := m.textarea.Value()
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		m.updateTextareaHeight()
		if newVal := m.textarea.Value(); newVal != old {
			m.resetPromptHistoryIfEdited()
			if strings.HasPrefix(newVal, "/") {
				if !m.completions.IsVisible() {
					m.completions.Show()
				}
				m.updateCompletions()
			} else if m.completions.IsVisible() {
				m.completions.Hide()
			}
		}
		return m, cmd
	}

	// Handle command palette (Ctrl+P)
	if key.Matches(msg, m.keyMap.Commands) {
		m.setTextareaValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle model picker (Ctrl+L)
	if key.Matches(msg, m.keyMap.SwitchModel) {
		history, _ := config.LoadModelHistory()
		m.dialog.ShowModelPicker(m.providerKey+":"+m.modelName, GetAvailableProviders(m.config), config.ModelHistoryOrder(history))
		return m, nil
	}

	// Cycle reasoning effort (Ctrl+R) without disturbing the current draft.
	if key.Matches(msg, m.keyMap.CycleEffort) {
		return m.cycleEffort()
	}

	// Handle new session (Ctrl+N)
	if key.Matches(msg, m.keyMap.NewSession) {
		return m.cmdNew()
	}

	// Handle MCP picker (Ctrl+T)
	if key.Matches(msg, m.keyMap.MCPPicker) {
		if m.mcpManager == nil {
			return m.showSystemMessage("MCP not initialized.")
		}
		if len(m.mcpManager.AvailableServers()) == 0 {
			return m.showMCPQuickStart()
		}
		m.dialog.ShowMCPPicker(m.mcpManager)
		return m, nil
	}

	// Handle clear
	if key.Matches(msg, m.keyMap.Clear) {
		return m.cmdClear()
	}

	// Handle tab completion for /mcp commands
	if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
		value := m.textarea.Value()
		valueLower := strings.ToLower(value)

		// Tab completion for /mcp add <server> (from bundled servers)
		if strings.HasPrefix(valueLower, "/mcp add ") {
			partial := strings.TrimSpace(value[9:]) // after "/mcp add "
			if partial != "" {
				bundled := mcp.GetBundledServers()
				partialLower := strings.ToLower(partial)

				var match string
				for _, s := range bundled {
					if strings.HasPrefix(strings.ToLower(s.Name), partialLower) {
						match = s.Name
						break
					}
				}
				if match == "" {
					for _, s := range bundled {
						if strings.Contains(strings.ToLower(s.Name), partialLower) {
							match = s.Name
							break
						}
					}
				}
				if match != "" {
					m.setTextareaValue("/mcp add " + match)
				}
			}
			return m, nil
		}

		// Tab completion for /mcp start <server> (from configured servers)
		if strings.HasPrefix(valueLower, "/mcp start ") && m.mcpManager != nil {
			partial := strings.TrimSpace(value[11:]) // after "/mcp start "
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue("/mcp start " + match)
				}
			}
			return m, nil
		}

		// Tab completion for /mcp stop <server> (from configured servers)
		if strings.HasPrefix(valueLower, "/mcp stop ") && m.mcpManager != nil {
			partial := strings.TrimSpace(value[10:]) // after "/mcp stop "
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue("/mcp stop " + match)
				}
			}
			return m, nil
		}

		// Tab completion for /mcp restart <server> (from configured servers)
		if strings.HasPrefix(valueLower, "/mcp restart ") && m.mcpManager != nil {
			partial := strings.TrimSpace(value[13:]) // after "/mcp restart "
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue("/mcp restart " + match)
				}
			}
			return m, nil
		}

		return m, nil
	}

	// Handle send
	if key.Matches(msg, m.keyMap.Send) {
		content := strings.TrimSpace(m.textarea.Value())

		// Check for backslash continuation
		if strings.HasSuffix(content, "\\") {
			// Remove backslash and insert newline
			m.setTextareaValue(strings.TrimSuffix(content, "\\") + "\n")
			return m, nil
		}

		// Check for slash commands. If the leading token isn't a known command or
		// command prefix, treat the text as a normal chat message so pasted absolute
		// paths like /tmp/foo do not trap the composer behind command handling.
		if strings.HasPrefix(content, "/") && isSlashCommandLike(content) {
			return m.handleSlashCommand(content)
		}

		// Expand inline paste placeholders back to real content before sending.
		content = m.expandPastePlaceholders(content)

		// Send message if not empty, or if there are pasted image attachments.
		if content != "" || len(m.images) > 0 {
			return m.sendMessage(content)
		}
		return m, nil
	}

	// Handle "/" at start of empty input to show completions
	if msg.String() == "/" && m.textarea.Value() == "" {
		m.setTextareaValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle web toggle (Ctrl+S)
	if key.Matches(msg, m.keyMap.ToggleWeb) {
		m.toggleSearch()
		return m, nil
	}

	// Page up/down for scrolling (inline mode only - alt screen handled above)
	if !m.altScreen {
		if key.Matches(msg, m.keyMap.PageUp) {
			totalMessages := len(m.messages)
			maxScroll := totalMessages - 1
			if maxScroll < 0 {
				maxScroll = 0
			}
			m.scrollOffset += 5
			if m.scrollOffset > maxScroll {
				m.scrollOffset = maxScroll
			}
			return m, nil
		}

		if key.Matches(msg, m.keyMap.PageDown) {
			m.scrollOffset -= 5
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
			return m, nil
		}
	}

	// Update textarea for other keys (skip during streaming - no text input needed)
	if !m.streaming {
		old := m.textarea.Value()
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		// Clear selection when user starts typing
		if m.selection.Active && m.textarea.Value() != old {
			m.selection = Selection{}
		}
		if m.textarea.Value() != old {
			m.resetPromptHistoryIfEdited()
		}
		m.updateTextareaHeight()
		// Show argument completions for commands that support them
		// (e.g., /handover @<partial> triggers agent name completions)
		if newVal := m.textarea.Value(); newVal != old && strings.HasPrefix(newVal, "/") && !m.completions.IsVisible() {
			m.updateCompletions()
		}
		return m, cmd
	}
	return m, nil
}

func (m *Model) handleDialogPasteMsg(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if !m.dialog.IsOpen() {
		return m, nil
	}

	switch m.dialog.Type() {
	case DialogModelPicker, DialogMCPPicker:
		if msg.Content != "" {
			m.dialog.SetQuery(m.dialog.Query() + msg.Content)
		}
	}

	return m, nil
}

// handlePasteMsg handles bracketed-paste events, collapsing large pastes
// into inline placeholders that expand on send.
func (m *Model) handlePasteMsg(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	if m.approvalModel != nil {
		return m, nil
	}
	if m.askUserModel != nil {
		cmd := m.askUserModel.UpdateEmbedded(msg)
		if m.askUserModel.IsDone() || m.askUserModel.IsCancelled() {
			if m.tracker != nil && !m.askUserModel.IsCancelled() {
				m.tracker.AddExternalUIResult(m.askUserModel.RenderPlainSummary())
			}
			if m.askUserModel.IsCancelled() {
				m.askUserDoneCh <- nil
			} else {
				m.askUserDoneCh <- m.askUserModel.Answers()
			}
			m.askUserModel = nil
			m.askUserDoneCh = nil
			m.pausedForExternalUI = false
			return m, m.withTerminalTitleCmd(m.spinner.Tick)
		}
		return m, cmd
	}
	if m.handoverPreview != nil {
		done, handled := m.handoverPreview.UpdateEmbedded(msg)
		if done {
			if m.handoverPreview.confirmed {
				return m.Update(handoverConfirmMsg{})
			}
			return m.Update(handoverCancelMsg{})
		}
		if handled {
			return m, nil
		}
		return m, nil
	}
	if m.dialog.IsOpen() {
		return m.handleDialogPasteMsg(msg)
	}
	if msg.Content == "" {
		if m.maybeAttachImageFromClipboard() {
			return m, nil
		}
		return m, nil
	}
	text := msg.Content
	if shouldCollapsePaste(text) {
		m.pasteSeq++
		id := m.pasteSeq
		if m.pasteChunks == nil {
			m.pasteChunks = make(map[int]string)
		}
		m.pasteChunks[id] = text
		text = pastePlaceholder(id, text)
	}

	old := m.textarea.Value()
	m.textarea.InsertString(text)
	if m.selection.Active && m.textarea.Value() != old {
		m.selection = Selection{}
	}
	if newVal := m.textarea.Value(); newVal != old {
		m.reflowTextarea()
		if strings.HasPrefix(newVal, "/") {
			if !m.completions.IsVisible() {
				m.completions.Show()
			}
			m.updateCompletions()
		} else if m.completions.IsVisible() {
			m.completions.Hide()
		}
	}
	m.updateTextareaHeight()
	return m, nil
}

func shouldCollapsePaste(text string) bool {
	return len(text) > pasteCollapseThreshold && strings.Contains(text, "\n")
}

// pastePlaceholder returns the inline placeholder text for a collapsed paste.
func pastePlaceholder(id int, text string) string {
	lines := strings.Count(text, "\n") + 1
	if lines > 1 {
		return fmt.Sprintf("[Pasted text #%d +%d lines]", id, lines)
	}
	return fmt.Sprintf("[Pasted text #%d +%d chars]", id, len(text))
}

func (m *Model) expandPastePlaceholderAtCursor() bool {
	if m == nil || len(m.pasteChunks) == 0 {
		return false
	}
	value := m.textarea.Value()
	if value == "" {
		return false
	}
	cursor := textareaCursorByteOffset(value, m.textarea.Line(), m.textarea.Column())
	id, text, start, end, ok := m.pastePlaceholderAtCursor(value, cursor)
	if !ok {
		return false
	}
	value = value[:start] + text + value[end:]
	delete(m.pasteChunks, id)
	m.setTextareaValue(value)
	m.moveTextareaCursorToByteOffset(start + len(text))
	return true
}

func (m *Model) pastePlaceholderAtCursor(value string, cursor int) (id int, text string, start int, end int, ok bool) {
	searchFrom := 0
	for searchFrom <= len(value) {
		bestID := 0
		bestText := ""
		bestStart := -1
		bestEnd := -1
		for candidateID, candidateText := range m.pasteChunks {
			placeholder := pastePlaceholder(candidateID, candidateText)
			idx := strings.Index(value[searchFrom:], placeholder)
			if idx < 0 {
				continue
			}
			candidateStart := searchFrom + idx
			candidateEnd := candidateStart + len(placeholder)
			if bestStart == -1 || candidateStart < bestStart || (candidateStart == bestStart && candidateID < bestID) {
				bestID = candidateID
				bestText = candidateText
				bestStart = candidateStart
				bestEnd = candidateEnd
			}
		}
		if bestStart == -1 {
			return 0, "", 0, 0, false
		}
		if cursor >= bestStart && cursor <= bestEnd {
			return bestID, bestText, bestStart, bestEnd, true
		}
		if cursor < bestStart {
			return 0, "", 0, 0, false
		}
		searchFrom = bestEnd
	}
	return 0, "", 0, 0, false
}

func textareaCursorByteOffset(value string, line, column int) int {
	if line < 0 {
		return 0
	}
	lines := strings.Split(value, "\n")
	if line >= len(lines) {
		return len(value)
	}
	offset := 0
	for i := 0; i < line; i++ {
		offset += len(lines[i]) + 1
	}
	return offset + byteOffsetForRuneColumn(lines[line], column)
}

func byteOffsetForRuneColumn(s string, column int) int {
	if column <= 0 {
		return 0
	}
	seen := 0
	for i := range s {
		if seen == column {
			return i
		}
		seen++
	}
	return len(s)
}

func (m *Model) moveTextareaCursorToByteOffset(offset int) {
	if offset < 0 {
		offset = 0
	}
	value := m.textarea.Value()
	if offset > len(value) {
		offset = len(value)
	}
	before := value[:offset]
	line := strings.Count(before, "\n")
	lastNewline := strings.LastIndex(before, "\n")
	lineStart := 0
	if lastNewline >= 0 {
		lineStart = lastNewline + 1
	}
	column := len([]rune(before[lineStart:]))

	m.textarea.MoveToBegin()
	for i := 0; i < line; i++ {
		m.textarea.CursorDown()
	}
	m.textarea.SetCursorColumn(column)
}

// expandPastePlaceholders replaces all inline paste placeholders with their
// actual content. Clears the pasteChunks map after expansion.
func (m *Model) expandPastePlaceholders(content string) string {
	if len(m.pasteChunks) == 0 {
		return content
	}
	for id, text := range m.pasteChunks {
		placeholder := pastePlaceholder(id, text)
		content = strings.ReplaceAll(content, placeholder, text)
	}
	m.pasteChunks = nil
	return content
}

func (m *Model) currentInterruptActivity() llm.InterruptActivity {
	activity := llm.InterruptActivity{
		CurrentTask: m.phase,
		ProseLen:    m.currentResponse.Len(),
	}
	if m.tracker != nil && m.tracker.HasPending() {
		activity.ActiveTool = "tool"
	}
	return activity
}

func (m *Model) nextPendingInterjectionID() string {
	m.interjectionSeq++
	return fmt.Sprintf("tui-interject-%d", m.interjectionSeq)
}

func (m *Model) syncLatestPendingInterjection() {
	if len(m.pendingInterjections) == 0 {
		m.pendingInterjectionID = ""
		m.pendingInterjection = ""
		m.pendingInterruptUI = ""
		m.selectedInterjection = -1
		return
	}
	if m.selectedInterjection >= len(m.pendingInterjections) {
		m.selectedInterjection = len(m.pendingInterjections) - 1
	}
	latest := m.pendingInterjections[len(m.pendingInterjections)-1]
	m.pendingInterjectionID = latest.ID
	m.pendingInterjection = latest.Text
	m.pendingInterruptUI = latest.UIState
}

func (m *Model) setPendingInterjection(interjectionID, content, uiState string) {
	for i := range m.pendingInterjections {
		if m.pendingInterjections[i].ID == interjectionID {
			m.pendingInterjections[i].Text = content
			m.pendingInterjections[i].UIState = uiState
			if m.selectedInterjection < 0 {
				m.selectedInterjection = i
			}
			m.syncLatestPendingInterjection()
			return
		}
	}
	m.pendingInterjections = append(m.pendingInterjections, pendingInterjectionUI{ID: interjectionID, Text: content, UIState: uiState})
	m.selectedInterjection = len(m.pendingInterjections) - 1
	m.syncLatestPendingInterjection()
}

func (m *Model) removePendingInterjectionByID(interjectionID string) bool {
	for i := range m.pendingInterjections {
		if m.pendingInterjections[i].ID == interjectionID {
			copy(m.pendingInterjections[i:], m.pendingInterjections[i+1:])
			m.pendingInterjections = m.pendingInterjections[:len(m.pendingInterjections)-1]
			if m.selectedInterjection == i {
				m.selectedInterjection = i
			}
			m.syncLatestPendingInterjection()
			return true
		}
	}
	return false
}

func (m *Model) clearPendingInterjection() {
	m.pendingInterjections = nil
	m.pendingInterjectionID = ""
	m.pendingInterjection = ""
	m.pendingInterruptUI = ""
	m.selectedInterjection = -1
}

func (m *Model) clearPendingInterjectionState() {
	m.activeInterruptSeq = 0
	m.clearPendingInterjection()
}

func (m *Model) cancelSelectedPendingInterjection() bool {
	if len(m.pendingInterjections) == 0 {
		return false
	}
	idx := m.selectedInterjection
	if idx < 0 || idx >= len(m.pendingInterjections) {
		idx = len(m.pendingInterjections) - 1
	}
	entry := m.pendingInterjections[idx]
	if entry.UIState == "interject" {
		if !m.engine.CancelInterjection(entry.ID) {
			return false
		}
	} else if entry.UIState == "deciding" && entry.ID == m.pendingInterjectionID {
		m.activeInterruptSeq = 0
	}
	copy(m.pendingInterjections[idx:], m.pendingInterjections[idx+1:])
	m.pendingInterjections = m.pendingInterjections[:len(m.pendingInterjections)-1]
	m.selectedInterjection = idx
	m.syncLatestPendingInterjection()
	return true
}

func (m *Model) queueInterruptClassification(interjectionID, content string, parts []llm.Part) tea.Cmd {
	m.interruptRequestSeq++
	requestID := m.interruptRequestSeq
	activity := m.currentInterruptActivity()

	m.activeInterruptSeq = requestID
	m.setPendingInterjection(interjectionID, content, "deciding")
	m.interruptNotice = ""
	m.setTextareaValue("")

	provider := m.fastProvider
	classifyText := content
	if classifyText == "" && len(parts) > 0 {
		classifyText = llm.MessageAttachmentSummary(llm.Message{Role: llm.RoleUser, Parts: parts})
	}
	return func() tea.Msg {
		action := llm.ClassifyInterrupt(context.Background(), provider, classifyText, activity)
		return interruptClassifiedMsg{
			RequestID:      requestID,
			InterjectionID: interjectionID,
			Content:        content,
			Parts:          parts,
			Action:         action,
		}
	}
}

func (m *Model) applyInterruptAction(interjectionID, content string, action llm.InterruptAction) {
	m.applyInterruptActionWithParts(interjectionID, content, nil, action)
}

func (m *Model) applyInterruptActionWithParts(interjectionID, content string, parts []llm.Part, action llm.InterruptAction) {
	m.activeInterruptSeq = 0
	m.setTextareaValue("")

	summary := content
	if strings.TrimSpace(summary) == "" && len(parts) > 0 {
		summary = llm.MessageAttachmentSummary(llm.Message{Role: llm.RoleUser, Parts: parts})
	}

	switch action {
	case llm.InterruptCancel:
		m.clearPendingInterjection()
		m.interruptNotice = "✕ cancelled current response — draft restored below"
		m.phase = "Stopping..."
		m.setTextareaValue(content)
		// Restore image attachments when classification chose to cancel the run.
		// The normal text draft is restored above; queued-interjection cancellation does not restore drafts.
		if len(parts) > 0 {
			// Parts already came from current composer images, so leave m.images as-is if still present.
		}
		if m.streamCancelFunc != nil {
			m.setStreamCancelRequested(true)
			m.streamCancelFunc()
		}
	case llm.InterruptInterject:
		m.setPendingInterjection(interjectionID, summary, "interject")
		msg := llm.Message{Role: llm.RoleUser, Parts: append([]llm.Part(nil), parts...)}
		if content != "" {
			msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartText, Text: content})
		}
		if len(msg.Parts) == 0 {
			msg = llm.UserText(content)
		}
		m.engine.QueueInterjection(llm.QueuedInterjection{ID: interjectionID, Message: msg, DisplayText: summary})
		m.images = nil
		m.selectedImage = -1
	}
}

func (m *Model) handleInterruptClassified(msg interruptClassifiedMsg) (tea.Model, tea.Cmd) {
	if msg.RequestID == 0 || msg.RequestID != m.activeInterruptSeq {
		return m, nil
	}
	if !m.streaming {
		m.clearPendingInterjectionState()
		if strings.TrimSpace(m.textarea.Value()) == "" {
			m.setTextareaValue(msg.Content)
		}
		return m, nil
	}

	m.applyInterruptActionWithParts(msg.InterjectionID, msg.Content, msg.Parts, msg.Action)
	return m, nil
}

func (m *Model) restorePendingInterjectionDraft() {
	if strings.TrimSpace(m.textarea.Value()) != "" {
		return
	}
	if entries := m.engine.DrainInterjections(); len(entries) > 0 {
		var textParts []string
		var images []ImageAttachment
		for _, entry := range entries {
			for _, part := range entry.Message.Parts {
				switch part.Type {
				case llm.PartText:
					if strings.TrimSpace(part.Text) != "" {
						textParts = append(textParts, part.Text)
					}
				case llm.PartImage:
					if part.ImageData != nil && part.ImageData.Base64 != "" {
						data, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
						if err == nil {
							images = append(images, ImageAttachment{MediaType: part.ImageData.MediaType, Data: data})
						}
					}
				}
			}
			if len(entry.Message.Parts) == 0 && strings.TrimSpace(entry.DisplayText) != "" {
				textParts = append(textParts, entry.DisplayText)
			}
		}
		m.images = append(m.images, images...)
		if len(m.images) == 0 {
			m.selectedImage = -1
		}
		m.setTextareaValue(strings.Join(textParts, "\n"))
		return
	}
	if m.pendingInterjection != "" && (m.pendingInterruptUI == "interject" || m.pendingInterruptUI == "deciding") {
		m.setTextareaValue(m.pendingInterjection)
	}
}

func (m *Model) handleSlashCommand(input string) (tea.Model, tea.Cmd) {
	return m.ExecuteCommand(input)
}
