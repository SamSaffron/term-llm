package chat

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

// disposeChatSessionRuntime releases a runtime replaced by an in-process
// session switch. Pending child lifecycle delivery retains its execution
// resources, even between main runs. Otherwise an active main run adopts cleanup,
// or cleanup runs off the UI goroutine after a bounded stream drain.
func disposeChatSessionRuntime(rt *chatSessionRuntime, mainRuns *chat.MainRunManager) {
	if rt == nil {
		return
	}
	if mainRuns != nil && mainRuns.AdoptAgentWakeResources(rt.model.SessionID(), rt.cleanupResources) {
		return
	}
	if mainRuns != nil && mainRuns.AdoptResources(rt.model.SessionID(), rt.cleanupResources) {
		return
	}
	go func() {
		if !rt.model.WaitStreamDone() {
			rt.model.WaitRuntimeOperations()
		}
		rt.cleanupResources()
	}()
}

// wireChatSessionUI points program-level UI callbacks at one session runtime.
// Callbacks installed on runtime-owned managers (Guardian prompts, subagent
// progress) stay bound to that runtime for its whole life so adopted
// background runs keep routing prompts to their owning session; the returned
// unwire only releases process-global hooks and the attached UI sink.
//
// attachSink installs the interactive-prompt sink immediately; it must be
// false on the in-process switch path, where the sink is attached by the swap
// handler only once the replacement model is the one the program renders.
func wireChatSessionUI(ctx context.Context, rt *chatSessionRuntime, p *tea.Program, mainRuns *chat.MainRunManager, attachSink bool) (unwire func()) {
	model := rt.model
	toolMgr := rt.toolMgr
	approvalMgr := rt.approvalMgr
	useAltScreen := rt.useAltScreen

	rt.storeWarnings.Attach(p)

	sessionUIID := model.SessionID()
	sendSessionUI := func(message tea.Msg) {
		if mainRuns != nil {
			if err := mainRuns.DeliverUI(sessionUIID, message); err == nil {
				return
			}
		}
		p.Send(message)
	}
	if mainRuns != nil && attachSink {
		model.AttachMainRunUISink(p.Send)
	}

	// Set up spawn_agent event callback for subagent progress visibility
	if toolMgr != nil {
		if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
			dispatcher := runtime.NewSubagentProgressDispatcher(func(callID string, event tools.SubagentEvent) {
				sendSessionUI(chat.SubagentProgressMsg{CallID: callID, Event: event})
			})
			spawnTool.SetEventCallback(dispatcher.Callback)
		}
	}

	// Set up the improved approval UI with git-aware heuristics.
	// This also powers /handover script approvals even when no shell tool is enabled.
	if approvalMgr != nil {
		approvalMgr.GuardianEventFunc = func(event tools.GuardianEvent) {
			sendSessionUI(chat.GuardianReviewMsg{Event: event})
		}
		approvalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
			// Process-scoped runs must never release or restore a stale Bubble Tea
			// program after navigation. Route their prompts through the attachable
			// in-program dialog even when the foreground uses inline rendering.
			if useAltScreen || mainRuns != nil {
				doneCh := make(chan tools.ApprovalResult, 1)
				sendSessionUI(chat.ApprovalRequestMsg{
					Path: path, IsWrite: isWrite, IsShell: isShell, WorkDir: workDir, DoneCh: doneCh,
				})
				select {
				case result := <-doneCh:
					return result, nil
				case <-ctx.Done():
					return tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}, fmt.Errorf("cancelled: %w", ctx.Err())
				}
			}

			// Inline mode: use external UI with terminal release.
			done := make(chan struct{})
			sendSessionUI(chat.FlushBeforeApprovalMsg{Done: done})
			<-done
			p.ReleaseTerminal()
			defer func() {
				p.RestoreTerminal()
				sendSessionUI(chat.ResumeFromExternalUIMsg{})
			}()
			if isShell {
				return tools.RunShellApprovalUI(path, workDir)
			}
			return tools.RunFileApprovalUI(path, isWrite)
		}
		approvalMgr.WorkspacePromptFunc = func(workspace string) (tools.WorkspaceApprovalResult, error) {
			if useAltScreen || mainRuns != nil {
				doneCh := make(chan tools.ApprovalResult, 1)
				sendSessionUI(chat.ApprovalRequestMsg{Path: workspace, IsWorkspace: true, DoneCh: doneCh})
				select {
				case result := <-doneCh:
					choice := result.Choice
					return tools.WorkspaceApprovalResult{
						Approved:  !result.Cancelled && (choice == tools.ApprovalChoiceWorkspace || choice == tools.ApprovalChoiceWorkspaceRemember),
						Remember:  !result.Cancelled && choice == tools.ApprovalChoiceWorkspaceRemember,
						Cancelled: result.Cancelled,
					}, nil
				case <-ctx.Done():
					return tools.WorkspaceApprovalResult{Cancelled: true}, fmt.Errorf("cancelled: %w", ctx.Err())
				}
			}

			done := make(chan struct{})
			sendSessionUI(chat.FlushBeforeApprovalMsg{Done: done})
			<-done
			p.ReleaseTerminal()
			defer func() {
				p.RestoreTerminal()
				sendSessionUI(chat.ResumeFromExternalUIMsg{})
			}()
			return tools.RunWorkspaceApprovalUI(workspace)
		}
		if toolManagerHasPathCapableTools(toolMgr) {
			model.SetStartupWorkspaceApproval(func() error {
				return approvalMgr.EnsurePrimaryWorkspaceAccess(ctx)
			})
		}
	}

	// Set up ask_user handling
	if useAltScreen {
		// In alt screen mode, use inline rendering
		tools.SetAskUserUIFunc(func(questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
			// Use buffered channel to prevent goroutine leak if TUI exits before responding
			doneCh := make(chan []tools.AskUserAnswer, 1)
			sendSessionUI(chat.AskUserRequestMsg{
				Questions: questions,
				DoneCh:    doneCh,
			})
			// Block until user responds or context is cancelled
			select {
			case answers := <-doneCh:
				if answers == nil {
					return nil, fmt.Errorf("cancelled by user")
				}
				return answers, nil
			case <-ctx.Done():
				return nil, fmt.Errorf("cancelled: %w", ctx.Err())
			}
		})
	} else {
		// In inline mode, use external UI with hooks
		start, end := tools.CreateTUIHooks(p, func() {
			done := make(chan struct{})
			sendSessionUI(chat.FlushBeforeAskUserMsg{Done: done})
			<-done
		})
		// Wrap end hook to also send resume message after terminal is restored
		originalEnd := end
		end = func() {
			originalEnd()
			sendSessionUI(chat.ResumeFromExternalUIMsg{})
		}
		tools.SetAskUserHooks(start, end)
	}

	// Set up initiate_handover handling — works in both alt screen and inline modes
	// because cmdHandover already handles both.
	tools.SetHandoverUIFunc(func(toolCtx context.Context, agent string) (bool, error) {
		doneCh := make(chan bool, 1)
		sendSessionUI(chat.HandoverRequestMsg{
			Agent:  agent,
			DoneCh: doneCh,
		})
		select {
		case confirmed := <-doneCh:
			return confirmed, nil
		case <-toolCtx.Done():
			return false, toolCtx.Err()
		}
	})

	return func() {
		// Covers both attach paths: the model records the manager detach for
		// sinks installed here and for sinks installed by the swap handler.
		model.DetachMainRunUISink()
		rt.storeWarnings.Detach()
		if useAltScreen {
			tools.ClearAskUserUIFunc()
		} else {
			tools.ClearAskUserHooks()
		}
		tools.ClearHandoverUIFunc()
	}
}
