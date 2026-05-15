package chat

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
)

// pasteCollapseThreshold is the minimum length (in characters) of a paste
// before it collapses into a placeholder instead of inline text.
const pasteCollapseThreshold = 100

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

func (m *Model) isYoloModeActive() bool {
	if m.yolo {
		return true
	}
	if m.approvalMgr != nil && m.approvalMgr.YoloEnabled() {
		return true
	}
	if m.handoverApprovalMgr != nil && m.handoverApprovalMgr.YoloEnabled() {
		return true
	}
	return false
}

func isChaosMonkeyKey(msg tea.KeyPressMsg) bool {
	return os.Getenv("TERM_LLM_CHAOS_MONKEY") != "" && key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+m", "ctrl+g")))
}

func (m *Model) setApprovalYoloMode(enabled bool) {
	if m.approvalMgr != nil {
		m.approvalMgr.SetYoloMode(enabled)
	}
	if m.handoverApprovalMgr != nil && m.handoverApprovalMgr != m.approvalMgr {
		m.handoverApprovalMgr.SetYoloMode(enabled)
	}
	if m.mcpManager != nil {
		m.mcpManager.SetSamplingYoloMode(enabled)
	}
}

func (m *Model) toggleYoloMode() (tea.Model, tea.Cmd) {
	m.yolo = !m.yolo
	m.setApprovalYoloMode(m.yolo)

	var cmds []tea.Cmd
	if m.yolo && m.approvalModel != nil && m.approvalDoneCh != nil {
		m.approvalDoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceOnce}
		m.approvalModel = nil
		m.approvalDoneCh = nil
		m.pausedForExternalUI = false
		cmds = append(cmds, m.spinner.Tick)
	}

	message := "Yolo mode disabled. Tool approvals will prompt."
	tone := "muted"
	if m.yolo {
		message = "Yolo mode enabled. Tool approvals will auto-approve."
		tone = "success"
	}
	_, footerCmd := m.showFooterMessageWithTone(message, tone)
	if footerCmd != nil {
		cmds = append(cmds, footerCmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) handleKeyMsg(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if isChaosMonkeyKey(msg) {
		if m.streaming && m.engine != nil {
			m.engine.TriggerChaosFailure()
			return m.showFooterMessageWithTone("Chaos monkey armed: simulating stream failure...", "warning")
		}
		return m.showFooterMessageWithTone("Chaos monkey is enabled; start streaming, then press ctrl+m/ctrl+g to fail the stream.", "muted")
	}

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
			return m, m.spinner.Tick
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
			return m, m.spinner.Tick
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
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	// Bracketed paste and Ctrl+V image attach support for the composer.
	if !m.streaming && m.maybeAttachImageFromPaste(msg) {
		return m, nil
	}

	// When image chips are present, allow keyboard selection/removal with arrows + backspace/delete.
	if !m.streaming && m.handleImageAttachmentKeys(msg) {
		return m, nil
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

		// Other dialogs (SessionList, DirApproval) use standard handling
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
			selected := m.dialog.Selected()
			if selected != nil {
				switch m.dialog.Type() {
				case DialogSessionList:
					m.dialog.Close()
					return m.cmdResume([]string{selected.ID})
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
				// Capture typed input before clearing
				input := m.textarea.Value()
				m.completions.Hide()
				m.setTextareaValue("")

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

	// Handle quit (Ctrl+C copies when text is selected)
	if key.Matches(msg, m.keyMap.Quit) {
		if m.selection.Active {
			cmd := m.copySelectionToClipboard()
			m.selection = Selection{}
			return m, cmd
		}
		if m.streaming && m.streamCancelFunc != nil {
			// Flush buffered text on cancel
			if m.smoothBuffer != nil {
				m.smoothBuffer.FlushAll()
				m.smoothBuffer.Reset()
			}
			m.streamCancelFunc()
			m.streaming = false

			// Clear callbacks and update status
			m.clearStreamCallbacks()
			if m.store != nil {
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusInterrupted)
			}

			// Drain any pending interjection (discard since we're quitting)
			_ = m.engine.DrainInterjection()
			m.clearPendingInterjectionState()

			return m, nil
		}
		m.quitting = true
		// Print stats if enabled
		if m.showStats && m.stats.LLMCallCount > 0 {
			m.stats.Finalize()
			return m, m.quitCmd(tea.Println(m.stats.Render()))
		}
		return m, m.quitCmd()
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
			// Flush buffered text on cancel
			if m.smoothBuffer != nil {
				m.smoothBuffer.FlushAll()
				m.smoothBuffer.Reset()
			}
			m.streamCancelFunc()
			m.streaming = false

			// Clear callbacks and update status
			m.clearStreamCallbacks()
			if m.store != nil {
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusInterrupted)
			}

			// Recover pending interjection text into textarea
			if residual := m.engine.DrainInterjection(); residual != "" {
				m.setTextareaValue(residual)
			}
			m.clearPendingInterjectionState()

			m.textarea.Focus()
			return m, nil
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
			// Collect tool specs for the inspector
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
			if len(m.localTools) > 0 {
				for _, specName := range m.localTools {
					if tool, ok := m.engine.Tools().Get(specName); ok {
						toolSpecs = append(toolSpecs, tool.Spec())
					}
				}
			}

			cfg := &inspector.Config{
				ProviderName: m.providerName,
				ModelName:    m.modelName,
				ToolSpecs:    toolSpecs,
			}
			m.inspectorMode = true
			m.inspectorModel = inspector.NewWithConfig(m.messages, m.width, m.height, m.styles, m.store, cfg)
			return m, nil
		}
		return m, nil
	}

	// Toggle expanded tool display (Ctrl+E) - works even during streaming
	if key.Matches(msg, m.keyMap.ExpandTools) {
		m.toolsExpanded = !m.toolsExpanded
		m.invalidateViewCache()
		if m.chatRenderer != nil {
			m.chatRenderer.SetToolsExpanded(m.toolsExpanded)
		}
		if m.tracker != nil {
			m.tracker.SetExpanded(m.toolsExpanded)
		}
		return m, nil
	}

	// Allow viewport scrolling even while streaming (in alt screen mode)
	if m.altScreen {
		if key.Matches(msg, m.keyMap.PageUp) {
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

	// During streaming: allow typing and interjection (send queues message for next turn)
	if m.streaming {
		if key.Matches(msg, m.keyMap.Send) {
			content := strings.TrimSpace(m.textarea.Value())
			if content == "" {
				m.phase = "Type to interject, or press Esc to cancel"
				return m, nil
			}

			interjectionID := m.nextPendingInterjectionID()
			if action, ok := llm.ClassifyInterruptImmediate(content); ok {
				m.applyInterruptAction(interjectionID, content, action)
				return m, nil
			}
			if m.fastProvider == nil {
				m.applyInterruptAction(interjectionID, content, llm.InterruptInterject)
				return m, nil
			}

			return m, m.queueInterruptClassification(interjectionID, content)
		}
		// Allow textarea to receive input
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		m.updateTextareaHeight()
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

	// Handle new session (Ctrl+N)
	if key.Matches(msg, m.keyMap.NewSession) {
		return m.cmdNew()
	}

	// Handle MCP picker (Ctrl+M)
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
			return m, m.spinner.Tick
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
		if !m.streaming && m.maybeAttachImageFromClipboard() {
			return m, nil
		}
		return m, nil
	}
	text := msg.Content
	if len(text) > pasteCollapseThreshold {
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
		if !m.streaming && strings.HasPrefix(newVal, "/") {
			if !m.completions.IsVisible() {
				m.completions.Show()
			}
			m.updateCompletions()
		} else if !m.streaming && m.completions.IsVisible() {
			m.completions.Hide()
		}
	}
	m.updateTextareaHeight()
	return m, nil
}

// pastePlaceholder returns the inline placeholder text for a collapsed paste.
func pastePlaceholder(id int, text string) string {
	lines := strings.Count(text, "\n") + 1
	if lines > 1 {
		return fmt.Sprintf("[Pasted text #%d +%d lines]", id, lines)
	}
	return fmt.Sprintf("[Pasted text #%d +%d chars]", id, len(text))
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

func (m *Model) setPendingInterjection(interjectionID, content, uiState string) {
	m.pendingInterjectionID = interjectionID
	m.pendingInterjection = content
	m.pendingInterruptUI = uiState
}

func (m *Model) clearPendingInterjection() {
	m.pendingInterjectionID = ""
	m.pendingInterjection = ""
	m.pendingInterruptUI = ""
}

func (m *Model) clearPendingInterjectionState() {
	m.activeInterruptSeq = 0
	m.clearPendingInterjection()
}

func (m *Model) queueInterruptClassification(interjectionID, content string) tea.Cmd {
	m.interruptRequestSeq++
	requestID := m.interruptRequestSeq
	activity := m.currentInterruptActivity()

	m.activeInterruptSeq = requestID
	m.setPendingInterjection(interjectionID, content, "deciding")
	m.interruptNotice = ""
	m.setTextareaValue("")

	provider := m.fastProvider
	return func() tea.Msg {
		action := llm.ClassifyInterrupt(context.Background(), provider, content, activity)
		return interruptClassifiedMsg{
			RequestID:      requestID,
			InterjectionID: interjectionID,
			Content:        content,
			Action:         action,
		}
	}
}

func (m *Model) applyInterruptAction(interjectionID, content string, action llm.InterruptAction) {
	m.activeInterruptSeq = 0
	m.setTextareaValue("")

	switch action {
	case llm.InterruptCancel:
		m.clearPendingInterjection()
		m.interruptNotice = "✕ cancelled current response — draft restored below"
		m.phase = "Stopping..."
		m.setTextareaValue(content)
		if m.streamCancelFunc != nil {
			m.streamCancelFunc()
		}
	case llm.InterruptInterject:
		m.setPendingInterjection(interjectionID, content, "interject")
		m.engine.InterjectWithID(interjectionID, content)
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

	m.applyInterruptAction(msg.InterjectionID, msg.Content, msg.Action)
	return m, nil
}

func (m *Model) restorePendingInterjectionDraft() {
	if strings.TrimSpace(m.textarea.Value()) != "" {
		return
	}
	if residual := m.engine.DrainInterjection(); residual != "" {
		m.setTextareaValue(residual)
		return
	}
	if m.pendingInterjection != "" && (m.pendingInterruptUI == "interject" || m.pendingInterruptUI == "deciding") {
		m.setTextareaValue(m.pendingInterjection)
	}
}

func (m *Model) handleSlashCommand(input string) (tea.Model, tea.Cmd) {
	return m.ExecuteCommand(input)
}
