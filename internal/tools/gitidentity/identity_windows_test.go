package gitidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootRejectsWindowsPartialAbsolutePaths(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	for _, name := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join("foo", ".git", name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join("foo", ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Root("foo"); got == "" {
		t.Fatal("ordinary relative checkout must remain discoverable")
	}
	for _, path := range []string{`\foo`, `/foo`, filepath.VolumeName(base) + "foo", filepath.VolumeName(base) + "."} {
		if got := Root(path); got != "" {
			t.Errorf("Root(%q) = %q; partial absolute paths must fail closed", path, got)
		}
	}
}

func TestContainsGitComponentWindowsAliases(t *testing.T) {
	for _, component := range []string{".git", ".GIT", ".git.", ".git ", ".git. "} {
		for _, path := range []string{filepath.Join("checkout", component), filepath.Join("checkout", component, "config")} {
			if !containsGitComponent(path) {
				t.Errorf("containsGitComponent(%q) = false; Win32 metadata aliases must be excluded", path)
			}
		}
	}
}
