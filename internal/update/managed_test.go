package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManagedMarker(t *testing.T, executable, content string) {
	t.Helper()
	if err := os.WriteFile(executable+ManagedMarkerSuffix, []byte(content), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestManagedUpgradeRefusalBlocksMarkerOwner(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "term-llm")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManagedMarker(t, executable,
		`{"schema_version":1,"owner":"term-llm macOS companion","update_hint":"Update it from the app."}`)

	err := managedUpgradeRefusal(executable)
	if err == nil {
		t.Fatal("expected a managed install to refuse self-upgrade")
	}
	message := err.Error()
	if !strings.Contains(message, "term-llm macOS companion") {
		t.Fatalf("error does not name the owner: %q", message)
	}
	if !strings.Contains(message, "Update it from the app.") {
		t.Fatalf("error does not carry the update hint: %q", message)
	}
}

func TestManagedUpgradeRefusalResolvesSymlink(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "term-llm")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManagedMarker(t, executable, `{"schema_version":1,"owner":"app installer"}`)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	if err := managedUpgradeRefusal(link); err == nil {
		t.Fatal("a symlink to a managed binary must still refuse self-upgrade")
	}
}

func TestManagedUpgradeRefusalAllowsUnmanagedBinary(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "term-llm")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := managedUpgradeRefusal(executable); err != nil {
		t.Fatalf("unmanaged binary should be upgradable: %v", err)
	}
}

func TestManagedUpgradeRefusalStillBlocksUnreadableMarker(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "term-llm")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManagedMarker(t, executable, "not json")
	err := managedUpgradeRefusal(executable)
	if err == nil {
		t.Fatal("a corrupt marker must not silently enable self-upgrade")
	}
	if !strings.Contains(err.Error(), "external installer") {
		t.Fatalf("corrupt marker should still name an owner generically: %q", err.Error())
	}
}
