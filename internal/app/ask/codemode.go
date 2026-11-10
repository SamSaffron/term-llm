package ask

import (
	tea "charm.land/bubbletea/v2"
	"context"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
)

// A one-shot ask still owns its cell prompts until explicit cancellation or
// runtime teardown. Native terminal UIs must not inherit an unbounded callback.
func terminalCodeModePromptBinder(before func(context.Context) error, after func()) func(core.CodeModeCellRef) tools.CodeModePrompts {
	return func(ref core.CodeModeCellRef) tools.CodeModePrompts {
		await := func(target string, write, shell, workspace bool, dir string) (tools.ApprovalResult, error) {
			if before != nil {
				if err := before(ref.Ctx); err != nil {
					return tools.ApprovalResult{Cancelled: true}, err
				}
				if after != nil {
					defer after()
				}
			}
			return tools.RunApprovalUIWithContext(ref.Ctx, target, write, shell, workspace, dir)
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

func bindAskCodeModePrompts(manager *tools.ToolManager, program *tea.Program) {
	manager.CodeModePromptBinder = terminalCodeModePromptBinder(func(ctx context.Context) error {
		done := make(chan struct{})
		program.Send(askFlushBeforeApprovalMsg{Done: done})
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		program.ReleaseTerminal()
		return nil
	}, func() { program.RestoreTerminal(); program.Send(askResumeFromExternalUIMsg{}) })
}
