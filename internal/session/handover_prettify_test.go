package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func staticSlugGen(slug string) HandoverSlugGenerator {
	return func(context.Context, string) (string, error) { return slug, nil }
}

// requireSymlinks skips the test when the platform (or privilege level, e.g.
// Windows without Developer Mode) does not support creating symlinks.
func requireSymlinks(t *testing.T) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Symlink("target", probe); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

func TestPrettifyHandoverNameBeforeFileExists(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-07-02-amber-anchor-apple.md")

	if err := PrettifyHandoverName(context.Background(), path, "fix the auth system", staticSlugGen("auth refactor")); err != nil {
		t.Fatalf("PrettifyHandoverName: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected symlink at %s (err %v)", path, err)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "2026-07-02-auth-refactor.md" {
		t.Fatalf("symlink target = %q", target)
	}

	// Writing through the original path lands in the descriptive file.
	if err := os.WriteFile(path, []byte("the plan"), 0o644); err != nil {
		t.Fatalf("write through symlink: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-07-02-auth-refactor.md"))
	if err != nil || string(data) != "the plan" {
		t.Fatalf("pretty file content = %q, err %v", data, err)
	}

	// Second call is a no-op (already a symlink).
	if err := PrettifyHandoverName(context.Background(), path, "fix the auth system", staticSlugGen("other words")); err != nil {
		t.Fatalf("second PrettifyHandoverName: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-07-02-other-words.md")); err == nil {
		t.Fatal("second call should not create another file")
	}
}

func TestPrettifyHandoverNameExistingFileCarriesContent(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-07-02-amber-anchor-apple.md")
	if err := os.WriteFile(path, []byte("already written"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := PrettifyHandoverName(context.Background(), path, "db migration work", staticSlugGen("db migration")); err != nil {
		t.Fatalf("PrettifyHandoverName: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-07-02-db-migration.md"))
	if err != nil || string(data) != "already written" {
		t.Fatalf("renamed file content = %q, err %v", data, err)
	}
	// Original path still readable through the symlink.
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "already written" {
		t.Fatalf("content via original path = %q, err %v", data, err)
	}
}

func TestPrettifyHandoverNameCollisionKeepsRandomWord(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	occupied := filepath.Join(dir, "2026-07-02-auth-refactor.md")
	if err := os.WriteFile(occupied, []byte("another session"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path := filepath.Join(dir, "2026-07-02-amber-anchor-apple.md")
	if err := PrettifyHandoverName(context.Background(), path, "fix auth", staticSlugGen("auth refactor")); err != nil {
		t.Fatalf("PrettifyHandoverName: %v", err)
	}

	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "2026-07-02-auth-refactor-amber.md" {
		t.Fatalf("symlink target = %q", target)
	}
	// The occupied file is untouched.
	data, _ := os.ReadFile(occupied)
	if string(data) != "another session" {
		t.Fatalf("occupied file was modified: %q", data)
	}
}

func TestPrettifyHandoverNameCapsSlugAtTwoWords(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-07-02-amber-anchor-apple.md")

	if err := PrettifyHandoverName(context.Background(), path, "task", staticSlugGen("one two three four")); err != nil {
		t.Fatalf("PrettifyHandoverName: %v", err)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "2026-07-02-one-two.md" {
		t.Fatalf("symlink target = %q", target)
	}
}

func TestPrettifyHandoverNameSkipsNonRandomNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2026-07-02-my-custom-plan.md")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := PrettifyHandoverName(context.Background(), path, "task", staticSlugGen("nice name")); err != nil {
		t.Fatalf("PrettifyHandoverName: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("descriptively named file must stay a regular file (err %v)", err)
	}
}
