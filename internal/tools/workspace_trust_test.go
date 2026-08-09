package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWorkspaceTrustStorePersistsExactWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "remembered-workspaces.yaml")
	store := &fileWorkspaceTrustStore{path: path}
	workspace := t.TempDir()
	other := t.TempDir()

	trusted, err := store.IsTrusted(context.Background(), workspace)
	if err != nil || trusted {
		t.Fatalf("initial trust = %v, %v", trusted, err)
	}
	if err := store.Remember(workspace); err != nil {
		t.Fatal(err)
	}
	trusted, err = (&fileWorkspaceTrustStore{path: path}).IsTrusted(context.Background(), workspace)
	if err != nil || !trusted {
		t.Fatalf("reloaded trust = %v, %v", trusted, err)
	}
	trusted, err = store.IsTrusted(context.Background(), other)
	if err != nil || trusted {
		t.Fatalf("unrelated workspace trust = %v, %v", trusted, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), workspace) != 1 {
		t.Fatalf("ledger did not contain one exact workspace record: %s", data)
	}
	if err := store.Remember(workspace); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), workspace) != 1 {
		t.Fatalf("duplicate remember changed ledger: %s", data)
	}
}

func TestFileWorkspaceTrustStoreMainWorktreeApprovalAppliesToLinkedWorktrees(t *testing.T) {
	main, linked, sibling := newWorkspaceTrustWorktrees(t)
	store := &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "remembered-workspaces.yaml")}
	if err := store.Remember(main); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{linked, sibling} {
		trusted, err := store.IsTrusted(context.Background(), workspace)
		if err != nil || !trusted {
			t.Fatalf("linked worktree %q trust = %v, %v", workspace, trusted, err)
		}
	}

	linkedOnly := &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "linked-only.yaml")}
	if err := linkedOnly.Remember(linked); err != nil {
		t.Fatal(err)
	}
	if trusted, err := linkedOnly.IsTrusted(context.Background(), main); err != nil || trusted {
		t.Fatalf("linked approval unexpectedly trusted main worktree: %v, %v", trusted, err)
	}
	if trusted, err := linkedOnly.IsTrusted(context.Background(), sibling); err != nil || trusted {
		t.Fatalf("linked approval unexpectedly trusted sibling worktree: %v, %v", trusted, err)
	}
}

func newWorkspaceTrustWorktrees(t *testing.T) (string, string, string) {
	t.Helper()
	main := t.TempDir()
	runGitTestCommand(t, main, "init", "-q")
	if err := os.WriteFile(filepath.Join(main, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, main, "add", "README.md")
	runGitTestCommand(t, main, "commit", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	sibling := filepath.Join(t.TempDir(), "sibling")
	runGitTestCommand(t, main, "worktree", "add", "--detach", linked, "HEAD")
	runGitTestCommand(t, main, "worktree", "add", "--detach", sibling, "HEAD")
	t.Cleanup(func() {
		_, _ = runGitTestCommandAllowError(main, "worktree", "remove", "--force", linked)
		_, _ = runGitTestCommandAllowError(main, "worktree", "remove", "--force", sibling)
	})
	return main, linked, sibling
}

func TestFileWorkspaceTrustStoreRequiresExactRegisteredWorktree(t *testing.T) {
	main, linked, _ := newWorkspaceTrustWorktrees(t)
	store := &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "remembered-workspaces.yaml")}
	if err := store.Remember(main); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(main, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if trusted, err := store.IsTrusted(context.Background(), nested); err != nil || trusted {
		t.Fatalf("nested directory trust = %v, %v", trusted, err)
	}

	fake := filepath.Join(t.TempDir(), "fake-worktree")
	if err := os.Mkdir(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedGitFile, err := os.ReadFile(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, ".git"), linkedGitFile, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(main, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(main, ".git"))
	t.Setenv("GIT_WORK_TREE", fake)
	if trusted, err := store.IsTrusted(context.Background(), fake); err != nil || trusted {
		t.Fatalf("spoofed worktree trust = %v, %v", trusted, err)
	}
}

func TestScrubGitRepositoryEnvironment(t *testing.T) {
	clean := scrubGitRepositoryEnvironment([]string{
		"PATH=/bin", "GIT_DIR=/tmp/repo/.git", "git_common_dir=/tmp/repo/.git", "GIT_WORK_TREE=/tmp/repo", "HOME=/tmp/home",
	})
	got := strings.Join(clean, "\n")
	if strings.Contains(strings.ToUpper(got), "GIT_DIR=") || strings.Contains(strings.ToUpper(got), "GIT_COMMON_DIR=") || strings.Contains(strings.ToUpper(got), "GIT_WORK_TREE=") {
		t.Fatalf("repository-selection environment was retained: %q", clean)
	}
	if !strings.Contains(got, "PATH=/bin") || !strings.Contains(got, "HOME=/tmp/home") {
		t.Fatalf("unrelated environment was removed: %q", clean)
	}
}

func TestFileWorkspaceTrustStoreRejectsInsecurePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered-workspaces.yaml")
	workspace := t.TempDir()
	data := []byte("version: 1\nworkspaces:\n  - path: " + workspace + "\n    approved_at: 2026-08-08T00:00:00Z\n")
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	store := &fileWorkspaceTrustStore{path: path}
	if trusted, err := store.IsTrusted(context.Background(), workspace); err == nil || trusted {
		t.Fatalf("insecure ledger trust = %v, %v", trusted, err)
	}
}

func TestFileWorkspaceTrustStoreRejectsMalformedLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered-workspaces.yaml")
	if err := os.WriteFile(path, []byte("version: [not-valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fileWorkspaceTrustStore{path: path}
	if trusted, err := store.IsTrusted(context.Background(), t.TempDir()); err == nil || trusted {
		t.Fatalf("malformed ledger trust = %v, %v", trusted, err)
	}
}
