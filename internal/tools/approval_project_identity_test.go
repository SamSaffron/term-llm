package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func persistMakeApproval(t *testing.T, root string) {
	t.Helper()
	pa, err := LoadProjectApprovals(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := pa.ApproveShellPattern("make *"); err != nil {
		t.Fatal(err)
	}
}

func requireMakeApproval(t *testing.T, mgr *ApprovalManager, path string, want ConfirmOutcome) {
	t.Helper()
	got, err := mgr.CheckShellApproval("make inspect", path)
	if err != nil || got != want {
		t.Fatalf("approval for %s = %v, %v; want %v (discovery=%+v)", path, got, err, want, DetectGitRepo(path))
	}
}

func TestApprovalProjectCoreWorktreeCannotRedirectTrust(t *testing.T) {
	for _, forged := range []bool{false, true} {
		name := "hostile_config"
		if forged {
			name = "trusted_config_and_forged_gitfile"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			trusted, hostile := t.TempDir(), t.TempDir()
			runGitTestCommand(t, trusted, "init", "-q")
			persistMakeApproval(t, trusted)
			if forged {
				runGitTestCommand(t, trusted, "config", "core.worktree", trusted)
				if err := os.WriteFile(filepath.Join(hostile, ".git"), []byte("gitdir: "+filepath.Join(trusted, ".git")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				runGitTestCommand(t, hostile, "init", "-q")
				runGitTestCommand(t, hostile, "config", "core.worktree", trusted)
			}
			// Git accepts this fixture and reports the trusted checkout even
			// though the command's physical cwd is hostile.
			runGitTestCommand(t, hostile, "rev-parse", "--show-toplevel")
			for _, warm := range []bool{false, true} {
				mgr := NewApprovalManager(NewToolPermissions())
				mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
				if warm {
					requireMakeApproval(t, mgr, trusted, ProceedAlways)
				}
				requireMakeApproval(t, mgr, hostile, Cancel)
			}
		})
	}
}

func TestApprovalProjectRevalidatesMembership(t *testing.T) {
	for _, change := range []string{"introduced_gitfile", "introduced_repo", "populated_missing_path", "symlink_retargeted", "replaced_worktree_gitfile", "changed_backlink", "changed_commondir"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			trusted, other := t.TempDir(), t.TempDir()
			runGitTestCommand(t, trusted, "init", "-q")
			runGitTestCommand(t, other, "init", "-q")
			persistMakeApproval(t, trusted)
			path := filepath.Join(trusted, "nested")
			if change != "populated_missing_path" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var admin string
			if change == "symlink_retargeted" {
				path = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(trusted, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if change == "replaced_worktree_gitfile" || change == "changed_backlink" || change == "changed_commondir" {
				runGitTestCommand(t, trusted, "commit", "--allow-empty", "-q", "-m", "init")
				path = filepath.Join(t.TempDir(), "linked")
				runGitTestCommand(t, trusted, "worktree", "add", "--detach", path, "HEAD")
				admin = filepath.Join(trusted, ".git", "worktrees", "linked")
			}
			mgr := NewApprovalManager(NewToolPermissions())
			mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
			requireMakeApproval(t, mgr, path, ProceedAlways)
			switch change {
			case "introduced_gitfile", "replaced_worktree_gitfile":
				if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "introduced_repo", "populated_missing_path":
				runGitTestCommand(t, trusted, "init", "-q", path)
			case "symlink_retargeted":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "changed_backlink":
				if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed_commondir":
				if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			requireMakeApproval(t, mgr, path, Cancel)
		})
	}
}

func TestGitIdentityWorktreeCRLFBacklink(t *testing.T) {
	trusted := t.TempDir()
	runGitTestCommand(t, trusted, "init", "-q")
	runGitTestCommand(t, trusted, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, trusted, "worktree", "add", "--detach", linked, "HEAD")
	admin := filepath.Join(trusted, ".git", "worktrees", "linked")
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(linked, ".git")+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, linked, "rev-parse", "--show-toplevel")
	if got, want := GetGitRepoID(linked), GetGitRepoID(trusted); got != want {
		t.Fatalf("CRLF registered worktree identity = %s, want %s", got, want)
	}
}
