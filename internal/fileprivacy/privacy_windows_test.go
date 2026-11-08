//go:build windows

package fileprivacy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsPrivacyPolicyDocumentsProfileACLAndWrites(t *testing.T) {
	if err := CheckFile(fakeWindowsInfo{perm: 0o666}); err != nil {
		t.Fatalf("CheckFile(0666): %v", err)
	}
	if err := CheckDir(fakeWindowsInfo{perm: 0o777}); err != nil {
		t.Fatalf("CheckDir(0777): %v", err)
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := WriteFileAtomic(path, []byte("secret")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "secret" {
		t.Fatalf("content=%q err=%v", got, err)
	}
}

type fakeWindowsInfo struct{ perm os.FileMode }

func (f fakeWindowsInfo) Name() string       { return "fake" }
func (f fakeWindowsInfo) Size() int64        { return 0 }
func (f fakeWindowsInfo) Mode() os.FileMode  { return f.perm }
func (f fakeWindowsInfo) ModTime() time.Time { return time.Time{} }
func (f fakeWindowsInfo) IsDir() bool        { return f.perm.IsDir() }
func (f fakeWindowsInfo) Sys() any           { return nil }
