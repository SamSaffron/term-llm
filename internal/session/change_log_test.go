package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSQLiteStoreChangeLogTailsExternalMutations(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cursor, err := store.StoreChangeCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 0 {
		t.Fatalf("fresh cursor = %d, want 0", cursor)
	}

	now := time.Now().UTC()
	project := &Project{
		ID: "project-change-log", Name: "Initial", CanonicalDir: t.TempDir(),
		CreatedAt: now, UpdatedAt: now, LastUsedAt: now,
	}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess := &Session{
		ID: "session-change-log", Provider: "test", Model: "model", Mode: ModeChat,
		ProjectID: project.ID, CreatedAt: now, UpdatedAt: now, Status: StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	changes, err := store.ListStoreChanges(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreChangeKinds(t, changes, StoreChangeProjectCreated, StoreChangeSessionCreated)
	for _, change := range changes {
		cursor = change.Sequence
	}

	message := NewMessage(sess.ID, llm.UserText("one durable turn"), -1)
	if err := store.AddMessage(ctx, sess.ID, message); err != nil {
		t.Fatal(err)
	}
	changes, err = store.ListStoreChanges(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreChangeKinds(t, changes, StoreChangeSessionTranscriptChanged)
	cursor = changes[0].Sequence

	// Write SQL directly to model a TUI or another process sharing this DB. The
	// triggers must preserve each transition without a session catalog scan.
	if _, err := store.db.ExecContext(ctx, `
		UPDATE sessions
		SET name = 'Renamed', transcript_rev = 7, message_count = 2,
		    status = 'complete', project_id = NULL
		WHERE id = ?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE projects SET name = 'Renamed project' WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}

	changes, err = store.ListStoreChanges(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreChangeKinds(t, changes,
		StoreChangeSessionMetadataChanged,
		StoreChangeSessionTranscriptChanged,
		StoreChangeSessionStatusChanged,
		StoreChangeProjectMembershipChanged,
		StoreChangeProjectUpdated,
	)
	for _, change := range changes {
		if change.Kind == StoreChangeSessionStatusChanged && change.Status != StatusComplete {
			t.Fatalf("status change = %#v, want completed", change)
		}
		if change.Kind == StoreChangeSessionTranscriptChanged && change.TranscriptRev != 7 {
			t.Fatalf("transcript change = %#v, want revision 7", change)
		}
		cursor = change.Sequence
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET agent = 'developer', tools = 'shell', mcp = 'browser' WHERE id = ?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	changes, err = store.ListStoreChanges(ctx, cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreChangeKinds(t, changes, StoreChangeSessionMetadataChanged)
	cursor = changes[0].Sequence

	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	changes, err = store.ListStoreChanges(ctx, cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreChangeKinds(t, changes, StoreChangeSessionDeleted)
	if len(changes) != 1 || changes[0].Sequence <= cursor {
		t.Fatalf("delete changes = %#v after cursor %d", changes, cursor)
	}
}

func TestSQLiteStoreChangeLogReportsTrimmedCursor(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO session_change_log(kind) VALUES('test.change')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8_500; i++ {
		if _, err := statement.ExecContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	statement.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	_, err = store.ListStoreChanges(ctx, 0, 100)
	var cursorErr *StoreChangeCursorError
	if !errors.As(err, &cursorErr) || cursorErr.Oldest <= 1 {
		t.Fatalf("trimmed cursor error = %#v", err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_change_log`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows > 8_447 {
		t.Fatalf("retained rows = %d, want bounded tail", rows)
	}
}

func assertStoreChangeKinds(t *testing.T, changes []StoreChange, expected ...string) {
	t.Helper()
	counts := make(map[string]int, len(changes))
	for _, change := range changes {
		counts[change.Kind]++
	}
	for _, kind := range expected {
		if counts[kind] == 0 {
			t.Fatalf("changes = %#v, missing %q", changes, kind)
		}
	}
	if len(changes) != len(expected) {
		t.Fatalf("changes = %#v, want exactly %v", changes, expected)
	}
}
