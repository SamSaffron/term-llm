package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// Ordinary repository approval is not consent to change Git's execution/config
// metadata. Exercise the real tools, not just Git project-identity discovery:
// primary workspace grants take a different approval path from project grants.
func TestGitMetadataSecurity_OrdinaryRepoApproval(t *testing.T) {
	for _, toolName := range []string{WriteFileToolName, EditFileToolName, UnifiedDiffToolName} {
		for _, target := range []string{"ordinary_file", "github_config", "config", "existing_hook", "new_hook", "config_symlink", "git_entry", "bare_config", "separate_config"} {
			if toolName != WriteFileToolName && target == "new_hook" {
				continue // edit_file requires an existing file.
			}
			t.Run(toolName+"/"+target, func(t *testing.T) {
				root := gitMetadataSecurityFixture(t)
				path := filepath.Join(root, "source.txt")
				switch target {
				case "github_config":
					path = filepath.Join(root, ".github", "config")
					writeGitMetadataSecurityFile(t, path, "# before\n")
				case "config":
					path = filepath.Join(root, ".git", "config")
				case "existing_hook":
					path = filepath.Join(root, ".git", "hooks", "pre-commit")
				case "new_hook":
					path = filepath.Join(root, ".git", "hooks", "commit-msg")
				case "git_entry":
					// A linked worktree's .git entry is a regular gitfile.
					path = filepath.Join(root, "linked", ".git")
					admin := filepath.Join(root, ".git", "worktrees", "linked")
					writeGitMetadataSecurityFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/main\n")
					writeGitMetadataSecurityFile(t, filepath.Join(admin, "commondir"), "../..\n")
					writeGitMetadataSecurityFile(t, filepath.Join(admin, "gitdir"), path+"\n")
					writeGitMetadataSecurityFile(t, path, "gitdir: ../.git/worktrees/linked\n")
				case "bare_config", "separate_config":
					admin := filepath.Join(root, target+"-store")
					createGitMetadataSecurityAdmin(t, admin)
					path = filepath.Join(admin, "config")
					if target == "separate_config" {
						writeGitMetadataSecurityFile(t, filepath.Join(root, "separate", ".git"), "gitdir: "+admin+"\n")
					}
				case "config_symlink":
					path = filepath.Join(root, "config-alias")
					if err := os.Symlink(filepath.Join(root, ".git", "config"), path); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}

				manager := NewApprovalManager(NewToolPermissions())
				manager.SetApprovalMode(ModePrompt)
				manager.IgnoreProjectApprovals = true
				manager.workspaceTrustStore = nil // No persisted trust/config or Git subprocesses.
				if err := manager.SetPrimaryWorkspace(root); err != nil {
					t.Fatal(err)
				}
				workspacePrompts := 0
				manager.WorkspacePromptFunc = func(got string) (WorkspaceApprovalResult, error) {
					workspacePrompts++
					if got != root {
						t.Fatalf("workspace prompt = %q, want %q", got, root)
					}
					return WorkspaceApprovalResult{Approved: true}, nil
				}
				if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); err != nil {
					t.Fatal(err)
				}
				distinctPrompts := 0
				manager.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
					distinctPrompts++
					return ApprovalResult{Choice: ApprovalChoiceDeny}, nil
				}

				original, err := os.ReadFile(path)
				if target != "new_hook" && err != nil {
					t.Fatal(err)
				}
				output := executeGitMetadataSecurityTool(t, manager, toolName, path)
				if workspacePrompts != 1 {
					t.Errorf("workspace prompts = %d, want exactly one ordinary repo confirmation", workspacePrompts)
				}
				data, err := os.ReadFile(path)
				if target == "ordinary_file" || target == "github_config" {
					if err != nil || string(data) != "# after\n" || output.IsError || distinctPrompts != 0 {
						t.Fatalf("ordinary repo write must remain autoapproved: content=%q err=%v output=%+v distinct prompts=%d", data, err, output, distinctPrompts)
					}
					return
				}

				if target == "new_hook" {
					if !os.IsNotExist(err) {
						t.Errorf("Git hook was created without distinct approval: content=%q err=%v distinct prompts=%d; want file to remain absent", data, err, distinctPrompts)
					}
				} else if err != nil || string(data) != string(original) {
					t.Errorf("Git execution/config metadata changed without distinct approval: content=%q err=%v distinct prompts=%d; want original content %q", data, err, distinctPrompts, original)
				}
				if !output.IsError && distinctPrompts == 0 {
					t.Errorf("ordinary repo approval silently authorized Git metadata mutation: output=%q distinct prompts=%d; want a permission error or a distinct human approval request denied by the test", output.Content, distinctPrompts)
				}
			})
		}
	}
}

// Construct only the minimal on-disk checkout layout recognized by gitidentity.
// No Git command runs; config and hooks contain inert comments, and hooks are
// non-executable. All tool writes stay in this temporary repository and never
// touch the user's repository or config.
func gitMetadataSecurityFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	createGitMetadataSecurityAdmin(t, filepath.Join(root, ".git"))
	writeGitMetadataSecurityFile(t, filepath.Join(root, "source.txt"), "# before\n")
	return root
}

func createGitMetadataSecurityAdmin(t *testing.T, admin string) {
	t.Helper()
	for _, dir := range []string{"objects", "refs", "hooks"} {
		if err := os.MkdirAll(filepath.Join(admin, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"HEAD": "ref: refs/heads/main\n", "config": "# before\n", filepath.Join("hooks", "pre-commit"): "# before\n",
	} {
		writeGitMetadataSecurityFile(t, filepath.Join(admin, path), content)
	}
}

func writeGitMetadataSecurityFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func executeGitMetadataSecurityTool(t *testing.T, manager *ApprovalManager, toolName, path string, configs ...*ToolConfig) core.ToolOutput {
	t.Helper()
	var args any = WriteFileArgs{Path: path, Content: "# after\n"}
	if toolName != WriteFileToolName {
		fixturePath := path
		if len(configs) > 0 {
			fixturePath = configs[0].ResolvePath(path)
		}
		before, err := os.ReadFile(fixturePath)
		if err != nil {
			t.Fatal(err)
		}
		args = EditFileArgs{Path: path, OldText: string(before), NewText: "# after\n"}
		if toolName == UnifiedDiffToolName {
			args = UnifiedDiffArgs{Diff: fmt.Sprintf("--- %s\n+++ %s\n@@ -1 +1 @@\n-%s+# after\n", path, path, before)}
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var output core.ToolOutput
	switch toolName {
	case EditFileToolName:
		output, err = NewEditFileTool(manager, configs...).Execute(context.Background(), raw)
	case UnifiedDiffToolName:
		output, err = NewUnifiedDiffTool(manager, configs...).Execute(context.Background(), raw)
	default:
		output, err = NewWriteFileTool(manager, configs...).Execute(context.Background(), raw)
	}
	if err != nil {
		t.Fatalf("%s Execute returned unexpected Go error: %v", toolName, err)
	}
	return output
}
