package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

func requireGitAdministrativeWriteDenied(t *testing.T, outcome ConfirmOutcome, err error) {
	t.Helper()
	var toolErr *ToolError
	if outcome != Cancel || !errors.As(err, &toolErr) || toolErr.Type != ErrPermissionDenied {
		t.Fatalf("Git administrative write = %v, %v; want permission-denied cancellation", outcome, err)
	}
}

func TestGitMetadataSecurity_OrdinaryAuthoritiesCannotBypass(t *testing.T) {
	for _, authority := range []string{"allowlist", "directory_cache", "parent_directory_cache", "exact_cache", "project_cache", "parent_workspace", "additional_workspace", "guardian", "human_prompt", "extension"} {
		t.Run(authority, func(t *testing.T) {
			root := gitMetadataSecurityFixture(t)
			manager := NewApprovalManager(NewToolPermissions())
			manager.workspaceTrustStore = nil
			manager.IgnoreProjectApprovals = true
			metadata := filepath.Join(root, ".git", "config")
			switch authority {
			case "allowlist":
				if err := manager.permissions.AddWriteDir(root); err != nil {
					t.Fatal(err)
				}
			case "directory_cache":
				manager.dirCache.Set(root, ProceedAlways, true)
			case "parent_directory_cache":
				parent := NewApprovalManager(NewToolPermissions())
				parent.dirCache.Set(root, ProceedAlways, true)
				if err := manager.SetParent(parent); err != nil {
					t.Fatal(err)
				}
			case "exact_cache":
				manager.ApprovePath(WriteFileToolName, metadata, ProceedAlways)
			case "project_cache":
				manager.IgnoreProjectApprovals = false
				manager.projectCache[root] = &ProjectApprovals{RepoRoot: root, WriteApproved: true}
				manager.rememberSessionProjectGrant(filepath.Join(root, "source.txt"), true)
			case "parent_workspace":
				parent := NewApprovalManager(NewToolPermissions())
				parent.workspaceTrustStore = nil
				if err := parent.SetPrimaryWorkspace(root); err != nil {
					t.Fatal(err)
				}
				parent.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
					return WorkspaceApprovalResult{Approved: true}, nil
				}
				if err := parent.EnsurePrimaryWorkspaceAccess(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := manager.SetParent(parent); err != nil {
					t.Fatal(err)
				}
			case "additional_workspace":
				manager.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
					return ApprovalResult{Choice: ApprovalChoiceOnce}, nil
				}
				if _, err := manager.GrantWorkspace(context.Background(), root, session.WorkspaceAccessWrite, "edit repository files"); err != nil {
					t.Fatal(err)
				}
			case "guardian":
				manager.SetApprovalMode(ModeAuto)
			case "extension":
				manager.extensionDirectory = root
			}
			manager.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
				t.Error("Git metadata write reached Guardian; generic policy cannot authorize it")
				return PolicyDecision{Allowed: true}, nil
			}, nil)
			manager.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
				t.Error("Git metadata write reached a generic human approval prompt")
				return ApprovalResult{Choice: ApprovalChoiceDirectory, Path: root}, nil
			}
			outcome, err := manager.CheckPathApprovalWithContext(context.Background(), WriteFileToolName, metadata, metadata, true)
			requireGitAdministrativeWriteDenied(t, outcome, err)
		})
	}
}

func TestGitMetadataSecurity_ReadsAndYoloUnchanged(t *testing.T) {
	root := gitMetadataSecurityFixture(t)
	manager := NewApprovalManager(NewToolPermissions())
	manager.IgnoreProjectApprovals = true
	manager.dirCache.Set(root, ProceedAlways, true)
	metadata := filepath.Join(root, ".git", "config")
	outcome, err := manager.CheckPathApproval(ReadFileToolName, metadata, metadata, false)
	if err != nil || outcome == Cancel {
		t.Fatalf("ordinary repo authority must still permit metadata reads: %v, %v", outcome, err)
	}
	for _, tool := range []string{WriteFileToolName, EditFileToolName, UnifiedDiffToolName} {
		t.Run(tool, func(t *testing.T) {
			writeGitMetadataSecurityFile(t, metadata, "# before\n")
			manager.SetApprovalMode(ModeYolo)
			output := executeGitMetadataSecurityTool(t, manager, tool, metadata)
			data, err := os.ReadFile(metadata)
			if err != nil || string(data) != "# after\n" || output.IsError {
				t.Fatalf("yolo metadata write = %q, %v, %+v", data, err, output)
			}
			manager.SetApprovalMode(ModePrompt)
			outcome, err := manager.CheckPathApproval(tool, metadata, metadata, true)
			requireGitAdministrativeWriteDenied(t, outcome, err)
		})
	}
}

func TestGitMetadataSecurity_GitEntrySymlinkPreservesRequestedPath(t *testing.T) {
	for _, tool := range []string{WriteFileToolName, EditFileToolName, UnifiedDiffToolName} {
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "gitfile")
			writeGitMetadataSecurityFile(t, target, "# before\n")
			if err := os.Symlink("gitfile", filepath.Join(root, ".git")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			manager := NewApprovalManager(NewToolPermissions())
			if err := manager.permissions.AddWriteDir(root); err != nil {
				t.Fatal(err)
			}
			config := &ToolConfig{BaseDir: root}
			output := executeGitMetadataSecurityTool(t, manager, tool, ".git", config)
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "# before\n" || !output.IsError {
				t.Fatalf("symlinked .git entry must not mutate its otherwise ordinary target: %q, %v, %+v", data, err, output)
			}
		})
	}
}

func TestGitMetadataSecurity_RevalidatesAdministrativeDirectories(t *testing.T) {
	root := t.TempDir()
	admin := filepath.Join(root, "arbitrary-store")
	path := filepath.Join(admin, "config")
	writeGitMetadataSecurityFile(t, path, "# before\n")
	manager := NewApprovalManager(NewToolPermissions())
	manager.dirCache.Set(root, ProceedAlways, true)
	if outcome, err := manager.CheckPathApproval(WriteFileToolName, path, path, true); err != nil || outcome == Cancel {
		t.Fatalf("ordinary config filename was blocked: %v, %v", outcome, err)
	}
	createGitMetadataSecurityAdmin(t, admin)
	outcome, err := manager.CheckPathApproval(WriteFileToolName, path, path, true)
	requireGitAdministrativeWriteDenied(t, outcome, err)
}

// A nil manager is the pre-existing, intentionally trusted embedding API. The
// security boundary applies when file tools are wired to an approval manager.
func TestGitMetadataSecurity_TrustedEmbeddingUnchanged(t *testing.T) {
	for _, tool := range []string{WriteFileToolName, EditFileToolName, UnifiedDiffToolName} {
		t.Run(tool, func(t *testing.T) {
			root := gitMetadataSecurityFixture(t)
			metadata := filepath.Join(root, ".git", "config")
			output := executeGitMetadataSecurityTool(t, nil, tool, metadata)
			data, err := os.ReadFile(metadata)
			if err != nil || string(data) != "# after\n" || output.IsError {
				t.Fatalf("trusted nil-manager write = %q, %v, %+v", data, err, output)
			}
		})
	}
}
