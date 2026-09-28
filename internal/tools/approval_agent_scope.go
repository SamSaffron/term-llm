package tools

// CloneForAgentRun snapshots the parent's approval scope before a child can
// detach. The clone owns its caches and workspace identity: later serve requests
// must never retarget an already-running child to another parent's grants.
func (m *ApprovalManager) CloneForAgentRun(sessionID string) *ApprovalManager {
	if m == nil {
		return nil
	}
	root := m.root()
	root.BindWorkspaceSessionID(sessionID)
	clone := NewApprovalManager(root.permissions)
	clone.promptShared = root.PromptLock()
	clone.IgnoreProjectApprovals = root.IgnoreProjectApprovals
	clone.DebugApproval = root.DebugApproval
	clone.WorkspacePolicy = root.WorkspacePolicy
	clone.PromptFunc = root.PromptFunc
	clone.PromptUIFunc = root.PromptUIFunc
	clone.SharedShellPromptUIFunc = root.SharedShellPromptUIFunc
	clone.WorkspacePromptFunc = root.WorkspacePromptFunc
	clone.GuardianEventFunc = root.GuardianEventFunc
	clone.SetApprovalMode(root.ApprovalMode())
	if reviewer := root.lookupPolicyReviewFunc(); reviewer != nil {
		clone.SetPolicyReviewFunc(reviewer, nil)
	}
	root.workspaceMu.Lock()
	clone.workspaceStore = root.workspaceStore
	clone.workspaceTrustStore = root.workspaceTrustStore
	sameOwner := root.workspaceSessionID == sessionID
	if sameOwner {
		clone.primaryWorkspace = root.primaryWorkspace
		clone.primaryWorkspaceGrant = root.primaryWorkspaceGrant
		clone.primaryWorkspaceDenied = root.primaryWorkspaceDenied
		for path, grant := range root.workspaceGrants {
			clone.workspaceGrants[path] = grant
		}
		for path, grant := range root.workspaceYoloGrants {
			clone.workspaceYoloGrants[path] = grant
		}
	}
	root.workspaceMu.Unlock()
	clone.workspaceSessionID = sessionID
	if sameOwner {
		root.cache.mu.RLock()
		for k, v := range root.cache.cache {
			clone.cache.cache[k] = v
		}
		root.cache.mu.RUnlock()
		root.dirCache.mu.RLock()
		for k, v := range root.dirCache.readDirs {
			clone.dirCache.readDirs[k] = v
		}
		for k, v := range root.dirCache.writeDirs {
			clone.dirCache.writeDirs[k] = v
		}
		root.dirCache.mu.RUnlock()
		copyShellCache(clone.shellCache, root.shellCache)
		copyShellCache(clone.sharedShellCache, root.sharedShellCache)
	}
	return clone
}
func copyShellCache(dst, src *ShellApprovalCache) {
	src.mu.RLock()
	defer src.mu.RUnlock()
	dst.patterns = append(dst.patterns, src.patterns...)
	dst.commands = append(dst.commands, src.commands...)
}
