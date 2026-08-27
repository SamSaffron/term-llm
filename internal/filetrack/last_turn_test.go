package filetrack

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLastTurnChangesUseLatestRun(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	record := func(rec ChangeRecord) {
		t.Helper()
		if _, err := recordTestChange(ctx, store, rec); err != nil {
			t.Fatal(err)
		}
	}

	record(ChangeRecord{SessionID: "session", RunID: "run-1", Path: "/work/old.txt", Before: []byte("old\n"), After: []byte("first\n")})
	record(ChangeRecord{SessionID: "session", RunID: "run-2", Path: "/work/current.txt", BeforeMissing: true, After: []byte("one\n")})
	record(ChangeRecord{SessionID: "session", RunID: "run-2", Path: "/work/current.txt", Before: []byte("one\n"), After: []byte("one\ntwo\n")})

	changes, err := store.ListRecentRunChanges(ctx, "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "/work/current.txt" || changes[0].Kind != KindCreate || changes[0].Adds != 2 {
		t.Fatalf("last turn changes = %#v", changes)
	}
	content, err := store.GetRecentRunFileDiffContent(ctx, "session", "/work/current.txt", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || len(content.Before) != 0 || string(content.After) != "one\ntwo\n" {
		t.Fatalf("last turn content = %#v", content)
	}
	if old, err := store.GetRecentRunFileDiffContent(ctx, "session", "/work/old.txt", 1, 0); err != nil || old != nil {
		t.Fatalf("older-run content = %#v, %v", old, err)
	}
}

func TestLastTurnChangesIncludeRunInProgress(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := recordTestChange(ctx, store, ChangeRecord{
		SessionID: "session", RunID: "run-1", Path: "/work/test.txt",
		BeforeMissing: true, After: []byte("123"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunStart(ctx, RunRecord{SessionID: "session", RunID: "run-2"}); err != nil {
		t.Fatal(err)
	}

	changes, err := store.ListRecentRunChanges(ctx, "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("new in-progress turn fell back to prior changes: %#v", changes)
	}

	if _, err := recordTestChange(ctx, store, ChangeRecord{
		SessionID: "session", RunID: "run-2", Path: "/work/test.txt",
		Before: []byte("123"), After: []byte("234"),
	}); err != nil {
		t.Fatal(err)
	}
	changes, err = store.ListRecentRunChanges(ctx, "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "/work/test.txt" || changes[0].Kind != KindModify || changes[0].Adds != 1 || changes[0].Dels != 1 {
		t.Fatalf("in-progress last turn changes = %#v", changes)
	}
	content, err := store.GetRecentRunFileDiffContent(ctx, "session", "/work/test.txt", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || string(content.Before) != "123" || string(content.After) != "234" {
		t.Fatalf("in-progress last turn content = %#v", content)
	}
}

func TestRecentRunChangesCoverRollingWindow(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	record := func(runID, path string, before, after []byte) {
		t.Helper()
		if _, err := recordTestChange(ctx, store, ChangeRecord{
			SessionID: "session", RunID: runID, Path: path, Before: before, After: after,
		}); err != nil {
			t.Fatal(err)
		}
	}

	record("run-1", "/work/old.txt", []byte("old\n"), []byte("older\n"))
	record("run-2", "/work/shared.txt", []byte("base\n"), []byte("two\n"))
	record("run-3", "/work/middle.txt", []byte("before\n"), []byte("middle\n"))
	record("run-4", "/work/shared.txt", []byte("two\n"), []byte("four\n"))

	changes, err := store.ListRecentRunChanges(ctx, "session", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("recent changes = %#v, want two entries", changes)
	}
	byPath := make(map[string]CumulativeChange, len(changes))
	for _, change := range changes {
		byPath[change.Path] = change
		if change.SnapshotSeq != 4 {
			t.Fatalf("snapshot seq for %s = %d, want 4", change.Path, change.SnapshotSeq)
		}
	}
	if _, ok := byPath["/work/old.txt"]; ok {
		t.Fatalf("oldest run leaked into rolling window: %#v", changes)
	}
	if got := byPath["/work/shared.txt"]; got.Seq != 4 || got.Kind != KindModify {
		t.Fatalf("shared change = %#v", got)
	}

	record("run-5", "/work/shared.txt", []byte("four\n"), []byte("five\n"))
	content, err := store.GetRecentRunFileDiffContent(ctx, "session", "/work/shared.txt", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || string(content.Before) != "base\n" || string(content.After) != "four\n" {
		t.Fatalf("pinned recent shared content = %#v", content)
	}
	content, err = store.GetRecentRunFileDiffContent(ctx, "session", "/work/shared.txt", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || string(content.Before) != "two\n" || string(content.After) != "five\n" {
		t.Fatalf("latest recent shared content = %#v", content)
	}
}

func TestRecentRunChangesOmitWindowNetNoOp(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	for _, rec := range []ChangeRecord{
		{SessionID: "session", RunID: "run-1", Path: "/work/file.txt", Before: []byte("base\n"), After: []byte("changed\n")},
		{SessionID: "session", RunID: "run-2", Path: "/work/other.txt", BeforeMissing: true, After: []byte("other\n")},
		{SessionID: "session", RunID: "run-3", Path: "/work/file.txt", Before: []byte("changed\n"), After: []byte("base\n")},
	} {
		if _, err := recordTestChange(ctx, store, rec); err != nil {
			t.Fatal(err)
		}
	}

	changes, err := store.ListRecentRunChanges(ctx, "session", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "/work/other.txt" {
		t.Fatalf("recent changes = %#v, want only other.txt", changes)
	}
}

func TestOpenMigratesRunIdentityColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (1);
		CREATE TABLE file_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, seq INTEGER NOT NULL,
			path TEXT NOT NULL, kind TEXT NOT NULL, tool_name TEXT, tool_call_id TEXT,
			before_hash TEXT, after_hash TEXT, before_size INTEGER NOT NULL DEFAULT 0,
			after_size INTEGER NOT NULL DEFAULT 0, adds INTEGER NOT NULL DEFAULT 0,
			dels INTEGER NOT NULL DEFAULT 0, truncated INTEGER NOT NULL DEFAULT 0,
			is_binary INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(session_id, seq)
		);
		INSERT INTO file_changes(session_id,seq,path,kind,tool_name) VALUES
			('historical',1,'/work/direct','modify','write_file'),
			('historical',2,'/work/shell','modify','shell'),
			('historical',3,'/work/custom','modify','custom_tool');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.db.Query(`SELECT tool_name, provenance FROM file_changes WHERE session_id='historical' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []struct{ tool, provenance string }{{"write_file", ProvenanceDirect}, {"shell", ProvenanceLegacyUnverified}, {"custom_tool", ProvenanceLegacyUnverified}}
	for i := 0; rows.Next(); i++ {
		var tool, provenance string
		if err := rows.Scan(&tool, &provenance); err != nil {
			t.Fatal(err)
		}
		if i >= len(want) || tool != want[i].tool || provenance != want[i].provenance {
			t.Fatalf("migration row %d = %q/%q, want %+v", i, tool, provenance, want[i])
		}
	}
	if _, err := recordTestChange(context.Background(), store, ChangeRecord{
		SessionID: "session", RunID: "run", Path: "/work/file", BeforeMissing: true, After: []byte("x\n"),
	}); err != nil {
		t.Fatalf("record after migration: %v", err)
	}
	changes, err := store.ListRecentRunChanges(context.Background(), "session", 1)
	if err != nil || len(changes) != 1 {
		t.Fatalf("last turn after migration = %#v, %v", changes, err)
	}
}

func TestOpenRepairsPartiallyAppliedRunIdentityMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (1);
		CREATE TABLE file_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, seq INTEGER NOT NULL,
			path TEXT NOT NULL, kind TEXT NOT NULL, tool_name TEXT, tool_call_id TEXT,
			before_hash TEXT, after_hash TEXT, before_size INTEGER NOT NULL DEFAULT 0,
			after_size INTEGER NOT NULL DEFAULT 0, adds INTEGER NOT NULL DEFAULT 0,
			dels INTEGER NOT NULL DEFAULT 0, truncated INTEGER NOT NULL DEFAULT 0,
			is_binary INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			run_id TEXT, UNIQUE(session_id, seq)
		);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("open partially migrated store: %v", err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

func TestLastTurnIgnoresLegacyRowsWithoutRunIdentity(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	if _, err := recordTestChange(ctx, store, ChangeRecord{SessionID: "session", Path: "/work/legacy.txt", BeforeMissing: true, After: []byte("legacy\n")}); err != nil {
		t.Fatal(err)
	}
	changes, err := store.ListRecentRunChanges(ctx, "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("legacy rows flooded last turn: %#v", changes)
	}
}
