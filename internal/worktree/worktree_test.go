package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dataHome, err := os.MkdirTemp("", "term-llm-worktree-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_DATA_HOME", dataHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dataHome)
	os.Exit(code)
}

func newGitRepoForWorktreeTest(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	runGitForWorktreeTest(t, repo, "init", "-q")
	runGitForWorktreeTest(t, repo, "config", "user.name", "Test User")
	runGitForWorktreeTest(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	runGitForWorktreeTest(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func runGitForWorktreeTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

func TestDiffIncludesUntrackedFiles(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("hello from untracked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile new.txt: %v", err)
	}
	diff, err := Diff(wt.Dir)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "new.txt") || !strings.Contains(diff, "+hello from untracked") {
		t.Fatalf("diff = %q, want untracked file diff", diff)
	}
}

func TestMergeBackStagesWorktreeChangesOnRoot(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "merge-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("WriteFile untracked: %v", err)
	}

	res, err := MergeBack(context.Background(), wt.Dir, MergeOptions{})
	if err != nil {
		t.Fatalf("MergeBack: %v", err)
	}
	if !res.Applied || res.Committed {
		t.Fatalf("MergeBack result = %+v, want applied staged without commit", res)
	}
	status := runGitForWorktreeTest(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "M  file.txt") || !strings.Contains(status, "A  new.txt") {
		t.Fatalf("root status = %q, want staged modification and addition", status)
	}
}

func TestMergeBackRefusesDirtyRootByDefault(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "dirty-root-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "root-only.txt"), []byte("dirty root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root dirty: %v", err)
	}
	_, err = MergeBack(context.Background(), wt.Dir, MergeOptions{})
	if !errors.Is(err, ErrRootDirty) {
		t.Fatalf("MergeBack error = %v, want ErrRootDirty", err)
	}
}

func TestMergeBackConflictCleansCherryPickState(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "conflict-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	runGitForWorktreeTest(t, repo, "commit", "-m", "root change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}

	res, err := MergeBack(context.Background(), wt.Dir, MergeOptions{})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("MergeBack error = %v, want ErrConflict (result=%+v)", err, res)
	}
	if res.Applied || !res.ConflictReset {
		t.Fatalf("MergeBack result = %+v, want not applied with conflict reset", res)
	}
	if len(res.Conflicts) == 0 || res.Conflicts[0] != "file.txt" {
		t.Fatalf("conflicts = %v, want file.txt", res.Conflicts)
	}
	if status := runGitForWorktreeTest(t, repo, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("root status after conflict cleanup = %q, want clean", status)
	}
	cherryPickHead := strings.TrimSpace(runGitForWorktreeTest(t, repo, "rev-parse", "--git-path", "CHERRY_PICK_HEAD"))
	if !filepath.IsAbs(cherryPickHead) {
		cherryPickHead = filepath.Join(repo, cherryPickHead)
	}
	if _, err := os.Stat(cherryPickHead); !os.IsNotExist(err) {
		t.Fatalf("CHERRY_PICK_HEAD should be absent after cleanup, stat err=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile root file: %v", err)
	}
	if got := string(data); got != "root changed\n" {
		t.Fatalf("root file = %q, want original root change", got)
	}
}

func TestPromoteToRootChecksOutBranchAndAppliesDirtyWorktreeChanges(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "promote-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("promoted tracked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Dir, "new.txt"), []byte("promoted untracked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile untracked: %v", err)
	}

	res, err := PromoteToRoot(context.Background(), wt.Dir, "feature/promote", PromoteOptions{})
	if err != nil {
		t.Fatalf("PromoteToRoot: %v (result=%+v)", err, res)
	}
	if !samePath(res.RootDir, repo) || !samePath(res.WorktreeDir, wt.Dir) || res.Branch != "feature/promote" || res.PreviousRootBranch != previousBranch {
		t.Fatalf("PromoteResult = %+v, want root/worktree/branch/previous branch", res)
	}
	if !res.Applied || res.SnapshotCommit == "" || len(res.ChangedFiles) == 0 || !res.OriginalWorktreeStillExists {
		t.Fatalf("PromoteResult = %+v, want dirty changes applied with snapshot and original worktree", res)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != "feature/promote" {
		t.Fatalf("root branch = %q, want feature/promote", got)
	}
	status := runGitForWorktreeTest(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "M  file.txt") || !strings.Contains(status, "A  new.txt") {
		t.Fatalf("root status = %q, want staged promoted tracked and untracked changes", status)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, wt.Dir, "branch", "--show-current")); got == "feature/promote" {
		t.Fatalf("source worktree should not have checked out promoted branch")
	}
}

func TestPromoteToRootRefusesDirtyRoot(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "promote-dirty-root"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })
	if err := os.WriteFile(filepath.Join(repo, "root-only.txt"), []byte("dirty root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root dirty: %v", err)
	}

	res, err := PromoteToRoot(context.Background(), wt.Dir, "feature-dirty", PromoteOptions{})
	if !errors.Is(err, ErrRootDirty) {
		t.Fatalf("PromoteToRoot error = %v, want ErrRootDirty (result=%+v)", err, res)
	}
	if exists, err := localBranchExists(repo, "feature-dirty"); err != nil || exists {
		t.Fatalf("feature-dirty exists=%v err=%v, want no branch", exists, err)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != previousBranch {
		t.Fatalf("root branch = %q, want %q", got, previousBranch)
	}
}

func TestPromoteToRootRejectsExistingBranch(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "promote-existing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })
	runGitForWorktreeTest(t, repo, "branch", "already-there")

	_, err = PromoteToRoot(context.Background(), wt.Dir, "already-there", PromoteOptions{})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("PromoteToRoot error = %v, want existing branch refusal", err)
	}
}

func TestPromoteToRootRollsBackAfterCheckoutFailure(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "promote-rollback"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })
	promoteToRootTestHook = func(stage string) error {
		if stage == "after-checkout" {
			return fmt.Errorf("forced promote failure")
		}
		return nil
	}
	t.Cleanup(func() { promoteToRootTestHook = nil })

	_, err = PromoteToRoot(context.Background(), wt.Dir, "feature-rollback", PromoteOptions{})
	if err == nil || !strings.Contains(err.Error(), "forced promote failure") {
		t.Fatalf("PromoteToRoot error = %v, want forced failure", err)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != previousBranch {
		t.Fatalf("root branch after rollback = %q, want %q", got, previousBranch)
	}
	if exists, err := localBranchExists(repo, "feature-rollback"); err != nil || exists {
		t.Fatalf("feature-rollback exists=%v err=%v, want branch removed", exists, err)
	}
	if status := runGitForWorktreeTest(t, repo, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("root status after rollback = %q, want clean", status)
	}
}

func TestStartAssistedMergeNoChangesDoesNotCreateRecoveryBranch(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "assist-noop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })

	res, err := StartAssistedMerge(context.Background(), wt.Dir, AssistedMergeOptions{Branch: "assist-noop-branch"})
	if err != nil {
		t.Fatalf("StartAssistedMerge: %v (result=%+v)", err, res)
	}
	if res.Branch != "" || res.Applied || res.NeedsResolution || len(res.ChangedFiles) != 0 {
		t.Fatalf("AssistedMergeResult = %+v, want no branch and no changes", res)
	}
	if exists, err := localBranchExists(repo, "assist-noop-branch"); err != nil || exists {
		t.Fatalf("assist-noop-branch exists=%v err=%v, want no branch created", exists, err)
	}
}

func TestStartAssistedMergeLeavesConflictsOnRecoveryBranch(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "assist-conflict"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = Remove(context.Background(), wt.Dir, RemoveOptions{Force: true}) })
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root assisted change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	runGitForWorktreeTest(t, repo, "commit", "-m", "root assisted change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree assisted change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}

	res, err := StartAssistedMerge(context.Background(), wt.Dir, AssistedMergeOptions{Branch: "assist-recovery"})
	t.Cleanup(func() {
		_, _ = runGit(repo, "reset", "--merge")
		_, _ = runGit(repo, "cherry-pick", "--quit")
		_, _ = runGit(repo, "checkout", previousBranch)
		_, _ = runGit(repo, "branch", "-D", "assist-recovery")
	})
	if err != nil {
		t.Fatalf("StartAssistedMerge: %v (result=%+v)", err, res)
	}
	if !res.NeedsResolution || res.Applied || res.Branch != "assist-recovery" || len(res.Conflicts) == 0 {
		t.Fatalf("AssistedMergeResult = %+v, want conflict on recovery branch", res)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != "assist-recovery" {
		t.Fatalf("root branch = %q, want assist-recovery", got)
	}
	status := runGitForWorktreeTest(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "UU file.txt") {
		t.Fatalf("root status = %q, want unmerged file", status)
	}
}
