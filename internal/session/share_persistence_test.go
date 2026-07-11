package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSessionSharePersistenceRoundTrip(t *testing.T) {
	store, err := NewSQLiteStore(Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	sess := &Session{ID: "sess-share", Provider: "mock", Model: "mock", Mode: ModeChat, CreatedAt: now, UpdatedAt: now, Share: &ShareState{GistID: "abc123", GistURL: "https://gist.github.com/u/abc123", PreviewURL: GistPreviewURL("abc123"), SharedAt: now, UpdatedAt: now}}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), sess.ID)
	if err != nil || got == nil || got.Share == nil || got.Share.GistID != "abc123" {
		t.Fatalf("Get share = %+v, err = %v", got, err)
	}
	updated := got.Share.Clone()
	updated.PreviewURL = "https://example.test/preview"
	updated.Public = true
	if err := UpdateShare(context.Background(), store, sess.ID, updated); err != nil {
		t.Fatal(err)
	}
	list, err := store.List(context.Background(), ListOptions{Limit: 10})
	if err != nil || len(list) != 1 || list[0].Share == nil || !list[0].Share.Public {
		t.Fatalf("List share = %+v, err = %v", list, err)
	}
	if err := store.UpdateShare(context.Background(), sess.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(context.Background(), sess.ID)
	if err != nil || got.Share != nil {
		t.Fatalf("cleared share = %+v, err = %v", got.Share, err)
	}
}

func TestSQLiteStoreMigratesVersion36Share(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE sessions DROP COLUMN share"); err != nil {
		t.Fatalf("remove share from version 36 fixture: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version(version) VALUES (36)"); err != nil {
		t.Fatalf("set schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("migrate version 36: %v", err)
	}
	defer store.Close()
	sess := &Session{ID: NewID(), Provider: "mock", Model: "mock", Mode: ModeChat, Share: &ShareState{GistID: "abc123"}}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create after migration: %v", err)
	}
	got, err := store.Get(context.Background(), sess.ID)
	if err != nil || got == nil || got.Share == nil || got.Share.GistID != "abc123" {
		t.Fatalf("Get after migration = %+v, err=%v", got, err)
	}
}

func TestGistPreviewURLRejectsInvalidID(t *testing.T) {
	if got := GistPreviewURL("ABC/123"); got != "" {
		t.Fatalf("GistPreviewURL invalid = %q", got)
	}
}
