package chat

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
)

func (m *Model) handleStreamingComposerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keyMap.Send) {
		return m.handleStreamingSend()
	}
	return m.updateStreamingComposerKey(msg)
}

func (m *Model) handleIdleComposerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Handle command palette (Ctrl+P)
	if key.Matches(msg, m.keyMap.Commands) {
		m.setTextareaValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle model picker (Ctrl+L)
	if key.Matches(msg, m.keyMap.SwitchModel) {
		m.hideMentionPopup()
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
		m.hideMentionPopup()
		if m.mcpManager == nil {
			return m.showSystemMessage("MCP not initialized.")
		}
		if len(m.mcpManager.AvailableServers()) == 0 {
			return m.showMCPQuickStart()
		}
		m.showMCPPicker()
		return m, nil
	}

	// Handle clear
	if key.Matches(msg, m.keyMap.Clear) {
		return m.cmdClear()
	}

	// Shell history completion mirrors Claude Code: type a partial command after
	// ! and press Tab to complete it from earlier shell-mode turns.
	if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) && m.directShellComposerActive() {
		if completion, ok := m.directShellHistoryCompletion(m.textarea.Value()); ok {
			m.setDirectShellComposerBody(completion)
			m.textarea.MoveToEnd()
		}
		return m, nil
	}

	// Handle tab completion for /mcp commands
	if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
		return m.handleIdleTabKey()
	}

	// Handle send
	if key.Matches(msg, m.keyMap.Send) {
		return m.handleIdleSend()
	}

	// Handle "/" at start of empty input to show completions
	if !m.directShellComposerActive() && msg.String() == "/" && m.textarea.Value() == "" {
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
	if handled, cmd := m.handleInlineNavigationKey(msg); handled {
		return m, cmd
	}

	// Update textarea for other keys when no response or direct command owns it.
	return m.updateIdleComposerKey(msg)
}

func (m *Model) handleStreamingSend() (tea.Model, tea.Cmd) {
	raw := strings.TrimSpace(m.textarea.Value())
	if m.directShellComposerActive() {
		return m.showFooterWarning("Wait for the current response to finish before running a shell command.")
	}
	if strings.HasSuffix(raw, "\\") {
		m.setTextareaValue(strings.TrimSuffix(raw, "\\") + "\n")
		return m, nil
	}
	if action, ok := llm.ClassifyInterruptImmediate(raw); ok && action == llm.InterruptCancel {
		m.applyInterruptActionWithParts(m.nextPendingSteeringID(), raw, m.imagePartList(), action)
		return m, nil
	}
	if strings.HasPrefix(raw, "/") {
		if updated, cmd, handled := m.queueMainSkillDuringStream(raw); handled {
			m.invalidateAltScreenStreamingViewportCache()
			return updated, tea.Sequence(tea.ClearScreen, cmd)
		}
	}
	if strings.HasPrefix(raw, "/") && m.isStreamingSlashCommand(raw) {
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
	delegationContext, err := m.agentMentionDelegationContext(raw)
	if err != nil {
		m.hideMentionPopup()
		return m.showFooterError(err.Error())
	}
	content := m.expandedPastePlaceholders(raw)
	parts := m.imagePartList()
	if delegationContext != "" {
		parts = append(parts, llm.Part{Type: llm.PartAgentMention, Text: delegationContext})
	}
	if eagerContext, _ := m.eagerMentionContext(content); eagerContext != "" {
		parts = append(parts, llm.Part{Type: llm.PartFile, Text: eagerContext})
	}
	if content == "" && len(parts) == 0 {
		m.phase = "Type to steer, attach an image, or press Esc to cancel"
		return m, nil
	}
	m.pasteChunks = nil

	steeringID := m.nextPendingSteeringID()
	m.applyInterruptActionWithParts(steeringID, content, parts, llm.InterruptSteer)
	return m, nil
}

func (m *Model) updateStreamingComposerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Allow textarea to receive input
	old := m.textarea.Value()
	oldCursor := textareaCursorByteOffset(old, m.textarea.Line(), m.textarea.Column())
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.updateTextareaHeight()
	newVal := m.textarea.Value()
	m.updateDirectShellEligibilityAfterKey(old, newVal)
	newVal = m.textarea.Value()
	newCursor := textareaCursorByteOffset(newVal, m.textarea.Line(), m.textarea.Column())
	if newVal != old {
		m.resetPromptHistoryIfEdited()
		if !m.directShellComposerActive() && strings.HasPrefix(newVal, "/") {
			if !m.completions.IsVisible() {
				m.completions.Show()
			}
			m.updateCompletions()
		} else if m.completions.IsVisible() {
			m.completions.Hide()
		}
	}
	if newVal != old || newCursor != oldCursor {
		return m, tea.Batch(cmd, m.updateMentionQuery())
	}
	return m, cmd
}

func (m *Model) handleIdleTabKey() (tea.Model, tea.Cmd) {
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

	// Tab completion for /mcp login|logout <server>.
	for _, subcommand := range []string{"login", "logout"} {
		prefix := "/mcp " + subcommand + " "
		if strings.HasPrefix(valueLower, prefix) && m.mcpManager != nil {
			partial := strings.TrimSpace(value[len(prefix):])
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue(prefix + match)
				}
			}
			return m, nil
		}
	}

	return m, nil
}

func (m *Model) handleIdleSend() (tea.Model, tea.Cmd) {
	rawInput := m.textarea.Value()
	content := strings.TrimSpace(rawInput)

	if m.directShellRun != nil {
		return m.showFooterWarning("Wait for the shell command to finish or press Esc to cancel it.")
	}
	if m.directShellComposerActive() {
		return m.startDirectShell(rawInput)
	}

	// Check for backslash continuation
	if strings.HasSuffix(content, "\\") {
		// Remove backslash and insert newline
		m.setTextareaValue(strings.TrimSuffix(content, "\\") + "\n")
		return m, nil
	}

	// Check for slash commands. If the leading token isn't a known command or
	// command prefix, treat the text as a normal chat message so pasted absolute
	// paths like /tmp/foo do not trap the composer behind command handling.
	if strings.HasPrefix(content, "/") && m.isSlashCommandLike(content) {
		return m.handleSlashCommand(content)
	}
	if m.branchContextInFlight() && (content != "" || len(m.images) > 0) {
		return m.queueBranchMessage(content)
	}
	// sendMessage expands collapsed paste placeholders only after deriving
	// delegation intent from the deliberately visible composer text.
	if content != "" || len(m.images) > 0 {
		return m.sendMessage(content)
	}
	return m, nil
}

func (m *Model) handleInlineNavigationKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
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
		return true, nil
	}

	if key.Matches(msg, m.keyMap.PageDown) {
		m.scrollOffset -= 5
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
		return true, nil
	}
	return false, nil
}

func (m *Model) updateIdleComposerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	old := m.textarea.Value()
	oldCursor := textareaCursorByteOffset(old, m.textarea.Line(), m.textarea.Column())
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	newVal := m.textarea.Value()
	m.updateDirectShellEligibilityAfterKey(old, newVal)
	newVal = m.textarea.Value()
	newCursor := textareaCursorByteOffset(newVal, m.textarea.Line(), m.textarea.Column())
	// Clear selection when user starts typing
	if m.selection.Active && newVal != old {
		m.selection = Selection{}
	}
	if newVal != old {
		m.resetPromptHistoryIfEdited()
	}
	m.updateTextareaHeight()
	// Show argument completions for commands that support them
	// (e.g., /handover @<partial> triggers agent name completions)
	if newVal != old && !m.directShellComposerActive() && strings.HasPrefix(newVal, "/") && !m.completions.IsVisible() {
		m.updateCompletions()
	}
	if newVal != old || newCursor != oldCursor {
		return m, tea.Batch(cmd, m.updateMentionQuery())
	}
	return m, cmd
}
