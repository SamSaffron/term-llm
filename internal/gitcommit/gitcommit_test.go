package gitcommit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/testutil"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvironment(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func writeTest(t *testing.T, dir, path, body string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func repoTest(t *testing.T) *Repository {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.name", "Test User")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	writeTest(t, dir, "base.txt", "base\n")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "-qm", "base")
	r, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestInspectMarshalsEmptyChangeKindsAsArrays(t *testing.T) {
	r := repoTest(t)
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"staged":[]`, `"unstaged":[]`, `"untracked":[]`, `"conflicted":[]`} {
		if !strings.Contains(string(body), field) {
			t.Fatalf("repository status must encode %s as an array: %s", field, body)
		}
	}
}

func TestInspectStageAllAndVerifiedCommit(t *testing.T) {
	r := repoTest(t)
	writeTest(t, r.root, "base.txt", "changed\n")
	writeTest(t, r.root, "new name.txt", "new\n")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Staged) != 0 || len(state.Unstaged) != 1 || len(state.Untracked) != 1 || !state.SelectionAvailable || state.StatusToken == "" {
		t.Fatalf("unexpected state: %+v", state)
	}
	if state.Unstaged[0].Additions != 1 || state.Unstaged[0].Deletions != 1 || state.Unstaged[0].Binary {
		t.Fatalf("unstaged stats=%+v", state.Unstaged[0])
	}
	if state.Untracked[0].Additions != 1 || state.Untracked[0].Deletions != 0 || state.Untracked[0].Binary {
		t.Fatalf("untracked stats=%+v", state.Untracked[0])
	}
	staged, err := r.Stage(context.Background(), StageRequest{Mode: StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Staged) != 2 || staged.Summary.Files != 2 || staged.Summary.Additions != 2 || staged.Summary.Deletions != 1 {
		t.Fatalf("unexpected staged state: %+v", staged)
	}
	for _, change := range staged.Staged {
		if change.Additions != 1 {
			t.Fatalf("missing per-file stats: %+v", change)
		}
	}
	result, err := r.Commit(context.Background(), "# preserved subject\n\nBody.\n", staged.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if result.Subject != "# preserved subject" || result.HeadOID == "" || result.BeforeHead == result.HeadOID || result.TreeChanged {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestExactSelectionReplacesExistingIndexWithoutDeletingWorktree(t *testing.T) {
	r := repoTest(t)
	writeTest(t, r.root, "base.txt", "selected\n")
	writeTest(t, r.root, "excluded.txt", "excluded\n")
	gitTest(t, r.root, "add", "excluded.txt")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.Stage(context.Background(), StageRequest{Mode: StageExactSelection, Paths: []string{"base.txt"}, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Staged) != 1 || next.Staged[0].Path != "base.txt" {
		t.Fatalf("staged=%+v", next.Staged)
	}
	if _, err := os.Stat(filepath.Join(r.root, "excluded.txt")); err != nil {
		t.Fatalf("excluded working file disappeared: %v", err)
	}
	if !strings.Contains(gitTest(t, r.root, "status", "--short"), "?? excluded.txt") {
		t.Fatalf("excluded file was not preserved: %s", gitTest(t, r.root, "status", "--short"))
	}
}

func TestStageAllRejectsWorkingTreeChangesAfterReview(t *testing.T) {
	r := repoTest(t)
	writeTest(t, r.root, "base.txt", "reviewed\n")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, r.root, "base.txt", "changed after review\n")
	refreshed, err := r.Stage(context.Background(), StageRequest{Mode: StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if !IsKind(err, ErrStale) {
		t.Fatalf("error=%v", err)
	}
	if len(refreshed.Staged) != 0 {
		t.Fatalf("stale stage-all mutated index: %+v", refreshed.Staged)
	}
}

func TestStageRejectsContentAndIndexStaleness(t *testing.T) {
	r := repoTest(t)
	writeTest(t, r.root, "a.txt", "one\n")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, r.root, "a.txt", "two\n")
	_, err = r.Stage(context.Background(), StageRequest{Mode: StageExactSelection, Paths: []string{"a.txt"}, StatusToken: state.StatusToken}, state.Fingerprint)
	if !IsKind(err, ErrStale) {
		t.Fatalf("error=%v", err)
	}
	state, err = r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, r.root, "b.txt", "b\n")
	gitTest(t, r.root, "add", "b.txt")
	_, err = r.Commit(context.Background(), "stale", state.Fingerprint)
	if !IsKind(err, ErrStale) {
		t.Fatalf("error=%v", err)
	}
}

func TestOpenScrubsInheritedRepositoryEnvironment(t *testing.T) {
	first := repoTest(t)
	second := repoTest(t)
	t.Setenv("GIT_DIR", filepath.Join(first.root, ".git"))
	writeTest(t, second.root, "second.txt", "second\n")
	state, err := second.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Untracked) != 1 || state.Untracked[0].Path != "second.txt" {
		t.Fatalf("inspected wrong repository: %+v", state)
	}
}

func TestUnbornAndUnsupportedOperation(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.name", "Test User")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	writeTest(t, dir, "first.txt", "first\n")
	r, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Unborn || state.Fingerprint.HeadState != HeadUnborn {
		t.Fatalf("state=%+v", state)
	}
	staged, err := r.Stage(context.Background(), StageRequest{Mode: StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Commit(context.Background(), "initial", staged.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if result.BeforeHead != "" {
		t.Fatalf("before=%q", result.BeforeHead)
	}
	writeTest(t, r.gitDir, "MERGE_HEAD", result.HeadOID+"\n")
	_, err = r.Inspect(context.Background())
	if !IsKind(err, ErrUnsupportedOperation) {
		t.Fatalf("error=%v", err)
	}
}

func TestUnbornHookFailureIsRecoverableNotUncertain(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.name", "Test")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	writeTest(t, dir, "first.txt", "first\n")
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := r.Stage(context.Background(), StageRequest{Mode: StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Commit(context.Background(), "initial", staged.Fingerprint)
	if !IsKind(err, ErrCommit) || IsKind(err, ErrUncertain) {
		t.Fatalf("error=%v", err)
	}
}

func TestCommitTimesOutHungHookAndReleasesCheckout(t *testing.T) {
	r := repoTest(t)
	writeTest(t, r.root, "base.txt", "changed\n")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := r.Stage(context.Background(), StageRequest{Mode: StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}

	pidPath := filepath.Join(t.TempDir(), "hook.pid")
	t.Setenv("TERM_LLM_TEST_HOOK_PID", pidPath)
	hook := filepath.Join(r.gitDir, "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s' $$ > \"$TERM_LLM_TEST_HOOK_PID\"\nsleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}

	oldTimeout, oldWaitDelay := commitTimeout, commitWaitDelay
	commitTimeout, commitWaitDelay = time.Second, 100*time.Millisecond
	t.Cleanup(func() {
		commitTimeout, commitWaitDelay = oldTimeout, oldWaitDelay
	})

	started := time.Now()
	_, err = r.Commit(context.Background(), "blocked", staged.Fingerprint)
	if !IsKind(err, ErrCommit) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline-backed commit failure", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("hung commit returned after %s, want at most 3s", elapsed)
	}

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read hook PID: %v", err)
	}
	hookPID, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatalf("parse hook PID %q: %v", pidBytes, err)
	}
	testutil.WaitForProcessExit(t, hookPID, 2*time.Second)

	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	commitTimeout, commitWaitDelay = oldTimeout, oldWaitDelay
	after, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect after timed-out commit: %v", err)
	}
	if _, err := r.Commit(context.Background(), "after timeout", after.Fingerprint); err != nil {
		t.Fatalf("commit after timed-out hook: %v", err)
	}
}

func TestCommitFailurePreservesIndexAndClassifiesIdentity(t *testing.T) {
	r := repoTest(t)
	writeTest(t, r.root, "a.txt", "a\n")
	state, _ := r.Inspect(context.Background())
	staged, err := r.Stage(context.Background(), StageRequest{Mode: StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"user.name", "user.email"} {
		cmd := exec.Command("git", "-C", r.root, "config", "--unset-all", key)
		cmd.Env = gitEnvironment(nil)
		_ = cmd.Run()
	}
	gitTest(t, r.root, "config", "user.useConfigOnly", "true")
	_, err = r.Commit(context.Background(), "message", staged.Fingerprint)
	if !IsKind(err, ErrMissingIdentity) {
		var typedErr *Error
		t.Logf("typed=%v kind=%v output=%q cause=%v", errors.As(err, &typedErr), typedErr.Kind, typedErr.Output, typedErr.Cause)
		t.Fatalf("error=%v", err)
	}
	after, inspectErr := r.Inspect(context.Background())
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if len(after.Staged) == 0 {
		t.Fatal("failed commit lost staged changes")
	}
}
