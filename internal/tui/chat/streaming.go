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
	runpkg "github.com/samsaffron/term-llm/internal/run"
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

func (m *Model) invalidateContextEstimateCacheLocked() {
	m.contextEstimateVersion++
	m.contextEstimateCachedVersion = 0
	m.contextEstimateCachedTokens = 0
	m.contextEstimateCachedStreaming = false
	m.contextEstimateCachedValid = false
}

func (m *Model) invalidateContextEstimateCache() {
	m.contextEstimateMu.Lock()
	m.invalidateContextEstimateCacheLocked()
	m.contextEstimateMu.Unlock()
}

func (m *Model) setStreamingContextMessages(messages []llm.Message) {
	m.contextEstimateMu.Lock()
	m.streamingContextMessages = copyLLMMessages(messages)
	m.streamingContextPendingAssistant = false
	m.invalidateContextEstimateCacheLocked()
	m.contextEstimateMu.Unlock()
}

func (m *Model) clearStreamingContextMessages() {
	m.contextEstimateMu.Lock()
	m.streamingContextMessages = nil
	m.streamingContextPendingAssistant = false
	m.invalidateContextEstimateCacheLocked()
	m.contextEstimateMu.Unlock()
}

func (m *Model) updateStreamingContextAssistant(assistantMsg llm.Message) {
	m.contextEstimateMu.Lock()
	defer m.contextEstimateMu.Unlock()
	if m.streamingContextPendingAssistant && len(m.streamingContextMessages) > 0 {
		m.streamingContextMessages[len(m.streamingContextMessages)-1] = assistantMsg
		m.invalidateContextEstimateCacheLocked()
		return
	}
	m.streamingContextMessages = append(m.streamingContextMessages, assistantMsg)
	m.streamingContextPendingAssistant = true
	m.invalidateContextEstimateCacheLocked()
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
	m.invalidateContextEstimateCacheLocked()
}

// clearStreamCallbacks detaches legacy direct-engine callbacks when chat is not
// using the shared runner and resets the per-turn "persist as we go" state. The
// runner path owns borrowed-engine callback lifetimes itself, so clearing them
// here would race the active run.
func (m *Model) clearStreamCallbacks() {
	if m.runner == nil {
		m.engine.SetAssistantSnapshotCallback(nil)
		m.engine.SetResponseCompletedCallback(nil)
		m.engine.SetTurnCompletedCallback(nil)
		m.engine.SetCompactionCallback(nil)
	}
	m.pendingMu.Lock()
	m.pendingAssistantMsgID = 0
	m.pendingAssistantTextSet = false
	m.pendingAssistantSnapshot = llm.Message{}
	m.pendingAssistantSnapshotSet = false
	m.completedAssistantTurns = 0
	m.pendingMu.Unlock()
	m.clearStreamingContextMessages()
}

// streamPersistenceCallbacks builds callbacks so assistant messages and tool
// results persist incrementally as the turn progresses.
func (m *Model) streamPersistenceCallbacks(streamStart time.Time) (llm.AssistantSnapshotCallback, llm.ResponseCompletedCallback, llm.TurnCompletedCallback) {
	streamSess := m.sess
	streamSessionID := ""
	if streamSess != nil {
		streamSessionID = streamSess.ID
	}
	reasoningCfg := m.effectiveReasoningConfig()
	staleStreamSession := func() bool {
		return streamSessionID != "" && (m.sess == nil || m.sess.ID != streamSessionID)
	}
	persistPendingAssistant := func(ctx context.Context, assistantMsg llm.Message, finalizeText bool) {
		if m.store == nil || streamSess == nil || staleStreamSession() {
			return
		}
		sessionMsg := session.NewMessageWithReasoningPolicy(streamSess.ID, assistantMsg, -1, reasoningCfg)
		sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
		m.pendingMu.Lock()
		m.pendingAssistantSnapshot = assistantMsg
		m.pendingAssistantSnapshotSet = true
		defer m.pendingMu.Unlock()
		if m.pendingAssistantMsgID != 0 {
			sessionMsg.ID = m.pendingAssistantMsgID
			err := session.UpdateStreamingMessage(ctx, m.store, streamSess.ID, sessionMsg, finalizeText)
			if err == nil {
				if finalizeText {
					m.pendingAssistantTextSet = true
				}
				return
			}
			if !errors.Is(err, session.ErrNotFound) {
				return
			}
			m.pendingAssistantMsgID = 0
			m.pendingAssistantTextSet = false
			m.pendingAssistantSnapshot = assistantMsg
			m.pendingAssistantSnapshotSet = true
			sessionMsg = session.NewMessageWithReasoningPolicy(streamSess.ID, assistantMsg, -1, reasoningCfg)
			sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
		}
		if err := m.store.AddMessage(ctx, streamSess.ID, sessionMsg); err != nil {
			return
		}
		m.pendingAssistantMsgID = sessionMsg.ID
		m.pendingAssistantTextSet = finalizeText
	}

	assistantSnapshot := func(ctx context.Context, _ int, assistantMsg llm.Message) error {
		if staleStreamSession() {
			return nil
		}
		m.updateStreamingContextAssistant(assistantMsg)
		persistPendingAssistant(ctx, assistantMsg, false)
		return nil
	}
	responseCompleted := func(ctx context.Context, _ int, assistantMsg llm.Message, _ llm.TurnMetrics) error {
		if staleStreamSession() {
			return nil
		}
		m.updateStreamingContextAssistant(assistantMsg)
		persistPendingAssistant(ctx, assistantMsg, true)
		return nil
	}
	turnCompleted := func(ctx context.Context, _ int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
		if staleStreamSession() {
			return nil
		}
		m.appendStreamingContextTurnMessages(turnMessages)

		appendStart := 0
		if len(turnMessages) > 0 && turnMessages[0].Role == llm.RoleAssistant {
			m.pendingMu.Lock()
			finalizeText := !m.pendingAssistantTextSet
			m.pendingMu.Unlock()
			persistPendingAssistant(ctx, turnMessages[0], finalizeText)
			appendStart = 1
		}
		if m.store != nil && streamSess != nil {
			for _, msg := range turnMessages[appendStart:] {
				if msg.Role == llm.RoleUser {
					continue
				}
				sessionMsg := session.NewMessageWithReasoningPolicy(streamSess.ID, msg, -1, reasoningCfg)
				_ = m.store.AddMessage(ctx, streamSess.ID, sessionMsg)
			}
		}
		m.pendingMu.Lock()
		m.pendingAssistantMsgID = 0
		m.pendingAssistantTextSet = false
		m.pendingAssistantSnapshot = llm.Message{}
		m.pendingAssistantSnapshotSet = false
		if appendStart > 0 {
			m.completedAssistantTurns++
		}
		m.pendingMu.Unlock()
		if m.store != nil && streamSess != nil {
			_ = m.store.UpdateMetrics(ctx, streamSess.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens, metrics.CacheWriteTokens)
			m.persistContextEstimate(ctx)
		}
		return nil
	}
	return assistantSnapshot, responseCompleted, turnCompleted
}

// setupStreamPersistenceCallbacks wires snapshot/response/turn callbacks on the engine.
func (m *Model) setupStreamPersistenceCallbacks(streamStart time.Time) {
	assistantSnapshot, responseCompleted, turnCompleted := m.streamPersistenceCallbacks(streamStart)
	m.engine.SetAssistantSnapshotCallback(assistantSnapshot)
	m.engine.SetResponseCompletedCallback(responseCompleted)
	m.engine.SetTurnCompletedCallback(turnCompleted)
}

func (m *Model) streamCompactionCallback(streamSess *session.Session) llm.CompactionCallback {
	streamSessionID := ""
	if streamSess != nil {
		streamSessionID = streamSess.ID
	}
	return func(ctx context.Context, result *llm.CompactionResult) error {
		if streamSessionID != "" && (m.sess == nil || m.sess.ID != streamSessionID) {
			return nil
		}
		m.messagesMu.Lock()
		full := append([]session.Message(nil), m.messages...)
		m.messagesMu.Unlock()
		updated, activeStart, refreshed, err := session.ApplyCompaction(ctx, m.store, streamSess, full, result)
		if err != nil {
			return err
		}
		m.messagesMu.Lock()
		m.messages = updated
		m.compactionIdx = activeStart
		m.messagesMu.Unlock()
		if refreshed != nil {
			m.sess = refreshed
		}
		if result != nil && m.program != nil {
			m.program.Send(compactionUsageMsg{
				sessionID: streamSessionID,
				model:     result.Model,
				usage:     result.Usage,
			})
		}
		if m.engine != nil {
			m.engine.SetContextEstimateBaseline(0, 0)
		}
		if result != nil {
			m.setStreamingContextMessages(result.NewMessages)
		}
		m.invalidateHistoryCache()
		// Any pending assistant row that snapshot had upserted is now stale:
		// compaction rewrote the message table. Clear the tracking so the
		// next snapshot/response inserts fresh instead of trying to update
		// a row that no longer exists.
		m.pendingMu.Lock()
		m.pendingAssistantMsgID = 0
		m.pendingAssistantTextSet = false
		m.pendingAssistantSnapshot = llm.Message{}
		m.pendingAssistantSnapshotSet = false
		m.completedAssistantTurns = 0
		m.pendingMu.Unlock()
		return nil
	}
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
	m.resetContextEstimateBaseline(context.Background())
}

func (m *Model) insertDeveloperMessage(msg session.Message) {
	insertAt := 0
	for insertAt < len(m.messages) && m.messages[insertAt].Role == llm.RoleSystem {
		insertAt++
	}
	m.messages = append(m.messages[:insertAt], append([]session.Message{msg}, m.messages[insertAt:]...)...)
	m.invalidateHistoryCache()
	m.resetContextEstimateBaseline(context.Background())
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
	if m.worktreeOperationBusy() {
		return m.showFooterWarning("Wait for the current worktree operation to finish before sending.")
	}
	m.clearFooterMessage()
	var preSendCmds []tea.Cmd
	if cmd := m.applyPendingStreamModelSwitch(); cmd != nil {
		preSendCmds = append(preSendCmds, cmd)
	}
	m.recordCurrentModelUse()

	// Build the full message content including file attachments
	fullContent := content
	var fileNames []string

	if len(m.files) > 0 {
		var filesContent strings.Builder
		filesContent.WriteString("\n\n" + llm.EmbeddedFileIntro + "\n\n")
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
			filesContent.WriteString(llm.FormatEmbeddedFileText(f.Name, "text/plain", f.Content))
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

	// Deferred model-switch markers from non-submitting shortcuts (Ctrl+R) are
	// appended only when the next user turn is submitted, so repeatedly cycling
	// effort while drafting does not spam the visible scrollback.
	m.appendPendingModelSwitchMarker()

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

	if cmd := m.scheduleTitleFallbackCmd(); cmd != nil {
		preSendCmds = append(preSendCmds, cmd)
	}

	// Name the handover file from the first user message so it carries a
	// descriptive filename from the start.
	if m.userMessageCount() == 1 {
		if cmd := m.maybeNameHandoverCmd(content); cmd != nil {
			preSendCmds = append(preSendCmds, cmd)
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
	m.resetPromptHistory()
	m.setTextareaValue("")
	m.files = nil
	m.images = nil
	m.selectedImage = -1
	m.pasteChunks = nil

	// Start streaming
	m.streaming = true
	// The previous turn's tracker is kept alive after stream-done so its
	// reasoning headers stay click-toggleable; clear it now that a fresh
	// assistant turn is beginning.
	m.resetRetainedStreamTracker()
	m.phase = "Thinking"
	m.streamStartTime = time.Now()
	if m.stats != nil {
		m.stats.SetModel(m.statsPricingModel())
		m.stats.RequestStart()
	}
	if m.altScreen {
		m.scrollToBottom = true
	}
	if m.streamPerf != nil && m.sess != nil {
		m.streamPerf.StartTurn(m.sess.ID, m.streamStartTime)
	}
	m.currentResponse.Reset()
	m.pendingMu.Lock()
	m.pendingAssistantMsgID = 0
	m.pendingAssistantTextSet = false
	m.pendingAssistantSnapshot = llm.Message{}
	m.pendingAssistantSnapshotSet = false
	m.completedAssistantTurns = 0
	m.pendingMu.Unlock()
	m.resetCurrentReasoning()
	m.resetAttemptUsage()
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
	m.streamRenderTickPending = false

	// Start the stream
	// In alt screen mode, View() renders history including user message
	// In inline mode, print user message to scrollback first
	if m.altScreen {
		cmds := []tea.Cmd{
			m.startStream(fullContent),
			m.spinner.Tick,
			m.tickEvery(),
		}
		cmds = append(preSendCmds, cmds...)
		m.appendTerminalTitleCmd(&cmds)
		return m, tea.Batch(cmds...)
	}
	cmds := []tea.Cmd{
		tea.Println(userDisplay.String()),
		m.startStream(fullContent),
		m.spinner.Tick,
		m.tickEvery(),
	}
	cmds = append(preSendCmds, cmds...)
	m.appendTerminalTitleCmd(&cmds)
	return m, tea.Batch(cmds...)
}

func (m *Model) startStream(content string) tea.Cmd {
	ctx, cancel := context.WithCancel(m.rootContext())
	m.streamGeneration++
	streamGeneration := m.streamGeneration
	m.streamCancelFunc = cancel
	m.setStreamCancelRequested(false)

	return func() tea.Msg {
		// Mark session as active when starting a new stream
		if m.store != nil && m.sess != nil {
			_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusActive)
		}

		// Create stream adapter for unified event handling with proper buffering
		adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
		m.streamChan = adapter.Events()
		m.streamCoalescer = &streamEventCoalescer{ch: m.streamChan}

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

		// Keep web search/fetch unavailable when search is off. Some registry-wide
		// tools (notably web_search/read_url) are registered so they can be injected
		// when search is enabled, but they should not leak into ordinary chats.
		reqTools = filterSearchToolSpecs(reqTools, m.searchEnabled)

		serviceTier, serviceTierSet := m.currentServiceTier()
		req := llm.Request{
			SessionID:               m.sess.ID,
			Model:                   strings.TrimSpace(m.modelName),
			Messages:                messages,
			Tools:                   reqTools,
			Search:                  m.searchEnabled,
			ForceExternalSearch:     m.forceExternalSearch,
			DisableExternalWebFetch: m.disableExternalWebFetch,
			ParallelToolCalls:       true,
			ServiceTier:             serviceTier,
			ServiceTierSet:          serviceTierSet,
			MaxTurns:                m.maxTurns,
		}
		if m.sess != nil && strings.EqualFold(m.sess.ReasoningMode, "pro") {
			if llm.SupportsReasoningMode(m.providerKey, m.modelName) {
				req.Responses = &llm.ResponsesOptions{ReasoningMode: "pro"}
			} else {
				m.sess.ReasoningMode = ""
				if m.store != nil {
					_ = m.store.Update(context.Background(), m.sess)
				}
			}
		}

		assistantSnapshotCB, responseCompletedCB, turnCompletedCB := m.streamPersistenceCallbacks(m.streamStartTime)
		if m.runner == nil {
			m.engine.SetAssistantSnapshotCallback(assistantSnapshotCB)
			m.engine.SetResponseCompletedCallback(responseCompletedCB)
			m.engine.SetTurnCompletedCallback(turnCompletedCB)
		}

		// Enable context compaction or tracking for models with known input limits.
		// Re-set each turn in case the provider/model changed mid-session.
		m.configureContextManagementForSession()

		// Set up compaction callback to update in-memory state and persist.
		// This runs on the engine goroutine, so we protect m.messages with a mutex.
		streamSess := m.sess
		compactionCB := m.streamCompactionCallback(streamSess)
		if m.runner == nil {
			m.engine.SetCompactionCallback(compactionCB)
		}
		syntheticUserCB := func(cbCtx context.Context, msg llm.Message) error {
			if streamSess == nil || m.store == nil || streamSess.ID == "" {
				return nil
			}
			msg.Role = llm.RoleUser
			m.contextEstimateMu.Lock()
			m.streamingContextMessages = append(m.streamingContextMessages, msg)
			m.streamingContextPendingAssistant = false
			m.invalidateContextEstimateCacheLocked()
			m.contextEstimateMu.Unlock()
			return m.store.AddMessage(cbCtx, streamSess.ID, session.NewMessage(streamSess.ID, msg, -1))
		}

		// Start streaming in background - adapter handles all event conversion
		m.streamDone = make(chan struct{})
		go func() {
			defer close(m.streamDone)
			if m.runner != nil {
				includeConfiguredTools := false
				searchEnabled := m.searchEnabled
				forceExternalSearch := m.forceExternalSearch
				runReq := runpkg.Request{
					Platform:                  runpkg.PlatformChat,
					AgentName:                 m.agentName,
					Messages:                  messages,
					Engine:                    m.engine,
					ProviderInstance:          m.provider,
					SessionID:                 req.SessionID,
					DeferSession:              true,
					DisableRuntimePersistence: true,
					Provider:                  strings.TrimSpace(m.providerKey),
					Model:                     strings.TrimSpace(m.modelName),
					Tools:                     m.toolsStr,
					MCP:                       m.mcpStr,
					SystemMessage:             m.config.Chat.Instructions,
					MaxTurns:                  m.maxTurns,
					MaxTurnsSet:               m.maxTurns > 0,
					Search:                    &searchEnabled,
					ForceExternalSearch:       &forceExternalSearch,
					DisableExternalWebFetch:   m.disableExternalWebFetch,
					ExtraTools:                reqTools,
					IncludeConfiguredTools:    &includeConfiguredTools,
					ServiceTier:               serviceTier,
					ServiceTierSet:            serviceTierSet,
					OnAssistantSnapshot:       assistantSnapshotCB,
					OnResponseCompleted:       responseCompletedCB,
					OnTurnCompleted:           turnCompletedCB,
					OnCompaction:              compactionCB,
					OnSyntheticUserMessage:    syntheticUserCB,
				}
				runCtx, cancelRun := context.WithCancel(ctx)
				defer cancelRun()
				pipe := runpkg.NewEventPipe(runCtx, ui.DefaultStreamBufferSize)
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, err := m.runner.Run(runCtx, runReq, pipe)
					pipe.CloseWithError(err)
				}()
				adapter.ProcessStream(runCtx, pipe)
				cancelRun()
				<-done
				return
			}

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
		return m.listenForStreamEventsSync(streamGeneration)
	}
}

// listenForStreamEvents returns a command that listens for the next stream event
func (m *Model) listenForStreamEvents() tea.Cmd {
	streamGeneration := m.streamGeneration
	return func() tea.Msg {
		return m.listenForStreamEventsSync(streamGeneration)
	}
}

// streamEventCoalescer reads stream events for a single stream, merging bursts
// of consecutive text deltas already buffered in the channel into one event so
// fast token streams don't pay a full Update/View cycle per delta. A non-text
// event pulled while merging is parked in pending and delivered on the next
// read, preserving event order. Reads are serialized by the bubbletea command
// loop: only one listener is outstanding per stream at a time.
type streamEventCoalescer struct {
	ch      <-chan ui.StreamEvent
	pending *ui.StreamEvent
}

// maxCoalescedTextEvents bounds a single merge so a producer that outpaces the
// UI can't starve rendering of the already-merged text.
const maxCoalescedTextEvents = 32

func (c *streamEventCoalescer) next() (ui.StreamEvent, bool) {
	if ev := c.pending; ev != nil {
		c.pending = nil
		return *ev, true
	}
	event, ok := <-c.ch
	if !ok {
		return ui.StreamEvent{}, false
	}
	if event.Type != ui.StreamEventText {
		return event, true
	}
	var merged strings.Builder
	merged.WriteString(event.Text)
	for i := 0; i < maxCoalescedTextEvents; i++ {
		var nextEv ui.StreamEvent
		var more bool
		select {
		case nextEv, more = <-c.ch:
		default:
			event.Text = merged.String()
			return event, true
		}
		if !more {
			// Channel closed; deliver merged text now, the next read
			// observes the closure and synthesizes Done upstream.
			event.Text = merged.String()
			return event, true
		}
		if nextEv.Type != ui.StreamEventText {
			c.pending = &nextEv
			event.Text = merged.String()
			return event, true
		}
		merged.WriteString(nextEv.Text)
	}
	event.Text = merged.String()
	return event, true
}

// listenForStreamEventsSync synchronously waits for the next stream event
func (m *Model) listenForStreamEventsSync(generation uint64) tea.Msg {
	mkMsg := func(event ui.StreamEvent) streamEventMsg {
		return streamEventMsg{event: event, generation: generation}
	}
	if co := m.streamCoalescer; co != nil {
		event, ok := co.next()
		if !ok {
			if m.isStreamCancelRequested() {
				return mkMsg(ui.ErrorEvent(context.Canceled))
			}
			return mkMsg(ui.DoneEvent(0))
		}
		if m.isStreamCancelRequested() && event.Type == ui.StreamEventDone {
			return mkMsg(ui.ErrorEvent(context.Canceled))
		}
		return mkMsg(event)
	}

	if m.streamChan == nil {
		if m.isStreamCancelRequested() {
			return mkMsg(ui.ErrorEvent(context.Canceled))
		}
		return mkMsg(ui.DoneEvent(0))
	}

	event, ok := <-m.streamChan
	if !ok {
		if m.isStreamCancelRequested() {
			return mkMsg(ui.ErrorEvent(context.Canceled))
		}
		return mkMsg(ui.DoneEvent(0))
	}
	if m.isStreamCancelRequested() && event.Type == ui.StreamEventDone {
		return mkMsg(ui.ErrorEvent(context.Canceled))
	}
	return mkMsg(event)
}

func (m *Model) buildMessages() []llm.Message {
	m.messagesMu.Lock()
	snapshot := make([]session.Message, len(m.messages))
	copy(snapshot, m.messages)
	compIdx := m.compactionIdx
	m.messagesMu.Unlock()

	return session.LLMActiveMessages(snapshot, compIdx, m.config.Chat.Instructions)
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

func (m *Model) estimateContextTokensCached() int {
	if m == nil || m.engine == nil {
		return 0
	}

	m.contextEstimateMu.Lock()
	version := m.contextEstimateVersion
	if m.streaming && len(m.streamingContextMessages) > 0 {
		if m.contextEstimateCachedValid && m.contextEstimateCachedVersion == version && m.contextEstimateCachedStreaming {
			tokens := m.contextEstimateCachedTokens
			m.contextEstimateMu.Unlock()
			return tokens
		}
		messages := copyLLMMessages(m.streamingContextMessages)
		m.contextEstimateMu.Unlock()

		tokens := m.engine.EstimateTokens(messages)

		m.contextEstimateMu.Lock()
		if m.contextEstimateVersion == version && m.streaming && len(m.streamingContextMessages) > 0 {
			m.contextEstimateCachedVersion = version
			m.contextEstimateCachedTokens = tokens
			m.contextEstimateCachedStreaming = true
			m.contextEstimateCachedValid = true
		}
		m.contextEstimateMu.Unlock()
		return tokens
	}
	if m.contextEstimateCachedValid && m.contextEstimateCachedVersion == version && !m.contextEstimateCachedStreaming {
		tokens := m.contextEstimateCachedTokens
		m.contextEstimateMu.Unlock()
		return tokens
	}
	m.contextEstimateMu.Unlock()

	messages := m.buildMessages()
	tokens := m.engine.EstimateTokens(messages)

	m.contextEstimateMu.Lock()
	if m.contextEstimateVersion == version && !m.streaming {
		m.contextEstimateCachedVersion = version
		m.contextEstimateCachedTokens = tokens
		m.contextEstimateCachedStreaming = false
		m.contextEstimateCachedValid = true
	}
	m.contextEstimateMu.Unlock()
	return tokens
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

// userMessageCount returns the number of user messages in this session.
func (m *Model) userMessageCount() int {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()
	n := 0
	for _, msg := range m.messages {
		if msg.Role == llm.RoleUser {
			n++
		}
	}
	return n
}

// fastSlugGen returns a HandoverSlugGenerator that formats content into
// promptFmt (which must contain a single %s), runs it through the fast
// provider, and returns the trimmed response. Content is truncated to keep
// the request small.
func fastSlugGen(provider llm.Provider, promptFmt string) session.HandoverSlugGenerator {
	return func(ctx context.Context, content string) (string, error) {
		if len(content) > 2000 {
			content = content[:2000]
		}
		prompt := fmt.Sprintf(promptFmt, content)
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		stream, err := provider.Stream(ctx, llm.Request{
			Ephemeral: true,
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
}

// maybeNameHandoverCmd names this session's handover file from the first user
// message: the fast provider produces two descriptive words which replace the
// random slug upfront, and a symlink from the original path (baked into the
// system prompt) keeps the agent's writes landing in the renamed file.
func (m *Model) maybeNameHandoverCmd(firstMessage string) tea.Cmd {
	if m.currentAgent == nil || !m.currentAgent.EnableHandover {
		return nil
	}
	provider := m.fastProvider
	if provider == nil {
		return nil
	}
	promptText := m.currentSystemPromptText()
	path, _, pinned, resolveErr := m.resolveHandoverPath(promptText)
	rootCtx := m.rootContext()
	return func() tea.Msg {
		if resolveErr != nil {
			return handoverRenameDoneMsg{err: resolveErr}
		}
		if !pinned || path == "" {
			return handoverRenameDoneMsg{}
		}
		slugGen := fastSlugGen(provider, "Generate exactly two lowercase dash-separated words that describe this task, e.g. auth-refactor. Reply with ONLY the two words, nothing else.\n\n%s")
		err := session.PrettifyHandoverName(rootCtx, path, firstMessage, slugGen)
		return handoverRenameDoneMsg{err: err}
	}
}

// maybeRenameHandoverCmd returns a tea.Cmd that checks the handover directory
// for a random-named file large enough to rename. If found, it uses the fast
// provider to generate a descriptive slug, renames the file, and creates a
// symlink from the old name so the system prompt path remains valid. This is
// the fallback for sessions where first-message naming did not run (e.g. no
// fast provider at the time); it skips files that are already symlinks.
func (m *Model) maybeRenameHandoverCmd() tea.Cmd {
	if m.currentAgent == nil || !m.currentAgent.EnableHandover {
		return nil
	}
	provider := m.fastProvider
	if provider == nil {
		return nil
	}
	// Snapshot the prompt and directory-derived path before the async command to
	// avoid racing on m.messages or a later worktree switch.
	promptText := m.currentSystemPromptText()
	path, dir, pinned, resolveErr := m.resolveHandoverPath(promptText)
	rootCtx := m.rootContext()
	return func() tea.Msg {
		if resolveErr != nil {
			return handoverRenameDoneMsg{err: resolveErr}
		}
		// Rename the file this session's agent writes to; only fall back to
		// the effective directory's latest .md for genuinely legacy prompts.
		if path == "" && !pinned {
			path, _ = findLatestHandoverFile(dir)
		}
		if path == "" {
			return handoverRenameDoneMsg{}
		}
		slugGen := fastSlugGen(provider, "Generate a short filesystem-safe slug (2-5 words, lowercase, dash-separated) that describes this document. Reply with ONLY the slug, nothing else.\n\n%s")
		err := session.MaybeRenameHandover(rootCtx, path, slugGen)
		return handoverRenameDoneMsg{err: err}
	}
}

func (m *Model) invalidateViewCache() {
	m.viewCache.historyValid = false
	m.viewCache.historyLines = nil
	m.viewCache.completedStream = ""
	m.viewCache.cachedCompletedContent = ""
	m.viewCache.cachedTrackerVersion = 0
	m.viewCache.lastTrackerVersion = 0
	m.viewCache.lastWavePos = 0
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.invalidateContextEstimateCache()
	if m.chatRenderer != nil {
		m.chatRenderer.InvalidateCache()
	}
	m.bumpContentVersion()
}

func (m *Model) invalidateHistoryCache() {
	m.viewCache.historyValid = false
	m.viewCache.historyLines = nil
	m.resetAltScreenStreamingAppendCache()
	m.invalidateContextEstimateCache()
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
