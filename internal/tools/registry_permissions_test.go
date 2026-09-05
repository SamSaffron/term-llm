package tools

import (
	"path/filepath"
	"testing"
)

func TestNewToolManagerSharesBuiltPermissions(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewToolManager(&ToolConfig{
		Enabled:  []string{ReadFileToolName},
		ReadDirs: []string{dir},
	}, nil)
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}
	defer mgr.ApprovalMgr.Close()

	if mgr.Registry.permissions != mgr.ApprovalMgr.permissions {
		t.Fatal("registry and approval manager do not share permissions")
	}
	readDirs, _, _ := mgr.Registry.permissions.Snapshot()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if len(readDirs) != 1 || readDirs[0] != filepath.Clean(resolvedDir) {
		t.Fatalf("read dirs = %v, want [%s]", readDirs, filepath.Clean(resolvedDir))
	}
}

func TestNewLocalToolRegistryBuildsPermissionsWithoutManager(t *testing.T) {
	dir := t.TempDir()
	registry, err := NewLocalToolRegistry(&ToolConfig{
		Enabled:  []string{ReadFileToolName},
		ReadDirs: []string{dir},
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewLocalToolRegistry: %v", err)
	}
	defer registry.approval.Close()

	if registry.permissions != registry.approval.permissions {
		t.Fatal("registry and generated approval manager do not share permissions")
	}
	allowed, err := registry.permissions.IsPathAllowedForRead(dir)
	if err != nil {
		t.Fatalf("IsPathAllowedForRead: %v", err)
	}
	if !allowed {
		t.Fatalf("generated permissions do not allow configured read dir %s", dir)
	}
}

func TestNewLocalToolRegistryReusesManagerPermissions(t *testing.T) {
	managerDir := t.TempDir()
	configDir := t.TempDir()
	perms := NewToolPermissions()
	if err := perms.AddReadDir(managerDir); err != nil {
		t.Fatalf("AddReadDir: %v", err)
	}
	approvalMgr := NewApprovalManager(perms)
	defer approvalMgr.Close()

	registry, err := NewLocalToolRegistry(&ToolConfig{
		Enabled:    []string{ReadFileToolName},
		ReadDirs:   []string{configDir},
		ShellAllow: []string{"echo ["}, // Rebuilding these permissions would fail.
	}, nil, approvalMgr)
	if err != nil {
		t.Fatalf("NewLocalToolRegistry: %v", err)
	}
	if registry.permissions != perms {
		t.Fatal("registry did not reuse the approval manager permissions")
	}
	allowed, err := registry.permissions.IsPathAllowedForRead(configDir)
	if err != nil {
		t.Fatalf("IsPathAllowedForRead: %v", err)
	}
	if allowed {
		t.Fatalf("registry rebuilt permissions from config instead of reusing manager permissions")
	}
}
