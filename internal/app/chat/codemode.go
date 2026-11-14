package chat

import (
	tea "charm.land/bubbletea/v2"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/tools"
	tuichat "github.com/term-llm/term-llm/internal/tui/chat"
)

func bindChatCodeModePrompts(manager *tools.ToolManager, send func(tea.Msg)) {
	if manager == nil {
		return
	}
	manager.CodeModePromptBinder = func(ref core.CodeModeCellRef) tools.CodeModePrompts {
		await := func(target string, write, shell, workspace bool, dir string) (tools.ApprovalResult, error) {
			done := make(chan tools.ApprovalResult, 1)
			send(tuichat.ApprovalRequestMsg{Context: ref.Ctx, NoAutoApprove: true, Path: target, IsWrite: write, IsShell: shell, IsWorkspace: workspace, WorkDir: dir, DoneCh: done})
			defer send(tuichat.DismissApprovalMsg{DoneCh: done})
			select {
			case result := <-done:
				return result, nil
			case <-ref.Ctx.Done():
				return tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}, ref.Ctx.Err()
			}
		}
		return tools.CodeModePrompts{Serial: true,
			Prompt: func(path string, write, shell bool, dir string) (tools.ApprovalResult, error) {
				return await(path, write, shell, false, dir)
			},
			Workspace: func(path string) (tools.WorkspaceApprovalResult, error) {
				r, err := await(path, true, false, true, "")
				return tools.WorkspaceResult(r, err)
			},
			SharedShell: func(command string) (tools.ApprovalResult, error) { return await(command, false, true, false, "") },
		}
	}
}
