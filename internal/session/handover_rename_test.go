package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIsRandomHandoverName(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     bool
	}{
		{"valid random 3 words", "2026-04-03-amber-creek-bloom.md", true},
		{"valid random different words", "2026-01-15-frost-cedar-oak.md", true},
		{"descriptive slug 3 words", "2026-04-03-fix-auth-bug.md", false},
		{"descriptive slug 2 words", "2026-04-03-auth-migration.md", false},
		{"descriptive slug 4 words", "2026-04-03-fix-auth-bug-now.md", false},
		{"mixed: 2 random + 1 not", "2026-04-03-amber-creek-fix.md", false},
		{"no date prefix", "amber-creek-bloom.md", false},
		{"wrong extension", "2026-04-03-amber-creek-bloom.txt", false},
		{"empty string", "", false},
		{"just date", "2026-04-03.md", false},
		{"uppercase words", "2026-04-03-Amber-Creek-Bloom.md", false},
		{"numbers in words", "2026-04-03-abc-def-123.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRandomHandoverName(tt.filename)
			if got != tt.want {
				t.Errorf("IsRandomHandoverName(%q) = %v, want %v", tt.filename, got, tt.want)
			}
		})
	}
}

func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple lowercase", "fix-auth-bug", "fix-auth-bug"},
		{"uppercase", "Fix Auth Bug", "fix-auth-bug"},
		{"special chars", "fix: auth & bug!", "fix-auth-bug"},
		{"multiple spaces", "fix   auth   bug", "fix-auth-bug"},
		{"leading/trailing dashes", "--fix-auth--", "fix-auth"},
		{"empty", "", ""},
		{"long slug truncated", "this-is-a-very-long-slug-that-should-be-truncated-to-fifty-characters-or-less-at-a-word-boundary", "this-is-a-very-long-slug-that-should-be-truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeSlug(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeSlug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMaybeRenameHandover(t *testing.T) {
	t.Run("skips non-random name", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-04-03-fix-auth-bug.md")
		os.WriteFile(path, make([]byte, 2000), 0644)

		err := MaybeRenameHandover(context.Background(), path, func(ctx context.Context, content string) (string, error) {
			return "should-not-be-called", nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// File should not be renamed
		if _, err := os.Stat(path); err != nil {
			t.Error("original file should still exist")
		}
	})

	t.Run("skips file below threshold", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-04-03-amber-creek-bloom.md")
		os.WriteFile(path, []byte("small"), 0644)

		called := false
		err := MaybeRenameHandover(context.Background(), path, func(ctx context.Context, content string) (string, error) {
			called = true
			return "renamed", nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Error("slugGen should not be called for small files")
		}
	})

	t.Run("renames and creates symlink", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-04-03-amber-creek-bloom.md")
		content := make([]byte, 1500)
		for i := range content {
			content[i] = 'x'
		}
		os.WriteFile(path, content, 0644)

		err := MaybeRenameHandover(context.Background(), path, func(ctx context.Context, c string) (string, error) {
			return "fix-auth-migration", nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// New file should exist
		newPath := filepath.Join(dir, "2026-04-03-fix-auth-migration.md")
		if _, err := os.Stat(newPath); err != nil {
			t.Error("renamed file should exist")
		}

		// Old path should be a symlink
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("Lstat failed: %v", err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Error("old path should be a symlink")
		}

		// Symlink should resolve to the new file
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("Readlink failed: %v", err)
		}
		if target != "2026-04-03-fix-auth-migration.md" {
			t.Errorf("symlink target = %q, want %q", target, "2026-04-03-fix-auth-migration.md")
		}
	})

	t.Run("skips already-symlinked path", func(t *testing.T) {
		dir := t.TempDir()
		realPath := filepath.Join(dir, "2026-04-03-fix-auth-migration.md")
		os.WriteFile(realPath, make([]byte, 2000), 0644)

		symlinkPath := filepath.Join(dir, "2026-04-03-amber-creek-bloom.md")
		os.Symlink("2026-04-03-fix-auth-migration.md", symlinkPath)

		called := false
		err := MaybeRenameHandover(context.Background(), symlinkPath, func(ctx context.Context, content string) (string, error) {
			called = true
			return "new-name", nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if called {
			t.Error("slugGen should not be called for symlinked paths")
		}
	})

	t.Run("skips nil slugGen", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "2026-04-03-amber-creek-bloom.md")
		os.WriteFile(path, make([]byte, 2000), 0644)

		err := MaybeRenameHandover(context.Background(), path, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
