package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExtensionDirectoryIsNarrowAndNeedsNoWorkspacePrompt(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(parent, "config"))
	main := filepath.Join(parent, "config", "term-llm", "config.yaml")
	dir := filepath.Join(filepath.Dir(main), "extensions")
	m := NewApprovalManager(NewToolPermissions())
	if err := m.SetPrimaryWorkspace(parent); err != nil {
		t.Fatal(err)
	}
	r := &LocalToolRegistry{approval: m}
	if err := r.SetExtensionDirectory(dir, main); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extensions.yaml"), []byte("enabled: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool, path string
		write      bool
	}{{WriteFileToolName, filepath.Join(dir, "spartan", "style.css"), true}, {ReadFileToolName, filepath.Join(dir, "extensions.yaml"), false}, {GlobToolName, dir, false}} {
		outcome, err := m.CheckPathApprovalWithContext(context.Background(), tc.tool, tc.path, tc.path, tc.write)
		if err != nil || outcome == Cancel {
			t.Fatalf("extension access blocked: %s: %v", tc.path, err)
		}
	}
	for _, p := range []string{main, filepath.Join(parent, "private.txt"), filepath.Join(filepath.Dir(dir), "extensions-other", "x")} {
		if outcome, err := m.CheckPathApprovalWithContext(context.Background(), WriteFileToolName, p, p, true); err == nil && outcome != Cancel {
			t.Fatalf("outside path allowed: %s", p)
		}
	}
	if m.extensionFileAllowed(ShellToolName, dir) {
		t.Fatal("extension capability authorized shell")
	}
	for _, cap := range m.WorkspaceCapabilities() {
		if cap.Primary && cap.Status != "proposed" {
			t.Fatal("confirmed broad primary workspace")
		}
	}
	if err := r.SetExtensionDirectory(filepath.Dir(main), main); err == nil {
		t.Fatal("granted main config parent")
	}
	if m.extensionFileAllowed(WriteFileToolName, filepath.Join(dir, "style.css")) {
		t.Fatal("old grant survived failed rebind")
	}
}
func TestExtensionDirectoryRejectsSymlinkEscapeAndDoesNotInherit(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(parent, "config"))
	dir := filepath.Join(parent, "extensions")
	main := filepath.Join(parent, "config.yaml")
	if err := os.WriteFile(main, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewApprovalManager(NewToolPermissions())
	if err := m.SetPrimaryWorkspace(parent); err != nil {
		t.Fatal(err)
	}
	r := &LocalToolRegistry{approval: m}
	if err := r.SetExtensionDirectory(dir, main); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape.yaml")
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}
	if outcome, err := m.CheckPathApprovalWithContext(context.Background(), WriteFileToolName, link, link, true); err == nil && outcome != Cancel {
		t.Fatal("symlink escaped directory")
	}
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(m); err != nil {
		t.Fatal(err)
	}
	if child.extensionFileAllowed(WriteFileToolName, filepath.Join(dir, "style.css")) {
		t.Fatal("capability leaked into a child agent")
	}
	next := filepath.Join(parent, "other-extensions")
	if err := r.SetExtensionDirectory(next, main); err != nil {
		t.Fatal(err)
	}
	if m.extensionFileAllowed(ReadFileToolName, dir) {
		t.Fatal("old directory remains allowed")
	}
	if !m.extensionFileAllowed(ReadFileToolName, next) {
		t.Fatal("new directory not allowed")
	}
}
