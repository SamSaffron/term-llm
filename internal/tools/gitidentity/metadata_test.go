package gitidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsAdministrativePath(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // Classification must not invoke Git.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-the-repository"))
	root := t.TempDir()
	for _, dir := range []string{filepath.Join(root, ".git"), filepath.Join(root, "arbitrary-store")} {
		for _, component := range []string{"objects", "refs"} {
			if err := os.MkdirAll(filepath.Join(dir, component), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitfile := filepath.Join(root, "linked", ".git")
	if err := os.MkdirAll(filepath.Dir(gitfile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitfile, []byte("gitdir: ../.git/worktrees/linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".git", true}, {filepath.Join(".git", "config"), true},
		{filepath.Join("new", ".git"), true}, {filepath.Join("new", ".git", "hooks", "pre-commit"), true},
		{filepath.Join(".GIT", "config"), true}, {filepath.Join("linked", ".git"), true},
		{"arbitrary-store", true}, {filepath.Join("arbitrary-store", "config"), true},
		{filepath.Join("arbitrary-store", "hooks", "missing", "hook"), true},
		{"source.go", false}, {filepath.Join("new", "source.go"), false},
		{"HEAD", true}, {"head", true}, {filepath.Join("new", "HEAD"), true},
		{"header.go", false}, {"head.txt", false}, {"HEADERS", false},
		{filepath.Join(".github", "config"), false}, {"file.git", false}, {"config", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := IsAdministrativePath(filepath.Join(root, tc.path)); got != tc.want {
				t.Errorf("IsAdministrativePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestIsAdministrativePathPhysicalAliases(t *testing.T) {
	root := t.TempDir()
	gitdir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitdir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(gitdir, "config")
	ordinary := filepath.Join(root, "source.txt")
	head := filepath.Join(root, "HEAD")
	for _, path := range []string{config, ordinary, head} {
		if err := os.WriteFile(path, []byte("# fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, target, suffix string
		want                 bool
	}{
		{"config_alias", config, "", true},
		{"directory_alias", gitdir, filepath.Join("missing", "hook"), true},
		{"gitfile_alias", filepath.Join(root, "linked", ".git"), "", true},
		{"ordinary_alias", ordinary, "", false},
		{"head_alias", head, "", true},
		{"dangling_head", filepath.Join("new", "HEAD"), "", true},
		{"dangling_sibling", "new-source.txt", "", false},
		{"dangling_metadata", filepath.Join(".git", "new-config"), "", true},
		{"symlink_loop", "symlink_loop", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alias := filepath.Join(root, tc.name)
			if err := os.Symlink(tc.target, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			path := alias
			if tc.suffix != "" {
				path += string(filepath.Separator) + tc.suffix
			}
			if got := IsAdministrativePath(path); got != tc.want {
				t.Errorf("IsAdministrativePath(%q) = %v, want %v", path, got, tc.want)
			}
		})
	}
}

func TestIsAdministrativePathRelativeDanglingSibling(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Symlink("new-source.txt", "alias"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if IsAdministrativePath("alias") {
		t.Fatal("ordinary relative dangling sibling must remain writable")
	}
}
