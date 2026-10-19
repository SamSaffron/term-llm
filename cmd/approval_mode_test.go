package cmd

import (
	"testing"

	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestChildRunnerInheritsWithoutIndependentEscalation(t *testing.T) {
	parent := tools.NewApprovalManager(tools.NewToolPermissions())
	parent.SetApprovalMode(tools.ModePrompt)
	got := resolvedRunnerApprovalMode(cmdRunnerOptions{
		ApprovalMode:      tools.ModeAuto,
		ApprovalModeSet:   true,
		ParentApprovalMgr: parent,
	}, runpkg.Request{Yolo: true, Auto: true})
	if got != tools.ModePrompt {
		t.Fatalf("child local mode = %v, want prompt so parent remains authoritative", got)
	}
}
