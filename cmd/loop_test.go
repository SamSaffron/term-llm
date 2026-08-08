package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestLoopWorkspaceApprovalTransport(t *testing.T) {
	for _, mode := range []tools.ApprovalMode{tools.ModeAuto, tools.ModeYolo} {
		if transport := loopWorkspaceApprovalTransport(mode); transport != nil {
			t.Fatalf("loop mode %v configured a human workspace approval transport", mode)
		}
	}
	if transport := loopWorkspaceApprovalTransport(tools.ModePrompt); transport == nil {
		t.Fatal("prompt loop mode has no workspace approval transport")
	}

	workspace := t.TempDir()
	path := filepath.Join(workspace, "fixture.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	permissions := tools.NewToolPermissions()
	if err := permissions.AddReadDir(workspace); err != nil {
		t.Fatal(err)
	}

	t.Run("unattended auto fails closed", func(t *testing.T) {
		mgr := tools.NewApprovalManager(permissions)
		mgr.SetApprovalMode(tools.ModeAuto)
		mgr.SetAutoHeadless(true)
		if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
			t.Fatal(err)
		}
		mgr.WorkspacePromptFunc = loopWorkspaceApprovalTransport(tools.ModeAuto)
		guardianCalls := 0
		mgr.SetPolicyReviewFunc(func(context.Context, tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
			guardianCalls++
			return tools.PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
		}, nil)

		outcome, err := mgr.CheckPathApproval(tools.ReadFileToolName, path, path, false)
		if outcome != tools.Cancel || err == nil || !strings.Contains(err.Error(), "no workspace approval transport is available") {
			t.Fatalf("first workspace access = %v, %v; want fail-closed transport denial", outcome, err)
		}
		if guardianCalls != 0 {
			t.Fatalf("Guardian reviewed first workspace access %d times; want no bypass of direct-human boundary", guardianCalls)
		}
	})

	t.Run("explicit yolo bypasses callback", func(t *testing.T) {
		mgr := tools.NewApprovalManager(permissions)
		mgr.SetApprovalMode(tools.ModeYolo)
		if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
			t.Fatal(err)
		}
		promptCalls := 0
		mgr.WorkspacePromptFunc = func(string) (tools.WorkspaceApprovalResult, error) {
			promptCalls++
			return tools.WorkspaceApprovalResult{Approved: true}, nil
		}

		outcome, err := mgr.CheckPathApproval(tools.ReadFileToolName, path, path, false)
		if outcome != tools.ProceedOnce || err != nil {
			t.Fatalf("yolo workspace access = %v, %v", outcome, err)
		}
		if promptCalls != 0 || mgr.IsWorkspacePathAllowed(path, false) {
			t.Fatalf("yolo prompt calls=%d persisted authority=%v", promptCalls, mgr.IsWorkspacePathAllowed(path, false))
		}
	})
}
