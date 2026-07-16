package session

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigration39RemovesRetiredV38SideSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	root := &Session{ID: "root", Provider: "mock", Model: "model"}
	if err := store.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"ALTER TABLE sessions ADD COLUMN root_id TEXT REFERENCES sessions(id)",
		"ALTER TABLE sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'root' CHECK (kind IN ('root', 'side'))",
		"ALTER TABLE sessions ADD COLUMN side_state TEXT",
		"UPDATE sessions SET root_id = id",
		"CREATE INDEX idx_sessions_root_kind ON sessions(root_id, kind, updated_at DESC)",
		"CREATE UNIQUE INDEX idx_sessions_one_open_side ON sessions(root_id) WHERE kind = 'side' AND side_state = 'open'",
		"CREATE TABLE side_context_messages (side_session_id TEXT, sequence INTEGER)",
		"INSERT INTO sessions(id, provider, model, created_at, updated_at, root_id, kind, side_state) SELECT 'legacy-side', provider, model, created_at, updated_at, id, 'side', 'open' FROM sessions WHERE id = 'root'",
		"UPDATE schema_version SET version = 38",
	} {
		if _, err := store.db.Exec(stmt); err != nil {
			store.Close()
			t.Fatalf("seed v38 with %q: %v", stmt, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d err=%v, want %d", version, err, schemaVersion)
	}
	legacy, err := store.Get(ctx, "legacy-side")
	if err != nil || legacy != nil {
		t.Fatalf("legacy side = %#v err=%v, want absent", legacy, err)
	}
	var tableCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='side_context_messages'").Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("side context table count = %d err=%v", tableCount, err)
	}
	listed, err := store.List(ctx, ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].ID != root.ID {
		t.Fatalf("listed sessions = %#v err=%v", listed, err)
	}
}
