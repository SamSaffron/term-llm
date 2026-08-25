package project

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/samsaffron/term-llm/internal/worktree"
)

// MatchesWorkspace reports whether an immutable session execution snapshot
// belongs to project. Git snapshots resolve to the main repository; non-Git
// projects require an exact resolved directory match.
func MatchesWorkspace(ctx context.Context, cwd, worktreeDir string, project Resolved) bool {
	workspace, ok := resolveWorkspaceIdentity(ctx, cwd, worktreeDir)
	return ok && workspace.Git == project.Git && SameIdentity(workspace.CanonicalDir, project.CanonicalDir)
}

type workspaceIdentity struct {
	CanonicalDir string
	Git          bool
}

// resolveWorkspaceIdentity validates an immutable session execution snapshot
// and reduces it to the same identity used by registered projects.
func resolveWorkspaceIdentity(ctx context.Context, cwd, worktreeDir string) (workspaceIdentity, bool) {
	cwd = strings.TrimSpace(cwd)
	worktreeDir = strings.TrimSpace(worktreeDir)
	if worktreeDir != "" {
		// Persisted managed-worktree snapshots always bind both execution fields
		// to the same checkout. Reject partial or inconsistent legacy rows.
		if cwd == "" || !SamePath(cwd, worktreeDir) {
			return workspaceIdentity{}, false
		}
		wt, err := worktree.Get(worktreeDir)
		if err != nil {
			return workspaceIdentity{}, false
		}
		managedRoot, err := worktree.ManagedRoot(wt.RepoRoot)
		if err != nil {
			return workspaceIdentity{}, false
		}
		wtDir, err := canonicalBoundary(wt.Dir)
		if err != nil {
			return workspaceIdentity{}, false
		}
		managedRoot, err = canonicalBoundary(managedRoot)
		if err != nil || wtDir == managedRoot || !withinDir(wtDir, managedRoot) {
			return workspaceIdentity{}, false
		}
		root, err := worktree.MainRepoRootContext(ctx, wtDir)
		if err != nil || !SamePath(root, wt.RepoRoot) {
			return workspaceIdentity{}, false
		}
		canonical, ok := canonicalWorkspaceDir(root)
		return workspaceIdentity{CanonicalDir: canonical, Git: true}, ok
	}
	if cwd == "" {
		return workspaceIdentity{}, false
	}
	if worktree.IsGitRepoContext(ctx, cwd) {
		root, err := worktree.MainRepoRootContext(ctx, cwd)
		if err != nil {
			return workspaceIdentity{}, false
		}
		canonical, ok := canonicalWorkspaceDir(root)
		return workspaceIdentity{CanonicalDir: canonical, Git: true}, ok
	}
	canonical, ok := canonicalWorkspaceDir(cwd)
	return workspaceIdentity{CanonicalDir: canonical}, ok
}

func canonicalWorkspaceDir(path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	resolved, err = canonicalBoundary(resolved)
	if err != nil {
		return "", false
	}
	return CanonicalStoragePath(resolved), true
}

// SamePath compares filesystem paths after best-effort canonicalization.
func SamePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return strings.TrimSpace(a) == "" && strings.TrimSpace(b) == ""
	}
	aa, errA := canonicalBoundary(a)
	bb, errB := canonicalBoundary(b)
	if errA != nil || errB != nil {
		aa, bb = filepath.Clean(a), filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func canonicalBoundary(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if os.IsNotExist(err) {
		return filepath.Clean(abs), nil
	}
	return "", err
}

func withinDir(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}
