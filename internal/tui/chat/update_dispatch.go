package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func handledUpdate(model tea.Model, cmd tea.Cmd) (tea.Model, tea.Cmd, bool) {
	return model, cmd, true
}

func (m *Model) dispatchTerminalUpdate(message tea.Msg, cmds *[]tea.Cmd) (tea.Model, tea.Cmd, bool) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		return handledUpdate(m.handleWindowSizeMsg(msg))
	case tea.KeyPressMsg:
		if m.inlineCompletionPending {
			if m.inlineCompletionPendingKey != nil {
				// Enter has already been accepted for submission. Keep the captured
				// composer state stable until the scrollback acknowledgement, while
				// still allowing the user to cancel that pending submission.
				if key.Matches(msg, m.keyMap.Cancel) || key.Matches(msg, m.keyMap.Quit) {
					m.inlineCompletionPendingKey = nil
					return handledUpdate(m.showFooterMuted("Pending send cancelled."))
				}
				return m, nil, true
			}
			if key.Matches(msg, m.keyMap.Send) {
				pendingKey := msg
				m.inlineCompletionPendingKey = &pendingKey
				return m, nil, true
			}
		}
		return handledUpdate(m.handleKeyMsg(msg))

	case tea.PasteMsg:
		if m.inlineCompletionPendingKey != nil {
			return m, nil, true
		}
		return handledUpdate(m.handlePasteMsg(msg))

	case tea.MouseMsg:
		return handledUpdate(m.handleMouseMsg(msg))
	case spinner.TickMsg:
		if (m.streaming || m.directShellRun != nil || m.sideQuestion.Running || m.branchContextInFlight() || m.sessionTransition != nil || m.commitBusy()) && !m.pausedForExternalUI {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			*cmds = append(*cmds, m.passiveCommand(cmd))
		}

	case tickMsg:
		if m.streaming || m.directShellRun != nil {
			*cmds = append(*cmds, m.tickEvery())
		}

	case streamGoalElapsedMsg:
		if !m.streaming || m.sess == nil || msg.goal == nil || m.sess.ID != msg.sessionID || !m.streamStartTime.Equal(msg.streamStarted) {
			return m, nil, true
		}
		if m.sess.Goal != nil && m.sess.Goal.UpdatedAt.After(msg.goal.UpdatedAt) {
			return m, nil, true
		}
		m.sess.Goal = msg.goal.Clone()
		m.streamElapsedOffset = m.goalStreamElapsedOffset()

	case streamRenderTickMsg:
		m.streamRenderTickPending = false
		// No explicit action is needed here: Bubble Tea re-renders after each Update.
		// This tick exists to ensure View() runs again after the throttle window, so
		// pending content can pass shouldThrottleSetContent().

	case tea.SuspendMsg:
		// Bubble Tea restores the terminal before delivering SuspendMsg to the
		// model. Renderer shutdown has removed Kitty resources, so invalidate all
		// acknowledgements before the first resumed frame is composed.
		m.resetImageUploadState()
		m.invalidateImageViewportContent()

	case postFrameImageResultMsg:
		return m, m.handlePostFrameImageResult(msg.Receipt, msg.Err), true

	case footerMessageClearMsg:
		if msg.Seq == m.footerMessageSeq {
			m.clearFooterMessage()
		}

	case FooterNoticeMsg:
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			return m, nil, true
		}
		tone := msg.Tone
		if tone == "" {
			tone = "error"
		}
		return handledUpdate(m.showFooterMessageWithTone(text, tone))

	case copyResultMsg:
		return handledUpdate(m.handleCopyResult(msg))

	case copyStatusClearMsg:
		if msg.seq == m.copyStatusSeq {
			m.copyStatus = ""
		}

	case directShellOutputMsg:
		return handledUpdate(m.handleDirectShellOutput(msg))

	case directShellDoneMsg:
		return handledUpdate(m.handleDirectShellDone(msg))

	case shellExitedMsg:
		m.setShellTerminalHandoff(false)
		if msg.err != nil {
			return handledUpdate(m.showFooterError(fmt.Sprintf("Shell failed: %v", msg.err)))
		}
		if msg.exitCode != 0 {
			return handledUpdate(m.showFooterMuted(fmt.Sprintf("Shell exited with status %d.", msg.exitCode)))
		}
		return handledUpdate(m.showFooterMuted("Shell exited."))

	case worktreeOperationDoneMsg:
		return handledUpdate(m.handleWorktreeOperationDone(msg))
	default:
		return nil, nil, false
	}
	return nil, nil, true
}

func (m *Model) dispatchFeatureUpdate(message tea.Msg, cmds *[]tea.Cmd) (tea.Model, tea.Cmd, bool) {
	switch msg := message.(type) {

	case shareCapabilitiesMsg:
		return handledUpdate(m.handleShareCapabilities(msg))

	case shareDoneMsg:
		return handledUpdate(m.handleShareDone(msg))

	case chatGPTModelsLoadedMsg:
		return handledUpdate(m.applyChatGPTModelsLoaded(msg))

	case providerUsageDoneMsg:
		return handledUpdate(m.handleProviderUsageDone(msg))

	case transcriptMutationDoneMsg:
		return handledUpdate(m.handleTranscriptMutationDone(msg))

	case conversationBranchCreatedMsg:
		return handledUpdate(m.handleConversationBranchCreated(msg))

	case conversationBranchNotesDoneMsg:
		return handledUpdate(m.handleConversationBranchNotesDone(msg))

	case promptHistoryLookupMsg:
		return handledUpdate(m.handlePromptHistoryLookupMsg(msg))

	case mcpOAuthResultMsg:
		m.refreshMCPPickerIfOpen()
		if msg.err != nil {
			detail := safeMCPOAuthMessage(msg.err)
			action := fmt.Sprintf("Try `/mcp login %s` again.", msg.name)
			if msg.logout {
				action = fmt.Sprintf("Try `/mcp logout %s` again.", msg.name)
			}
			m.dialog.ShowContent("MCP authentication", detail+"\n\n"+action)
			_, footerCmd := m.showFooterMessageWithTone("MCP authentication failed: "+detail, "error")
			*cmds = append(*cmds, footerCmd)
		} else {
			verb := "Signed in to"
			if msg.logout {
				verb = "Signed out of"
			}
			_, footerCmd := m.showFooterMessage(verb + " MCP server " + msg.name)
			*cmds = append(*cmds, footerCmd)
		}

	case mcpStatusUpdateMsg:
		m.refreshMCPPickerIfOpen()
		*cmds = append(*cmds, m.listenForMCPStatusUpdates())
		if msg.update.Status == mcp.StatusFailed {
			failureMessage := formatMCPFailureMessage(msg.update)
			*cmds = append(*cmds, tea.Println(m.renderMarkdown(failureMessage)+"\n"))
			footer := fmt.Sprintf("MCP server %s failed", msg.update.Name)
			if msg.update.Error != nil {
				footer += ": " + strings.Join(strings.Fields(msg.update.Error.Error()), " ")
			}
			_, footerCmd := m.showFooterMessageWithToneFor(footer, "error", mcpFailureFooterDuration)
			*cmds = append(*cmds, footerCmd)
		}

	case GuardianReviewMsg:
		m.recordGuardianUsage(context.Background(), msg.Event.Model, msg.Event.Usage)
		message := strings.TrimSpace(msg.Event.Message)
		tone := guardianFooterTone(message)
		if m.tracker != nil {
			if msg.Event.ToolCallID != "" {
				m.tracker.HandleGuardianEvent(msg.Event)
			} else if message != "" {
				// Session-level guardian status (for example a circuit breaker)
				// has no tool row to annotate, so retain it durably in the stream.
				m.tracker.AddExternalUIResult(message)
			}
			m.invalidateViewCache()
		}
		_, cmd := m.showFooterMessageWithTone(message, tone)
		if cmd != nil {
			*cmds = append(*cmds, cmd)
		}

	case compactDoneMsg:
		return handledUpdate(m.handleCompactDone(msg))
	case handoverDoneMsg:
		return handledUpdate(m.handleHandoverDone(msg))
	case handoverConfirmMsg:
		return handledUpdate(m.handleHandoverConfirm(msg))
	case handoverCancelMsg:
		return handledUpdate(m.handleHandoverCancel(msg))
	case handoverRenameDoneMsg:
		// Silently ignore — rename is best-effort background work.
		return m, nil, true

	case titleFallbackTickMsg:
		return handledUpdate(m.handleTitleFallbackTick(msg))
	case titleGeneratedMsg:
		return handledUpdate(m.handleTitleGenerated(msg))
	default:
		return nil, nil, false
	}
	return nil, nil, true
}

func (m *Model) dispatchConversationUpdate(message tea.Msg, cmds *[]tea.Cmd, flushCmds *[]tea.Cmd) (tea.Model, tea.Cmd, bool) {
	switch msg := message.(type) {

	case ui.WaveTickMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWaveTick(); cmd != nil {
				*cmds = append(*cmds, m.passiveCommand(cmd))
			}
		}

	case ui.WavePauseMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWavePause(); cmd != nil {
				*cmds = append(*cmds, m.passiveCommand(cmd))
			}
		}

	case startupWorkspaceApprovalMsg:
		if m.rootContext().Err() != nil {
			return m, nil, true
		}
		if errors.Is(msg.err, tools.ErrWorkspaceApprovalCancelled) {
			return handledUpdate(m.showFooterMuted("Workspace decision deferred; file tools will ask again when needed."))
		}
		if msg.err != nil {
			return handledUpdate(m.showFooterWarning("Workspace access was not granted; your message was not sent."))
		}
		return m, m.initialAutoSendCmd(), true

	case autoSendMsg:
		m.autoSendPending = false
		// Auto-send has no editable recovery loop. Fail fast and retain the queued
		// input instead of silently dropping it and waiting forever for a stream.
		if _, err := m.agentMentionDelegationContext(m.textarea.Value()); err != nil {
			m.err = err
			m.quitting = true
			_, footerCmd := m.showFooterError(err.Error())
			return m, tea.Sequence(footerCmd, tea.Quit), true
		}
		if draft := m.transitionAutoSendDraft; draft != nil {
			m.transitionAutoSendDraft = nil
			updated, cmd := m.sendMessage(m.textarea.Value())
			model := updated.(*Model)
			model.restoreComposerSnapshot(draft.composer)
			model.files = draft.files
			model.images = draft.images
			model.selectedImage = draft.selectedImage
			model.pasteChunks = draft.pasteChunks
			return model, cmd, true
		}
		return handledUpdate(m.sendMessage(m.textarea.Value()))

	case ui.SmoothTickMsg:
		m.handleSmoothTick(msg, cmds, flushCmds)
	case streamEventMsg:
		updated, streamCmd, immediate, streamCmds, streamFlushCmds := m.handleStreamEvent(msg)
		if immediate {
			return updated, streamCmd, true
		}
		*cmds = append(*cmds, streamCmds...)
		*flushCmds = append(*flushCmds, streamFlushCmds...)
	case sessionSavedMsg:
		// Session saved successfully, nothing to do

	case sessionLoadedMsg:
		if msg.sess != nil {
			m.sess = msg.sess
			m.messages = msg.messages
			m.seedStatsFromSession()
			m.configureContextManagementForSession()
			m.resetTitleGenerationStateForSession()
			m.olderScrollbackLoaded = true
			m.invalidateHistoryCache()
			m.scrollOffset = 0
			if m.store != nil {
				_ = m.store.SetCurrent(context.Background(), m.sess.ID)
			}
		}

	case FlushBeforeAskUserMsg:
		return handledUpdate(m.flushBeforeExternalUI(msg.Done))

	case FlushBeforeApprovalMsg:
		return handledUpdate(m.flushBeforeExternalUI(msg.Done))

	case ResumeFromExternalUIMsg:
		return handledUpdate(m.handleResumeFromExternalUI(msg))
	case ApprovalRequestMsg:
		return handledUpdate(m.handleApprovalRequest(msg))
	case AskUserRequestMsg:
		return handledUpdate(m.handleAskUserRequest(msg))
	case HandoverRequestMsg:
		return handledUpdate(m.handleHandoverRequest(msg))
	case SubagentProgressMsg:
		// Handle subagent progress events and update segment stats.
		if msg.Event.Type == tools.SubagentEventGuardian && msg.Event.Guardian != nil {
			m.recordGuardianUsage(context.Background(), msg.Event.Guardian.Model, msg.Event.Guardian.Usage)
		}
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, msg.CallID, msg.Event)
		if msg.Event.Type == tools.SubagentEventToolEnd {
			for _, media := range msg.Event.Media {
				if reference := strings.ToLower(strings.TrimSpace(media.Reference)); reference != "" {
					if m.mediaByReference == nil {
						m.mediaByReference = make(map[string]llm.MediaArtifact)
					}
					m.mediaByReference[reference] = media
				}
			}
		}
	default:
		return nil, nil, false
	}
	return nil, nil, true
}
