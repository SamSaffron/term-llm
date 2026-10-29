package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// Exercise the public approval path with approvals persisted by a different
// manager, not a session shell-pattern grant.
func TestApprovalManager_GitIndirectionPersistedShellApproval(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	trusted := t.TempDir()
	runGitTestCommand(t, trusted, "init", "-q")
	runGitTestCommand(t, trusted, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, trusted, "worktree", "add", "--detach", linked, "HEAD")

	approvals, err := LoadProjectApprovals(trusted)
	if err != nil {
		t.Fatal(err)
	}
	if err := approvals.ApproveShellPattern("make *"); err != nil {
		t.Fatal(err)
	}

	linkedGitFile, err := os.ReadFile(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		gitFile []byte
	}{
		{"main_git_dir", []byte("gitdir: " + filepath.Join(trusted, ".git") + "\n")},
		{"registered_worktree_git_dir", linkedGitFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostile := t.TempDir()
			if err := os.WriteFile(filepath.Join(hostile, ".git"), tc.gitFile, 0o644); err != nil {
				t.Fatal(err)
			}
			// Prove Git accepts the forged .git file; rejection must come from
			// approval identity validation, not an invalid Git fixture.
			runGitTestCommand(t, hostile, "rev-parse", "--git-common-dir")

			for _, warmTrusted := range []bool{false, true} {
				mgr := NewApprovalManager(NewToolPermissions())
				if warmTrusted {
					for _, dir := range []string{trusted, linked} {
						outcome, err := mgr.CheckShellApproval("make build", dir)
						if err != nil || outcome != ProceedAlways {
							t.Fatalf("legitimate repo %s: outcome=%v err=%v", dir, outcome, err)
						}
					}
				}
				prompted := 0
				mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
					prompted++
					return Cancel, ""
				}
				outcome, err := mgr.CheckShellApproval("make build", hostile)
				if err != nil || outcome != Cancel || prompted != 1 {
					t.Errorf("forged .git (warmTrusted=%v): outcome=%v err=%v prompts=%d; must prompt, not inherit trusted make *", warmTrusted, outcome, err, prompted)
				}
				if warmTrusted {
					// Cached identities must not require another Git subprocess.
					t.Setenv("PATH", t.TempDir())
					for _, dir := range []string{trusted, linked} {
						outcome, err := mgr.CheckShellApproval("make test", dir)
						if err != nil || outcome != ProceedAlways {
							t.Errorf("cached legitimate repo %s: outcome=%v err=%v", dir, outcome, err)
						}
					}
				}
			}
		})
	}
}
