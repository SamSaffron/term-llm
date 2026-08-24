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
	cwd = strings.TrimSpace(cwd)
	worktreeDir = strings.TrimSpace(worktreeDir)
	if project.Git {
		if worktreeDir != "" {
			// Persisted managed-worktree snapshots always bind both execution fields
			// to the same checkout. Reject partial or inconsistent legacy rows.
			if cwd == "" || !SamePath(cwd, worktreeDir) {
				return false
			}
			wt, err := worktree.Get(worktreeDir)
			if err != nil || !SamePath(wt.RepoRoot, project.CanonicalDir) {
				return false
			}
			managedRoot, err := worktree.ManagedRoot(project.CanonicalDir)
			if err != nil {
				return false
			}
			wtDir, err := canonicalBoundary(wt.Dir)
			if err != nil {
				return false
			}
			managedRoot, err = canonicalBoundary(managedRoot)
			if err != nil || wtDir == managedRoot || !withinDir(wtDir, managedRoot) {
				return false
			}
			root, err := worktree.MainRepoRootContext(ctx, wtDir)
			return err == nil && SamePath(root, project.CanonicalDir)
		}
		if cwd == "" || !worktree.IsGitRepoContext(ctx, cwd) {
			return false
		}
		root, err := worktree.MainRepoRootContext(ctx, cwd)
		return err == nil && SamePath(root, project.CanonicalDir)
	}
	if worktreeDir != "" || cwd == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	return err == nil && SamePath(resolved, project.CanonicalDir)
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
