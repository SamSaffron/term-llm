package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitReadFileGrant(t *testing.T) {
	dir := t.TempDir()
	uploaded := filepath.Join(dir, "upload.bin")
	sibling := filepath.Join(dir, "other.bin")
	for _, path := range []string{uploaded, sibling} {
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	parent := NewApprovalManager(NewToolPermissions())
	if err := parent.AddReadFile(uploaded); err != nil {
		t.Fatal(err)
	}
	if err := parent.AddReadFile(dir); err == nil {
		t.Fatal("granted a directory")
	}
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(parent); err != nil {
		t.Fatal(err)
	}
	for _, mgr := range []*ApprovalManager{parent, child, NewApprovalManager(NewToolPermissions())} {
		for _, tc := range []struct {
			path        string
			write, want bool
		}{
			{uploaded, false, mgr == parent || mgr == child},
			{uploaded, true, false}, {sibling, false, false}, {dir, false, false},
		} {
			_, allowed, err := mgr.checkPathApprovalNoPrompt("read_file", tc.path, tc.path, tc.write)
			if err != nil || allowed != tc.want {
				t.Fatalf("path=%s write=%v: allowed=%v err=%v want=%v", tc.path, tc.write, allowed, err, tc.want)
			}
		}
	}
	// Replacing an upload with a symlink must not authorize its new target.
	if err := os.Remove(uploaded); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, uploaded); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if parent.isExplicitReadFile(uploaded) {
		t.Fatal("grant followed a replaced symlink")
	}
}
