package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSQLiteStorePersistsReasoningMode(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:            NewID(),
		Provider:      "openai",
		ProviderKey:   "openai",
		Model:         "gpt-5.6-sol",
		ReasoningMode: "pro",
		Mode:          ModeChat,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil || got.ReasoningMode != "pro" {
		t.Fatalf("created session = %+v, want reasoning_mode pro", got)
	}

	got.ReasoningMode = ""
	if err := store.Update(ctx, got); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get() after update error = %v", err)
	}
	if got == nil || got.ReasoningMode != "" {
		t.Fatalf("updated session = %+v, want empty reasoning_mode", got)
	}
}

func TestSQLiteStoreMigratesVersion35ReasoningMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE sessions DROP COLUMN reasoning_mode"); err != nil {
		db.Close()
		t.Fatalf("remove reasoning_mode from version 35 fixture: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version(version) VALUES (35)"); err != nil {
		db.Close()
		t.Fatalf("set schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture DB: %v", err)
	}

	store, err := NewStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("NewStore() migration error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "openai", Model: "gpt-5.6-sol", ReasoningMode: "pro", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create() after migration error = %v", err)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil || got == nil || got.ReasoningMode != "pro" {
		t.Fatalf("Get() after migration = %+v, err=%v", got, err)
	}
}
