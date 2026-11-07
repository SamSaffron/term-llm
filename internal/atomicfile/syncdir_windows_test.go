//go:build windows

package atomicfile

import "testing"

func TestSyncDirIsNoOpOnWindows(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
}
