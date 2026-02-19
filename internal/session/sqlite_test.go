package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStoreUpdateMetricsIncludesCachedTokens(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:        NewID(),
		Provider:  "ChatGPT (gpt-5.2-codex)",
		Model:     "gpt-5.2-codex",
		Mode:      ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	if err := store.UpdateMetrics(ctx, sess.ID, 2, 3, 1000, 250, 700); err != nil {
		t.Fatalf("failed to update session metrics: %v", err)
	}

	loaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to load session: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session to exist")
	}

	if loaded.LLMTurns != 2 {
		t.Errorf("expected llm_turns=2, got %d", loaded.LLMTurns)
	}
	if loaded.ToolCalls != 3 {
		t.Errorf("expected tool_calls=3, got %d", loaded.ToolCalls)
	}
	if loaded.InputTokens != 1000 {
		t.Errorf("expected input_tokens=1000, got %d", loaded.InputTokens)
	}
	if loaded.OutputTokens != 250 {
		t.Errorf("expected output_tokens=250, got %d", loaded.OutputTokens)
	}
	if loaded.CachedInputTokens != 700 {
		t.Errorf("expected cached_input_tokens=700, got %d", loaded.CachedInputTokens)
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session summary, got %d", len(summaries))
	}
	if summaries[0].CachedInputTokens != 700 {
		t.Errorf("expected summary cached_input_tokens=700, got %d", summaries[0].CachedInputTokens)
	}
}

func TestSQLiteStoreCustomPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "custom", "sessions.db")

	store, err := NewSQLiteStore(Config{
		Enabled: true,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("failed to create sqlite store with custom path: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected database file at %q: %v", dbPath, err)
	}
}
