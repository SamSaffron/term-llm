package serve

import (
	"net/http"

	"github.com/term-llm/term-llm/internal/app/serve/approvalprompt"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/tools"
)

func (rt *serveRuntime) codeModeCellPrompts(ref core.CodeModeCellRef) tools.CodeModePrompts {
	return tools.CodeModePrompts{
		Prompt: func(target string, write, shell bool, dir string) (tools.ApprovalResult, error) {
			return rt.awaitCellApproval(ref, target, write, shell, false, dir, "")
		},
		SharedShell: func(command string) (tools.ApprovalResult, error) {
			return rt.awaitCellApproval(ref, command, false, true, false, "", "shared_shell")
		},
		Workspace: func(path string) (tools.WorkspaceApprovalResult, error) {
			r, err := rt.awaitCellApproval(ref, path, true, false, true, "", "")
			return approvalprompt.WorkspaceResult(r, err)
		},
	}
}
func (rt *serveRuntime) awaitCellApproval(ref core.CodeModeCellRef, target string, write, shell, workspace bool, dir, scope string) (tools.ApprovalResult, error) {
	pending := newServePendingApprovalScoped(target, write, shell, workspace, dir, scope)
	pending.CellID = ref.CellID
	rt.approvalMu.Lock()
	if rt.pendingApprovals == nil {
		rt.pendingApprovals = map[string]*servePendingApproval{}
	}
	rt.pendingApprovals[pending.ApprovalID] = pending
	event := rt.approvalEventFunc
	rt.approvalMu.Unlock()
	defer rt.removePendingApproval(pending.ApprovalID, pending)
	if event != nil {
		_ = event("response.approval.prompt", pending.snapshot().Payload())
	}
	// An ended reply or browser disconnect retires presentation only. The cell
	// context is the sole cancellation owner; pending prompts remain discoverable.
	select {
	case submission := <-pending.responseC:
		return submission.Result, submission.Err
	case <-ref.Ctx.Done():
		return tools.ApprovalResult{Cancelled: true, Choice: tools.ApprovalChoiceCancelled}, ref.Ctx.Err()
	}
}
func (rt *serveRuntime) rebindCellApprovals(event func(string, map[string]any) error) {
	for _, p := range rt.pendingApprovalPrompts() {
		if p.CellID != "" {
			_ = event("response.approval.prompt", p.Payload())
		}
	}
}
func (s *serveServer) codeCellSummaries(id string) []core.CodeModeCellSummary {
	if s.sessionMgr == nil {
		return nil
	}
	rt, ok := s.sessionMgr.Get(id)
	if !ok || rt == nil || rt.engine == nil {
		return nil
	}
	cells := rt.engine.CodeModeCells(id)
	prompts := rt.pendingApprovalPrompts()
	for i := range cells {
		for _, p := range prompts {
			if p.CellID == cells[i].CellID && cells[i].Live {
				cells[i].Status = "awaiting_approval"
			}
		}
	}
	return cells
}
func (s *serveServer) handleCodeCells(w http.ResponseWriter, r *http.Request, id, suffix string) {
	if suffix == "code-cells" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
			return
		}
		var prompts []serveApprovalPrompt
		if s.sessionMgr != nil {
			if rt, ok := s.sessionMgr.Get(id); ok && rt != nil {
				for _, p := range rt.pendingApprovalPrompts() {
					if p.CellID != "" {
						prompts = append(prompts, p)
					}
				}
			}
		}
		writeJSON(w, 200, map[string]any{"cells": s.codeCellSummaries(id), "pending_approvals": prompts})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, 415, "invalid_request_error", err.Error())
		return
	}
	var request struct {
		CellID string `json:"cell_id"`
	}
	if err := decodeJSONBody(r, &request); err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	if s.sessionMgr == nil {
		writeOpenAIError(w, 404, "not_found_error", "session runtime unavailable")
		return
	}
	rt, ok := s.sessionMgr.Get(id)
	if !ok || rt == nil || rt.engine == nil {
		writeOpenAIError(w, 404, "not_found_error", "session runtime unavailable")
		return
	}
	if request.CellID != "" {
		_, _ = rt.engine.CancelCodeMode(core.ContextWithSessionID(r.Context(), id), request.CellID)
	} else {
		rt.engine.InterruptCodeModeSession(id)
	}
	writeJSON(w, 200, map[string]any{"cells": s.codeCellSummaries(id)})
}

// cellsProtectRuntime includes borrowed-engine cells without reading mutable session metadata.
func (rt *serveRuntime) cellsProtectRuntime() bool {
	return rt.engine != nil && rt.engine.HasAnyCodeModeCells()
}
