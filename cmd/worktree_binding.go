package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/worktree"
)

// BindWorktreeSession binds a session/tool manager to dir using the unified
// BaseDir mechanism. It never calls os.Chdir.
func BindWorktreeSession(ctx context.Context, store session.Store, sess *session.Session, toolMgr *tools.ToolManager, dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("worktree directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil {
		return fmt.Errorf("worktree directory is not accessible: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("worktree path is not a directory: %s", abs)
	}
	if !worktree.IsGitRepo(abs) {
		return fmt.Errorf("not a git worktree: %s", abs)
	}
	if toolMgr != nil {
		if err := toolMgr.SetBaseDirWithContext(ctx, abs); err != nil {
			return err
		}
		abs = toolMgr.BaseDir()
	}
	// Touching metadata is best-effort: manually-created/non-managed git
	// worktrees are still valid session BaseDirs.
	_ = worktree.TouchLastBound(abs)
	if sess != nil {
		changed := filepath.Clean(sess.WorktreeDir) != filepath.Clean(abs) || filepath.Clean(sess.CWD) != filepath.Clean(abs)
		sess.WorktreeDir = abs
		sess.CWD = abs
		if store != nil && changed {
			if err := store.Update(ctx, sess); err != nil {
				return err
			}
		}
	}
	return nil
}

// BindRootSession clears a worktree binding and roots tools at rootDir.
func BindRootSession(ctx context.Context, store session.Store, sess *session.Session, toolMgr *tools.ToolManager, rootDir string) error {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		var err error
		rootDir, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return err
	}
	if toolMgr != nil {
		if err := toolMgr.SetBaseDirWithContext(ctx, abs); err != nil {
			return err
		}
		abs = toolMgr.BaseDir()
	}
	if sess != nil {
		changed := strings.TrimSpace(sess.WorktreeDir) != "" || filepath.Clean(sess.CWD) != filepath.Clean(abs)
		sess.WorktreeDir = ""
		sess.CWD = abs
		if store != nil && changed {
			if err := store.Update(ctx, sess); err != nil {
				return err
			}
		}
	}
	return nil
}

// RestoreWorktreeBinding validates and reapplies a persisted binding. If the
// worktree is gone, it clears the binding and falls back to the recorded CWD/root.
func RestoreWorktreeBinding(ctx context.Context, store session.Store, sess *session.Session, toolMgr *tools.ToolManager) error {
	if sess == nil {
		return nil
	}
	if strings.TrimSpace(sess.ProjectID) != "" {
		projects, ok := session.AsProjectReader(store)
		if !ok {
			return fmt.Errorf("project workspace is unavailable")
		}
		project, err := projects.GetProject(ctx, sess.ProjectID)
		if err != nil || project == nil {
			return fmt.Errorf("project unavailable")
		}
		status := projectStatus(*project)
		if !status.Available {
			return fmt.Errorf("project unavailable")
		}
		binding, err := persistedProjectWorkspaceBinding(*project, status, *sess, "")
		if err != nil {
			return err
		}
		// Keep the durable snapshot untouched, but expose the validated execution
		// fallback on the in-memory resume object so prompt/skill discovery does not
		// prefer a vanished worktree.
		sess.CWD = binding.RuntimeDir
		if binding.WorktreeDir != "" && !sameServePath(binding.RuntimeDir, binding.WorktreeDir) {
			sess.WorktreeDir = ""
		}
		if toolMgr != nil {
			if err := toolMgr.SetBaseDirWithContext(ctx, binding.RuntimeDir); err != nil {
				return err
			}
		}
		return nil
	}
	if strings.TrimSpace(sess.WorktreeDir) == "" {
		fallback, err := sessionRootFallback(sess.CWD)
		if err != nil {
			return err
		}
		return BindRootSession(ctx, store, sess, toolMgr, fallback)
	}
	if _, err := os.Stat(sess.WorktreeDir); err == nil && worktree.IsGitRepo(sess.WorktreeDir) {
		return BindWorktreeSession(ctx, store, sess, toolMgr, sess.WorktreeDir)
	}
	fallback, err := sessionRootFallback(sess.CWD)
	if err != nil {
		return err
	}
	return BindRootSession(ctx, store, sess, toolMgr, fallback)
}

func sessionRootFallback(dir string) (string, error) {
	fallback := strings.TrimSpace(dir)
	if fallback != "" && dirExists(fallback) {
		if worktree.IsGitRepo(fallback) {
			if root, err := worktree.MainRepoRoot(fallback); err == nil {
				return root, nil
			}
		}
		abs, err := filepath.Abs(fallback)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if worktree.IsGitRepo(cwd) {
		if root, err := worktree.MainRepoRoot(cwd); err == nil {
			return root, nil
		}
	}
	return cwd, nil
}

func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}
