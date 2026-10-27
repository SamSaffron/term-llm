package pairing

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrivateDirRejectsInsecureLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions and symlinks")
	}
	base := t.TempDir()

	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGrantStore(filepath.Join(open, "grants.json")); err == nil || !strings.Contains(err.Error(), "too open") {
		t.Fatalf("0755 state dir accepted: %v", err)
	}
	if _, err := LoadOrCreateIdentity(filepath.Join(open, "host.key")); err == nil {
		t.Fatal("identity created in 0755 dir")
	}
	if _, err := os.Stat(filepath.Join(open, "host.key")); !os.IsNotExist(err) {
		t.Fatalf("key written into insecure dir: %v", err)
	}

	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGrantStore(filepath.Join(link, "grants.json")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked state dir accepted: %v", err)
	}

	// Shared ancestors (like t.TempDir's parents or /tmp) are not inspected;
	// a private leaf under a 0755 parent is fine.
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGrantStore(filepath.Join(shared, "leaf", "grants.json")); err != nil {
		t.Fatalf("private leaf under shared parent rejected: %v", err)
	}
}

func TestReadPrivateFileRejectsSymlinkAndSwap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions and symlinks")
	}
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "host.key")
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "linked.key")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked key accepted: %v", err)
	}

	// Replace the inspected file with a different private file between
	// Lstat and Open: the descriptor no longer matches.
	other := filepath.Join(dir, "other.key")
	if _, err := LoadOrCreateIdentity(other); err != nil {
		t.Fatal(err)
	}
	testHookAfterLstat = func(p string) {
		if p == path {
			if err := os.Rename(other, path); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { testHookAfterLstat = nil }()
	if _, err := readPrivateFile(path); err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("swapped file accepted: %v", err)
	}

	// Swap to a symlink: O_NOFOLLOW refuses to open it.
	if err := os.Symlink(filepath.Join(dir, "absent"), other); err != nil {
		t.Fatal(err)
	}
	testHookAfterLstat = func(p string) {
		if p == path {
			if err := os.Rename(other, path); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := readPrivateFile(path); err == nil {
		t.Fatal("file swapped for symlink accepted")
	}
}
