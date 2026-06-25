package tools

import (
	"strings"
	"testing"
)

func TestCheckPathApprovalNoPromptReturnsActionableNonInteractiveError(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())

	_, err := mgr.CheckPathApproval("read_file", t.TempDir(), "", false)
	if err == nil {
		t.Fatal("expected error")
	}
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("error type = %T, want *ToolError", err)
	}
	if toolErr.Type != ErrPermissionDenied {
		t.Fatalf("type = %s, want %s", toolErr.Type, ErrPermissionDenied)
	}
	for _, want := range []string{"requires approval", "--yolo", "--read-dir", "--write-dir"} {
		if !strings.Contains(toolErr.Message, want) {
			t.Fatalf("message %q missing %q", toolErr.Message, want)
		}
	}
	if strings.Contains(toolErr.Message, "/dev/tty") {
		t.Fatalf("message should not expose /dev/tty failure: %q", toolErr.Message)
	}
}

func TestCheckShellApprovalNoPromptReturnsActionableNonInteractiveError(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())

	_, err := mgr.CheckShellApproval("echo hi", t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("error type = %T, want *ToolError", err)
	}
	if toolErr.Type != ErrPermissionDenied {
		t.Fatalf("type = %s, want %s", toolErr.Type, ErrPermissionDenied)
	}
	for _, want := range []string{"requires approval", "--yolo", "--shell-allow"} {
		if !strings.Contains(toolErr.Message, want) {
			t.Fatalf("message %q missing %q", toolErr.Message, want)
		}
	}
	if strings.Contains(toolErr.Message, "/dev/tty") {
		t.Fatalf("message should not expose /dev/tty failure: %q", toolErr.Message)
	}
}
