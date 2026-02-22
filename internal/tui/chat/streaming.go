package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) sendMessage(content string) (tea.Model, tea.Cmd) {
	// Build the full message content including file attachments
	fullContent := content
	var fileNames []string

	if len(m.files) > 0 {
		var filesContent strings.Builder
		filesContent.WriteString("\n\n---\n**Attached files:**\n")
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
			filesContent.WriteString(fmt.Sprintf("\n### %s\n```\n%s\n```\n", f.Name, f.Content))
		}
		fullContent += filesContent.String()
	}

	imageLabels := m.imageAttachmentLabels()
	parts := m.imagePartList()
	if fullContent != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: fullContent})
	} else if len(parts) == 0 {
		parts = []llm.Part{{Type: llm.PartText, Text: fullContent}}
	}

	displayText := fullContent
	if strings.TrimSpace(displayText) == "" && len(imageLabels) > 0 {
		displayText = "[" + strings.Join(imageLabels, ", ") + "]"
	}

	// Create user message and store it
	userMsg := &session.Message{
		SessionID:   m.sess.ID,
		Role:        llm.RoleUser,
		Parts:       parts,
		TextContent: displayText,
		CreatedAt:   time.Now(),
		Sequence:    -1, // Auto-allocate sequence
	}
	m.messages = append(m.messages, *userMsg)
	if m.store != nil {
		// Save system message first if this is a new session with instructions
		// Check if there's no existing system message in history
		hasSystemMsg := false
		for _, msg := range m.messages {
			if msg.Role == llm.RoleSystem {
				hasSystemMsg = true
				break
			}
		}
		if m.config.Chat.Instructions != "" && !hasSystemMsg {
			sysMsg := &session.Message{
				SessionID:   m.sess.ID,
				Role:        llm.RoleSystem,
				Parts:       []llm.Part{{Type: llm.PartText, Text: m.config.Chat.Instructions}},
				TextContent: m.config.Chat.Instructions,
				CreatedAt:   time.Now(),
				Sequence:    -1, // Auto-allocate sequence
			}
			_ = m.store.AddMessage(context.Background(), m.sess.ID, sysMsg)
			// Prepend to m.messages so we don't save it again on subsequent sends
			m.messages = append([]session.Message{*sysMsg}, m.messages...)
		}

		_ = m.store.AddMessage(context.Background(), m.sess.ID, userMsg)
		// Sync the assigned ID back to the copy in m.messages to avoid cache collisions
		// (AddMessage sets userMsg.ID, but the copy was made before that)
		m.messages[len(m.messages)-1].ID = userMsg.ID
		_ = m.store.IncrementUserTurns(context.Background(), m.sess.ID)
		m.sess.UserTurns++ // Keep in-memory value in sync
		// Update session summary from first user message
		if m.sess.Summary == "" {
			m.sess.Summary = session.TruncateSummary(content)
			_ = m.store.Update(context.Background(), m.sess)
		}
	}

	// Print user message permanently to scrollback (inline mode)
	theme := m.styles.Theme()
	promptStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	prompt := promptStyle.Render("❯") + " "
	promptWidth := lipgloss.Width(prompt)

	// Wrap content to fit terminal width minus prompt
	wrapWidth := m.width - promptWidth
	if wrapWidth < 20 {
		wrapWidth = 20
	}
	wrappedContent := wordwrap.String(content, wrapWidth)

	// Add prompt to first line, indent continuation lines
	lines := strings.Split(wrappedContent, "\n")
	var userDisplay strings.Builder
	for i, line := range lines {
		if i == 0 {
			userDisplay.WriteString(prompt)
		} else {
			userDisplay.WriteString("\n  ") // 2-space indent for continuation
		}
		userDisplay.WriteString(line)
	}
	var attachmentNames []string
	attachmentNames = append(attachmentNames, imageLabels...)
	attachmentNames = append(attachmentNames, fileNames...)
	if len(attachmentNames) > 0 {
		userDisplay.WriteString("\n")
		userDisplay.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render(
			fmt.Sprintf("[with: %s]", strings.Join(attachmentNames, ", "))))
	}
	// tea.Println adds newline, no need for extra

	// Clear input and attachments
	m.setTextareaValue("")
	m.files = nil
	m.images = nil
	m.selectedImage = -1

	// Start streaming
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now()
	if m.streamPerf != nil && m.sess != nil {
		m.streamPerf.StartTurn(m.sess.ID, m.streamStartTime)
	}
	m.currentResponse.Reset()
	m.err = nil // Clear any previous error
	m.webSearchUsed = false
	m.viewCache.completedStream = "" // Clear previous response's diffs/tools
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
	m.smoothTickPending = false
	m.streamRenderTickPending = false

	// Start the stream
	// In alt screen mode, View() renders history including user message
	// In inline mode, print user message to scrollback first
	if m.altScreen {
		return m, tea.Batch(
			m.startStream(fullContent),
			m.spinner.Tick,
			m.tickEvery(),
		)
	}
	return m, tea.Batch(
		tea.Println(userDisplay.String()),
		m.startStream(fullContent),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

func (m *Model) startStream(content string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		m.streamCancelFunc = cancel

		// Mark session as active when starting a new stream
		if m.store != nil && m.sess != nil {
			_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusActive)
		}

		// Create stream adapter for unified event handling with proper buffering
		adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
		m.streamChan = adapter.Events()

		// Build messages from conversation history
		messages := m.buildMessages()

		// Collect MCP tools if available and register them with the engine
		var reqTools []llm.ToolSpec
		if m.mcpManager != nil {
			mcpTools := m.mcpManager.AllTools()
			for _, t := range mcpTools {
				reqTools = append(reqTools, llm.ToolSpec{
					Name:        t.Name,
					Description: t.Description,
					Schema:      t.Schema,
				})
				// Register MCP tool with engine for execution
				m.engine.RegisterTool(mcp.NewMCPTool(m.mcpManager, t))
			}
		}

		// Add local tools (read_file, write_file, shell, etc.) if enabled
		// These are already registered in the engine, we just need their specs
		if len(m.localTools) > 0 {
			for _, specName := range m.localTools {
				if tool, ok := m.engine.Tools().Get(specName); ok {
					reqTools = append(reqTools, tool.Spec())
				}
			}
		}

		// Add any engine-registered tools not covered by localTools.
		// activate_skill is registered directly on the engine by RegisterSkillToolWithEngine
		// but is not part of the agent's tools.enabled list, so it would be silently dropped.
		for _, spec := range m.engine.Tools().AllSpecs() {
			found := false
			for _, existing := range reqTools {
				if existing.Name == spec.Name {
					found = true
					break
				}
			}
			if !found {
				reqTools = append(reqTools, spec)
			}
		}

		req := llm.Request{
			SessionID:           m.sess.ID,
			Messages:            messages,
			Tools:               reqTools,
			Search:              m.searchEnabled,
			ForceExternalSearch: m.forceExternalSearch,
			ParallelToolCalls:   true,
			MaxTurns:            m.maxTurns,
		}

		// Set up callbacks for incremental message saving (sequence auto-allocated)
		// Capture streamStartTime for duration calculation
		streamStart := m.streamStartTime
		if m.store != nil && m.sess != nil {
			// Response callback saves assistant message immediately (before tool execution)
			// This ensures the message is persisted even if tool execution fails/crashes
			m.engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
				// Calculate duration from stream start
				durationMs := time.Since(streamStart).Milliseconds()

				sessionMsg := session.NewMessage(m.sess.ID, assistantMsg, -1)
				sessionMsg.DurationMs = durationMs
				_ = m.store.AddMessage(ctx, m.sess.ID, sessionMsg)
				return nil
			})

			// Turn callback saves messages and updates metrics
			// Note: When tools are used, TurnCompletedCallback receives tool results only (assistant saved by ResponseCompletedCallback)
			// When no tools are used, TurnCompletedCallback receives the assistant message (ResponseCompletedCallback never fires)
			m.engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
				for _, msg := range turnMessages {
					sessionMsg := session.NewMessage(m.sess.ID, msg, -1)
					if msg.Role == llm.RoleAssistant {
						sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
					}
					_ = m.store.AddMessage(ctx, m.sess.ID, sessionMsg)
				}
				_ = m.store.UpdateMetrics(ctx, m.sess.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens)
				return nil
			})
		}

		// Enable context compaction or tracking for models with known input limits.
		// Re-set each turn in case the provider/model changed mid-session.
		m.engine.ConfigureContextManagement(m.provider, m.sess.Provider, m.sess.Model, m.config.AutoCompact)

		// Set up compaction callback to update in-memory state and persist.
		// This runs on the engine goroutine, so we protect m.messages with a mutex.
		m.engine.SetCompactionCallback(func(ctx context.Context, result *llm.CompactionResult) error {
			var newSessionMsgs []session.Message
			for _, msg := range result.NewMessages {
				newSessionMsgs = append(newSessionMsgs, *session.NewMessage(m.sess.ID, msg, -1))
			}
			m.messagesMu.Lock()
			m.messages = newSessionMsgs
			m.messagesMu.Unlock()
			if m.store != nil {
				return m.store.ReplaceMessages(ctx, m.sess.ID, newSessionMsgs)
			}
			return nil
		})

		// Start streaming in background - adapter handles all event conversion
		go func() {
			stream, err := m.engine.Stream(ctx, req)
			if err != nil {
				adapter.EmitErrorAndClose(err)
				return
			}
			defer stream.Close()
			// ProcessStream handles all events and closes the channel when done
			adapter.ProcessStream(ctx, stream)
		}()

		// Return initial listen command
		return m.listenForStreamEventsSync()
	}
}

// listenForStreamEvents returns a command that listens for the next stream event
func (m *Model) listenForStreamEvents() tea.Cmd {
	return func() tea.Msg {
		return m.listenForStreamEventsSync()
	}
}

// listenForStreamEventsSync synchronously waits for the next stream event
func (m *Model) listenForStreamEventsSync() tea.Msg {
	if m.streamChan == nil {
		return streamEventMsg{event: ui.DoneEvent(0)}
	}

	event, ok := <-m.streamChan
	if !ok {
		return streamEventMsg{event: ui.DoneEvent(0)}
	}
	return streamEventMsg{event: event}
}

func (m *Model) buildMessages() []llm.Message {
	m.messagesMu.Lock()
	snapshot := make([]session.Message, len(m.messages))
	copy(snapshot, m.messages)
	m.messagesMu.Unlock()

	var messages []llm.Message

	// Check if history already starts with a system message
	historyHasSystem := len(snapshot) > 0 && snapshot[0].Role == llm.RoleSystem

	// Add system instructions if configured and not already in history
	if m.config.Chat.Instructions != "" && !historyHasSystem {
		messages = append(messages, llm.SystemText(m.config.Chat.Instructions))
	}

	// Add conversation history - convert session messages to llm messages
	for _, msg := range snapshot {
		messages = append(messages, msg.ToLLMMessage())
	}

	return messages
}

func (m *Model) tickEvery() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) saveSessionCmd() tea.Cmd {
	return func() tea.Msg {
		// Sessions are now auto-saved via the store
		// This is kept for compatibility but does nothing
		return sessionSavedMsg{}
	}
}

func (m *Model) invalidateViewCache() {
	m.viewCache.historyValid = false
	m.viewCache.completedStream = ""
	m.viewCache.cachedCompletedContent = ""
	m.viewCache.cachedTrackerVersion = 0
	m.viewCache.lastTrackerVersion = 0
	m.viewCache.lastWavePos = 0
	m.viewCache.lastSetContentAt = time.Time{}
	if m.chatRenderer != nil {
		m.chatRenderer.InvalidateCache()
	}
	m.bumpContentVersion()
}

func (m *Model) bumpContentVersion() {
	m.viewCache.contentVersion++
	if m.streamPerf != nil {
		m.streamPerf.RecordContentVersionBump()
	}
}

