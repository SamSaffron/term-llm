package chat

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) handleStreamProgress(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	switch ev.Type {
	case ui.StreamEventToolStart:
		m.handleStreamToolStart(msg, ev, state)
	case ui.StreamEventToolEnd:
		m.handleStreamToolEnd(msg, ev, state)
	case ui.StreamEventUsage:
		m.handleStreamUsage(msg, ev, state)
	case ui.StreamEventText:
		m.handleStreamText(msg, ev, state)
	case ui.StreamEventGenerationActivity:
		m.handleStreamGenerationActivity(msg, ev, state)
	case ui.StreamEventReasoning:
		m.handleStreamReasoning(msg, ev, state)
	case ui.StreamEventAttemptDiscard:
		m.handleStreamAttemptDiscard(msg, ev, state)
	case ui.StreamEventPhase:
		m.handleStreamPhase(msg, ev, state)
	case ui.StreamEventModelSwitch:
		m.handleStreamModelSwitch(msg, ev, state)
	case ui.StreamEventRetry:
		m.handleStreamRetry(msg, ev, state)
	case ui.StreamEventImage:
		m.handleStreamImage(msg, ev, state)
	case ui.StreamEventMedia:
		m.handleStreamMedia(msg, ev, state)
	case ui.StreamEventDiff:
		m.handleStreamDiff(msg, ev, state)
	case ui.StreamEventSteering:
		m.handleStreamSteering(msg, ev, state)
	}
}

func (m *Model) handleStreamToolStart(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.commitCurrentReasoningToStream()
	m.setRetryStatus("")
	m.markAttemptCommitted()
	m.stats.ToolStart()
	if m.tracker != nil {
		m.tracker.SetExpandHintShown(m.toolExpandHintShown)
	}

	// Flush smooth buffer before tool starts (user wants to see tool output right away)
	if m.smoothBuffer != nil {
		remaining := m.smoothBuffer.FlushAll()
		if remaining != "" {
			m.currentResponse.WriteString(remaining)
			if m.tracker != nil {
				m.tracker.AddTextSegment(remaining, m.width)
			}
		}
		m.smoothTickPending = false
	}

	// Mark current text segment as complete before starting tool
	if m.tracker != nil {
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return m.renderMarkdown(text)
		})
		if m.tracker.HandleToolStart(ev.ToolCallID, ev.ToolName, ev.ToolInfo, ev.ToolArgs) {
			m.toolExpandHintShown = m.tracker.ExpandHintShown()
			// New segment added, start wave animation (but not for ask_user which has its own UI)
			if ev.ToolName != tools.AskUserToolName {
				state.cmds = append(state.cmds, m.tracker.StartWave())
			}
		} else {
			// Already have pending segment, just restart wave (but not for ask_user)
			if ev.ToolName != tools.AskUserToolName {
				state.cmds = append(state.cmds, m.tracker.StartWave())
			}
		}
		ui.AttachSubagentProgressToSegment(m.tracker, m.subagentTracker, ev.ToolCallID)
	}

	// Check for web search
	if ev.ToolName == llm.WebSearchToolName || ev.ToolName == "WebSearch" {
		m.webSearchUsed = true
	}

}

func (m *Model) handleStreamToolEnd(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.setRetryStatus("")
	m.resetAttemptUsage()
	m.stats.ToolEnd()
	// Update segment status
	if m.tracker != nil {
		m.tracker.HandleToolEndWithInfo(ev.ToolCallID, ev.ToolSuccess, ev.ToolInfo)

		// Remove from subagent tracker when spawn_agent completes
		if m.subagentTracker != nil {
			m.subagentTracker.Remove(ev.ToolCallID)
		}

		// Back to thinking phase if no more pending tools
		if !m.tracker.HasPending() {
			m.phase = "Thinking"
		}

		// Flush completed segments (chronological order)
		if m.scrollOffset == 0 {
			if cmd := m.maybeFlushToScrollback(); cmd != nil {
				state.flushCmds = append(state.flushCmds, cmd)
			}
		}
	}

}

func (m *Model) handleStreamUsage(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if m.stats != nil {
		m.stats.GenerationEnd()
	}
	inputTokens := ev.InputTokens
	outputTokens := ev.OutputTokens
	cachedTokens := ev.CachedTokens
	writeTokens := ev.WriteTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if writeTokens < 0 {
		writeTokens = 0
	}
	if m.stats != nil {
		// Even an all-zero terminal usage event consumes pending request
		// timing; SessionStats intentionally does not count it as a call.
		m.stats.AddUsage(inputTokens, outputTokens, cachedTokens, writeTokens)
	}
	if inputTokens > 0 || outputTokens > 0 || cachedTokens > 0 || writeTokens > 0 {
		if !m.attemptUsageCommitted {
			m.attemptInput += inputTokens
			m.attemptOutput += outputTokens
			m.attemptCached += cachedTokens
			m.attemptCacheWrite += writeTokens
			m.attemptUsageCalls++
		}
	}
}

func (m *Model) handleStreamText(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.commitCurrentReasoningToStream()
	m.attemptUsageCommitted = false
	if m.stats != nil && ev.Text != "" {
		m.stats.ObserveOutput()
	}
	text := ev.Text
	if m.newlineCompactor == nil {
		m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
	}
	text = m.newlineCompactor.CompactChunk(text)
	if text == "" {
		return
	}

	// Buffer text for smooth 60fps rendering instead of immediate display
	if m.smoothBuffer != nil {
		m.smoothBuffer.Write(text)
		if m.streamPerf != nil {
			m.streamPerf.RecordTextDelta(text, m.smoothBuffer.Len())
		}
		// Start smooth tick if not already running
		if !m.smoothTickPending {
			m.smoothTickPending = true
			if m.streamPerf != nil {
				m.streamPerf.RecordSmoothTickScheduled()
			}
			state.cmds = append(state.cmds, m.passiveCommand(ui.SmoothTick()))
		}
	} else {
		// Fallback: direct display if no smooth buffer
		m.currentResponse.WriteString(text)
		if m.tracker != nil {
			m.tracker.AddTextSegment(text, m.width)
		}
		if m.streamPerf != nil {
			m.streamPerf.RecordTextDelta(text, 0)
		}
	}

	m.phase = "Responding"
	m.setRetryStatus("")

}

func (m *Model) handleStreamGenerationActivity(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if m.stats != nil {
		m.stats.ObserveOutput()
	}

}

func (m *Model) handleStreamReasoning(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if m.stats != nil && ev.ReasoningText != "" {
		m.stats.ObserveOutput()
	}
	m.handleReasoningStreamEvent(ev)

}

func (m *Model) handleStreamAttemptDiscard(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if m.stats != nil {
		m.stats.DiscardUsage(m.attemptInput, m.attemptOutput, m.attemptCached, m.attemptCacheWrite, m.attemptUsageCalls)
	}
	m.resetAttemptUsage()
	m.currentResponse.Reset()
	m.pendingMu.Lock()
	m.pendingAssistantSnapshot = llm.Message{}
	m.pendingAssistantSnapshotSet = false
	m.pendingMu.Unlock()
	m.resetCurrentReasoning()
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.smoothTickPending = false
	m.streamRenderTickPending = false
	if m.tracker != nil {
		m.tracker.DiscardAttempt()
	}
	m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
	m.setRetryStatus("Interrupted response discarded; retrying...")
	m.phase = "Retrying"
	m.viewCache.cachedCompletedContent = ""
	m.viewCache.cachedTrackerVersion = 0
	m.viewCache.lastViewportView = ""
	m.resetAltScreenStreamingAppendCache()
	// Discard is a rollback, not another append. Force the very next View to
	// rebuild viewport content immediately even when streaming render throttling
	// is active; otherwise stale partial text can remain visible until resize or
	// the next throttle tick.
	m.viewCache.lastSetContentAt = time.Time{}
	m.scrollToBottom = true
	m.bumpContentVersion()
	if m.altScreen {
		state.cmds = append(state.cmds, tea.ClearScreen)
	}

}

func (m *Model) handleStreamPhase(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if ev.Phase == llm.PhaseCompactingResumeTask {
		// The resume phase is the ordered handoff before continuation provider
		// text. Earlier stream events stay ahead of the durable boundary.
		if m.applyNextPendingCompactionToUI(msg.generation) {
			m.resetLivePresentationAfterCompaction()
		}
	}
	m.phase = ev.Phase
	m.setRetryStatus("")
	// Display WARNING phases as visible text in the conversation
	if strings.HasPrefix(ev.Phase, llm.WarningPhasePrefix) && m.tracker != nil {
		m.tracker.AddTextSegment(ev.Phase+"\n", m.width)
	}

}

func (m *Model) handleStreamModelSwitch(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if m.stats != nil {
		m.stats.SetModel(ev.Text)
	}
	if m.markPendingStreamModelSwitchApplied(ev.Text) {
		m.bumpContentVersion()
	}

}

func (m *Model) handleStreamRetry(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	if m.stats != nil {
		m.stats.ScheduleRetryStart(ev.RetryWait)
	}
	m.setRetryStatus(ev.RetryStatus("Retrying stream", 1, "..."))

}

func (m *Model) handleStreamImage(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.setRetryStatus("")
	// Add image segment for inline display
	if m.tracker != nil && ev.ImagePath != "" {
		m.tracker.AddImageSegment(ev.ImagePath)
		// In alt-screen mode, pre-render now so View can attach image
		// upload/placement bytes to its renderer-owned PostFrame. Rendering
		// again in View hits the image cache and reuses the display cells.
		if m.altScreen {
			_ = m.renderViewportImageArtifact(ev.ImagePath)
		}
		// Flush to scrollback so image appears
		if m.scrollOffset == 0 {
			if cmd := m.maybeFlushToScrollback(); cmd != nil {
				state.flushCmds = append(state.flushCmds, cmd)
			}
		}
	}

}

func (m *Model) handleStreamMedia(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.setRetryStatus("")
	if reference := strings.ToLower(strings.TrimSpace(ev.Media.Reference)); reference != "" {
		if m.mediaByReference == nil {
			m.mediaByReference = make(map[string]llm.MediaArtifact)
		}
		m.mediaByReference[reference] = ev.Media
	}

}

func (m *Model) handleStreamDiff(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.setRetryStatus("")
	// Add diff segment for inline display
	if m.tracker != nil && ev.DiffPath != "" {
		m.tracker.AddDiffSegmentWithOperation(ev.DiffPath, ev.DiffOld, ev.DiffNew, ev.DiffLine, ev.DiffOperation)
		// Flush to scrollback so diff appears
		if m.scrollOffset == 0 {
			if cmd := m.maybeFlushToScrollback(); cmd != nil {
				state.flushCmds = append(state.flushCmds, cmd)
			}
		}
	}

}

func (m *Model) handleStreamSteering(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) {
	m.setRetryStatus("")
	// User steered a message mid-stream (injected between tool turns).
	matchedPending := false
	switch {
	case ev.SteeringID != "":
		// FIFO steering events may arrive for an older queue item while a newer
		// item is still pending, so remove by ID across the whole stack rather than
		// comparing only with the latest pending row.
		matchedPending = m.removePendingSteeringByID(ev.SteeringID)
	case strings.TrimSpace(ev.Text) != "":
		// Legacy/no-ID fallback: remove the first same-text pending item.
		for i := range m.pendingSteering {
			if strings.TrimSpace(m.pendingSteering[i].Text) == strings.TrimSpace(ev.Text) {
				matchedPending = true
				copy(m.pendingSteering[i:], m.pendingSteering[i+1:])
				m.pendingSteering = m.pendingSteering[:len(m.pendingSteering)-1]
				m.syncLatestPendingSteering()
				break
			}
		}
		if !matchedPending && m.pendingSteeringID == "" && strings.TrimSpace(ev.Text) == strings.TrimSpace(m.pendingSteeringText) {
			matchedPending = true
			m.clearPendingSteering()
		}
	}
	_ = matchedPending
	// Flush smooth buffer so any pending text appears before the steering.
	if m.smoothBuffer != nil {
		remaining := m.smoothBuffer.FlushAll()
		if remaining != "" {
			m.currentResponse.WriteString(remaining)
			if m.tracker != nil {
				m.tracker.AddTextSegment(remaining, m.width)
			}
		}
		m.smoothTickPending = false
	}
	// Mark current text segment as complete before steering
	if m.tracker != nil {
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return m.renderMarkdown(text)
		})
		// Add steering as a pre-rendered text segment.
		// Store the styled version directly so it bypasses markdown rendering
		// and avoids ANSI escape code artifacts in the output.
		theme := m.styles.Theme()
		promptStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
		rendered := promptStyle.Render("❯") + " " + ev.Text + "\n\n"
		m.tracker.AddPreRenderedTextSegment(rendered)
	}
	// Once the steering is visibly injected into the transcript, force the
	// viewport to the bottom so the user sees where it landed.
	m.scrollToBottom = true
	// Persist steered message to session store, preserving structured parts.
	if m.store != nil {
		m.persistSteering(context.Background(), ev.Text, ev.Message)
	}

}

func (m *Model) handleSmoothTick(msg ui.SmoothTickMsg, cmds, flushCmds *[]tea.Cmd) {
	tickStart := time.Now()
	m.smoothTickPending = false
	// Release buffered text word-by-word for smooth 60fps rendering
	if m.smoothBuffer != nil && m.streaming {
		words := m.smoothBuffer.NextWords()
		hadWords := words != ""
		if words != "" {
			m.currentResponse.WriteString(words)
			if m.tracker != nil {
				m.tracker.AddTextSegment(words, m.width)
			}
			m.phase = "Responding"
			// Flush excess content if needed
			if m.scrollOffset == 0 {
				if cmd := m.maybeFlushToScrollback(); cmd != nil {
					*flushCmds = append(*flushCmds, cmd)
				}
			}
		}
		// Continue ticking if not drained
		if !m.smoothBuffer.IsDrained() {
			if !m.smoothTickPending {
				m.smoothTickPending = true
				if m.streamPerf != nil {
					m.streamPerf.RecordSmoothTickScheduled()
				}
				*cmds = append(*cmds, m.passiveCommand(ui.SmoothTick()))
			}
		}
		if m.streamPerf != nil {
			m.streamPerf.RecordSmoothTickHandled(hadWords, m.smoothBuffer.Len())
			m.streamPerf.RecordDuration(durationMetricSmoothTick, time.Since(tickStart))
		}
	}
}
