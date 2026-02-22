package plan

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Modal overlays consume all input except window resize.
	if m.hasActiveOverlay() {
		if ws, ok := msg.(tea.WindowSizeMsg); ok {
			m.width = ws.Width
			m.height = ws.Height
			m.recalcEditorHeight()
			m.viewport.Width = m.width
			m.viewport.Height = m.height - 2
			m.chatInput.SetWidth(m.width - 4)
			return m, nil
		}
		return m.updateOverlay(msg)
	}

	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalcEditorHeight()
		m.viewport.Width = m.width
		m.viewport.Height = m.height - 2
		m.chatInput.SetWidth(m.width - 4)
		if m.askUserModel != nil {
			m.askUserModel.SetWidth(m.width)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.MouseMsg:
		return m.handleMouseMsg(msg)

	case spinner.TickMsg:
		if m.agentActive {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case ui.WaveTickMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWaveTick(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case ui.WavePauseMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWavePause(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case statusClearMsg:
		m.statusMsg = ""
		return m, nil

	case streamEventsMsg:
		if len(msg.events) == 0 {
			return m, m.listenForStreamEvents()
		}

		done := false
		for _, ev := range msg.events {
			cmds = append(cmds, m.handleStreamEvent(ev)...)
			if ev.Type == ui.StreamEventDone || ev.Type == ui.StreamEventError {
				done = true
				break
			}
		}

		// Continue listening for more events unless stream ended.
		if !done {
			cmds = append(cmds, m.listenForStreamEvents())
		}

	case editorSyncMsg:
		m.editorSyncPending = false
		if cmd := m.flushEditorSync(); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case AskUserRequestMsg:
		// Handle ask_user from the planner agent
		m.askUserDoneCh = msg.DoneCh
		m.askUserModel = tools.NewEmbeddedAskUserModel(msg.Questions, m.width)
		return m, nil

	case SubagentProgressMsg:
		// Handle subagent progress events and update tracker
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, msg.CallID, msg.Event)
		return m, nil

	case pendingPromptMsg:
		// Trigger planner with the pending prompt
		return m.triggerPlannerWithPrompt(msg.prompt)

	case tickMsg:
		if len(m.deferredEditEvents) > 0 && !m.shouldDeferStreamEdits() {
			m.flushDeferredStreamEdits()
		}
		// Periodic tick for elapsed time updates
		if m.agentActive {
			cmds = append(cmds, m.tickEvery())
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleStreamEvent(ev ui.StreamEvent) []tea.Cmd {
	var cmds []tea.Cmd

	if len(m.deferredEditEvents) > 0 && ev.Type != ui.StreamEventDone && !m.shouldDeferStreamEdits() {
		m.flushDeferredStreamEdits()
	}

	switch ev.Type {
	case ui.StreamEventError:
		if ev.Err != nil {
			m.agentActive = false
			m.agentStreaming = false
			m.agentError = ev.Err
			m.setStatus(fmt.Sprintf("Agent error: %v", ev.Err))
		}

	case ui.StreamEventToolStart:
		if m.tracker != nil {
			if m.tracker.HandleToolStart(ev.ToolCallID, ev.ToolName, ev.ToolInfo, ev.ToolArgs) {
				// Don't start wave for ask_user - it has its own UI
				if ev.ToolName != tools.AskUserToolName {
					cmds = append(cmds, m.tracker.StartWave())
				}
			}
		}
		if m.stats != nil {
			m.stats.ToolStart()
		}
		m.agentPhase = fmt.Sprintf("Using %s...", ev.ToolName)

	case ui.StreamEventToolEnd:
		if m.tracker != nil {
			m.tracker.HandleToolEnd(ev.ToolCallID, ev.ToolSuccess)
			if !m.tracker.HasPending() {
				m.agentPhase = "Thinking"
			}
		}
		if m.stats != nil {
			m.stats.ToolEnd()
		}

	case ui.StreamEventUsage:
		if m.stats != nil {
			m.stats.AddUsage(ev.InputTokens, ev.OutputTokens, ev.CachedTokens, ev.WriteTokens)
		}
		m.currentTurn = m.stats.LLMCallCount

	case ui.StreamEventPhase:
		m.agentPhase = ev.Phase

	case ui.StreamEventText:
		// Capture agent reasoning text for activity panel with bounded memory.
		m.appendReasoningDelta(ev.Text)
		if m.agentPhase != "Inserting" && m.agentPhase != "Deleting" {
			m.agentPhase = "Analyzing"
		}

	case ui.StreamEventPartialInsert:
		if m.shouldDeferStreamEdits() {
			m.deferStreamEdit(ev)
			m.agentPhase = "Inserting"
			return cmds
		}
		// Handle streaming partial insert - insert a single line as it arrives
		m.executePartialInsert(ev.InlineAfter, ev.InlineLine)
		if cmd := m.scheduleEditorSync(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.agentPhase = "Inserting"

	case ui.StreamEventInlineInsert:
		if m.shouldDeferStreamEdits() {
			m.deferStreamEdit(ev)
			m.agentPhase = "Inserting"
			return cmds
		}
		// Handle inline INSERT marker (complete) - reset partial tracking
		// The lines have already been inserted via partial inserts
		m.partialInsertIdx = -1
		m.partialInsertAfter = ""
		m.agentPhase = "Inserting"

	case ui.StreamEventInlineDelete:
		if m.shouldDeferStreamEdits() {
			m.deferStreamEdit(ev)
			m.agentPhase = "Deleting"
			return cmds
		}
		// Handle inline DELETE marker - delete lines
		m.executeInlineDelete(ev.InlineFrom, ev.InlineTo, true)
		m.agentPhase = "Deleting"

	case ui.StreamEventDone:
		m.flushDeferredStreamEdits()
		if cmd := m.flushEditorSync(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		m.agentActive = false
		m.agentStreaming = false
		m.agentPhase = ""
		m.tracker = ui.NewToolTracker()             // Reset tracker
		m.subagentTracker = ui.NewSubagentTracker() // Reset subagent tracker
		m.partialInsertIdx = -1                     // Reset partial insert tracking
		m.partialInsertAfter = ""
		m.agentReasoningTail = ""

		// Add turn to history so agent has context for next invocation
		m.addTurnToHistory()

		// Check for pending prompts and auto-trigger next one
		if len(m.pendingPrompts) > 0 {
			prompt := m.pendingPrompts[0]
			m.pendingPrompts = m.pendingPrompts[1:]
			m.setStatus(fmt.Sprintf("Processing queued: %s", truncateResult(prompt, 30)))
			// Return a command to trigger the planner with the pending prompt
			cmds = append(cmds, func() tea.Msg {
				return pendingPromptMsg{prompt: prompt}
			})
		} else {
			m.setStatus("Agent finished")
		}
	}

	return cmds
}

func (m *Model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Ignore mouse events when ask_user UI is active
	if m.askUserModel != nil {
		return m, nil
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		// Scroll up - move cursor up multiple lines
		m.noteEditorInput()
		for i := 0; i < 3; i++ {
			m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
		}
		return m, nil

	case tea.MouseButtonWheelDown:
		// Scroll down - move cursor down multiple lines
		m.noteEditorInput()
		for i := 0; i < 3; i++ {
			m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		return m, nil

	case tea.MouseButtonMiddle:
		if msg.Action == tea.MouseActionPress {
			m.noteEditorInput()
			// Middle-click paste: read from primary selection (Linux) or clipboard (macOS)
			text, err := clipboard.ReadPrimarySelection()
			if err == nil && text != "" {
				// Position cursor at click location first
				m.moveCursorToMouse(msg.X, msg.Y)
				// Insert the text at cursor position
				m.editor.InsertString(text)
			}
			return m, nil
		}

	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress || msg.Action == tea.MouseActionMotion {
			// Determine if click is in chat zone or editor zone
			chatY := m.chatInputY()
			if msg.Y >= chatY && msg.Y < chatY+1 {
				// Click in chat input area — focus chat
				if !m.chatFocused {
					return m.toggleChatFocus()
				}
				return m, nil
			}
			// Click in editor area — focus editor
			if m.chatFocused {
				m.toggleChatFocus()
			}
			m.noteEditorInput()
			m.moveCursorToMouse(msg.X, msg.Y)
			return m, nil
		}
	}

	return m, nil
}

// moveCursorToMouse positions the cursor based on mouse coordinates.
func (m *Model) moveCursorToMouse(mouseX, mouseY int) {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")

	// Account for any padding/margin in the editor (2 chars on left)
	editorX := mouseX - 2
	if editorX < 0 {
		editorX = 0
	}

	// The editor has a viewport, so we need to account for scroll offset
	// mouseY is relative to the screen, we need to find which line that corresponds to
	// The editor's internal line is the visual line + scroll offset
	visibleLine := mouseY // Assuming editor starts at top

	// Get current scroll position from the editor
	// The textarea doesn't expose scroll offset directly, so we calculate based on cursor
	currentLine := m.editor.Line()
	editorHeight := m.editor.Height()

	// Estimate scroll offset: if cursor is visible, scroll offset is approximately
	// currentLine - (position of cursor in viewport)
	// This is an approximation since textarea doesn't expose scroll offset
	scrollOffset := 0
	if currentLine >= editorHeight {
		scrollOffset = currentLine - editorHeight/2
	}

	targetLine := visibleLine + scrollOffset
	if targetLine < 0 {
		targetLine = 0
	}
	if targetLine >= len(lines) {
		targetLine = len(lines) - 1
	}

	// Get target column, clamped to line length
	targetCol := editorX
	if targetLine >= 0 && targetLine < len(lines) {
		lineLen := len(lines[targetLine])
		if targetCol > lineLen {
			targetCol = lineLen
		}
	}

	// Move cursor to target position
	// First, go to document start
	m.editor.SetCursor(0)

	// Navigate to target line
	for i := 0; i < targetLine; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}

	// Go to start of line, then navigate to target column
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	for i := 0; i < targetCol; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
}
