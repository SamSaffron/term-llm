package tools

// getProjectApprovals revalidates physical checkout membership on every lookup.
// Only approval data is cached: path-to-root mappings become unsafe as soon as
// a .git entry appears, registration changes, or a path symlink is retargeted.
func (m *ApprovalManager) getProjectApprovals(path string) *ProjectApprovals {
	repoInfo := DetectGitRepo(path)
	if !repoInfo.IsRepo {
		return nil
	}
	return m.projectApprovalsForRoot(repoInfo.Root)
}

// projectApprovalsForRoot loads data only after the caller validates membership.
func (m *ApprovalManager) projectApprovalsForRoot(root string) *ProjectApprovals {
	m.projectMu.Lock()
	pa := m.projectCache[root]
	m.projectMu.Unlock()
	if pa != nil {
		return pa
	}

	// Discovery and data reads must not hold the manager-wide lock: a slow
	// filesystem or cold approval file must not stall unrelated warm lookups.
	loaded, err := LoadProjectApprovals(root)
	if err != nil {
		return nil
	}
	m.projectMu.Lock()
	defer m.projectMu.Unlock()
	if pa = m.projectCache[root]; pa != nil {
		return pa
	}
	m.projectCache[root] = loaded
	return loaded
}

// sessionProjectGrant records only this session's explicit file repo choices,
// not flags loaded from disk. Write choices also allow reads, like DirCache.
type sessionProjectGrant struct {
	read  bool
	write bool
}

func (m *ApprovalManager) rememberSessionProjectGrant(path string, isWrite bool) {
	repo := DetectGitRepo(path)
	if !repo.IsRepo {
		return
	}
	m.projectMu.Lock()
	defer m.projectMu.Unlock()
	if m.sessionProjectGrants == nil {
		m.sessionProjectGrants = make(map[string]sessionProjectGrant)
	}
	grant := m.sessionProjectGrants[repo.Root]
	if isWrite {
		grant.write = true
	} else {
		grant.read = true
	}
	m.sessionProjectGrants[repo.Root] = grant
}

// isProjectPathApproved shares fresh discovery between session and persisted
// checks. Metadata and newly nested repos cannot inherit a broad prefix grant.
func (m *ApprovalManager) isProjectPathApproved(path string, isWrite bool) bool {
	repo := DetectGitRepo(path)
	if !repo.IsRepo {
		return false
	}
	for cur := m; cur != nil; cur = cur.parent {
		cur.projectMu.Lock()
		grant := cur.sessionProjectGrants[repo.Root]
		cur.projectMu.Unlock()
		if grant.write || (!isWrite && grant.read) {
			return true
		}
	}
	if m.IgnoreProjectApprovals {
		return false
	}
	pa := m.projectApprovalsForRoot(repo.Root)
	return pa != nil && pa.IsPathApproved(path, isWrite)
}
