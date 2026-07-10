package session

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGetHandoverPathUniquePerCall(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	first, err := GetHandoverPath(".", "2026-07-02")
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	base := filepath.Base(first)
	if !strings.HasPrefix(base, "2026-07-02-") {
		t.Fatalf("expected date prefix on %q", base)
	}
	if !IsRandomHandoverName(base) {
		t.Fatalf("expected random handover name, got %q", base)
	}

	second, err := GetHandoverPath(".", "2026-07-02")
	if err != nil {
		t.Fatalf("GetHandoverPath: %v", err)
	}
	if second == first {
		t.Fatal("expected distinct paths per call so concurrent sessions get their own files")
	}
}

func TestExtractHandoverPath(t *testing.T) {
	dir := filepath.Join("/data", "handover", "proj-abc123")
	path := filepath.Join(dir, "2026-07-02-amber-creek-bloom.md")
	prompt := "Your plan lives at exactly this path:\n\n`" + path + "`\n\nWrite to it incrementally."

	if got := ExtractHandoverPath(prompt, dir); got != path {
		t.Fatalf("got %q, want %q", got, path)
	}

	// Descriptive (renamed) slugs match too.
	renamed := filepath.Join(dir, "2026-07-02-fix-auth-migration.md")
	if got := ExtractHandoverPath("plan at "+renamed+" please", dir); got != renamed {
		t.Fatalf("got %q, want %q", got, renamed)
	}

	// A different project dir must not match.
	if got := ExtractHandoverPath(prompt, filepath.Join("/data", "handover", "other-def456")); got != "" {
		t.Fatalf("expected no match for other dir, got %q", got)
	}

	if got := ExtractHandoverPath("", dir); got != "" {
		t.Fatalf("expected no match for empty prompt, got %q", got)
	}
	if got := ExtractHandoverPath(prompt, ""); got != "" {
		t.Fatalf("expected no match for empty dir, got %q", got)
	}
}

func TestResolvePinnedHandoverPathAnchoredAssignmentIgnoresForeignReference(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	rootDir, err := GetHandoverDir(t.TempDir())
	if err != nil {
		t.Fatalf("GetHandoverDir root: %v", err)
	}
	worktreeDir, err := GetHandoverDir(t.TempDir())
	if err != nil {
		t.Fatalf("GetHandoverDir worktree: %v", err)
	}
	foreign := filepath.Join(rootDir, "2026-07-02-foreign-plan-reference.md")
	assigned := filepath.Join(worktreeDir, "2026-07-02-amber-creek-bloom.md")
	prompt := "Earlier context mentioned " + foreign + ".\n\n" +
		"Your plan lives at exactly this path, decided upfront and fixed for this session:\n\n`" + assigned + "`"

	got, pinned := ResolvePinnedHandoverPath(prompt, rootDir)
	if !pinned || got != assigned {
		t.Fatalf("ResolvePinnedHandoverPath = (%q, %v), want (%q, true)", got, pinned, assigned)
	}
}

func TestResolvePinnedHandoverPathAmbiguousAssignmentPreventsFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	firstDir, _ := GetHandoverDir(t.TempDir())
	secondDir, _ := GetHandoverDir(t.TempDir())
	first := filepath.Join(firstDir, "2026-07-02-amber-creek-bloom.md")
	second := filepath.Join(secondDir, "2026-07-02-coral-delta-ember.md")
	prompt := "Your plan lives at exactly this path:\n`" + first + "`\n" +
		"Your plan lives at exactly this path:\n`" + second + "`"

	got, pinned := ResolvePinnedHandoverPath(prompt)
	if !pinned || got != "" {
		t.Fatalf("ResolvePinnedHandoverPath = (%q, %v), want ambiguous pinned result", got, pinned)
	}
}
