package session

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

func scopeV69Fixture(t *testing.T) *sql.DB {
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
	_, err = db.Exec(`DROP TRIGGER sessions_scope_lock_bu; UPDATE schema_version SET version=69;
 INSERT INTO sessions(id,provider,model,agent,model_policy,created_at,updated_at) VALUES('legacy','mock','mock','old','{"rules":[{"agent":"old","allowed":["mock:*"]}]}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func triggerExists(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='sessions_scope_lock_bu'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestScopeMigrationV70RollbackRetryAndSignature(t *testing.T) {
	db := scopeV69Fixture(t)
	original := migrations
	t.Cleanup(func() { migrations = original })
	migrations = append([]migration(nil), original...)
	index := slices.IndexFunc(migrations, func(m migration) bool { return m.version == 70 })
	up := migrations[index].up
	migrations[index].up = func(db schemaExecutor) error {
		if err := up(db); err != nil {
			return err
		}
		return errors.New("scope migration canary failure")
	}
	if err := initSchema(db); err == nil {
		t.Fatal("expected rollback")
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 69 || triggerExists(t, db) {
		t.Fatalf("after rollback version=%d err=%v trigger=%v", version, err, triggerExists(t, db))
	}
	migrations = original
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion || !triggerExists(t, db) {
		t.Fatalf("after retry version=%d err=%v", version, err)
	}
	// Existing rows carry no lock and keep ordinary mobility.
	if _, err := db.Exec(`UPDATE sessions SET agent='new', model_policy=NULL WHERE id='legacy'`); err != nil {
		t.Fatalf("legacy row update: %v", err)
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
