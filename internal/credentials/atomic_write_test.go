//go:build !windows

package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicFailureLeavesExistingFileUntouched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission failure is not reliable as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	original := []byte(`{"access_token":"old"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write original: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	if err := writeFileAtomic(path, []byte(`{"access_token":"new"}`), 0o600); err == nil {
		t.Fatal("writeFileAtomic succeeded unexpectedly")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original after failed write: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("file contents changed after failed write: %q", got)
	}
}
