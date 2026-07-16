package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApprovalManagerExplicitMutationPolicyOverridesYoloAndCaches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	perms := NewToolPermissions()
	if err := perms.AddWriteDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := perms.AddShellPattern("*"); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(perms)
	mgr.SetYoloMode(true)
	mgr.SetRequireExplicitMutations(true)
	prompts := 0
	mgr.PromptUIFunc = func(value string, isWrite, isShell bool, workDir string) (ApprovalResult, error) {
		prompts++
		return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
	}
	if outcome, err := mgr.CheckPathApproval(WriteFileToolName, path, path, true); err != nil || outcome != ProceedOnce {
		t.Fatalf("write outcome=%v err=%v", outcome, err)
	}
	if outcome, err := mgr.CheckShellApproval("echo mutate", dir); err != nil || outcome != ProceedOnce {
		t.Fatalf("shell outcome=%v err=%v", outcome, err)
	}
	if prompts != 2 {
		t.Fatalf("prompts=%d, want 2", prompts)
	}

	// "Always" choices are deliberately narrowed to the current mutation and
	// never enter side-session caches (or project persistence).
	mgr.PromptUIFunc = func(value string, isWrite, isShell bool, workDir string) (ApprovalResult, error) {
		prompts++
		if isShell {
			return ApprovalResult{Choice: ApprovalChoicePattern, Pattern: "echo *", SaveToRepo: true}, nil
		}
		return ApprovalResult{Choice: ApprovalChoiceRepoWrite, Path: dir}, nil
	}
	for i := 0; i < 2; i++ {
		if outcome, err := mgr.CheckPathApproval(WriteFileToolName, path, path, true); err != nil || outcome != ProceedOnce {
			t.Fatalf("remembered write outcome=%v err=%v", outcome, err)
		}
		if outcome, err := mgr.CheckShellApproval("echo mutate", dir); err != nil || outcome != ProceedOnce {
			t.Fatalf("remembered shell outcome=%v err=%v", outcome, err)
		}
	}
	if prompts != 6 {
		t.Fatalf("always choices escaped one-shot policy; prompts=%d, want 6", prompts)
	}

	// Reads remain eligible for deterministic approval; the side policy is about
	// shared-state mutation rather than making inherited context unusable.
	if err := perms.AddReadDir(dir); err != nil {
		t.Fatal(err)
	}
	if outcome, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedOnce {
		t.Fatalf("read outcome=%v err=%v", outcome, err)
	}
	if prompts != 6 {
		t.Fatalf("read unexpectedly prompted; prompts=%d", prompts)
	}
}
