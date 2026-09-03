package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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
	updated.URL = "https://example.test/preview"
	updated.Visibility = "public"
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

func TestShareStateLegacyNormalizationAndGitHubDualWrite(t *testing.T) {
	legacy := parseShareJSONString(sql.NullString{Valid: true, String: `{"gist_id":"abc123","gist_url":"https://gist.github.com/u/abc123","preview_url":"https://preview.example/abc123","public":true}`})
	if legacy == nil || legacy.Provider != "github" || legacy.ID != "abc123" || legacy.URL != "https://preview.example/abc123" || legacy.SourceURL != "https://gist.github.com/u/abc123" || legacy.Visibility != "public" || legacy.Scope != ShareScopeSession {
		t.Fatalf("normalized legacy state=%+v", legacy)
	}
	if !legacy.Exists() {
		t.Fatal("normalized legacy state does not exist")
	}
	withoutPreview := parseShareJSONString(sql.NullString{Valid: true, String: `{"gist_id":"def456","gist_url":"https://gist.github.com/u/def456"}`})
	if withoutPreview == nil || withoutPreview.URL != GistPreviewURL("def456") || withoutPreview.PreviewURL != GistPreviewURL("def456") || withoutPreview.SourceURL != "https://gist.github.com/u/def456" || withoutPreview.Visibility != "unlisted" {
		t.Fatalf("legacy state without preview=%+v", withoutPreview)
	}
	genericEmptyVisibility := (&ShareState{Provider: "github", ID: "feed123", Public: true}).Clone()
	if genericEmptyVisibility == nil || genericEmptyVisibility.Visibility != "public" || genericEmptyVisibility.URL != GistPreviewURL("feed123") {
		t.Fatalf("GitHub state with empty legacy visibility=%+v", genericEmptyVisibility)
	}

	generic := &ShareState{
		Provider: "github", ID: "def456", URL: "https://preview.example/def456",
		SourceURL: "https://gist.github.com/u/def456", Visibility: "unlisted", Scope: ShareScopeSession,
	}
	raw := shareJSONString(generic)
	if !raw.Valid {
		t.Fatal("generic GitHub state was not serialized")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw.String), &fields); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"provider": "github", "id": "def456", "url": "https://preview.example/def456",
		"source_url": "https://gist.github.com/u/def456", "visibility": "unlisted", "scope": "session",
		"gist_id": "def456", "gist_url": "https://gist.github.com/u/def456", "preview_url": "https://preview.example/def456",
	} {
		if fields[key] != want {
			t.Fatalf("serialized %s=%v, want %q: %s", key, fields[key], want, raw.String)
		}
	}
}

func TestShareStateCommandProviderDoesNotWriteLegacyGistFields(t *testing.T) {
	raw := shareJSONString(&ShareState{Provider: "acme", ID: "opaque", URL: "https://share.example/opaque", Visibility: "private", Scope: ShareScopeSession})
	if !raw.Valid || strings.Contains(raw.String, "gist_id") || strings.Contains(raw.String, "preview_url") || strings.Contains(raw.String, `"public"`) {
		t.Fatalf("custom provider serialization=%q", raw.String)
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
