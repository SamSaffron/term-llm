package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newGitRepoForWorktreeTest(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll repo: %v", err)
	}
	runGitForWorktreeTest(t, repo, "init")
	runGitForWorktreeTest(t, repo, "config", "user.email", "test@example.com")
	runGitForWorktreeTest(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	runGitForWorktreeTest(t, repo, "commit", "-m", "init")
	return repo
}

func runGitForWorktreeTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

func TestDiffIncludesUntrackedFiles(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
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
	t.Setenv("XDG_DATA_HOME", t.TempDir())
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
	t.Setenv("XDG_DATA_HOME", t.TempDir())
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
	if err == nil || !strings.Contains(err.Error(), "root checkout has uncommitted changes") {
		t.Fatalf("MergeBack error = %v, want dirty root refusal", err)
	}
}
