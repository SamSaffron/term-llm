package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestConfigureAskNonTUIApprovalPromptSkipsHeadless(t *testing.T) {
	mgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{"read_file"}}, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}

	configureAskNonTUIApprovalPrompt(mgr, false, false)
	if mgr.ApprovalMgr.PromptUIFunc != nil {
		t.Fatal("PromptUIFunc was set for a headless non-TUI run")
	}

	_, err = mgr.ApprovalMgr.CheckPathApproval("read_file", t.TempDir(), "", false)
	if err == nil {
		t.Fatal("expected non-interactive permission error")
	}
	toolErr, ok := err.(*tools.ToolError)
	if !ok {
		t.Fatalf("error type = %T, want *tools.ToolError", err)
	}
	if toolErr.Type != tools.ErrPermissionDenied {
		t.Fatalf("type = %s, want %s", toolErr.Type, tools.ErrPermissionDenied)
	}
}

func TestConfigureAskNonTUIApprovalPromptSetsWhenTTYAvailable(t *testing.T) {
	mgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{"read_file"}}, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}

	configureAskNonTUIApprovalPrompt(mgr, false, true)
	if mgr.ApprovalMgr.PromptUIFunc == nil {
		t.Fatal("PromptUIFunc was not set for a non-TUI run with TTY")
	}
}
