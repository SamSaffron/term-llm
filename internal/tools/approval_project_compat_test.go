package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitIdentitySeparateGitDirectory(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "checkout")
	admin := filepath.Join(t.TempDir(), "admin")
	runGitTestCommand(t, filepath.Dir(checkout), "init", "-q", "--separate-git-dir", admin, checkout)
	if got := DetectGitRepo(checkout); !got.IsRepo || got.Root != checkout {
		t.Fatalf("separate Git directory must use checkout identity: %+v", got)
	}
}

func TestGitIdentitySubmoduleOwnsApprovals(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source, parent := t.TempDir(), t.TempDir()
	for _, root := range []string{source, parent} {
		runGitTestCommand(t, root, "init", "-q")
		runGitTestCommand(t, root, "commit", "--allow-empty", "-q", "-m", "init")
	}
	submodule := filepath.Join(parent, "child")
	runGitTestCommand(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", source, "child")
	persistMakeApproval(t, parent)
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
	requireMakeApproval(t, mgr, parent, ProceedAlways)
	requireMakeApproval(t, mgr, submodule, Cancel)
	if got := DetectGitRepo(submodule); !got.IsRepo || got.Root != submodule {
		t.Fatalf("submodule must use its own checkout identity: %+v", got)
	}
}

func TestGitIdentityPhysicalFileSymlink(t *testing.T) {
	trusted, other := t.TempDir(), t.TempDir()
	for _, root := range []string{trusted, other} {
		runGitTestCommand(t, root, "init", "-q")
	}
	target := filepath.Join(other, "target")
	if err := os.WriteFile(target, []byte("outside trusted checkout"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(trusted, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := DetectGitRepo(alias); !got.IsRepo || got.Root != other {
		t.Fatalf("file symlink must use physical target checkout: %+v", got)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if got := DetectGitRepo(alias); got.IsRepo {
		t.Fatalf("dangling file symlink must fail closed: %+v", got)
	}
}

func TestApprovalProjectNoGitSubprocess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	trusted := t.TempDir()
	runGitTestCommand(t, trusted, "init", "-q")
	runGitTestCommand(t, trusted, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, trusted, "worktree", "add", "--detach", linked, "HEAD")
	persistMakeApproval(t, trusted)
	mgr := NewApprovalManager(NewToolPermissions())
	for _, root := range []string{trusted, linked} {
		requireMakeApproval(t, mgr, root, ProceedAlways)
	}
	// Even a never-seen path and cold manager can discover approvals without
	// Git. Warm successes must not depend on a stale exact-path identity cache.
	t.Setenv("PATH", t.TempDir())
	for _, manager := range []*ApprovalManager{mgr, NewApprovalManager(NewToolPermissions())} {
		for _, root := range []string{trusted, linked} {
			requireMakeApproval(t, manager, root, ProceedAlways)
			requireMakeApproval(t, manager, filepath.Join(root, "new", "file"), ProceedAlways)
		}
	}
}

func TestApprovalProjectInvalidGitEntryStopsAncestorTrust(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	trusted := t.TempDir()
	runGitTestCommand(t, trusted, "init", "-q")
	persistMakeApproval(t, trusted)
	path := filepath.Join(trusted, "nested")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
	requireMakeApproval(t, mgr, path, Cancel)
}

func TestGitIdentityReftableMainAndWorktree(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	if out, err := runGitTestCommandAllowError(root, "init", "-q", "--ref-format=reftable"); err != nil {
		t.Skipf("Git does not support reftable: %v\n%s", err, out)
	}
	runGitTestCommand(t, root, "commit", "--allow-empty", "-q", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	runGitTestCommand(t, root, "worktree", "add", "--detach", linked, "HEAD")
	persistMakeApproval(t, root)
	mgr := NewApprovalManager(NewToolPermissions())
	for _, path := range []string{root, linked} {
		if got := DetectGitRepo(path); !got.IsRepo || got.Root != root {
			t.Fatalf("reftable checkout %s: %+v", path, got)
		}
		requireMakeApproval(t, mgr, path, ProceedAlways)
	}
}

func TestGitIdentityPhysicalParentAfterSymlink(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	trusted, other := t.TempDir(), t.TempDir()
	for _, root := range []string{trusted, other} {
		runGitTestCommand(t, root, "init", "-q")
	}
	subdir := filepath.Join(other, "child")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(trusted, "alias")
	if err := os.Symlink(subdir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	persistMakeApproval(t, trusted)
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
	// Do not use filepath.Join here: it erases alias/.. lexically, before
	// the operating system can resolve the symlink's physical parent.
	path := alias + string(filepath.Separator) + ".."
	for _, target := range []string{path, path + string(filepath.Separator) + "missing"} {
		if got := DetectGitRepo(target); !got.IsRepo || got.Root != other {
			t.Fatalf("physical parent checkout %s: %+v, want %s", target, got, other)
		}
		requireMakeApproval(t, mgr, target, Cancel)
	}
}

func TestApprovalProjectSavePatternUsesPhysicalWorkDir(t *testing.T) {
	for _, variant := range []string{"symlink_parent", "trailing_space", "empty_cwd"} {
		t.Run(variant, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			wrong, physical := t.TempDir(), t.TempDir()
			workDir := physical
			switch variant {
			case "symlink_parent":
				child := filepath.Join(physical, "child")
				if err := os.Mkdir(child, 0o700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(wrong, "alias")
				if err := os.Symlink(child, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				workDir = alias + string(filepath.Separator) + ".."
			case "trailing_space":
				physical = wrong + " "
				if err := os.Mkdir(physical, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(physical) })
				workDir = physical
			case "empty_cwd":
				t.Chdir(physical)
				workDir = ""
			}
			for _, root := range []string{wrong, physical} {
				runGitTestCommand(t, root, "init", "-q")
			}
			mgr := NewApprovalManager(NewToolPermissions())
			prompts := 0
			mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
				prompts++
				return ApprovalResult{Choice: ApprovalChoicePattern, Pattern: "make *", SaveToRepo: true}, nil
			}
			requireMakeApproval(t, mgr, workDir, ProceedAlways)
			if prompts != 1 {
				t.Fatalf("save-pattern prompts = %d, want 1", prompts)
			}
			// A fresh manager must find the durable grant only in the physical
			// checkout, not inherit the prompting manager's session pattern.
			fresh := NewApprovalManager(NewToolPermissions())
			fresh.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) { return Cancel, "" }
			requireMakeApproval(t, fresh, physical, ProceedAlways)
			requireMakeApproval(t, fresh, wrong, Cancel)
			requireMakeApproval(t, fresh, workDir, ProceedAlways)
		})
	}
}
