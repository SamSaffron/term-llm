package tools

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestToolManagerIsReadPathApprovedDoesNotPromptOrMutatePermissions(t *testing.T) {
	root := t.TempDir()
	allowedPath := filepath.Join(root, "allowed.txt")
	if err := os.WriteFile(allowedPath, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}

	permissions := NewToolPermissions()
	if err := permissions.AddReadDir(root); err != nil {
		t.Fatal(err)
	}
	beforeRead, beforeWrite, beforeShell := permissions.Snapshot()
	approval := NewApprovalManager(permissions)
	prompted := false
	approval.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{}, nil
	}
	manager := &ToolManager{ApprovalMgr: approval}

	if !manager.IsReadPathApproved(allowedPath) {
		t.Fatal("pre-approved path was denied")
	}
	if manager.IsReadPathApproved(outside) {
		t.Fatal("unapproved path was allowed")
	}
	afterRead, afterWrite, afterShell := permissions.Snapshot()
	if prompted || !reflect.DeepEqual(beforeRead, afterRead) || !reflect.DeepEqual(beforeWrite, afterWrite) || !reflect.DeepEqual(beforeShell, afterShell) {
		t.Fatalf("non-interactive check prompted=%v or mutated permissions: before=%v/%v/%v after=%v/%v/%v", prompted, beforeRead, beforeWrite, beforeShell, afterRead, afterWrite, afterShell)
	}
}
