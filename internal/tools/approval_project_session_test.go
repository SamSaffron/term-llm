package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeProjectSessionFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkProjectSessionPath(t *testing.T, mgr *ApprovalManager, path string, isWrite bool, want ConfirmOutcome, prompts *int) {
	t.Helper()
	before := *prompts
	tool := ReadFileToolName
	if isWrite {
		tool = WriteFileToolName
	}
	outcome, err := mgr.CheckPathApproval(tool, path, "", isWrite)
	wantPrompts := 0
	if want == Cancel {
		wantPrompts = 1
	}
	if err != nil || outcome != want || *prompts-before != wantPrompts {
		t.Errorf("%s (write=%v): outcome=%v err=%v prompts=%d; want %v, prompts=%d", path, isWrite, outcome, err, *prompts-before, want, wantPrompts)
	}
}

func TestApprovalProjectUIRepoGrantSessionBoundaries(t *testing.T) {
	for _, isWrite := range []bool{false, true} {
		for _, ignore := range []bool{false, true} {
			for _, storage := range []string{"normal", "load_error", "save_error"} {
				t.Run(fmt.Sprintf("write=%v/ignore=%v/%s", isWrite, ignore, storage), func(t *testing.T) {
					config := t.TempDir()
					t.Setenv("XDG_CONFIG_HOME", config)
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
					source := filepath.Join(root, "src", "main.go")
					otherSource := filepath.Join(root, "src", "other.go")
					linkedSource := filepath.Join(linked, "src", "main.go")
					nested := filepath.Join(root, "nested")
					nestedSource := filepath.Join(nested, "main.go")
					hook := filepath.Join(root, ".git", "hooks", "pre-commit")
					for _, path := range []string{source, otherSource, linkedSource, nestedSource, hook} {
						writeProjectSessionFixture(t, path)
					}
					if ignore && storage == "normal" {
						// Disk grants must not be promoted into the explicit session
						// grant. In particular a RepoRead choice must not enable writes.
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
					}
					if storage == "load_error" {
						writeProjectSessionFixture(t, filepath.Join(config, "term-llm"))
					}
					owner := NewApprovalManager(NewToolPermissions())
					owner.IgnoreProjectApprovals = ignore
					child := NewApprovalManager(NewToolPermissions())
					child.IgnoreProjectApprovals = ignore
					if err := child.SetParent(owner); err != nil {
						t.Fatal(err)
					}
					grandchild := NewApprovalManager(NewToolPermissions())
					grandchild.IgnoreProjectApprovals = ignore
					if err := grandchild.SetParent(child); err != nil {
						t.Fatal(err)
					}
					prompts := 0
					owner.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
						prompts++
						if prompts != 1 {
							return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
						}
						if storage == "save_error" {
							// Discovery already loaded approvals; make only the save fail.
							writeProjectSessionFixture(t, filepath.Join(config, "term-llm"))
						}
						choice := ApprovalChoiceRepoRead
						if isWrite {
							choice = ApprovalChoiceRepoWrite
						}
						return ApprovalResult{Choice: choice, Path: root, SaveToRepo: true}, nil
					}
					tool := ReadFileToolName
					if isWrite {
						tool = WriteFileToolName
					}
					outcome, err := owner.CheckPathApproval(tool, source, "", isWrite)
					if err != nil || outcome != ProceedAlways || prompts != 1 {
						t.Fatalf("UI repo choice: outcome=%v err=%v prompts=%d", outcome, err, prompts)
					}
					for _, mgr := range []*ApprovalManager{owner, child, grandchild} {
						for _, path := range []string{otherSource, linkedSource, nestedSource} {
							checkProjectSessionPath(t, mgr, path, isWrite, ProceedAlways, &prompts)
						}
						// Write grants also permit reads; read grants must not permit
						// writes, even if ignored disk approvals happened to allow them.
						opposite := Cancel
						if isWrite {
							opposite = ProceedAlways
						}
						checkProjectSessionPath(t, mgr, otherSource, !isWrite, opposite, &prompts)
						for _, path := range []string{filepath.Join(root, ".git"), filepath.Join(root, ".git", "config"), hook, filepath.Join(linked, ".git"), filepath.Join(bare, "config"), filepath.Join(separate, "config")} {
							if isWrite {
								before := prompts
								outcome, err := mgr.CheckPathApproval(WriteFileToolName, path, "", true)
								requireGitAdministrativeWriteDenied(t, outcome, err)
								if prompts != before {
									t.Error("metadata write reached a generic approval prompt")
								}
							} else {
								checkProjectSessionPath(t, mgr, path, false, Cancel, &prompts)
							}
						}
					}
					// A path approved earlier cannot inherit its ancestor's grant
					// after it becomes an independent repository.
					runGitTestCommand(t, root, "init", "-q", nested)
					for _, mgr := range []*ApprovalManager{owner, child, grandchild} {
						checkProjectSessionPath(t, mgr, nestedSource, isWrite, Cancel, &prompts)
					}
					// An unrelated ignoring manager never inherits this session's
					// choice, even when persistence succeeded.
					fresh := NewApprovalManager(NewToolPermissions())
					fresh.IgnoreProjectApprovals = true
					fresh.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
						prompts++
						return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
					}
					checkProjectSessionPath(t, fresh, source, isWrite, Cancel, &prompts)
				})
			}
		}
	}
}

func TestApprovalProjectUIRepoGrantUsesActualRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, other := t.TempDir(), t.TempDir()
	for _, dir := range []string{root, other} {
		runGitTestCommand(t, dir, "init", "-q")
		writeProjectSessionFixture(t, filepath.Join(dir, "main.go"))
	}
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.IgnoreProjectApprovals = true
	prompts := 0
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompts++
		if prompts == 1 {
			return ApprovalResult{Choice: ApprovalChoiceRepoWrite, Path: other}, nil
		}
		return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
	}
	path := filepath.Join(root, "main.go")
	outcome, err := mgr.CheckPathApproval(WriteFileToolName, path, "", true)
	if err != nil || outcome != ProceedAlways || prompts != 1 {
		t.Fatalf("UI repo choice: outcome=%v err=%v prompts=%d", outcome, err, prompts)
	}
	checkProjectSessionPath(t, mgr, path, true, ProceedAlways, &prompts)
	checkProjectSessionPath(t, mgr, filepath.Join(other, "main.go"), true, Cancel, &prompts)
}
