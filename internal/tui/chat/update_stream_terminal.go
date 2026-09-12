package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) handleStreamError(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer, streamEventStart time.Time) (tea.Model, tea.Cmd, bool, []tea.Cmd, []tea.Cmd) {
	var suspended *llm.SuspendedError
	if errors.As(ev.Err, &suspended) {
		model, command := m.suspendForReload(suspended.Continuation)
		return model, command, true, nil, nil
	}
	m.applyAllPendingCompactionsToUI(msg.generation)
	if ev.Err != nil {
		if m.stats != nil {
			m.stats.Finalize()
		}
		m.setRetryStatus("")
		// Flush any buffered text on error
		if m.smoothBuffer != nil {
			remaining := m.smoothBuffer.FlushAll()
			if remaining != "" {
				m.currentResponse.WriteString(remaining)
				if m.tracker != nil {
					m.tracker.AddTextSegment(remaining, m.width)
				}
			}
			m.smoothBuffer.Reset()
		}
		m.smoothTickPending = false
		// This resets local scheduling state for the next stream.
		// An already in-flight render tick may still arrive and no-op.
		m.streamRenderTickPending = false
		m.preserveStreamingContentOnError()
		errorOutputCmds := m.flushStreamingContentOnErrorToScrollback()
		hadScrollbackOutput := len(errorOutputCmds) > 0
		salvageResult := m.salvageInterruptedAssistantMessage()
		m.resetCurrentReasoning()
		m.streaming = false
		m.restoreSkillAllowedTools()
		m.releaseStreamCancelFunc()
		m.setStreamCancelRequested(false)
		m.err = nil
		var footerCmd tea.Cmd
		if !errors.Is(ev.Err, context.Canceled) {
			_, footerCmd = m.showFooterMessageWithTone(formatStreamErrorFooter(ev.Err), "error")
		}
		// Stream errors are transient status, not durable conversation content.
		// Force the viewport to repaint now so stale streamed rows do not linger
		// until an external resize invalidates Bubble Tea's render cache.
		if m.altScreen {
			m.viewCache.historyValid = false
			m.viewCache.lastViewportView = ""
			m.viewCache.cachedCompletedContent = ""
			m.viewCache.cachedTrackerVersion = 0
			m.resetAltScreenStreamingAppendCache()
			m.bumpContentVersion()
		}

		// Clear callbacks and update status
		m.clearStreamCallbacks()
		if m.store != nil && m.sess != nil {
			// Use interrupted for cancellation, error for other failures
			status := session.StatusError
			if errors.Is(ev.Err, context.Canceled) {
				status = session.StatusInterrupted
			}
			ctx := context.Background()
			_ = m.store.UpdateStatus(ctx, m.sess.ID, status)
			if err := m.reloadMessagesFromStore(ctx); err != nil {
				if footerCmd == nil {
					_, footerCmd = m.showFooterMessageWithTone(fmt.Sprintf("Session message reload failed after interruption: %v", err), "error")
				}
			} else {
				m.mergeUnpersistedInterruptedAssistant(salvageResult)
				if cmd := m.loadPersistedSubagentsCmd(); cmd != nil {
					errorOutputCmds = append(errorOutputCmds, cmd)
				}
			}
		}

		m.flushPendingSkillResults()
		if cmd := m.applyPendingStreamModelSwitch(); cmd != nil {
			errorOutputCmds = append(errorOutputCmds, cmd)
		}
		if cmd := m.startNextQueuedMainSkill(); cmd != nil {
			errorOutputCmds = append(errorOutputCmds, cmd)
		}

		if m.streamPerf != nil {
			m.streamPerf.RecordDuration(durationMetricStreamEvent, time.Since(streamEventStart))
			m.streamPerf.EmitSummaryIfActive(time.Now())
		}

		m.textarea.Focus()

		// Recover pending steering text into textarea on error.
		// If the engine queue is already empty but we never rendered the
		// steering inline, fall back to the visible pending draft.
		m.restorePendingSteeringDraft()
		if m.steeringHandoff == "" && len(m.listPendingSteering()) == 0 {
			m.clearPendingSteering()
		}

		titleCmd := m.terminalTitleCmd()
		if m.altScreen {
			// Rush is a continuous handoff, not a failed conversation. A
			// queued ClearScreen can erase the next frame while startup waits.
			if m.steeringHandoff != "" {
				return m, tea.Batch(footerCmd, titleCmd), true, nil, nil
			}
			return m, tea.Batch(tea.ClearScreen, footerCmd, titleCmd), true, nil, nil
		}
		if titleCmd != nil {
			errorOutputCmds = append(errorOutputCmds, titleCmd)
		}
		if footerCmd != nil {
			errorOutputCmds = append(errorOutputCmds, footerCmd)
		}
		if hadScrollbackOutput {
			m.inlineCompletionPending = true
			m.inlineCompletionPendingKey = nil
			errorOutputCmds = append(errorOutputCmds, func() tea.Msg { return inlineCompletionRenderedMsg{} })
		}
		return m, tea.Sequence(errorOutputCmds...), true, nil, nil
	}
	return m, nil, false, state.cmds, state.flushCmds
}

func (m *Model) handleStreamDone(msg streamEventMsg, ev ui.StreamEvent, state *streamCommandBuffer) (tea.Model, tea.Cmd, bool, []tea.Cmd, []tea.Cmd) {
	if m.stats != nil {
		m.stats.Finalize()
	}
	m.applyAllPendingCompactionsToUI(msg.generation)
	m.setRetryStatus("")
	m.currentTokens = ev.Tokens
	m.resetAttemptUsage()

	// Flush any remaining buffered text and mark done
	if m.smoothBuffer != nil {
		remaining := m.smoothBuffer.FlushAll()
		if remaining != "" {
			m.currentResponse.WriteString(remaining)
			if m.tracker != nil {
				m.tracker.AddTextSegment(remaining, m.width)
			}
		}
		m.smoothBuffer.MarkDone()
	}

	m.persistContextEstimate(context.Background())
	m.streaming = false
	m.restoreSkillAllowedTools()
	m.releaseStreamCancelFunc()
	m.setStreamCancelRequested(false)

	// Flag to scroll to bottom after response completes (alt screen mode)
	if m.altScreen {
		m.scrollToBottom = true
	}

	// Clear callbacks
	m.clearStreamCallbacks()

	// Mark all text segments as complete and render
	m.commitCurrentReasoningToStream()
	if m.tracker != nil {
		m.tracker.CompleteTextSegments(func(text string) string {
			return m.renderMarkdown(text)
		})

		if m.altScreen {
			if !m.mainRunViewComplete {
				// A durable-prefix fallback may only contain events observed after
				// reattachment. Do not let that partial tracker hide the complete
				// persisted assistant/tool turn after the session reload below.
				m.viewCache.completedStream = ""
			} else {
				// The stream is finished, so no tool should still be pending.
				// Force any stragglers complete before rendering: the tracker
				// is now retained after done (for reasoning click-toggling),
				// and CompletedSegments() truncates at the first pending tool,
				// so a stale pending tool would otherwise drop trailing content
				// both here and in later rerenderCompletedStreamFromTracker calls.
				m.tracker.ForceCompletePendingTools()
				// In alt screen mode, save the full rendered content to completedStream.
				// This preserves the correct position of images/diffs relative to text.
				// The last assistant message will be skipped in renderHistory() to avoid duplication.
				completed := m.tracker.CompletedSegments()
				m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(completed, m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
				m.bumpContentVersion()
			}
		} else {
			// In inline mode, print remaining content to scrollback before
			// acknowledging that the normal composer may submit another turn.
			m.tracker.ForceCompletePendingTools()
			result := m.tracker.FlushAllRemaining(m.width, 0, m.renderMd)
			state.flushCmds = append(state.flushCmds, ui.ScrollbackPrintlnCommands(result.ToPrint, true)...)
		}
	} else if !m.altScreen {
		state.flushCmds = append(state.flushCmds, ui.ScrollbackPrintlnCommands("", true)...)
	}
	if !m.altScreen {
		m.inlineCompletionPending = true
		m.inlineCompletionPendingKey = nil
		state.flushCmds = append(state.flushCmds, func() tea.Msg { return inlineCompletionRenderedMsg{} })
	}

	// Sync in-memory messages with persisted state.
	// Keep full scrollback loaded for live sessions, but recompute the
	// compacted-window prefix so the next LLM request skips old history.
	if m.store != nil {
		ctx := context.Background()
		if err := m.refreshSessionFromStore(ctx); err != nil {
			_, cmd := m.showFooterError(fmt.Sprintf("Session refresh failed after compaction: %v", err))
			state.cmds = append(state.cmds, cmd)
		} else if loadedMsgs, compactionIdx, err := loadSessionMessagesForScrollback(ctx, m.store, m.sess); err != nil {
			_, cmd := m.showFooterError(fmt.Sprintf("Session message reload failed after compaction: %v", err))
			state.cmds = append(state.cmds, cmd)
		} else {
			m.messagesMu.Lock()
			m.messages = loadedMsgs
			m.compactionIdx = compactionIdx
			m.messagesMu.Unlock()
			m.invalidateHistoryCache()
			if cmd := m.loadPersistedSubagentsCmd(); cmd != nil {
				state.cmds = append(state.cmds, cmd)
			}
		}
		_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusComplete)
	} else {
		// No store - append locally for in-memory only sessions
		responseContent := m.currentResponse.String()
		if responseContent != "" {
			part := llm.Part{Type: llm.PartText, Text: responseContent}
			if reasoningContent, reasoningKind, reasoningTitle := m.currentReasoningPartMetadata(); reasoningContent != "" {
				part.ReasoningContent = reasoningContent
				part.ReasoningKind = reasoningKind
				part.ReasoningSummaryTitle = reasoningTitle
			}
			assistantMsg := session.Message{
				SessionID:   m.sess.ID,
				Role:        llm.RoleAssistant,
				Parts:       []llm.Part{part},
				TextContent: responseContent,
				CreatedAt:   time.Now(),
				Sequence:    len(m.messages),
			}
			m.messages = append(m.messages, assistantMsg)
			m.invalidateHistoryCache()
		}
	}

	// Reset streaming state
	m.currentResponse.Reset()
	m.resetCurrentReasoning()
	m.currentTokens = 0
	m.webSearchUsed = false
	m.setRetryStatus("")
	// Keep the tracker's completed segments alive after the stream ends.
	// In alt-screen mode the finished turn is shown from completedStream,
	// but the tracker still backs reasoning-header click toggling (via
	// rerenderCompletedStreamFromTracker). The tracker is reset when the
	// next assistant turn starts (sendMessage), so leaving it populated
	// here is render-neutral while preserving click metadata.
	if !m.altScreen {
		m.resetTracker()
	}
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.newlineCompactor = nil
	m.smoothTickPending = false
	// This resets local scheduling state for the next stream.
	// An already in-flight render tick may still arrive and no-op.
	m.streamRenderTickPending = false
	if m.streamPerf != nil {
		m.streamPerf.EmitSummaryIfActive(time.Now())
	}

	m.flushPendingSkillResults()
	if cmd := m.applyPendingStreamModelSwitch(); cmd != nil {
		state.cmds = append(state.cmds, cmd)
	}
	if cmd := m.startNextQueuedMainSkill(); cmd != nil {
		state.cmds = append(state.cmds, cmd)
	}

	// Auto-save session
	state.cmds = append(state.cmds, m.saveSessionCmd())

	// Try to rename random-word handover files to descriptive slugs
	if cmd := m.maybeRenameHandoverCmd(); cmd != nil {
		state.cmds = append(state.cmds, cmd)
	}

	// Generate a short task title for session browsers and live terminal titles.
	if cmd := m.maybeGenerateSessionTitleCmd(); cmd != nil {
		state.cmds = append(state.cmds, cmd)
	}

	m.appendTerminalTitleCmd(&state.cmds)

	// In auto-send mode, check if there are more messages to send
	if m.autoSendQueue != nil {
		var messageStatsCmd tea.Cmd
		if summary := m.autoSendMessageStats(); summary != "" {
			// tea.Println writes above the managed program output without
			// bypassing Bubble Tea's renderer.
			messageStatsCmd = tea.Println(summary)
		}

		if len(m.autoSendQueue) > 0 {
			// Validate before removing the queue head. A blocked delegation
			// terminates deterministic auto-send instead of losing an entry and
			// stalling with no stream to trigger the next pop.
			next := m.autoSendQueue[0]
			if _, err := m.agentMentionDelegationContext(next); err != nil {
				m.err = err
				m.quitting = true
				_, footerCmd := m.showFooterError(err.Error())
				failureCmds := []tea.Cmd{footerCmd, tea.Quit}
				if messageStatsCmd != nil {
					failureCmds = append([]tea.Cmd{messageStatsCmd}, failureCmds...)
				}
				return m, ui.ComposeFlushFirstCommands(state.flushCmds, []tea.Cmd{tea.Sequence(failureCmds...)}), true, nil, nil
			}
			m.textarea.SetValue(next)
			m.updateTextareaHeight()
			model, sendCmd := m.sendMessage(next)
			m.autoSendQueue = m.autoSendQueue[1:]
			followupCmd := tea.Sequence(messageStatsCmd, sendCmd)
			return model, ui.ComposeFlushFirstCommands(state.flushCmds, []tea.Cmd{followupCmd}), true, nil, nil
		}

		if m.autoSendExitOnDone {
			// Queue exhausted and exit requested, quit after the final inline
			// response and spacer have reached scrollback.
			m.quitting = true
			var quitCmd tea.Cmd
			if summary := m.exitStatsSummary(); summary != "" {
				quitCmd = m.quitCmd(messageStatsCmd, tea.Println(summary))
			} else {
				quitCmd = m.quitCmd(messageStatsCmd)
			}
			return m, ui.ComposeFlushFirstCommands(state.flushCmds, []tea.Cmd{quitCmd}), true, nil, nil
		}

		// Queue exhausted, continue in interactive mode
		if messageStatsCmd != nil {
			state.cmds = append(state.cmds, messageStatsCmd)
		}
		m.autoSendQueue = nil
	}

	// Re-enable textarea
	m.textarea.Focus()

	// Recover any pending steering that wasn't consumed. If the
	// engine queue is already empty but the UI still shows a pending
	// steering, restore that draft rather than letting it vanish.
	m.restorePendingSteeringDraft()
	if m.steeringHandoff == "" && len(m.listPendingSteering()) == 0 {
		m.clearPendingSteering()
	}
	return m, nil, false, state.cmds, state.flushCmds
}
