package session

import (
	"context"
	"testing"
)

// TestWorktreeDirRoundTrip verifies the worktree_dir column persists through
// Create/Get/Update, including clearing it (rebinding to the root checkout).
func TestWorktreeDirRoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	sess := &Session{
		ID:          "wt-test-1",
		Provider:    "test",
		Model:       "test-model",
		WorktreeDir: "/data/worktrees/abcd/neon-canyon",
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorktreeDir != sess.WorktreeDir {
		t.Fatalf("WorktreeDir = %q, want %q", got.WorktreeDir, sess.WorktreeDir)
	}

	// Switch the binding to another worktree.
	got.WorktreeDir = "/data/worktrees/abcd/quiet-comet"
	if err := store.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if got2.WorktreeDir != "/data/worktrees/abcd/quiet-comet" {
		t.Fatalf("after update WorktreeDir = %q", got2.WorktreeDir)
	}

	// Clear the binding (rebind to root checkout).
	got2.WorktreeDir = ""
	if err := store.Update(ctx, got2); err != nil {
		t.Fatalf("update clear: %v", err)
	}
	got3, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get3: %v", err)
	}
	if got3.WorktreeDir != "" {
		t.Fatalf("after clear WorktreeDir = %q, want empty", got3.WorktreeDir)
	}
}
