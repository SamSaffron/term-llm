package tools

import "testing"

func TestAgentApprovalScopesIsolateConcurrentParents(t *testing.T) {
	parent := NewApprovalManager(&ToolPermissions{})
	parent.BindWorkspaceSessionID("parent-one")
	parent.cache.Set(ReadFileToolName, "/parent-one/file", ProceedAlways)
	first := parent.CloneForAgentRun("parent-one")
	parent.workspaceMu.Lock()
	parent.workspaceSessionID = "parent-two"
	parent.workspaceMu.Unlock()
	parent.cache.Set(ReadFileToolName, "/parent-two/file", ProceedAlways)
	second := parent.CloneForAgentRun("parent-two")
	if first.workspaceSessionID != "parent-one" || second.workspaceSessionID != "parent-two" {
		t.Fatalf("child approvals changed scope: %q, %q", first.workspaceSessionID, second.workspaceSessionID)
	}
	if _, ok := first.cache.Get(ReadFileToolName, "/parent-two/file"); ok {
		t.Fatal("detached child observed later parent's grant")
	}
	if _, ok := second.cache.Get(ReadFileToolName, "/parent-one/file"); ok {
		t.Fatal("second parent inherited first parent's grant")
	}
}
