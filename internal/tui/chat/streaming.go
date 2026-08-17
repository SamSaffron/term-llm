package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
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
	boundary := m.runBoundary
	boundaryRunID := ""
	if boundary != nil {
		boundaryRunID = boundary.RunID()
	}
	staleStreamSession := func() bool {
		return streamSessionID != "" && (m.sess == nil || m.sess.ID != streamSessionID)
	}
	persistPendingAssistant := func(ctx context.Context, assistantMsg llm.Message, finalizeText bool) (int64, bool) {
		if m.store == nil || streamSess == nil || staleStreamSession() {
			return 0, false
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
				return sessionMsg.ID, true
			}
			if !errors.Is(err, session.ErrNotFound) {
				return 0, false
			}
			m.pendingAssistantMsgID = 0
			m.pendingAssistantTextSet = false
			m.pendingAssistantSnapshot = assistantMsg
			m.pendingAssistantSnapshotSet = true
			sessionMsg = session.NewMessageWithReasoningPolicy(streamSess.ID, assistantMsg, -1, reasoningCfg)
			sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
		}
		if err := m.store.AddMessage(ctx, streamSess.ID, sessionMsg); err != nil {
			return 0, false
		}
		m.pendingAssistantMsgID = sessionMsg.ID
		m.pendingAssistantTextSet = finalizeText
		return sessionMsg.ID, sessionMsg.ID > 0
	}

	assistantSnapshot := func(ctx context.Context, _ int, assistantMsg llm.Message) error {
		if staleStreamSession() {
			return nil
		}
		m.updateStreamingContextAssistant(assistantMsg)
		if boundary != nil {
			boundary.UpdateAssistant(boundaryRunID, assistantMsg)
		}
		_, _ = persistPendingAssistant(ctx, assistantMsg, false)
		return nil
	}
	responseCompleted := func(ctx context.Context, _ int, assistantMsg llm.Message, _ llm.TurnMetrics) error {
		if staleStreamSession() {
			return nil
		}
		m.updateStreamingContextAssistant(assistantMsg)
		if boundary != nil {
			boundary.UpdateAssistant(boundaryRunID, assistantMsg)
		}
		_, _ = persistPendingAssistant(ctx, assistantMsg, true)
		return nil
	}
	turnCompleted := func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
		if staleStreamSession() {
			return nil
		}
		m.appendStreamingContextTurnMessages(turnMessages)

		appendStart := 0
		persistComplete := m.store != nil && streamSess != nil
		m.pendingMu.Lock()
		lastDurableID := m.pendingAssistantMsgID
		pendingAssistantPersisted := m.pendingAssistantTextSet
		m.pendingMu.Unlock()
		if len(turnMessages) == 0 && (!pendingAssistantPersisted || lastDurableID <= 0) {
			persistComplete = false
		}
		if len(turnMessages) > 0 && turnMessages[0].Role == llm.RoleAssistant {
			m.pendingMu.Lock()
			finalizeText := !m.pendingAssistantTextSet
			m.pendingMu.Unlock()
			var ok bool
			lastDurableID, ok = persistPendingAssistant(ctx, turnMessages[0], finalizeText)
			persistComplete = persistComplete && ok
			appendStart = 1
		}
		if m.store != nil && streamSess != nil {
			for _, msg := range turnMessages[appendStart:] {
				if msg.Role == llm.RoleUser {
					// Interjections are persisted by the UI event path. Until that write
					// is coordinated, conservatively retain the preceding boundary.
					persistComplete = false
					continue
				}
				sessionMsg := session.NewMessageWithReasoningPolicy(streamSess.ID, msg, -1, reasoningCfg)
				if err := m.store.AddMessage(ctx, streamSess.ID, sessionMsg); err != nil || sessionMsg.ID <= 0 {
					persistComplete = false
					continue
				}
				lastDurableID = sessionMsg.ID
			}
		}
		if boundary != nil && boundary.Commit(boundaryRunID, turnIndex, turnMessages) && persistComplete && lastDurableID > 0 {
			boundary.PublishDurable(boundaryRunID, turnIndex, lastDurableID)
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

func (m *Model) streamCompactionCallback(streamSess *session.Session, generation uint64) llm.CompactionCallback {
	streamSessionID := ""
	if streamSess != nil {
		streamSessionID = streamSess.ID
	}
	return func(ctx context.Context, result *llm.CompactionResult) error {
		m.messagesMu.Lock()
		full := append([]session.Message(nil), m.messages...)
		m.messagesMu.Unlock()
		updated, activeStart, refreshed, err := session.ApplyCompaction(ctx, m.store, streamSess, full, result)
		if err != nil {
			return err
		}

		msg := compactionAppliedMsg{
			generation:  generation,
			sessionID:   streamSessionID,
			messages:    updated,
			activeStart: activeStart,
			refreshed:   refreshed,
		}
		if result != nil {
			// Streaming context has its own mutex and must move immediately. Later
			// persistence callbacks extend this compacted base with assistant/tool
			// work produced after the boundary; replaying a frozen snapshot on the
			// UI goroutine would erase those additions.
			m.setStreamingContextMessages(result.ActiveMessages())
			msg.model = result.Model
			msg.usage = result.Usage
		}
		// Snapshot persistence may already own an assistant row from before the
		// boundary. Clear that mutex-protected identity immediately so the response
		// callback inserts the loose tool call on the compacted side before its
		// result is persisted.
		m.resetPendingAssistantAfterCompaction()
		m.queueCompactionForUI(msg)
		return nil
	}
}

func (m *Model) queueCompactionForUI(msg compactionAppliedMsg) {
	m.compactionApplyMu.Lock()
	m.pendingCompactionApplies = append(m.pendingCompactionApplies, msg)
	m.compactionApplyMu.Unlock()
}

func (m *Model) discardPendingCompactionsBeforeGeneration(generation uint64) {
	m.compactionApplyMu.Lock()
	kept := m.pendingCompactionApplies[:0]
	for _, msg := range m.pendingCompactionApplies {
		if msg.generation >= generation {
			kept = append(kept, msg)
		}
	}
	m.pendingCompactionApplies = kept
	m.compactionApplyMu.Unlock()
}

func (m *Model) takePendingCompactions(generation uint64, all bool) []compactionAppliedMsg {
	m.compactionApplyMu.Lock()
	defer m.compactionApplyMu.Unlock()

	pending := m.pendingCompactionApplies
	kept := pending[:0]
	var taken []compactionAppliedMsg
	for _, msg := range pending {
		switch {
		case msg.generation < generation:
			// A terminal event from the old stream was ignored after a new stream
			// started. Its durable state belongs to that old generation and must not
			// leak into this one.
			continue
		case msg.generation == generation && (all || len(taken) == 0):
			taken = append(taken, msg)
		default:
			kept = append(kept, msg)
		}
	}
	m.pendingCompactionApplies = kept
	return taken
}

func (m *Model) applyNextPendingCompactionToUI(generation uint64) bool {
	applied := false
	for _, pending := range m.takePendingCompactions(generation, false) {
		m.applyCompactionToUI(pending)
		applied = true
	}
	return applied
}

func (m *Model) applyAllPendingCompactionsToUI(generation uint64) {
	for _, pending := range m.takePendingCompactions(generation, true) {
		m.applyCompactionToUI(pending)
	}
}

func (m *Model) applyCompactionToUI(msg compactionAppliedMsg) {
	if msg.sessionID != "" && (m.sess == nil || m.sess.ID != msg.sessionID) {
		return
	}
	m.messagesMu.Lock()
	m.messages = msg.messages
	m.compactionIdx = msg.activeStart
	m.messagesMu.Unlock()
	if msg.refreshed != nil {
		m.sess = msg.refreshed
	}
	if m.engine != nil {
		m.engine.SetContextEstimateBaseline(0, 0)
	}
	m.invalidateHistoryCache()

	if !msg.usage.BillableCountersZero() {
		m.recordCompactionUsage(context.Background(), msg.sessionID, msg.model, msg.usage)
	}
}

func (m *Model) resetLivePresentationAfterCompaction() {
	// The callback's in-memory snapshot can lag response/tool persistence from the
	// turn that crossed the boundary. Reload before clearing the tracker so history
	// already owns those pre-boundary rows; otherwise the marker appears above the
	// current turn and jumps downward when StreamEventDone performs the same reload.
	if m.store != nil && m.sess != nil {
		if loaded, compactionIdx, err := loadSessionMessagesForScrollback(context.Background(), m.store, m.sess); err != nil {
			slog.Warn("reload TUI scrollback at compaction boundary failed", "error", err)
		} else {
			m.messagesMu.Lock()
			m.messages = loaded
			m.compactionIdx = compactionIdx
			m.messagesMu.Unlock()
			m.invalidateHistoryCache()
		}
	}

	// History now owns everything before the boundary. The live tracker must only
	// contain continuation events; retaining its earlier segments duplicates the
	// pre-compaction tool/text rows below the durable marker and makes the marker
	// jump when StreamEventDone reloads persisted history.
	m.resetTracker()
	m.currentResponse.Reset()
	m.resetCurrentReasoning()
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
	m.smoothTickPending = false
	m.viewCache.completedStream = ""
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
}

func (m *Model) resetPendingAssistantAfterCompaction() {
	// Compaction rewrites the message table, so any snapshot-upsert row is stale.
	m.pendingMu.Lock()
	m.pendingAssistantMsgID = 0
	m.pendingAssistantTextSet = false
	m.pendingAssistantSnapshot = llm.Message{}
	m.pendingAssistantSnapshotSet = false
	m.completedAssistantTurns = 0
	m.pendingMu.Unlock()
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
	if m.directShellRun != nil {
		return m.showFooterWarning("Wait for the shell command to finish or press Esc to cancel it.")
	}
	// Delegation intent comes only from text deliberately visible in the composer.
	// Collapsed paste payloads are expanded for the provider after this check, so
	// hidden pasted prose cannot synthesize an @agent request.
	delegationContext, err := m.agentMentionDelegationContext(content)
	if err != nil {
		m.hideMentionPopup()
		return m.showFooterError(err.Error())
	}
	content = m.expandedPastePlaceholders(content)

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

	// Build provider-facing parts separately from visible/session text. The typed
	// delegation part is converted to provider text only at the provider boundary;
	// renderers, exports, FTS, resume UI, and prompt history never infer hidden
	// content by stripping arbitrary text.
	displayText := content
	var fileNames []string
	var explicitFiles string

	if len(m.files) > 0 {
		var filesContent strings.Builder
		filesContent.WriteString("\n\n" + llm.EmbeddedFileIntro + "\n\n")
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
			filesContent.WriteString(llm.FormatEmbeddedFileText(f.Name, "text/plain", f.Content))
		}
		explicitFiles = filesContent.String()
		displayText += explicitFiles
	}

	mentionContext, mentionLabels := m.eagerMentionContext(content)
	fileNames = append(fileNames, mentionLabels...)

	imageLabels := m.imageAttachmentLabels()
	parts := m.imagePartList()
	if content != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: content})
	}
	if delegationContext != "" {
		parts = append(parts, llm.Part{Type: llm.PartAgentMention, Text: delegationContext})
	}
	if explicitFiles != "" {
		parts = append(parts, llm.Part{Type: llm.PartText, Text: explicitFiles})
	}
	if mentionContext != "" {
		parts = append(parts, llm.Part{Type: llm.PartFile, Text: mentionContext})
	}
	if len(parts) == 0 {
		parts = []llm.Part{{Type: llm.PartText}}
	}

	if strings.TrimSpace(displayText) == "" && len(imageLabels) > 0 {
		displayText = "[" + strings.Join(imageLabels, ", ") + "]"
	}

	// Retain the preceding legal boundary until the active user row has been
	// durably appended. A failed append must never turn row ID zero into a root
	// fork fallback.
	m.activeBranchAnchorID = lastSafeBranchMessageID(m.messages)

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
		if err := m.store.AddMessage(context.Background(), m.sess.ID, userMsg); err == nil && userMsg.ID > 0 {
			// The active user is the first completed and durable boundary of this run.
			m.messages[len(m.messages)-1].ID = userMsg.ID
			m.activeBranchAnchorID = userMsg.ID
		}
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
	return m.beginUserResponse(content, userDisplay.String(), preSendCmds)
}

// beginUserResponse transitions from a committed user message to the normal
// assistant stream. Keeping this separate lets direct shell mode persist its
// own structured, literal command result without re-running composer semantics
// such as @ mentions, pasted placeholders, or attachment expansion.
func (m *Model) beginUserResponse(content, userDisplay string, preSendCmds []tea.Cmd) (tea.Model, tea.Cmd) {
	// Clear input and attachments
	m.resetPromptHistory()
	m.setTextareaValue("")
	m.hideMentionPopup()
	m.files = nil
	m.images = nil
	m.selectedImage = -1
	m.pasteChunks = nil

	// Start streaming
	m.streaming = true
	m.mainRunViewComplete = true
	// Manager sequences restart at one for every run. Clear the previous run's
	// cursor before Start so events published before attachment are replayed.
	m.mainRunID = ""
	m.mainRunLastSeq = 0
	m.mainRunSubscription++
	m.mainRunReplay = nil
	m.mainRunLive = nil
	m.mainRunCoalescer = nil
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

	// Start the stream. In alt screen mode, View() renders history including the
	// user message. Inline mode prints the prepared user turn to scrollback first.
	if m.altScreen {
		cmds := []tea.Cmd{
			m.startStream(content),
			m.spinner.Tick,
			m.tickEvery(),
		}
		cmds = append(preSendCmds, cmds...)
		m.appendTerminalTitleCmd(&cmds)
		return m, tea.Batch(cmds...)
	}
	cmds := []tea.Cmd{
		tea.Println(userDisplay),
		m.startStream(content),
		m.spinner.Tick,
		m.tickEvery(),
	}
	cmds = append(preSendCmds, cmds...)
	m.appendTerminalTitleCmd(&cmds)
	return m, tea.Batch(cmds...)
}

func (m *Model) startStream(content string) tea.Cmd {
	ctx, cancel := context.WithCancel(m.rootContext())
	sessionID := m.SessionID()
	m.streamGeneration++
	m.discardPendingCompactionsBeforeGeneration(m.streamGeneration)
	streamGeneration := m.streamGeneration
	m.streamCancelFunc = func() {
		cancel()
		if m.mainRunManager != nil {
			m.mainRunManager.Cancel(sessionID)
		}
	}
	m.setStreamCancelRequested(false)

	return func() tea.Msg {
		// Mark session as active when starting a new stream
		if m.store != nil && m.sess != nil {
			_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusActive)
		}

		// Legacy model-owned streams write directly to the visible listener. A
		// process-scoped run creates its adapter inside the execution host instead.
		var adapter *ui.StreamAdapter
		if m.mainRunManager == nil {
			adapter = ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
			m.streamChan = adapter.Events()
			m.streamCoalescer = &streamEventCoalescer{ch: m.streamChan}
		}

		// Build messages from conversation history
		messages := m.buildMessagesForStream()
		m.setStreamingContextMessages(messages)
		boundaryRunID := fmt.Sprintf("%s:%d", sessionID, streamGeneration)
		boundary := runboundary.New(boundaryRunID, messages, m.activeBranchAnchorID, m.activeBranchAnchorID > 0)
		m.runBoundary = boundary

		// The discovery planner registers MCP wrappers for execution and owns
		// provider visibility. Start with non-MCP request tools only.
		var reqTools []llm.ToolSpec

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
			WorkingDir:              m.effectiveWorkingDir(),
			Model:                   strings.TrimSpace(m.modelName),
			Messages:                messages,
			Tools:                   reqTools,
			EnableToolDiscovery:     m.discoveryPlanner != nil,
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
		compactionCB := m.streamCompactionCallback(streamSess, streamGeneration)
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

		runWithAdapter := func(runCtx context.Context, streamAdapter *ui.StreamAdapter) {
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
					Cwd:                       req.WorkingDir,
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
				pipe := runpkg.NewEventPipe(runCtx, ui.DefaultStreamBufferSize)
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, err := m.runner.Run(runCtx, runReq, pipe)
					pipe.CloseWithError(err)
				}()
				streamAdapter.ProcessStream(runCtx, pipe)
				<-done
				return
			}

			stream, err := m.engine.Stream(runCtx, req)
			if err != nil {
				streamAdapter.EmitErrorAndClose(err)
				return
			}
			defer stream.Close()
			streamAdapter.ProcessStream(runCtx, stream)
		}

		if m.mainRunManager != nil {
			if err := ctx.Err(); err != nil {
				return streamEventMsg{event: ui.ErrorEvent(err), generation: streamGeneration}
			}
			runSessionID := req.SessionID
			snapshot, err := m.mainRunManager.Start(runSessionID, MainRunExecution{
				Execute: func(runCtx context.Context, emit func(ui.StreamEvent)) error {
					runCtx = tools.ContextWithAskUserUIFunc(runCtx, func(promptCtx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
						done := make(chan []tools.AskUserAnswer, 1)
						if err := m.mainRunManager.DeliverUI(runSessionID, AskUserRequestMsg{Questions: questions, DoneCh: done}); err != nil {
							return nil, err
						}
						select {
						case answers := <-done:
							return answers, nil
						case <-promptCtx.Done():
							return nil, promptCtx.Err()
						}
					})
					runCtx = tools.ContextWithHandoverFunc(runCtx, func(promptCtx context.Context, agent string) (bool, error) {
						done := make(chan bool, 1)
						if err := m.mainRunManager.DeliverUI(runSessionID, HandoverRequestMsg{Agent: agent, DoneCh: done}); err != nil {
							return false, err
						}
						select {
						case confirmed := <-done:
							return confirmed, nil
						case <-promptCtx.Done():
							return false, promptCtx.Err()
						}
					})
					hostAdapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
					done := make(chan struct{})
					go func() {
						defer close(done)
						runWithAdapter(runCtx, hostAdapter)
					}()
					var runErr error
					for event := range hostAdapter.Events() {
						if event.Type == ui.StreamEventError {
							runErr = event.Err
						}
						emit(event)
					}
					<-done
					return runErr
				},
				Finalize: func(runErr error) {
					if m.store == nil {
						return
					}
					status := session.StatusComplete
					if runErr != nil {
						status = session.StatusError
						if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
							status = session.StatusInterrupted
						}
					}
					_ = m.store.UpdateStatus(context.Background(), runSessionID, status)
				},
				QueueInterjection: func(interjection llm.QueuedInterjection) llm.InterjectionQueueStatus {
					_, status := m.engine.QueueInterjectionWithStatus(interjection)
					return status
				},
				CancelInterjection: m.engine.CancelInterjection,
				DiscardInterjections: func() {
					m.engine.DiscardPendingInterjections()
				},
				ListInterjections:  m.engine.ListPendingInterjections,
				DrainInterjections: m.engine.DrainInterjections,
				AnchorMessageID:    m.activeBranchAnchorID,
				Boundary:           boundary,
			})
			if err != nil {
				return streamEventMsg{event: ui.ErrorEvent(err), generation: streamGeneration}
			}
			cancel()
			return mainRunStartedMsg{sessionID: runSessionID, runID: snapshot.RunID, generation: streamGeneration}
		}

		// Legacy model-owned execution remains for embedders that do not install a
		// process-scoped manager.
		done := make(chan struct{})
		m.streamDone = done
		go func() {
			defer close(done)
			runWithAdapter(ctx, adapter)
		}()
		return m.listenForStreamEventsSync(streamGeneration)
	}
}

func (m *Model) attachMainRun(sessionID string) tea.Cmd {
	if m.mainRunManager == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if m.mainRunDetach != nil {
		m.mainRunDetach()
		m.mainRunDetach = nil
	}
	// A freshly relaunched model reconstructs the already-produced prefix from
	// durable session messages. An in-process session switch can additionally
	// transfer the exact tool/subagent presentation and its stream cursor; replay
	// then starts immediately after that cursor. Without a retained presentation,
	// skip the old event prefix and let durable messages remain authoritative.
	reattach := !m.streaming && m.mainRunLastSeq == 0
	afterSequence := m.mainRunLastSeq
	var presentation *mainRunPresentation
	if reattach {
		presentation = m.mainRunManager.TakePresentation(sessionID)
		if presentation != nil {
			afterSequence = presentation.sequence
		} else {
			afterSequence = ^uint64(0)
		}
	}
	replay, live, snapshotRequired, snapshot, detach := m.mainRunManager.Subscribe(sessionID, afterSequence)
	if presentation != nil && snapshot.RunID != "" && presentation.runID != snapshot.RunID {
		// The prior run completed and a new one started between taking its retained
		// presentation and subscribing. Never apply the prior run's cursor to the
		// replacement run.
		detach()
		presentation = nil
		replay, live, snapshotRequired, snapshot, detach = m.mainRunManager.Subscribe(sessionID, ^uint64(0))
	}
	if snapshot.RunID == "" {
		detach()
		return nil
	}
	if !snapshot.Active {
		detach()
		m.mainRunManager.Visit(sessionID, snapshot.RunID)
		if m.streaming {
			if m.store != nil && m.sess != nil {
				if loaded, compactionIdx, err := loadSessionMessagesForScrollback(context.Background(), m.store, m.sess); err == nil {
					m.applyLoadedScrollback(loaded, compactionIdx)
				}
			}
			generation := m.streamGeneration
			runID := snapshot.RunID
			subscription := m.mainRunSubscription
			if snapshot.Err != nil {
				return func() tea.Msg {
					return streamEventMsg{event: ui.ErrorEvent(snapshot.Err), generation: generation, mainRunID: runID, mainRunSubscription: subscription}
				}
			}
			return func() tea.Msg {
				return streamEventMsg{event: ui.DoneEvent(0), generation: generation, mainRunID: runID, mainRunSubscription: subscription}
			}
		}
		return nil
	}
	if reattach {
		m.mainRunViewComplete = false
		m.mainRunLastSeq = snapshot.LastSequence
		if presentation != nil && presentation.runID == snapshot.RunID {
			m.tracker = presentation.tracker
			m.subagentTracker = presentation.subagentTracker
			m.mainRunLastSeq = presentation.sequence
			m.mainRunViewComplete = !snapshotRequired
		}
		if snapshot.DurableAnchorValid {
			m.activeBranchAnchorID = snapshot.DurableAnchorID
		} else {
			m.activeBranchAnchorID = snapshot.AnchorMessageID
		}
		if len(snapshot.CompletedMessages) > 0 {
			m.runBoundary = runboundary.New(snapshot.RunID, snapshot.CompletedMessages, snapshot.DurableAnchorID, snapshot.DurableAnchorValid)
		}
		// Elapsed time belongs to the run, not the visible model: a relaunched
		// model must resume the timer from the run's true start.
		m.streamStartTime = snapshot.StartedAt
		if m.phase == "" {
			m.phase = "Responding"
		}
		if m.store != nil && m.sess != nil {
			if loaded, compactionIdx, err := loadSessionMessagesForScrollback(context.Background(), m.store, m.sess); err == nil {
				m.applyLoadedScrollback(loaded, compactionIdx)
			}
		}
		for _, interjection := range m.mainRunManager.ListInterjections(sessionID) {
			m.setPendingInterjection(interjection.ID, interjection.DisplayText)
		}
	}
	m.mainRunID = snapshot.RunID
	m.mainRunSubscription++
	m.mainRunReplay = replay
	m.mainRunLive = live
	if live != nil {
		m.mainRunCoalescer = &mainRunEventCoalescer{ch: live}
	} else {
		m.mainRunCoalescer = nil
	}
	m.mainRunDetach = detach
	m.streaming = true
	m.streamDone = snapshot.Done
	m.streamCancelFunc = func() { m.mainRunManager.Cancel(sessionID) }
	if snapshotRequired {
		m.mainRunViewComplete = false
	}
	if snapshotRequired && m.store != nil && m.sess != nil {
		if loaded, compactionIdx, err := loadSessionMessagesForScrollback(context.Background(), m.store, m.sess); err == nil {
			m.applyLoadedScrollback(loaded, compactionIdx)
		}
	}
	return m.listenForMainRunEvents()
}

func (m *Model) detachMainRun() {
	if m.mainRunManager != nil && m.mainRunViewComplete {
		m.mainRunManager.RetainPresentation(m.SessionID(), m.mainRunID, m.mainRunLastSeq, m.tracker, m.subagentTracker)
	}
	if m.mainRunDetach != nil {
		m.mainRunDetach()
	}
	if m.mainRunUIDetach != nil {
		m.mainRunUIDetach()
	}
	m.mainRunDetach = nil
	m.mainRunUIDetach = nil
	m.mainRunLive = nil
	m.mainRunCoalescer = nil
	m.mainRunReplay = nil
	m.mainRunSubscription++
}

func (m *Model) resetMainRunSessionBinding() {
	m.detachMainRun()
	m.mainRunID = ""
	m.mainRunLastSeq = 0
}

func (m *Model) attachVisibleMainRunUISink() {
	if m.mainRunManager != nil && m.program != nil && m.SessionID() != "" {
		m.AttachMainRunUISink(m.program.Send)
	}
}

func (m *Model) listenForMainRunEvents() tea.Cmd {
	replay := m.mainRunReplay
	coalescer := m.mainRunCoalescer
	generation := m.streamGeneration
	sessionID := m.SessionID()
	runID := m.mainRunID
	subscription := m.mainRunSubscription
	return func() tea.Msg {
		if len(replay) > 0 {
			event := replay[0]
			for i := 1; i < len(replay) && i < maxCoalescedTextEvents && event.Event.Type == ui.StreamEventText && replay[i].Event.Type == ui.StreamEventText; i++ {
				event.Event.Text += replay[i].Event.Text
				event.Sequence = replay[i].Sequence
			}
			return streamEventMsg{event: event.Event, generation: generation, mainRunID: runID, mainRunSubscription: subscription, mainRunSeq: event.Sequence}
		}
		if coalescer == nil {
			return nil
		}
		event, ok := coalescer.next()
		if !ok {
			return mainRunSubscriberClosedMsg{sessionID: sessionID, runID: runID, subscription: subscription}
		}
		return streamEventMsg{event: event.Event, generation: generation, mainRunID: runID, mainRunSubscription: subscription, mainRunSeq: event.Sequence}
	}
}

type mainRunEventCoalescer struct {
	ch      <-chan MainRunEvent
	pending *MainRunEvent
}

func (c *mainRunEventCoalescer) next() (MainRunEvent, bool) {
	if event := c.pending; event != nil {
		c.pending = nil
		return *event, true
	}
	event, ok := <-c.ch
	if !ok {
		return MainRunEvent{}, false
	}
	if event.Event.Type != ui.StreamEventText {
		return event, true
	}
	for i := 1; i < maxCoalescedTextEvents; i++ {
		select {
		case next, open := <-c.ch:
			if !open {
				return event, true
			}
			if next.Event.Type != ui.StreamEventText {
				c.pending = &next
				return event, true
			}
			event.Event.Text += next.Event.Text
			event.Sequence = next.Sequence
		default:
			return event, true
		}
	}
	return event, true
}

// listenForStreamEvents returns a command that listens for the next stream event
func (m *Model) listenForStreamEvents() tea.Cmd {
	if m.mainRunManager != nil && (len(m.mainRunReplay) > 0 || m.mainRunLive != nil) {
		return m.listenForMainRunEvents()
	}
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

// invalidateAltScreenStreamingViewportCache discards the rendered viewport and
// append state when a streaming-local command clears the composer. This is
// deliberately narrower than textarea editing: normal keystrokes only change
// the footer and must not rebuild the streaming viewport on every frame.
func (m *Model) invalidateAltScreenStreamingViewportCache() {
	if !m.altScreen {
		return
	}
	m.viewCache.lastViewportView = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
}

func (m *Model) bumpContentVersion() {
	m.viewCache.contentVersion++
	if m.streamPerf != nil {
		m.streamPerf.RecordContentVersionBump()
	}
}
