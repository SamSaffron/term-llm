package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var worktreeTestRepoTemplate string

func TestMain(m *testing.M) {
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test User",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test User",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", key, err)
			os.Exit(1)
		}
	}

	dataHome, err := os.MkdirTemp("", "term-llm-worktree-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_DATA_HOME", dataHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	worktreeTestRepoTemplate = filepath.Join(dataHome, "repo-template")
	if err := os.MkdirAll(worktreeTestRepoTemplate, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create git template: %v\n", err)
		os.Exit(1)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "file.txt"},
		{"commit", "-q", "-m", "init"},
	} {
		if len(args) == 2 && args[0] == "add" {
			if err := os.WriteFile(filepath.Join(worktreeTestRepoTemplate, "file.txt"), []byte("base\n"), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write git template file: %v\n", err)
				os.Exit(1)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = worktreeTestRepoTemplate
		cmd.Env = worktreeTestGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "initialize git template with git %v: %v\n%s", args, err, out)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dataHome)
	os.Exit(code)
}

func worktreeTestGitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
}

func newGitRepoForWorktreeTest(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.CopyFS(repo, os.DirFS(worktreeTestRepoTemplate)); err != nil {
		t.Fatalf("copy git test template: %v", err)
	}
	return repo
}

func runGitForWorktreeTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = worktreeTestGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %v failed: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
	return string(out)
}

func cleanupWorktreeTest(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove worktree test directory: %v", err)
		}
	})
}

func TestCreateMovesRootChangesIntoNewWorktree(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "older.txt"), []byte("existing stash\n"), 0o644); err != nil {
		t.Fatalf("WriteFile older stash: %v", err)
	}
	runGitForWorktreeTest(t, repo, "stash", "push", "--include-untracked", "--message", "existing")
	wantStashes := runGitForWorktreeTest(t, repo, "stash", "list", "--format=%H")

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("unstaged\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("WriteFile staged: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile untracked: %v", err)
	}
	wantStatus := runGitForWorktreeTest(t, repo, "status", "--porcelain")

	wt, err := Create(context.Background(), repo, CreateOptions{Name: "move-changes", MoveChanges: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

	if got := runGitForWorktreeTest(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("root status = %q, want clean", got)
	}
	if got := runGitForWorktreeTest(t, wt.Dir, "status", "--porcelain"); got != wantStatus {
		t.Fatalf("worktree status = %q, want %q", got, wantStatus)
	}
	if got := runGitForWorktreeTest(t, repo, "stash", "list", "--format=%H"); got != wantStashes {
		t.Fatalf("stash list = %q, want existing stash %q", got, wantStashes)
	}
	if got := runGitForWorktreeTest(t, repo, "for-each-ref", "--format=%(refname)", "refs/term-llm/worktree-migrations"); got != "" {
		t.Fatalf("migration refs = %q, want none after success", got)
	}
}

func TestCreateRestoresRootChangesWhenSetupFails(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatalf("WriteFile untracked: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	wantStatus := runGitForWorktreeTest(t, repo, "status", "--porcelain")

	_, err := Create(context.Background(), repo, CreateOptions{
		Name:        "move-rollback",
		MoveChanges: true,
		SetupScript: "exit 1",
	})
	if err == nil {
		t.Fatal("Create succeeded, want setup failure")
	}
	if got := runGitForWorktreeTest(t, repo, "status", "--porcelain"); got != wantStatus {
		t.Fatalf("root status after rollback = %q, want %q", got, wantStatus)
	}
	if got := runGitForWorktreeTest(t, repo, "stash", "list"); got != "" {
		t.Fatalf("stash list = %q, want migration stash removed after rollback", got)
	}
	if got := runGitForWorktreeTest(t, repo, "for-each-ref", "--format=%(refname)", "refs/term-llm/worktree-migrations"); got != "" {
		t.Fatalf("migration refs = %q, want none after rollback", got)
	}
}

func TestCreateRetainsRecoveryCopiesWhenRootRestoreFails(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("original changes\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}

	_, err := Create(context.Background(), repo, CreateOptions{
		Name:        "move-restore-failure",
		MoveChanges: true,
		SetupScript: fmt.Sprintf("printf 'concurrent changes\\n' > %q; exit 1", filepath.Join(repo, "file.txt")),
	})
	if err == nil {
		t.Fatal("Create succeeded, want setup and restore failure")
	}
	if !strings.Contains(err.Error(), "failed worktree and recovery ref retained") {
		t.Fatalf("Create error = %v, want retained recovery details", err)
	}

	items, listErr := List(repo)
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(items) != 1 || items[0].Name != "move-restore-failure" {
		t.Fatalf("managed worktrees = %+v, want failed worktree retained", items)
	}
	cleanupWorktreeTest(t, items[0].Dir)
	gotMoved, readErr := os.ReadFile(filepath.Join(items[0].Dir, "file.txt"))
	if readErr != nil {
		t.Fatalf("ReadFile retained worktree: %v", readErr)
	}
	if string(gotMoved) != "original changes\n" {
		t.Fatalf("retained worktree file = %q, want original changes", gotMoved)
	}
	refs := strings.TrimSpace(runGitForWorktreeTest(t, repo, "for-each-ref", "--format=%(refname)", "refs/term-llm/worktree-migrations"))
	if refs == "" {
		t.Fatal("migration recovery ref missing after restore failure")
	}
	t.Cleanup(func() { runGitForWorktreeTest(t, repo, "update-ref", "-d", refs) })
}

func TestCreateCopyFilesDoesNotOverwriteMovedChanges(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("moved changes\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}

	wt, err := Create(context.Background(), repo, CreateOptions{
		Name:        "move-copy-overlap",
		MoveChanges: true,
		CopyFiles:   []string{"file.txt"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	got, err := os.ReadFile(filepath.Join(wt.Dir, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile moved file: %v", err)
	}
	if string(got) != "moved changes\n" {
		t.Fatalf("worktree file = %q, want moved changes", got)
	}
}

func TestCreateKeepsSuccessfulWorktreeWhenRecoveryRefCleanupFails(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("moved changes\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	finishMigrationTestHook = func(*changeMigration) error {
		return errors.New("forced ref cleanup failure")
	}
	t.Cleanup(func() { finishMigrationTestHook = nil })

	var progress []string
	wt, err := Create(context.Background(), repo, CreateOptions{
		Name:        "move-ref-cleanup",
		MoveChanges: true,
		ProgressFn:  func(message string) { progress = append(progress, message) },
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(progress) == 0 || !strings.Contains(progress[len(progress)-1], "recovery ref cleanup failed") {
		t.Fatalf("final progress = %q, want recovery ref cleanup warning", progress)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.Dir) })
	if got := runGitForWorktreeTest(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("root status = %q, want clean", got)
	}
	got, readErr := os.ReadFile(filepath.Join(wt.Dir, "file.txt"))
	if readErr != nil || string(got) != "moved changes\n" {
		t.Fatalf("retained worktree file = %q, %v; want moved changes", got, readErr)
	}
	refs := strings.TrimSpace(runGitForWorktreeTest(t, repo, "for-each-ref", "--format=%(refname)", "refs/term-llm/worktree-migrations"))
	if refs == "" {
		t.Fatal("recovery ref missing after forced cleanup failure")
	}
	t.Cleanup(func() { runGitForWorktreeTest(t, repo, "update-ref", "-d", refs) })
}

func TestCreateRestoresUnrelatedStashDroppedByConcurrentChange(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("moved changes\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tracked: %v", err)
	}
	injected := false
	runGitTestHook = func(dir string, args []string) {
		if injected || len(args) < 2 || args[0] != "stash" || args[1] != "drop" {
			return
		}
		injected = true
		if err := os.WriteFile(filepath.Join(dir, "concurrent.txt"), []byte("concurrent stash\n"), 0o644); err != nil {
			t.Fatalf("write concurrent stash file: %v", err)
		}
		runGitForWorktreeTest(t, dir, "stash", "push", "--include-untracked", "--message", "concurrent")
	}
	t.Cleanup(func() { runGitTestHook = nil })

	_, err := Create(context.Background(), repo, CreateOptions{Name: "move-stash-race", MoveChanges: true})
	if err == nil {
		t.Fatal("Create succeeded after concurrent stash change, want safe abort")
	}
	if got := runGitForWorktreeTest(t, repo, "status", "--porcelain"); !strings.Contains(got, "file.txt") {
		t.Fatalf("root status = %q, want original changes restored", got)
	}
	if got := runGitForWorktreeTest(t, repo, "stash", "list", "--format=%gs"); !strings.Contains(got, "concurrent") {
		t.Fatalf("stash list = %q, want concurrently dropped stash restored", got)
	}
}

func TestListUsesPorcelainMetadataWithoutPerWorktreeGitProbes(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt1, err := Create(context.Background(), repo, CreateOptions{Name: "list-fast-one"})
	if err != nil {
		t.Fatalf("Create wt1: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt1.Dir) })
	wt2, err := Create(context.Background(), repo, CreateOptions{Name: "list-fast-two"})
	if err != nil {
		t.Fatalf("Create wt2: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt2.Dir) })

	type gitCall struct {
		dir  string
		args []string
	}
	var callsMu sync.Mutex
	var calls []gitCall
	runGitTestHook = func(dir string, args []string) {
		callsMu.Lock()
		defer callsMu.Unlock()
		calls = append(calls, gitCall{dir: dir, args: args})
	}
	t.Cleanup(func() { runGitTestHook = nil })

	items, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("List returned %d worktrees, want 2: %+v", len(items), items)
	}
	wantByDir := map[string]Worktree{
		filepath.Clean(wt1.Dir): *wt1,
		filepath.Clean(wt2.Dir): *wt2,
	}
	for _, item := range items {
		want, ok := wantByDir[filepath.Clean(item.Dir)]
		if !ok {
			t.Fatalf("List returned unexpected worktree: %+v", item)
		}
		if item.Name != want.Name || item.Base != want.Base || item.Branch != want.Branch || item.HeadSHA != want.HeadSHA {
			t.Fatalf("List metadata = %+v, want name=%q base=%q branch=%q head=%q", item, want.Name, want.Base, want.Branch, want.HeadSHA)
		}
	}

	worktreeDirs := map[string]bool{filepath.Clean(wt1.Dir): true, filepath.Clean(wt2.Dir): true}
	callsMu.Lock()
	callsSnapshot := append([]gitCall(nil), calls...)
	callsMu.Unlock()
	statusCalls := 0
	for _, call := range callsSnapshot {
		if !worktreeDirs[filepath.Clean(call.dir)] {
			continue
		}
		args := strings.Join(call.args, " ")
		if args == "status --porcelain" {
			statusCalls++
			continue
		}
		if strings.HasPrefix(args, "rev-parse ") || strings.HasPrefix(args, "symbolic-ref ") || strings.HasPrefix(args, "merge-base ") {
			t.Fatalf("List ran per-worktree metadata probe in %s: git %s\nfull calls: %#v", call.dir, args, calls)
		}
	}
	if statusCalls != 2 {
		t.Fatalf("per-worktree status calls = %d, want 2\nfull calls: %#v", statusCalls, calls)
	}
}

func TestListKeepsDetachedBranchEmpty(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "detached-list"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	runGitForWorktreeTest(t, wt.Dir, "checkout", "--detach", "-q")

	items, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d worktrees, want 1: %+v", len(items), items)
	}
	if !items[0].Detached || items[0].Branch != "" {
		t.Fatalf("detached worktree = %+v, want Detached true with empty Branch", items[0])
	}
}

func TestListComparesDetachedWorktreeWithMainCheckout(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "main-divergence"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

	if err := os.WriteFile(filepath.Join(wt.Dir, "worktree.txt"), []byte("worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForWorktreeTest(t, wt.Dir, "add", "worktree.txt")
	runGitForWorktreeTest(t, wt.Dir, "commit", "-q", "-m", "worktree commit")
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForWorktreeTest(t, repo, "add", "main.txt")
	runGitForWorktreeTest(t, repo, "commit", "-q", "-m", "main commit")

	items, err := List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d worktrees, want 1: %+v", len(items), items)
	}
	if !items[0].MainDivergenceAvailable || items[0].MainAhead != 1 || items[0].MainBehind != 1 {
		t.Fatalf("main divergence = available:%t ahead:%d behind:%d, want true/1/1", items[0].MainDivergenceAvailable, items[0].MainAhead, items[0].MainBehind)
	}
}

func TestMetadataByDirUsesCanonicalPathKeys(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	want := metadata{Name: "canonical", Dir: linkDir, Base: "base"}
	if err := writeMetadata(root, want); err != nil {
		t.Fatalf("writeMetadata: %v", err)
	}

	key, err := samePathKey(realDir)
	if err != nil {
		t.Fatalf("samePathKey: %v", err)
	}
	got, ok := metadataByDir(root)[key]
	if !ok || got.Name != want.Name {
		t.Fatalf("metadataByDir[%q] = %+v, %v; want metadata %q", key, got, ok, want.Name)
	}
}

func TestDiffIncludesUntrackedFiles(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

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

func TestDiffUsesConstantGitProcessesAndCapsUntrackedFiles(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-process-bound"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	t.Cleanup(func() { runGitTestHook = nil })

	created := 0
	for _, count := range []int{1, 100, 1000} {
		for created < count {
			name := fmt.Sprintf("generated-%04d.txt", created)
			if err := os.WriteFile(filepath.Join(wt.Dir, name), []byte("generated\n"), 0o644); err != nil {
				t.Fatalf("WriteFile %s: %v", name, err)
			}
			created++
		}

		var calls [][]string
		runGitTestHook = func(_ string, args []string) {
			calls = append(calls, append([]string(nil), args...))
		}
		result, err := DiffContext(context.Background(), wt.Dir)
		runGitTestHook = nil
		if err != nil {
			t.Fatalf("DiffContext with %d untracked files: %v", count, err)
		}

		diffCalls := 0
		for _, args := range calls {
			if len(args) > 0 && args[0] == "diff" {
				diffCalls++
			}
			if strings.Contains(strings.Join(args, " "), "--no-index") {
				t.Fatalf("DiffContext with %d files launched per-file git diff: %v", count, args)
			}
		}
		if diffCalls != 2 {
			t.Fatalf("DiffContext with %d files launched %d diff processes, want 2; calls=%v", count, diffCalls, calls)
		}
		wantTruncated := count > maxDiffUntrackedFiles
		if result.Truncated != wantTruncated {
			t.Fatalf("DiffContext with %d files truncated=%t, want %t; reasons=%v", count, result.Truncated, wantTruncated, result.TruncationReasons)
		}
		if wantTruncated && !slices.Contains(result.TruncationReasons, diffUntrackedFileLimitReason) {
			t.Fatalf("DiffContext with %d files reasons=%v, want %q", count, result.TruncationReasons, diffUntrackedFileLimitReason)
		}
	}
}

func TestDiffRetriesAfterUntrackedPathDisappears(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-untracked-race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	t.Cleanup(func() { runGitTestHook = nil })

	stablePath := filepath.Join(wt.Dir, "stable.txt")
	if err := os.WriteFile(stablePath, []byte("stable\n"), 0o644); err != nil {
		t.Fatalf("WriteFile stable: %v", err)
	}
	vanishingPath := filepath.Join(wt.Dir, "vanishing.txt")
	if err := os.WriteFile(vanishingPath, []byte("vanishing\n"), 0o644); err != nil {
		t.Fatalf("WriteFile vanishing: %v", err)
	}

	addCalls := 0
	var removeErr error
	runGitTestHook = func(_ string, args []string) {
		if len(args) == 0 || args[0] != "add" {
			return
		}
		addCalls++
		if addCalls == 1 {
			removeErr = os.Remove(vanishingPath)
		}
	}
	result, err := DiffContext(context.Background(), wt.Dir)
	runGitTestHook = nil
	if err != nil {
		t.Fatalf("DiffContext: %v", err)
	}
	if removeErr != nil {
		t.Fatalf("remove vanishing path: %v", removeErr)
	}
	if addCalls != 2 {
		t.Fatalf("git add calls = %d, want one retry", addCalls)
	}
	if result.Truncated {
		t.Fatalf("result unexpectedly truncated: %v", result.TruncationReasons)
	}
	if !strings.Contains(result.Diff, "stable.txt") || !strings.Contains(result.Diff, "+stable") {
		t.Fatalf("diff = %q, want surviving untracked file", result.Diff)
	}
	if strings.Contains(result.Diff, "vanishing.txt") {
		t.Fatalf("diff = %q, unexpectedly contains vanished file", result.Diff)
	}
}

func TestDiffSignalsFailedUntrackedDiscovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim")
	}
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-untracked-failure"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath git: %v", err)
	}
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	script := `#!/bin/sh
if [ "$1" = "ls-files" ] && [ "$2" = "--others" ]; then
  echo "forced untracked discovery failure" >&2
  exit 1
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile git shim: %v", err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, err := DiffContext(context.Background(), wt.Dir)
	if err != nil {
		t.Fatalf("DiffContext: %v", err)
	}
	if !result.Truncated || !slices.Contains(result.TruncationReasons, diffUntrackedUnavailableReason) {
		t.Fatalf("result truncated=%t reasons=%v, want unavailable-untracked signal", result.Truncated, result.TruncationReasons)
	}
}

func TestDiffBoundsLargeOutput(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-output-bound"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

	large := strings.Repeat("a changed line that cannot fit in the diff limit\n", maxDiffBytes/20)
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte(large), 0o644); err != nil {
		t.Fatalf("WriteFile file.txt: %v", err)
	}
	result, err := DiffContext(context.Background(), wt.Dir)
	if err != nil {
		t.Fatalf("DiffContext: %v", err)
	}
	if !result.Truncated || !slices.Contains(result.TruncationReasons, diffOutputLimitReason) {
		t.Fatalf("result truncated=%t reasons=%v, want output truncation", result.Truncated, result.TruncationReasons)
	}
	if len(result.Diff) > maxDiffBytes {
		t.Fatalf("diff length = %d, want at most %d", len(result.Diff), maxDiffBytes)
	}
}

func TestDiffContextCancelsActiveGitProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim")
	}
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "diff-cancel"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile file.txt: %v", err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath git: %v", err)
	}
	shimDir := t.TempDir()
	ready := filepath.Join(shimDir, "ready")
	shim := filepath.Join(shimDir, "git")
	script := `#!/bin/sh
if [ "$1" = "diff" ]; then
  : > "$DIFF_READY"
  while :; do :; done
fi
exec "$REAL_GIT" "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile git shim: %v", err)
	}
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("DIFF_READY", ready)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := DiffContext(ctx, wt.Dir)
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git shim did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DiffContext error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DiffContext did not terminate the canceled git process")
	}
}

func TestMergeBackStagesWorktreeChangesOnRoot(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "merge-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

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
	cleanupWorktreeTest(t, wt.Dir)

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

func TestConcurrentMergeBackSerializesRootMutation(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	firstWorktree, err := Create(context.Background(), repo, CreateOptions{Name: "concurrent-first"})
	if err != nil {
		t.Fatalf("Create first worktree: %v", err)
	}
	cleanupWorktreeTest(t, firstWorktree.Dir)
	secondWorktree, err := Create(context.Background(), repo, CreateOptions{Name: "concurrent-second"})
	if err != nil {
		t.Fatalf("Create second worktree: %v", err)
	}
	cleanupWorktreeTest(t, secondWorktree.Dir)

	if err := os.WriteFile(filepath.Join(firstWorktree.Dir, "file.txt"), []byte("first merge\n"), 0o644); err != nil {
		t.Fatalf("WriteFile first worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondWorktree.Dir, "file.txt"), []byte("second merge\n"), 0o644); err != nil {
		t.Fatalf("WriteFile second worktree: %v", err)
	}

	firstRootClean := make(chan struct{})
	secondAttempted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)
	var hookMu sync.Mutex
	beforeLockCalls := 0
	rootCleanCalls := 0
	mergeBackTestHook = func(stage string) {
		hookMu.Lock()
		switch stage {
		case "before-lock":
			beforeLockCalls++
			if beforeLockCalls == 2 {
				close(secondAttempted)
			}
			hookMu.Unlock()
		case "root-clean":
			rootCleanCalls++
			first := rootCleanCalls == 1
			if first {
				close(firstRootClean)
			}
			hookMu.Unlock()
			if first {
				<-releaseFirst
			}
		default:
			hookMu.Unlock()
		}
	}
	t.Cleanup(func() { mergeBackTestHook = nil })

	type mergeCall struct {
		result MergeResult
		err    error
	}
	firstDone := make(chan mergeCall, 1)
	go func() {
		result, err := MergeBack(context.Background(), firstWorktree.Dir, MergeOptions{})
		firstDone <- mergeCall{result: result, err: err}
	}()
	<-firstRootClean

	secondDone := make(chan mergeCall, 1)
	go func() {
		result, err := MergeBack(context.Background(), secondWorktree.Dir, MergeOptions{})
		secondDone <- mergeCall{result: result, err: err}
	}()
	<-secondAttempted

	var earlySecond *mergeCall
	select {
	case call := <-secondDone:
		earlySecond = &call
	case <-time.After(200 * time.Millisecond):
	}
	release()

	first := <-firstDone
	var second mergeCall
	if earlySecond != nil {
		second = *earlySecond
	} else {
		second = <-secondDone
	}
	if earlySecond != nil {
		t.Fatalf("second merge completed while first root mutation was in flight: result=%+v err=%v", second.result, second.err)
	}
	if first.err != nil || !first.result.Applied {
		t.Fatalf("first MergeBack result=%+v err=%v, want applied", first.result, first.err)
	}
	if !errors.Is(second.err, ErrRootDirty) {
		t.Fatalf("second MergeBack result=%+v err=%v, want ErrRootDirty", second.result, second.err)
	}
	if got := runGitForWorktreeTest(t, repo, "status", "--porcelain"); !strings.Contains(got, "M  file.txt") {
		t.Fatalf("root status = %q, want successful merge staged", got)
	}
	data, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile root file: %v", err)
	}
	if got := string(data); got != "first merge\n" {
		t.Fatalf("root file = %q, want successful merge content", got)
	}
}

func TestMergeBackConflictCleansCherryPickState(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "conflict-test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.Dir) })

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

func TestCleanupCherryPickStatePreservesChangesWithoutCherryPick(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("unrelated staged change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	before := runGitForWorktreeTest(t, repo, "status", "--porcelain")

	if err := cleanupCherryPickState(repo); err != nil {
		t.Fatalf("cleanupCherryPickState: %v", err)
	}
	if after := runGitForWorktreeTest(t, repo, "status", "--porcelain"); after != before {
		t.Fatalf("root status changed from %q to %q without cherry-pick state", before, after)
	}
	data, err := os.ReadFile(filepath.Join(repo, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile root: %v", err)
	}
	if got := string(data); got != "unrelated staged change\n" {
		t.Fatalf("root file = %q, want unrelated staged change preserved", got)
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
	cleanupWorktreeTest(t, wt.Dir)
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
	cleanupWorktreeTest(t, wt.Dir)
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
	cleanupWorktreeTest(t, wt.Dir)
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

func TestStartAssistedMergeNoChangesLeavesRootUnchanged(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "assist-noop"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)

	res, err := StartAssistedMerge(context.Background(), wt.Dir, AssistedMergeOptions{})
	if err != nil {
		t.Fatalf("StartAssistedMerge: %v (result=%+v)", err, res)
	}
	if res.Applied || res.NeedsResolution || len(res.ChangedFiles) != 0 {
		t.Fatalf("AssistedMergeResult = %+v, want no changes", res)
	}
	if status := strings.TrimSpace(runGitForWorktreeTest(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("root status = %q, want clean", status)
	}
}

func TestStartAssistedMergeRefusesDirtyRootWithoutChangingIt(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "assist-dirty-root"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	if err := os.WriteFile(filepath.Join(wt.Dir, "source.txt"), []byte("source change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "root.txt"), []byte("root change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	before := runGitForWorktreeTest(t, repo, "status", "--porcelain")

	res, err := StartAssistedMerge(context.Background(), wt.Dir, AssistedMergeOptions{})
	if !errors.Is(err, ErrRootDirty) {
		t.Fatalf("StartAssistedMerge error = %v, want ErrRootDirty (result=%+v)", err, res)
	}
	if got := runGitForWorktreeTest(t, repo, "status", "--porcelain"); got != before {
		t.Fatalf("root status changed from %q to %q", before, got)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != previousBranch {
		t.Fatalf("root branch = %q, want unchanged branch %q", got, previousBranch)
	}
}

func TestStartAssistedMergeAppliesCleanlyOnCurrentRootBranch(t *testing.T) {
	t.Parallel()

	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "assist-clean"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanupWorktreeTest(t, wt.Dir)
	if err := os.WriteFile(filepath.Join(wt.Dir, "assisted.txt"), []byte("applied to root\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}

	res, err := StartAssistedMerge(context.Background(), wt.Dir, AssistedMergeOptions{})
	if err != nil {
		t.Fatalf("StartAssistedMerge: %v (result=%+v)", err, res)
	}
	if !res.Applied || res.NeedsResolution || len(res.ChangedFiles) == 0 {
		t.Fatalf("AssistedMergeResult = %+v, want clean staged application", res)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != previousBranch {
		t.Fatalf("root branch = %q, want unchanged branch %q", got, previousBranch)
	}
	status := runGitForWorktreeTest(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "A  assisted.txt") {
		t.Fatalf("root status = %q, want staged assisted.txt", status)
	}
}

func TestStartAssistedMergeLeavesConflictsOnCurrentRootBranch(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	previousBranch := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current"))
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "assist-conflict"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wt.Dir) })
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root assisted change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile root: %v", err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	runGitForWorktreeTest(t, repo, "commit", "-m", "root assisted change")
	if err := os.WriteFile(filepath.Join(wt.Dir, "file.txt"), []byte("worktree assisted change\n"), 0o644); err != nil {
		t.Fatalf("WriteFile worktree: %v", err)
	}

	res, err := StartAssistedMerge(context.Background(), wt.Dir, AssistedMergeOptions{})
	t.Cleanup(func() {
		_, _ = runGit(repo, "reset", "--merge")
		_, _ = runGit(repo, "cherry-pick", "--quit")
	})
	if err != nil {
		t.Fatalf("StartAssistedMerge: %v (result=%+v)", err, res)
	}
	if !res.NeedsResolution || res.Applied || len(res.Conflicts) == 0 {
		t.Fatalf("AssistedMergeResult = %+v, want conflict on current root branch", res)
	}
	if got := strings.TrimSpace(runGitForWorktreeTest(t, repo, "branch", "--show-current")); got != previousBranch {
		t.Fatalf("root branch = %q, want unchanged branch %q", got, previousBranch)
	}
	status := runGitForWorktreeTest(t, repo, "status", "--porcelain")
	if !strings.Contains(status, "UU file.txt") {
		t.Fatalf("root status = %q, want unmerged file", status)
	}
}
