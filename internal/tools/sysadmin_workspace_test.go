package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestRootReadGrant(t *testing.T) {
	p := NewToolPermissions()
	if err := p.AddReadDir(string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(string(filepath.Separator), "etc", "hosts")
	if !p.isPathInDirs(target, p.ReadDirs) {
		t.Fatalf("root did not contain %s", target)
	}
	base := t.TempDir()
	sibling := base + "-other"
	if err := p.AddReadDir(base); err != nil {
		t.Fatal(err)
	}
	if p.isPathInDirs(sibling, []string{base}) {
		t.Fatal("sibling escaped normal root")
	}
}
func TestWorkspaceNoneRebind(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{ReadFileToolName}
	cfg.BaseDir = first
	cfg.ShellWorkingDir = first
	cfg.Workspace = "none"
	r, err := NewLocalToolRegistry(&cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetBaseDirWithContext(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if got := r.BaseDir(); got != second {
		t.Fatalf("base=%q", got)
	}
	if r.approval.root().primaryWorkspace != "" {
		t.Fatal("workspace proposal created")
	}
}
func TestWorkspaceNoneChild(t *testing.T) {
	root := NewApprovalManager(NewToolPermissions())
	workspace := t.TempDir()
	if err := root.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	child := NewApprovalManager(NewToolPermissions())
	child.WorkspacePolicy = "none"
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	called := false
	root.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) { called = true; return WorkspaceApprovalResult{}, nil }
	if err := child.EnsurePrimaryWorkspaceAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("inherited workspace prompted")
	}
	if root.primaryWorkspace != workspace {
		t.Fatal("parent proposal mutated")
	}
}
func TestWorkspaceNoneToolSpecs(t *testing.T) {
	m := NewApprovalManager(NewToolPermissions())
	m.WorkspacePolicy = "none"
	specs := []llm.ToolSpec{{Name: ManageWorkspaceToolName}, {Name: ReadFileToolName}}
	got := FilterToolSpecsForApprovalMode(specs, m)
	if len(got) != 1 || got[0].Name != ReadFileToolName {
		t.Fatalf("specs=%v", got)
	}
}
