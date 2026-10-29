package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApprovalProjectDoesNotAuthorizeGitMetadata(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, root, "init", "-q")
	runGitTestCommand(t, root, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, root, "worktree", "add", "--detach", linked, "HEAD")
	bare := filepath.Join(root, "bare-store")
	runGitTestCommand(t, root, "init", "--bare", "-q", bare)
	separate := filepath.Join(root, "admin-store")
	runGitTestCommand(t, root, "init", "-q", "--separate-git-dir", separate, filepath.Join(t.TempDir(), "checkout"))
	for _, path := range []string{filepath.Join(root, "src", "main.go"), filepath.Join(root, ".git", "hooks", "pre-commit")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pa, err := LoadProjectApprovals(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := pa.ApproveRead(); err != nil {
		t.Fatal(err)
	}
	if err := pa.ApproveWrite(); err != nil {
		t.Fatal(err)
	}
	paths := []struct{ name, path string }{
		{"git_entry", filepath.Join(root, ".git")},
		{"config", filepath.Join(root, ".git", "config")},
		{"hook", filepath.Join(root, ".git", "hooks", "pre-commit")},
		{"missing_hook", filepath.Join(root, ".git", "hooks", "new-hook")},
		{"missing_git_entry", filepath.Join(root, "src", ".git")},
		{"missing_metadata_tree", filepath.Join(root, "src", ".git", "hooks", "new-hook")},
		{"worktree_gitfile", filepath.Join(linked, ".git")},
		{"bare_entry", bare},
		{"bare_config", filepath.Join(bare, "config")},
		{"bare_missing_hook", filepath.Join(bare, "hooks", "new-hook")},
		{"separate_entry", separate},
		{"separate_config", filepath.Join(separate, "config")},
		{"separate_missing_hook", filepath.Join(separate, "hooks", "new-hook")},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			for _, isWrite := range []bool{false, true} {
				if _, err := os.Stat(tc.path); !isWrite && os.IsNotExist(err) {
					continue // Missing reads are denied before approval discovery.
				}
				mgr := NewApprovalManager(NewToolPermissions())
				prompts := 0
				mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
					prompts++
					return Cancel, ""
				}
				tool := ReadFileToolName
				if isWrite {
					tool = WriteFileToolName
				}
				// Ordinary source remains approved and warms the data cache.
				outcome, err := mgr.CheckPathApproval(tool, filepath.Join(root, "src", "main.go"), "", isWrite)
				if err != nil || outcome != ProceedAlways || prompts != 0 {
					t.Fatalf("source (write=%v): outcome=%v err=%v prompts=%d", isWrite, outcome, err, prompts)
				}
				outcome, err = mgr.CheckPathApproval(tool, tc.path, "", isWrite)
				if isWrite {
					requireGitAdministrativeWriteDenied(t, outcome, err)
					if prompts != 0 {
						t.Errorf("metadata write prompted %d times; want fail-closed denial", prompts)
					}
				} else if err != nil || outcome != Cancel || prompts != 1 {
					t.Errorf("metadata read: outcome=%v err=%v prompts=%d; want prompted cancel", outcome, err, prompts)
				}
			}
			if got := DetectGitRepo(tc.path); got.IsRepo {
				t.Errorf("metadata must not have a project approval identity: %+v", got)
			}
		})
	}
	for _, target := range []string{filepath.Join(root, ".git"), filepath.Join(root, ".git", "config"), filepath.Join(linked, ".git"), filepath.Join(separate, "config"), bare} {
		t.Run("symlink_"+filepath.Base(filepath.Dir(target))+"_"+filepath.Base(target), func(t *testing.T) {
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(target, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			t.Cleanup(func() { _ = os.Remove(alias) })
			if got := DetectGitRepo(alias); got.IsRepo {
				t.Errorf("resolved metadata must not have a project approval identity: %+v", got)
			}
			for _, isWrite := range []bool{false, true} {
				mgr := NewApprovalManager(NewToolPermissions())
				mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
				outcome, _ := mgr.CheckPathApproval(WriteFileToolName, alias, "", isWrite)
				// The existing symlink-escape guard may deny without prompting;
				// project approval must never authorize the resolved metadata.
				if outcome != Cancel {
					t.Errorf("symlink metadata (write=%v): outcome=%v; want cancel", isWrite, outcome)
				}
			}
		})
	}
}
