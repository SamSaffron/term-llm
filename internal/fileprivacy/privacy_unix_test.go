//go:build !windows

package fileprivacy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateModesAndChecks(t *testing.T) {
	for _, mode := range []os.FileMode{0o400, 0o600} {
		info := fakeInfo{mode: mode}
		if err := CheckFile(info); err != nil {
			t.Fatalf("CheckFile(%04o): %v", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := CheckFile(fakeInfo{mode: mode}); err == nil {
			t.Fatalf("CheckFile(%04o) unexpectedly passed", mode)
		}
	}
	if err := CheckDir(fakeInfo{mode: 0o700}); err != nil {
		t.Fatalf("CheckDir(0700): %v", err)
	}
	if err := CheckDir(fakeInfo{mode: 0o755}); err == nil {
		t.Fatal("CheckDir(0755) unexpectedly passed")
	}
}

func TestWriteFileAtomicUsesPrivateModeAndDurabilityHelper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")
	if err := WriteFileAtomic(path, []byte("secret")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %04o, want 0600", perm)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "secret" {
		t.Fatalf("content = %q, err=%v", got, err)
	}
}

type fakeInfo struct{ mode os.FileMode }

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

func TestWriteFileAtomicFailureLeavesExistingFileUntouched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permission failure is not reliable as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if err := WriteFileAtomic(path, []byte("new")); err == nil {
		t.Fatal("WriteFileAtomic succeeded in a read-only directory")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "old" {
		t.Fatalf("content changed after failed write: %q, err=%v", got, err)
	}
}
