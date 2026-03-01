package chat

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
)

// updateInspectorMode handles updates while in inspector mode
func (m *Model) updateInspectorMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Pass to inspector
		if m.inspectorModel != nil {
			m.inspectorModel, _ = m.inspectorModel.Update(msg)
		}
		return m, nil

	case tea.KeyMsg:
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
		// Only exit alt screen if chat isn't in alt screen mode
		if !m.altScreen {
			return m, tea.ExitAltScreen
		}
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

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
					// Refresh the picker to show updated state (stays open!)
					// Preserve query and cursor position
					query := m.dialog.Query()
					cursor := m.dialog.Cursor()
					m.dialog.ShowMCPPicker(m.mcpManager)
					m.dialog.SetQuery(query)
					m.dialog.SetCursor(cursor)
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
				// Extract any args typed after the command prefix
				// e.g., "/mo son" -> command "model", args "son"
				input := m.textarea.Value()
				args := ""
				if idx := strings.Index(input, " "); idx != -1 {
					args = strings.TrimSpace(input[idx+1:])
				}
				m.completions.Hide()
				m.setTextareaValue("")
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
				m.completions.Hide()
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

	// Handle quit
	if key.Matches(msg, m.keyMap.Quit) {
		if m.streaming && m.streamCancelFunc != nil {
			// Flush buffered text on cancel
			if m.smoothBuffer != nil {
				m.smoothBuffer.FlushAll()
				m.smoothBuffer.Reset()
			}
			m.streamCancelFunc()
			m.streaming = false

			// Clear callbacks and update status
			m.engine.SetResponseCompletedCallback(nil)
			m.engine.SetTurnCompletedCallback(nil)
			if m.store != nil {
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusInterrupted)
			}

			// Drain any pending interjection (discard since we're quitting)
			_ = m.engine.DrainInterjection()
			m.pendingInterjection = "" // Clear visual indicator

			return m, nil
		}
		m.quitting = true
		// Print stats if enabled
		if m.showStats && m.stats.LLMCallCount > 0 {
			m.stats.Finalize()
			return m, tea.Sequence(tea.Println(m.stats.Render()), tea.Quit)
		}
		return m, tea.Quit
	}

	// Handle cancel during streaming
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
			m.engine.SetResponseCompletedCallback(nil)
			m.engine.SetTurnCompletedCallback(nil)
			if m.store != nil {
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusInterrupted)
			}

			// Recover pending interjection text into textarea
			if residual := m.engine.DrainInterjection(); residual != "" {
				m.setTextareaValue(residual)
			}
			m.pendingInterjection = "" // Clear visual indicator

			m.textarea.Focus()
			return m, nil
		}
		// Clear input if not empty
		if m.textarea.Value() != "" {
			m.setTextareaValue("")
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
			// Only enter alt screen if chat isn't already in alt screen mode
			if !m.altScreen {
				return m, tea.EnterAltScreen
			}
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
				m.viewport.LineUp(scrollAmount)
				return m, nil
			}
			if key.Matches(msg, m.keyMap.HistoryDown) {
				m.viewport.LineDown(scrollAmount)
				return m, nil
			}
		}
	}

	// During streaming: allow typing and interjection (send queues message for next turn)
	if m.streaming {
		if key.Matches(msg, m.keyMap.Send) {
			content := strings.TrimSpace(m.textarea.Value())
			if content == "" {
				return m, nil
			}

			activity := llm.InterruptActivity{
				CurrentTask: m.phase,
				ProseLen:    m.currentResponse.Len(),
			}
			if m.tracker != nil && m.tracker.HasPending() {
				activity.ActiveTool = "tool"
			}

			action := llm.ClassifyInterrupt(context.Background(), m.fastProvider, content, activity)
			switch action {
			case llm.InterruptCancel:
				m.pendingInterjection = ""
				m.queuedInterjection = ""
				// Keep typed content so user can resend or edit after cancellation.
				if m.streamCancelFunc != nil {
					m.streamCancelFunc()
				}
			case llm.InterruptInterject:
				// Queue interjection in the engine — it will be injected after the
				// current turn's tool results, before the next LLM turn begins.
				m.engine.Interject(content)
				m.pendingInterjection = content
				m.setTextareaValue("")
			default:
				// Defer as a queued follow-up to run immediately after current stream ends.
				m.queuedInterjection = content
				m.pendingInterjection = content
				m.setTextareaValue("")
			}
			return m, nil
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
		m.dialog.ShowModelPicker(m.modelName, GetAvailableProviders(m.config))
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

	// Handle newline (must be before Send since shift+enter contains enter)
	if key.Matches(msg, m.keyMap.Newline) || key.Matches(msg, m.keyMap.NewlineAlt) {
		m.textarea.InsertString("\n")
		m.updateTextareaHeight()
		return m, nil
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

		// Check for slash commands
		if strings.HasPrefix(content, "/") {
			return m.handleSlashCommand(content)
		}

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
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		m.updateTextareaHeight()
		return m, cmd
	}
	return m, nil
}

func (m *Model) handleSlashCommand(input string) (tea.Model, tea.Cmd) {
	return m.ExecuteCommand(input)
}
