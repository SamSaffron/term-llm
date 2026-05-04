package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func copyLLMMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	copied := make([]llm.Message, len(messages))
	copy(copied, messages)
	return copied
}

func (m *Model) setStreamingContextMessages(messages []llm.Message) {
	m.contextEstimateMu.Lock()
	m.streamingContextMessages = copyLLMMessages(messages)
	m.streamingContextPendingAssistant = false
	m.contextEstimateMu.Unlock()
}

func (m *Model) clearStreamingContextMessages() {
	m.contextEstimateMu.Lock()
	m.streamingContextMessages = nil
	m.streamingContextPendingAssistant = false
	m.contextEstimateMu.Unlock()
}

func (m *Model) updateStreamingContextAssistant(assistantMsg llm.Message) {
	m.contextEstimateMu.Lock()
	defer m.contextEstimateMu.Unlock()
	if m.streamingContextPendingAssistant && len(m.streamingContextMessages) > 0 {
		m.streamingContextMessages[len(m.streamingContextMessages)-1] = assistantMsg
		return
	}
	m.streamingContextMessages = append(m.streamingContextMessages, assistantMsg)
	m.streamingContextPendingAssistant = true
}

func (m *Model) appendStreamingContextTurnMessages(turnMessages []llm.Message) {
	m.contextEstimateMu.Lock()
	defer m.contextEstimateMu.Unlock()

	appendStart := 0
	if len(turnMessages) > 0 && turnMessages[0].Role == llm.RoleAssistant {
		if m.streamingContextPendingAssistant && len(m.streamingContextMessages) > 0 {
			m.streamingContextMessages[len(m.streamingContextMessages)-1] = turnMessages[0]
		} else {
			m.streamingContextMessages = append(m.streamingContextMessages, turnMessages[0])
		}
		appendStart = 1
	}
	if appendStart < len(turnMessages) {
		m.streamingContextMessages = append(m.streamingContextMessages, turnMessages[appendStart:]...)
	}
	m.streamingContextPendingAssistant = false
}

// clearStreamCallbacks detaches every engine callback wired in startStream
// and resets the per-turn "persist as we go" state. Safe to call even when
// streaming never started; safe to call from any goroutine.
func (m *Model) clearStreamCallbacks() {
	m.engine.SetAssistantSnapshotCallback(nil)
	m.engine.SetResponseCompletedCallback(nil)
	m.engine.SetTurnCompletedCallback(nil)
	m.pendingMu.Lock()
	m.pendingAssistantMsgID = 0
	m.pendingMu.Unlock()
	m.clearStreamingContextMessages()
}

// setupStreamPersistenceCallbacks wires snapshot/response/turn callbacks on the
// engine so assistant messages and tool results persist incrementally as the
// turn progresses. The snapshot and response callbacks upsert the same pending
// row so a mid-stream crash still leaves something on disk. The turn callback
// skips RoleUser messages (interjections) because the ui.StreamEventInterjection
// handler persists those — appending them here would create duplicate rows.
func (m *Model) setupStreamPersistenceCallbacks(streamStart time.Time) {
	persistPendingAssistant := func(ctx context.Context, assistantMsg llm.Message) {
		if m.store == nil || m.sess == nil {
			return
		}
		sessionMsg := session.NewMessage(m.sess.ID, assistantMsg, -1)
		sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
		m.pendingMu.Lock()
		defer m.pendingMu.Unlock()
		if m.pendingAssistantMsgID != 0 {
			sessionMsg.ID = m.pendingAssistantMsgID
			err := m.store.UpdateMessage(ctx, m.sess.ID, sessionMsg)
			if err == nil {
				return
			}
			if !errors.Is(err, session.ErrNotFound) {
				return
			}
			m.pendingAssistantMsgID = 0
			sessionMsg = session.NewMessage(m.sess.ID, assistantMsg, -1)
			sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
		}
		if err := m.store.AddMessage(ctx, m.sess.ID, sessionMsg); err != nil {
			return
		}
		m.pendingAssistantMsgID = sessionMsg.ID
	}

	m.engine.SetAssistantSnapshotCallback(func(ctx context.Context, _ int, assistantMsg llm.Message) error {
		m.updateStreamingContextAssistant(assistantMsg)
		persistPendingAssistant(ctx, assistantMsg)
		return nil
	})

	m.engine.SetResponseCompletedCallback(func(ctx context.Context, _ int, assistantMsg llm.Message, _ llm.TurnMetrics) error {
		m.updateStreamingContextAssistant(assistantMsg)
		persistPendingAssistant(ctx, assistantMsg)
		return nil
	})

	m.engine.SetTurnCompletedCallback(func(ctx context.Context, _ int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
		m.appendStreamingContextTurnMessages(turnMessages)

		appendStart := 0
		if len(turnMessages) > 0 && turnMessages[0].Role == llm.RoleAssistant {
			// The estimate snapshot was already updated above. Persist/update the
			// pending assistant row separately.
			persistPendingAssistant(ctx, turnMessages[0])
			appendStart = 1
		}
		if m.store != nil && m.sess != nil {
			for _, msg := range turnMessages[appendStart:] {
				if msg.Role == llm.RoleUser {
					continue
				}
				sessionMsg := session.NewMessage(m.sess.ID, msg, -1)
				_ = m.store.AddMessage(ctx, m.sess.ID, sessionMsg)
			}
		}
		m.pendingMu.Lock()
		m.pendingAssistantMsgID = 0
		m.pendingMu.Unlock()
		if m.store != nil && m.sess != nil {
			_ = m.store.UpdateMetrics(ctx, m.sess.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens, metrics.CacheWriteTokens)
			m.persistContextEstimate(ctx)
		}
		return nil
	})
}

func (m *Model) shouldInjectPlatformDeveloperMessage() bool {
	if strings.TrimSpace(m.platformDeveloperMessage) == "" {
		return false
	}

	hasUserMsg := false
	for _, msg := range m.messages {
		if msg.Role == llm.RoleUser {
			hasUserMsg = true
			break
		}
	}
	if !hasUserMsg {
		return true
	}

	if m.sess == nil {
		return false
	}
	return m.sess.Origin != m.currentOrigin
}

func (m *Model) prependMessage(msg session.Message) {
	m.messages = append([]session.Message{msg}, m.messages...)
	m.invalidateHistoryCache()
}

func (m *Model) insertDeveloperMessage(msg session.Message) {
	insertAt := 0
	for insertAt < len(m.messages) && m.messages[insertAt].Role == llm.RoleSystem {
		insertAt++
	}
	m.messages = append(m.messages[:insertAt], append([]session.Message{msg}, m.messages[insertAt:]...)...)
	m.invalidateHistoryCache()
}

func (m *Model) ensureContextMessages() {
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
			Sequence:    -1,
		}
		if m.store != nil {
			_ = m.store.AddMessage(context.Background(), m.sess.ID, sysMsg)
		}
		m.prependMessage(*sysMsg)
	}

	if !m.shouldInjectPlatformDeveloperMessage() {
		return
	}

	devText := strings.TrimSpace(m.platformDeveloperMessage)
	devMsg := &session.Message{
		SessionID:   m.sess.ID,
		Role:        llm.RoleDeveloper,
		Parts:       []llm.Part{{Type: llm.PartText, Text: devText}},
		TextContent: devText,
		CreatedAt:   time.Now(),
		Sequence:    -1,
	}
	if m.store != nil {
		_ = m.store.AddMessage(context.Background(), m.sess.ID, devMsg)
	}
	m.insertDeveloperMessage(*devMsg)

	if m.sess != nil {
		m.sess.Origin = m.currentOrigin
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
}

func (m *Model) sendMessage(content string) (tea.Model, tea.Cmd) {
	m.selection = Selection{}
	m.interruptNotice = ""
	m.clearFooterMessage()
	m.recordCurrentModelUse()

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

	// Ensure system/platform context messages exist before the user turn.
	m.ensureContextMessages()

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
	m.invalidateHistoryCache()
	if m.store != nil {
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
	m.pasteChunks = nil

	// Start streaming
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now()
	if m.altScreen {
		m.scrollToBottom = true
	}
	if m.streamPerf != nil && m.sess != nil {
		m.streamPerf.StartTurn(m.sess.ID, m.streamStartTime)
	}
	m.currentResponse.Reset()
	m.err = nil // Clear any previous error
	m.webSearchUsed = false
	m.viewCache.completedStream = "" // Clear previous response's diffs/tools
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
	m.smoothTickPending = false
	m.deferredStreamRead = false
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
		messages := m.buildMessagesForStream()
		m.setStreamingContextMessages(messages)

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
			SessionID:               m.sess.ID,
			Messages:                messages,
			Tools:                   reqTools,
			Search:                  m.searchEnabled,
			ForceExternalSearch:     m.forceExternalSearch,
			DisableExternalWebFetch: m.disableExternalWebFetch,
			ParallelToolCalls:       true,
			MaxTurns:                m.maxTurns,
		}

		// Set up callbacks for incremental message saving (sequence auto-allocated)
		m.setupStreamPersistenceCallbacks(m.streamStartTime)

		// Enable context compaction or tracking for models with known input limits.
		// Re-set each turn in case the provider/model changed mid-session.
		m.configureContextManagementForSession()

		// Set up compaction callback to update in-memory state and persist.
		// This runs on the engine goroutine, so we protect m.messages with a mutex.
		m.engine.SetCompactionCallback(func(ctx context.Context, result *llm.CompactionResult) error {
			var newSessionMsgs []session.Message
			for _, msg := range result.NewMessages {
				newSessionMsgs = append(newSessionMsgs, *session.NewMessage(m.sess.ID, msg, -1))
			}
			if m.store != nil {
				if err := m.store.CompactMessages(ctx, m.sess.ID, newSessionMsgs); err != nil {
					return err
				}
			}
			m.messagesMu.Lock()
			m.compactionIdx = len(m.messages)
			m.messages = append(m.messages, newSessionMsgs...)
			m.messagesMu.Unlock()
			m.invalidateHistoryCache()
			// Any pending assistant row that snapshot had upserted is now stale:
			// compaction rewrote the message table. Clear the tracking so the
			// next snapshot/response inserts fresh instead of trying to update
			// a row that no longer exists.
			m.pendingMu.Lock()
			m.pendingAssistantMsgID = 0
			m.pendingMu.Unlock()
			return nil
		})

		// Start streaming in background - adapter handles all event conversion
		m.streamDone = make(chan struct{})
		go func() {
			defer close(m.streamDone)
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
	compIdx := m.compactionIdx
	m.messagesMu.Unlock()

	// If there's a compaction boundary, only send post-compaction messages to the LLM.
	// The older messages are kept in m.messages for scrollback display only.
	if compIdx > 0 && compIdx < len(snapshot) {
		snapshot = snapshot[compIdx:]
	}

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

	return llm.FilterConversationMessages(messages)
}

func (m *Model) buildMessagesForStream() []llm.Message {
	return m.buildMessages()
}

func (m *Model) buildMessagesForContextEstimate() []llm.Message {
	m.contextEstimateMu.Lock()
	if m.streaming && len(m.streamingContextMessages) > 0 {
		messages := copyLLMMessages(m.streamingContextMessages)
		m.contextEstimateMu.Unlock()
		return messages
	}
	m.contextEstimateMu.Unlock()
	return m.buildMessages()
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

// maybeRenameHandoverCmd returns a tea.Cmd that checks the handover directory
// for a random-named file large enough to rename. If found, it uses the fast
// provider to generate a descriptive slug, renames the file, and creates a
// symlink from the old name so the system prompt path remains valid.
func (m *Model) maybeRenameHandoverCmd() tea.Cmd {
	if m.currentAgent == nil || !m.currentAgent.EnableHandover {
		return nil
	}
	provider := m.fastProvider
	if provider == nil {
		return nil
	}
	return func() tea.Msg {
		dir, err := session.GetHandoverDir(".")
		if err != nil {
			return handoverRenameDoneMsg{err: err}
		}
		path, _ := findLatestHandoverFile(dir)
		if path == "" {
			return handoverRenameDoneMsg{}
		}
		slugGen := func(ctx context.Context, content string) (string, error) {
			// Truncate content to first 2000 chars to keep the request small
			if len(content) > 2000 {
				content = content[:2000]
			}
			prompt := fmt.Sprintf("Generate a short filesystem-safe slug (2-5 words, lowercase, dash-separated) that describes this document. Reply with ONLY the slug, nothing else.\n\n%s", content)
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			stream, err := provider.Stream(ctx, llm.Request{
				Messages: []llm.Message{
					llm.UserText(prompt),
				},
				MaxTurns: 1,
			})
			if err != nil {
				return "", err
			}
			defer stream.Close()
			var b strings.Builder
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return "", err
				}
				if ev.Type == llm.EventTextDelta {
					b.WriteString(ev.Text)
				}
			}
			return strings.TrimSpace(b.String()), nil
		}
		err = session.MaybeRenameHandover(context.Background(), path, slugGen)
		return handoverRenameDoneMsg{err: err}
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
	m.resetAltScreenStreamingAppendCache()
	if m.chatRenderer != nil {
		m.chatRenderer.InvalidateCache()
	}
	m.bumpContentVersion()
}

func (m *Model) invalidateHistoryCache() {
	m.viewCache.historyValid = false
	m.resetAltScreenStreamingAppendCache()
	if m.chatRenderer != nil {
		m.chatRenderer.InvalidateCache()
	}
	m.bumpContentVersion()
}

func (m *Model) resetAltScreenStreamingAppendCache() {
	m.viewCache.lastStreamingContent = ""
	m.viewCache.lastContentHistoryPlusStream = false
	m.viewCache.lastContentStr = ""
	m.contentLines = nil
}

func (m *Model) bumpContentVersion() {
	m.viewCache.contentVersion++
	if m.streamPerf != nil {
		m.streamPerf.RecordContentVersionBump()
	}
}
