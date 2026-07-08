package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStorePersistsWorktreeDir(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	worktreeDir := filepath.Join(t.TempDir(), "repo-wt")
	sess := &Session{
		ID:          "sess_worktree",
		Provider:    "mock",
		Model:       "tiny",
		Mode:        ModeChat,
		CWD:         worktreeDir,
		WorktreeDir: worktreeDir,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Status:      StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.WorktreeDir != worktreeDir {
		t.Fatalf("Get WorktreeDir = %q, want %q", got.WorktreeDir, worktreeDir)
	}

	updatedDir := filepath.Join(t.TempDir(), "repo-wt-2")
	got.WorktreeDir = updatedDir
	got.CWD = updatedDir
	if err := store.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.WorktreeDir != updatedDir {
		t.Fatalf("updated WorktreeDir = %q, want %q", got.WorktreeDir, updatedDir)
	}

	list, err := store.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	if list[0].WorktreeDir != updatedDir {
		t.Fatalf("summary WorktreeDir = %q, want %q", list[0].WorktreeDir, updatedDir)
	}
}
