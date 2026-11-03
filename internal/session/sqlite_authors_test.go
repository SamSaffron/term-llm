package session

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

func TestAuthorsPersistenceAndStarter(t *testing.T) {
	ctx := context.Background()
	for _, writer := range []string{"add", "batch", "replace", "reset", "compact"} {
		t.Run(writer, func(t *testing.T) {
			store, sess := newBranchTestStore(t)
			msg := core.UserText("attributed request")
			msg.AuthorID = "alice"
			msg.ClientMessageID = "intent"
			row := NewMessage(sess.ID, msg, -1)
			var err error
			switch writer {
			case "add":
				err = store.AddMessage(ctx, sess.ID, row)
			case "batch":
				_, err = store.AppendMessagesWithTranscriptRev(ctx, sess.ID, []*Message{row})
			case "replace":
				err = store.ReplaceMessages(ctx, sess.ID, []Message{*row})
			case "reset":
				err = store.ResetMessages(ctx, sess.ID, []Message{*row})
			case "compact":
				err = store.CompactMessages(ctx, sess.ID, []Message{*row})
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := store.GetMessages(ctx, sess.ID, 0, 0)
			if err != nil || len(rows) != 1 || rows[0].AuthorID != "alice" || rows[0].ToLLMMessage().AuthorID != "alice" {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			got, _ := store.Get(ctx, sess.ID)
			if got.CreatedBy != "alice" {
				t.Fatalf("starter=%q", got.CreatedBy)
			}
			list, err := store.List(ctx, ListOptions{CreatedBy: "alice"})
			if err != nil || len(list) != 1 || list[0].CreatedBy != "alice" {
				t.Fatalf("list=%+v err=%v", list, err)
			}
			if writer != "compact" {
				applyUndoRedo(t, store, sess.ID, false)
				applyUndoRedo(t, store, sess.ID, true)
				rows, _ = store.GetMessages(ctx, sess.ID, 0, 0)
				if len(rows) != 1 || rows[0].AuthorID != "alice" {
					t.Fatalf("redo=%+v", rows)
				}
			}
		})
	}
	store, sess := newBranchTestStore(t)
	addBranchTestMessage(t, store, sess.ID, core.UserText("anonymous first"))
	m := core.UserText("later")
	m.AuthorID = "alice"
	anchor := addBranchTestMessage(t, store, sess.ID, m)
	got, _ := store.Get(ctx, sess.ID)
	if got.CreatedBy != "" {
		t.Fatalf("anonymous starter changed: %q", got.CreatedBy)
	}
	branch, err := store.CreateBranch(ctx, sess.ID, CreateBranchOptions{AnchorMessageID: anchor.ID, CreatedBy: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if branch.Session.CreatedBy != "bob" {
		t.Fatalf("branch starter=%q", branch.Session.CreatedBy)
	}
	rows, _ := store.GetMessages(ctx, branch.Session.ID, 0, 0)
	if len(rows) != 2 || rows[1].AuthorID != "alice" {
		t.Fatalf("branch rows=%+v", rows)
	}
	p, err := store.SessionParticipants(ctx, []string{sess.ID, branch.Session.ID})
	if err != nil || p[sess.ID].Count != 1 || p[branch.Session.ID].IDs[0] != "alice" {
		t.Fatalf("participants=%+v err=%v", p, err)
	}
}

func TestAuthorDirectorySnapshotAndParticipantOrder(t *testing.T) {
	store, sess := newBranchTestStore(t)
	ctx := context.Background()
	for _, a := range []Author{{ID: "alice", Name: "Old"}, {ID: "alice", Name: "Renamed", Email: "a@example.com"}} {
		if err := store.UpsertAuthor(ctx, a, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAuthor(ctx, Author{ID: "alice", Name: "Stale device"}, true); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAuthors(ctx, []string{"alice"})
	if err != nil || got["alice"].Name != "Renamed" {
		t.Fatalf("authors=%+v err=%v", got, err)
	}
	for _, id := range []string{"bob", "alice", "bob", "c", "d", "e", "f"} {
		m := core.UserText(id)
		m.AuthorID = id
		addBranchTestMessage(t, store, sess.ID, m)
	}
	p, err := store.SessionParticipants(ctx, []string{sess.ID})
	if err != nil || p[sess.ID].Count != 6 || !reflect.DeepEqual(p[sess.ID].IDs, []string{"bob", "alice", "c", "d", "e"}) {
		t.Fatalf("participants=%+v err=%v", p, err)
	}
	matches, err := store.Search(ctx, SearchOptions{Query: "bob", CreatedBy: "bob"})
	if err != nil || len(matches) != 1 || matches[0].CreatedBy != "bob" {
		t.Fatalf("search=%+v err=%v", matches, err)
	}
}

// Remove exactly the additive v69 objects to reproduce the released v68 shape.
func authorV68Fixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`DROP TRIGGER messages_created_by_insert; DROP INDEX idx_messages_session_author; DROP TABLE authors; ALTER TABLE messages DROP COLUMN author_id; ALTER TABLE sessions DROP COLUMN created_by; UPDATE schema_version SET version=68;
 INSERT INTO sessions(id,provider,model,created_at,updated_at) VALUES('legacy','mock','mock',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 INSERT INTO messages(session_id,role,parts,text_content,sequence) VALUES('legacy','user','[]','preserve me',0);`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAuthorMigrationV69PreservationRollbackRetryAndSignature(t *testing.T) {
	db := authorV68Fixture(t)
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = append([]migration(nil), original...)
	last := len(migrations) - 1
	up := migrations[last].up
	migrations[last].up = func(db schemaExecutor) error {
		if err := up(db); err != nil {
			return err
		}
		return errors.New("author migration canary failure")
	}
	if err := initSchema(db); err == nil {
		t.Fatal("expected rollback")
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 68 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "messages", "author_id"); err != nil || exists {
		t.Fatalf("column escaped rollback: %v %v", exists, err)
	}
	if exists, err := sqliteutil.TableExists(db, "authors"); err != nil || exists {
		t.Fatalf("author directory escaped rollback: %v %v", exists, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "sessions", "created_by"); err != nil || exists {
		t.Fatalf("starter column escaped rollback: %v %v", exists, err)
	}
	migrations = original
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var text, author, starter string
	if err := db.QueryRow(`SELECT text_content,author_id,created_by FROM messages JOIN sessions ON sessions.id=messages.session_id`).Scan(&text, &author, &starter); err != nil || text != "preserve me" || author != "" || starter != "" {
		t.Fatalf("preservation %q %q %q %v", text, author, starter, err)
	}
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	got, err := sqliteutil.SchemaSignature(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	want, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh/migrated schema differ\ngot=%v\nwant=%v", got, want)
	}
}
