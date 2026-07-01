package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// requireSymlinks skips the test when the platform (or privilege level, e.g.
// Windows without Developer Mode) does not support creating symlinks.
func requireSymlinks(t *testing.T) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.Symlink("target", probe); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

func TestResolveWriteTarget(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()

	regular := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := resolveWriteTarget(regular); got != regular {
		t.Fatalf("regular file resolved to %q", got)
	}

	missing := filepath.Join(dir, "missing.md")
	if got := resolveWriteTarget(missing); got != missing {
		t.Fatalf("missing path resolved to %q", got)
	}

	// Dangling same-directory symlinks resolve to their target.
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink("target.md", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	want := filepath.Join(dir, "target.md")
	if got := resolveWriteTarget(link); got != want {
		t.Fatalf("dangling link resolved to %q, want %q", got, want)
	}

	// Chains of same-directory links resolve through intermediates.
	link2 := filepath.Join(dir, "link2.md")
	if err := os.Symlink("link.md", link2); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if got := resolveWriteTarget(link2); got != want {
		t.Fatalf("chained link resolved to %q, want %q", got, want)
	}

	// Links leaving their own directory are never followed: permission
	// approval ran against the link path, so following these could redirect
	// an approved write elsewhere.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	absEscape := filepath.Join(dir, "abs-escape.md")
	if err := os.Symlink(filepath.Join(sub, "x.md"), absEscape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if got := resolveWriteTarget(absEscape); got != absEscape {
		t.Fatalf("absolute-target link followed to %q", got)
	}
	relEscape := filepath.Join(dir, "rel-escape.md")
	if err := os.Symlink(filepath.Join("sub", "x.md"), relEscape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if got := resolveWriteTarget(relEscape); got != relEscape {
		t.Fatalf("subdir-target link followed to %q", got)
	}
	upEscape := filepath.Join(sub, "up-escape.md")
	if err := os.Symlink(filepath.Join("..", "plain.md"), upEscape); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if got := resolveWriteTarget(upEscape); got != upEscape {
		t.Fatalf("parent-target link followed to %q", got)
	}
}

// TestWriteFileToolWritesThroughSymlink ensures the atomic temp+rename write
// follows same-directory symlinks (including dangling ones) instead of
// replacing the link with a regular file. Renamed handover plan files rely
// on this.
func TestWriteFileToolWritesThroughSymlink(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "2026-07-02-amber-anchor-apple.md")
	if err := os.Symlink("2026-07-02-auth-refactor.md", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tool := NewWriteFileTool(nil)
	args, err := json.Marshal(WriteFileArgs{Path: link, Content: "the plan"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The link must survive and the content must land in its target.
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced (err %v)", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "2026-07-02-auth-refactor.md"))
	if err != nil || string(data) != "the plan" {
		t.Fatalf("target content = %q, err %v", data, err)
	}
}

// TestWriteFileToolDoesNotFollowEscapingSymlink ensures a dangling symlink
// pointing outside its own directory cannot redirect an approved write:
// the link is replaced with a regular file and the outside target is never
// created.
func TestWriteFileToolDoesNotFollowEscapingSymlink(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escaped.md")
	link := filepath.Join(dir, "2026-07-02-amber-anchor-apple.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tool := NewWriteFileTool(nil)
	args, err := json.Marshal(WriteFileArgs{Path: link, Content: "content"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if _, err := os.Stat(outside); err == nil {
		t.Fatal("write escaped through symlink to outside directory")
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected link replaced by regular file (err %v)", err)
	}
	data, err := os.ReadFile(link)
	if err != nil || string(data) != "content" {
		t.Fatalf("link path content = %q, err %v", data, err)
	}
}
