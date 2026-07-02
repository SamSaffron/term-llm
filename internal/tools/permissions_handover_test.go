package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHandoverPathsAlwaysAllowed ensures agents can maintain their handover
// plan file — which lives in term-llm's own data directory — without explicit
// read/write dir grants or approval prompts.
func TestHandoverPathsAlwaysAllowed(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	handoverDir := filepath.Join(xdg, "term-llm", "handover", "proj-abc123")
	if err := os.MkdirAll(handoverDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	plan := filepath.Join(handoverDir, "2026-07-02-auth-refactor.md")

	perms := NewToolPermissions() // no read/write dirs granted

	allowed, err := perms.IsPathAllowedForWrite(plan)
	if err != nil {
		t.Fatalf("IsPathAllowedForWrite: %v", err)
	}
	if !allowed {
		t.Fatal("handover plan file should be writable without grants")
	}

	if err := os.WriteFile(plan, []byte("plan"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	allowed, err = perms.IsPathAllowedForRead(plan)
	if err != nil {
		t.Fatalf("IsPathAllowedForRead: %v", err)
	}
	if !allowed {
		t.Fatal("handover plan file should be readable without grants")
	}

	// Other files in the data dir stay protected.
	other := filepath.Join(xdg, "term-llm", "sessions.db")
	allowed, err = perms.IsPathAllowedForWrite(other)
	if err != nil {
		t.Fatalf("IsPathAllowedForWrite other: %v", err)
	}
	if allowed {
		t.Fatal("non-handover data files must not be writable without grants")
	}
}
