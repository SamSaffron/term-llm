package session

import (
	"context"
	"database/sql"
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

	// Turn 1: 1000 input, 250 output, 700 cache read, 500 cache write
	if err := store.UpdateMetrics(ctx, sess.ID, 2, 3, 1000, 250, 700, 500); err != nil {
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
	if loaded.CacheWriteTokens != 500 {
		t.Errorf("expected cache_write_tokens=500, got %d", loaded.CacheWriteTokens)
	}

	// Turn 2: verify accumulation
	if err := store.UpdateMetrics(ctx, sess.ID, 1, 2, 50, 100, 1200, 50); err != nil {
		t.Fatalf("failed to update session metrics (turn 2): %v", err)
	}
	loaded2, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to load session after turn 2: %v", err)
	}
	if loaded2.InputTokens != 1050 {
		t.Errorf("expected input_tokens=1050 after accumulation, got %d", loaded2.InputTokens)
	}
	if loaded2.CachedInputTokens != 1900 {
		t.Errorf("expected cached_input_tokens=1900 after accumulation, got %d", loaded2.CachedInputTokens)
	}
	if loaded2.CacheWriteTokens != 550 {
		t.Errorf("expected cache_write_tokens=550 after accumulation, got %d", loaded2.CacheWriteTokens)
	}
	if loaded2.OutputTokens != 350 {
		t.Errorf("expected output_tokens=350 after accumulation, got %d", loaded2.OutputTokens)
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session summary, got %d", len(summaries))
	}
	if summaries[0].CachedInputTokens != 1900 {
		t.Errorf("expected summary cached_input_tokens=1900, got %d", summaries[0].CachedInputTokens)
	}
	if summaries[0].CacheWriteTokens != 550 {
		t.Errorf("expected summary cache_write_tokens=550, got %d", summaries[0].CacheWriteTokens)
	}
}

func TestUpdateDoesNotClobberTokenMetrics(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:       NewID(),
		Provider: "test",
		Model:    "test-model",
		Mode:     ModeChat,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Accumulate metrics via UpdateMetrics
	if err := store.UpdateMetrics(ctx, sess.ID, 3, 5, 1000, 250, 700, 500); err != nil {
		t.Fatalf("UpdateMetrics: %v", err)
	}

	// Now call Update to change metadata (e.g. summary).
	// The in-memory sess still has zero token counts — this must NOT reset the DB.
	sess.Summary = "updated summary"
	if err := store.Update(ctx, sess); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Summary != "updated summary" {
		t.Errorf("expected summary=%q, got %q", "updated summary", reloaded.Summary)
	}
	if reloaded.InputTokens != 1000 {
		t.Errorf("Update clobbered input_tokens: expected 1000, got %d", reloaded.InputTokens)
	}
	if reloaded.CachedInputTokens != 700 {
		t.Errorf("Update clobbered cached_input_tokens: expected 700, got %d", reloaded.CachedInputTokens)
	}
	if reloaded.CacheWriteTokens != 500 {
		t.Errorf("Update clobbered cache_write_tokens: expected 500, got %d", reloaded.CacheWriteTokens)
	}
	if reloaded.OutputTokens != 250 {
		t.Errorf("Update clobbered output_tokens: expected 250, got %d", reloaded.OutputTokens)
	}
	if reloaded.LLMTurns != 3 {
		t.Errorf("Update clobbered llm_turns: expected 3, got %d", reloaded.LLMTurns)
	}
	if reloaded.ToolCalls != 5 {
		t.Errorf("Update clobbered tool_calls: expected 5, got %d", reloaded.ToolCalls)
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

func TestSQLiteStoreProviderKeyRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:          NewID(),
		Provider:    "OpenAI (gpt-5)",
		ProviderKey: "openai",
		Model:       "gpt-5",
		Mode:        ModeChat,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	loaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to load session: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session to exist")
	}
	if loaded.ProviderKey != "openai" {
		t.Fatalf("expected provider_key %q, got %q", "openai", loaded.ProviderKey)
	}

	loaded.ProviderKey = "chatgpt"
	if err := store.Update(ctx, loaded); err != nil {
		t.Fatalf("failed to update session: %v", err)
	}

	reloaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to reload session: %v", err)
	}
	if reloaded.ProviderKey != "chatgpt" {
		t.Fatalf("expected updated provider_key %q, got %q", "chatgpt", reloaded.ProviderKey)
	}
}

func TestSQLiteStoreMigratesProviderKeyColumn(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions-v7.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}
	seedSQL := `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    number INTEGER,
    name TEXT,
    summary TEXT,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    mode TEXT DEFAULT 'chat',
    agent TEXT,
    cwd TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived BOOLEAN DEFAULT FALSE,
    parent_id TEXT REFERENCES sessions(id),
    search BOOLEAN DEFAULT FALSE,
    tools TEXT,
    mcp TEXT,
    user_turns INTEGER DEFAULT 0,
    llm_turns INTEGER DEFAULT 0,
    tool_calls INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    cached_input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    tags TEXT
);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES (7);
`
	if _, err := db.Exec(seedSQL); err != nil {
		db.Close()
		t.Fatalf("failed to seed v7 schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("failed to close seed database: %v", err)
	}

	store, err := NewSQLiteStore(Config{
		Enabled: true,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("failed to open migrated sqlite store: %v", err)
	}
	defer store.Close()

	// Verify migration added provider_key.
	rows, err := store.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		t.Fatalf("failed to inspect sessions table: %v", err)
	}
	defer rows.Close()

	var hasProviderKey bool
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("failed to scan table info: %v", err)
		}
		if name == "provider_key" {
			hasProviderKey = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("failed iterating table info: %v", err)
	}
	if !hasProviderKey {
		t.Fatal("expected provider_key column after migration")
	}
}

func TestReadOnlyOldDBWithoutCompactionSeq(t *testing.T) {
	// Simulate an old database that doesn't have the compaction_seq column.
	// A read-only store should still be able to read sessions from it.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create DB with old schema (no compaction_seq)
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			number INTEGER,
			name TEXT,
			summary TEXT,
			provider TEXT NOT NULL,
			provider_key TEXT,
			model TEXT NOT NULL,
			mode TEXT DEFAULT 'chat',
			agent TEXT,
			cwd TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			archived BOOLEAN DEFAULT FALSE,
			parent_id TEXT REFERENCES sessions(id),
			search BOOLEAN DEFAULT FALSE,
			tools TEXT,
			mcp TEXT,
			user_turns INTEGER DEFAULT 0,
			llm_turns INTEGER DEFAULT 0,
			tool_calls INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			cached_input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			tags TEXT
		);
		CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// Insert a test session
	_, err = db.Exec(`
		INSERT INTO sessions (id, number, name, summary, provider, model, created_at, updated_at)
		VALUES ('test-id-123', 1, 'test session', 'summary', 'openai', 'gpt-5', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	db.Close()

	// Open in read-only mode (skips migrations)
	store, err := NewSQLiteStore(Config{
		Path:     dbPath,
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	defer store.Close()

	if store.hasCompactionSeq {
		t.Error("old DB should not have compaction_seq")
	}

	ctx := context.Background()

	// Get should work and default CompactionSeq to -1
	sess, err := store.Get(ctx, "test-id-123")
	if err != nil {
		t.Fatalf("Get failed on old DB: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if sess.CompactionSeq != -1 {
		t.Errorf("CompactionSeq = %d, want -1 (default for missing column)", sess.CompactionSeq)
	}

	// GetByNumber should also work
	sess, err = store.GetByNumber(ctx, 1)
	if err != nil {
		t.Fatalf("GetByNumber failed on old DB: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session from GetByNumber, got nil")
	}
	if sess.CompactionSeq != -1 {
		t.Errorf("CompactionSeq = %d, want -1", sess.CompactionSeq)
	}
}
