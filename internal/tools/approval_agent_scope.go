package tools

// CloneForAgentRun snapshots the parent's approval scope before a child can
// detach. A child must not inherit a mutable root whose workspaceSessionID a
// subsequent serve request can rebind to a different parent session.
func (m *ApprovalManager) CloneForAgentRun(sessionID string) *ApprovalManager {
	if m == nil {
		return nil
	}
	root := m.root()
	clone := NewApprovalManager(root.permissions)
	clone.cache = root.cache
	clone.dirCache = root.dirCache
	clone.shellCache = root.shellCache
	clone.sharedShellCache = root.sharedShellCache
	clone.IgnoreProjectApprovals = root.IgnoreProjectApprovals
	clone.DebugApproval = root.DebugApproval
	clone.WorkspacePolicy = root.WorkspacePolicy
	clone.PromptFunc = root.PromptFunc
	clone.PromptUIFunc = root.PromptUIFunc
	clone.SharedShellPromptUIFunc = root.SharedShellPromptUIFunc
	clone.WorkspacePromptFunc = root.WorkspacePromptFunc
	clone.GuardianEventFunc = root.GuardianEventFunc
	clone.SetApprovalMode(root.ApprovalMode())
	root.workspaceMu.RLock()
	clone.workspaceStore = root.workspaceStore
	clone.workspaceTrustStore = root.workspaceTrustStore
	clone.primaryWorkspace = root.primaryWorkspace
	clone.primaryWorkspaceGrant = root.primaryWorkspaceGrant
	clone.primaryWorkspaceDenied = root.primaryWorkspaceDenied
	for path, grant := range root.workspaceGrants {
		clone.workspaceGrants[path] = grant
	}
	for path, grant := range root.workspaceYoloGrants {
		clone.workspaceYoloGrants[path] = grant
	}
	root.workspaceMu.RUnlock()
	clone.workspaceSessionID = sessionID
	return clone
}
