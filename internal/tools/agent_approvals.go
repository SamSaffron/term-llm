package tools

import "time"

// trackApprovals exposes a detached child waiting on a host prompt instead of
// making a background approval look like a stalled running model turn.
func (m *agentManager) trackApprovals(e *agentEntry, scope *ApprovalManager) {
	if scope == nil {
		return
	}
	if prompt := scope.PromptUIFunc; prompt != nil {
		scope.PromptUIFunc = func(path string, isWrite, isShell bool, workDir string) (ApprovalResult, error) {
			m.markAwaitingApproval(e, true)
			defer m.markAwaitingApproval(e, false)
			return prompt(path, isWrite, isShell, workDir)
		}
	}
	if prompt := scope.PromptFunc; prompt != nil {
		scope.PromptFunc = func(req *ApprovalRequest) (ConfirmOutcome, string) {
			m.markAwaitingApproval(e, true)
			defer m.markAwaitingApproval(e, false)
			return prompt(req)
		}
	}
	if prompt := scope.WorkspacePromptFunc; prompt != nil {
		scope.WorkspacePromptFunc = func(workspace string) (WorkspaceApprovalResult, error) {
			m.markAwaitingApproval(e, true)
			defer m.markAwaitingApproval(e, false)
			return prompt(workspace)
		}
	}
	if prompt := scope.SharedShellPromptUIFunc; prompt != nil {
		scope.SharedShellPromptUIFunc = func(command string) (ApprovalResult, error) {
			m.markAwaitingApproval(e, true)
			defer m.markAwaitingApproval(e, false)
			return prompt(command)
		}
	}
}

func (m *agentManager) markAwaitingApproval(e *agentEntry, awaiting bool) {
	m.mu.Lock()
	if e.record.Status != "running" && e.record.Status != "awaiting_approval" {
		m.mu.Unlock()
		return
	}
	if awaiting {
		e.record.Status = "awaiting_approval"
	} else {
		e.record.Status = "running"
	}
	e.record.UpdatedAt = time.Now()
	record := e.record
	m.mu.Unlock()
	m.save(record)
}
