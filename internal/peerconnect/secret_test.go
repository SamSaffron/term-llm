package peerconnect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCapabilityFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cap")
	token := strings.Repeat("x", 43)
	if err := os.WriteFile(p, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadCapabilityFile(p); err != nil || got != token {
		t.Fatalf("read %q %v", got, err)
	}
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapabilityFile(p); err == nil {
		t.Fatal("accepted public capability file")
	}
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(p, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapabilityFile(symlink); err == nil {
		t.Fatal("accepted symlink")
	}
	for _, bad := range []string{"short", strings.Repeat("a", 129), strings.Repeat("a", 32) + " b", strings.Repeat("a", 32) + "\x00", strings.Repeat("a", 4097)} {
		if err := os.WriteFile(p, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadCapabilityFile(p); err == nil {
			t.Errorf("accepted invalid capability len%d", len(bad))
		}
	}
}
