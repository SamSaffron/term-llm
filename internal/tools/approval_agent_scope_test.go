package tools

import (
	"context"
	"testing"
)

func TestAgentApprovalScopeBindsFirstToolAndSharesPromptLock(t *testing.T) {
	parent := NewApprovalManager(&ToolPermissions{})
	parent.cache.Set(ReadFileToolName, "/first/file", ProceedAlways)
	first := parent.CloneForAgentRun("parent-one")
	if first.workspaceSessionID != "parent-one" || parent.workspaceSessionID != "parent-one" {
		t.Fatalf("first-tool scope was not bound: %q / %q", first.workspaceSessionID, parent.workspaceSessionID)
	}
	if _, ok := first.cache.Get(ReadFileToolName, "/first/file"); !ok {
		t.Fatal("first child lost parent's approval")
	}
	if first.PromptLock() != parent.PromptLock() {
		t.Fatal("child prompts do not serialize with parent")
	}
	if err := parent.ConfigureWorkspacePersistence(context.Background(), nil, "parent-two"); err != nil {
		t.Fatal(err)
	}
	parent.cache.Set(ReadFileToolName, "/second/file", ProceedAlways)
	second := parent.CloneForAgentRun("parent-two")
	if _, ok := second.cache.Get(ReadFileToolName, "/second/file"); !ok {
		t.Fatal("second parent's child lost its approvals")
	}
	if _, ok := second.cache.Get(ReadFileToolName, "/first/file"); ok {
		t.Fatal("second parent's child inherited first parent's approval")
	}
	if first.PromptLock() != second.PromptLock() {
		t.Fatal("concurrent child prompts are not serialized")
	}
}

func TestAgentApprovalScopesIsolateConcurrentParents(t *testing.T) {
	parent := NewApprovalManager(&ToolPermissions{})
	parent.BindWorkspaceSessionID("parent-one")
	parent.cache.Set(ReadFileToolName, "/parent-one/file", ProceedAlways)
	first := parent.CloneForAgentRun("parent-one")
	if err := parent.ConfigureWorkspacePersistence(context.Background(), nil, "parent-two"); err != nil {
		t.Fatal(err)
	}
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
