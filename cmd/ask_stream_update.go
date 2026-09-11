package cmd

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m askStreamModel) handleStreamEventMessage(msg askStreamEventMsg) (tea.Model, tea.Cmd) {
	var innerMsg tea.Msg
	ev := msg.event
	switch ev.Type {
	case ui.StreamEventRetry:
		innerMsg = askRetryMsg{Attempt: ev.RetryAttempt, MaxAttempts: ev.RetryMax, WaitSecs: ev.RetryWait}
	case ui.StreamEventUsage:
		innerMsg = askUsageMsg{InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens}
	case ui.StreamEventPhase:
		innerMsg = askPhaseMsg(ev.Phase)
	case ui.StreamEventToolEnd:
		innerMsg = askToolEndMsg{CallID: ev.ToolCallID, Success: ev.ToolSuccess}
	case ui.StreamEventToolStart:
		innerMsg = askToolStartMsg{CallID: ev.ToolCallID, Name: ev.ToolName, Info: ev.ToolInfo, ToolArgs: ev.ToolArgs}
	case ui.StreamEventGuardian:
		innerMsg = askGuardianReviewMsg{Event: ev.Guardian}
	case ui.StreamEventText:
		innerMsg = askContentMsg(ev.Text)
	case ui.StreamEventReasoning:
		innerMsg = askReasoningMsg(ev)
	case ui.StreamEventAttemptDiscard:
		innerMsg = askAttemptDiscardMsg{}
	case ui.StreamEventImage:
		innerMsg = askImageMsg(ev.ImagePath)
	case ui.StreamEventMedia:
		if reference := strings.ToLower(strings.TrimSpace(ev.Media.Reference)); reference != "" {
			if m.mediaByReference == nil {
				m.mediaByReference = make(map[string]llm.MediaArtifact)
			}
			m.mediaByReference[reference] = ev.Media
		}
	case ui.StreamEventDiff:
		innerMsg = askDiffMsg{Path: ev.DiffPath, Old: ev.DiffOld, New: ev.DiffNew, Line: ev.DiffLine, Operation: ev.DiffOperation}
	case ui.StreamEventDone:
		innerMsg = askDoneMsg{}
	case ui.StreamEventError:
		if ev.Err != nil {
			m.streamErr = ev.Err
			innerMsg = askCancelledMsg{}
		}
	}

	var innerCmd tea.Cmd
	if innerMsg != nil {
		updated, cmd := m.Update(innerMsg)
		m = updated.(askStreamModel)
		innerCmd = cmd
	}

	if ev.Type != ui.StreamEventDone && (ev.Type != ui.StreamEventError || ev.Err == nil) {
		if ev.Type == ui.StreamEventText && m.smoothTickPending {
			m.deferredStreamRead = true
		} else {
			return m, ui.ComposeFlushFirstCommands(nil, []tea.Cmd{innerCmd, m.listenForStreamEvents()})
		}
	}
	return m, innerCmd
}

func (m askStreamModel) handleReasoningMessage(msg askReasoningMsg) (tea.Model, tea.Cmd) {
	ev := ui.StreamEvent(msg)
	cfg := m.reasoningConfig
	kind := llm.NormalizeReasoningKind(ev.ReasoningKind)
	if internalreasoning.IsDisplayable(string(kind), cfg) && internalreasoning.StatusEnabled(cfg) {
		internalreasoning.AppendStreamItemText(m.reasoningBuilder(), &m.currentReasoningItemID, ev.ReasoningText, ev.ReasoningItemID)
		title := strings.TrimSpace(ev.ReasoningTitle)
		if title == "" && kind == llm.ReasoningKindSummary {
			reasoningText := m.currentReasoningString()
			reasoningText = internalreasoning.LimitReasoningText(string(kind), reasoningText, cfg)
			title = internalreasoning.SummaryTitle(reasoningText, cfg)
		}
		if title != "" {
			m.currentReasoningTitle = title
		}
		if m.retryStatus == "" && !m.tracker.HasPending() && m.currentReasoningTitle != "" {
			phase := strings.TrimSpace(m.phase)
			if phase == "" || phase == "Thinking" || strings.HasPrefix(phase, "Thinking:") || m.reasoningPhaseActive {
				// Reasoning-derived status titles are already scoped by the spinner/status UI.
				// Keep the line compact by showing the action directly instead of "Thinking: …".
				m.phase = m.currentReasoningTitle
				m.reasoningPhaseActive = true
			}
		}
	}
	return m, nil
}

func (m askStreamModel) handleDoneMessage(msg askDoneMsg) (tea.Model, tea.Cmd) {
	m.done = true // Prevent spinner from showing in final View()
	m.resetCurrentReasoning()
	m.smoothTickPending = false
	m.deferredStreamRead = false
	// Ensure we have a valid width
	if m.width <= 0 {
		m.width = getTerminalWidth()
	}
	// Flush buffered words before final segment completion.
	m.flushSmoothBufferToTracker()
	if m.smoothBuffer != nil {
		m.smoothBuffer.MarkDone()
	}
	// Complete text segments (finalizes TextBuilder -> Text)
	m.tracker.CompleteTextSegments(func(text string) string {
		return m.renderMd(text)
	})

	// Force-complete any pending tools (defensive - shouldn't happen normally)
	m.tracker.ForceCompletePendingTools()

	// Flush everything remaining to scrollback
	res := m.tracker.FlushAllRemaining(m.width, 0, m.renderMdWithWidth)
	if res.ToPrint != "" {
		return m, tea.Sequence(tea.Printf("%s", res.ToPrint), tea.Quit)
	}
	return m, tea.Quit
}

func (m askStreamModel) handleCancelledMessage(msg askCancelledMsg) (tea.Model, tea.Cmd) {
	if m.streamErr == nil {
		m.streamErr = context.Canceled
	}
	m.done = true
	m.resetCurrentReasoning()
	m.smoothTickPending = false
	m.deferredStreamRead = false
	// Ensure we have a valid width
	if m.width <= 0 {
		m.width = getTerminalWidth()
	}
	// Flush buffered words so cancellation keeps latest visible text.
	m.flushSmoothBufferToTracker()
	if m.smoothBuffer != nil {
		m.smoothBuffer.MarkDone()
	}
	// Flush whatever has been rendered so far (including partial text) to scrollback
	res := m.tracker.FlushAllRemaining(m.width, 0, m.renderMdWithWidth)
	if res.ToPrint != "" {
		return m, tea.Sequence(tea.Printf("%s", res.ToPrint), tea.Quit)
	}
	return m, tea.Quit
}

func (m askStreamModel) handleSmoothTickMessage(msg ui.SmoothTickMsg) (tea.Model, tea.Cmd) {
	if m.smoothBuffer == nil || m.done {
		return m, nil
	}
	m.smoothTickPending = false

	var flushCmds []tea.Cmd
	var asyncCmds []tea.Cmd

	words := m.smoothBuffer.NextWords()
	if words != "" {
		m.tracker.AddTextSegment(words, m.width)
		m.contentDirty = true

		// Flush as soon as we have any safe boundary to avoid duplication/corruption.
		streamingFlushThreshold := m.streamingFlushThreshold()
		if m.width > 0 {
			result := m.tracker.FlushStreamingText(streamingFlushThreshold, m.width, m.renderMdWithWidth)
			if result.ToPrint != "" {
				m.cachedContent = "" // Invalidate cache since state changed
				flushCmds = append(flushCmds, tea.Printf("%s", result.ToPrint))
			}
		}
	}

	if !m.smoothBuffer.IsDrained() {
		if !m.smoothTickPending {
			m.smoothTickPending = true
			asyncCmds = append(asyncCmds, ui.SmoothTick())
		}
	}
	if m.deferredStreamRead && !m.done {
		m.deferredStreamRead = false
		asyncCmds = append(asyncCmds, m.listenForStreamEvents())
	}

	cmd := ui.ComposeFlushFirstCommands(flushCmds, asyncCmds)
	if cmd == nil {
		return m, nil
	}
	return m, cmd
}
