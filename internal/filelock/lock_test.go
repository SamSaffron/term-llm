package filelock

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLockReleaseAndReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat lock file: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("lock file permissions = %o, want no group/other access", info.Mode().Perm())
	}
	if err := unlock(); err != nil {
		t.Fatalf("first unlock: %v", err)
	}
	unlockAgain, err := Lock(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if err := unlockAgain(); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
	if err := unlock(); err == nil {
		t.Fatal("double unlock unexpectedly succeeded")
	}
}
