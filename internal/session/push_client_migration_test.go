package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// Frozen v66 push table, including its lifecycle columns and original defaults.
const pushSubscriptionsSchemaV66 = `CREATE TABLE push_subscriptions (
    id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL UNIQUE,
    key_p256dh TEXT NOT NULL,
    key_auth TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    vapid_key_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT '',
    last_used_at TEXT,
    last_failure_code TEXT NOT NULL DEFAULT '',
    last_failure TEXT NOT NULL DEFAULT '',
    last_failure_at TEXT
);`

// v67 changes only the push table. Keep the other canonical bootstrap objects
// and replace this table with the faithful released v66 definition and marker.
func openPushV66DB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE push_subscriptions;` + pushSubscriptionsSchemaV66 + `
		UPDATE schema_version SET version=66;
		INSERT INTO push_subscriptions(id,endpoint,key_p256dh,key_auth,status,vapid_key_id,created_at,updated_at,last_used_at,last_failure_code,last_failure,last_failure_at)
		VALUES('browser-active','https://push.example/active','key-one','auth-one','active','vapid-one','2026-01-01 01:02:03','2026-02-01 01:02:03','2026-02-01 01:02:03','','',NULL),
		      ('browser-stale','https://push.example/stale','key-two','auth-two','stale','vapid-two','2026-01-02 01:02:03','2026-02-02 01:02:03',NULL,'http_410','expired endpoint','2026-02-02 01:02:03');
		INSERT INTO completion_push_outbox(event_id,response_id,subscription_id,payload)
		VALUES('event','response','browser-active',X'010203');`); err != nil {
		t.Fatal(err)
	}
	return db
}

func pushV66Rows(t *testing.T, db *sql.DB) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT id,endpoint,key_p256dh,key_auth,status,vapid_key_id,created_at,updated_at,last_used_at,last_failure_code,last_failure,last_failure_at FROM push_subscriptions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result [][]any
	for rows.Next() {
		values := make([]any, 12)
		pointers := make([]any, len(values))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPushClientKindMigrationV67PreservesRowsAndMatchesFresh(t *testing.T) {
	db := openPushV66DB(t, ":memory:")
	before := pushV66Rows(t, db)
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if got := pushV66Rows(t, db); len(got) != 2 || !reflect.DeepEqual(before, got) {
		t.Fatalf("migration lost data: before=%v after=%v", before, got)
	}
	var browsers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM push_subscriptions WHERE client_kind='browser'`).Scan(&browsers); err != nil || browsers != 2 {
		t.Fatalf("backfilled browser count=%d, err=%v", browsers, err)
	}
	var payload []byte
	var subscription string
	if err := db.QueryRow(`SELECT subscription_id,payload FROM completion_push_outbox WHERE event_id='event'`).Scan(&subscription, &payload); err != nil || subscription != "browser-active" || !reflect.DeepEqual(payload, []byte{1, 2, 3}) {
		t.Fatalf("outbox subscription=%q payload=%v err=%v", subscription, payload, err)
	}
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	want, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sqliteutil.SchemaSignature(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("fresh and migrated schemas differ\nwant=%v\ngot=%v", want, got)
	}
	for name, candidate := range map[string]*sql.DB{"fresh": fresh, "migrated": db} {
		t.Run(name, func(t *testing.T) {
			if _, err := candidate.Exec(`INSERT INTO push_subscriptions(id,endpoint,key_p256dh,key_auth) VALUES('default','https://push.example/default','key','auth')`); err != nil {
				t.Fatal(err)
			}
			var kind string
			if err := candidate.QueryRow(`SELECT client_kind FROM push_subscriptions WHERE id='default'`).Scan(&kind); err != nil || kind != PushClientBrowser {
				t.Fatalf("SQL default=%q, err=%v", kind, err)
			}
			for _, invalid := range []any{nil, "", "APP", "native"} {
				if _, err := candidate.Exec(`INSERT INTO push_subscriptions(id,endpoint,key_p256dh,key_auth,client_kind) VALUES('invalid','https://push.example/invalid','key','auth',?)`, invalid); err == nil {
					t.Fatalf("SQL constraint accepted %v", invalid)
				}
				if _, err := candidate.Exec(`UPDATE push_subscriptions SET client_kind=? WHERE id='default'`, invalid); err == nil {
					t.Fatalf("SQL update constraint accepted %v", invalid)
				}
			}
			if _, err := candidate.Exec(`UPDATE push_subscriptions SET client_kind='app' WHERE id='default'`); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPushClientKindMigrationV67RollbackAndRetry(t *testing.T) {
	db := openPushV66DB(t, ":memory:")
	before := pushV66Rows(t, db)
	original := migrations
	defer func() { migrations = original }()
	migrations = append([]migration(nil), original...)
	index := -1
	for i := range migrations {
		if migrations[i].version == 67 {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("v67 migration missing")
	}
	up := migrations[index].up
	migrations[index].up = func(db schemaExecutor) error {
		if err := up(db); err != nil {
			return err
		}
		return errors.New("injected v67 failure")
	}
	if err := initSchema(db); err == nil || !strings.Contains(err.Error(), "injected v67 failure") {
		t.Fatalf("failure=%v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 66 {
		t.Fatalf("marker=%d err=%v", version, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "push_subscriptions", "client_kind"); err != nil || exists {
		t.Fatalf("rolled-back column exists=%v err=%v", exists, err)
	}
	if got := pushV66Rows(t, db); !reflect.DeepEqual(before, got) {
		t.Fatalf("rollback changed rows: before=%v after=%v", before, got)
	}
	migrations = original
	if err := initSchema(db); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("retried marker=%d err=%v", version, err)
	}
	if got := pushV66Rows(t, db); !reflect.DeepEqual(before, got) {
		t.Fatalf("retry changed rows: before=%v after=%v", before, got)
	}
}

func TestPushClientKindReadOnlyV66DefaultsToBrowser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	db := openPushV66DB(t, path)
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	subs, err := store.ListPushSubscriptions(ctx)
	if err != nil || len(subs) != 2 {
		t.Fatalf("legacy list=%+v err=%v", subs, err)
	}
	for _, sub := range subs {
		if sub.ClientKind != PushClientBrowser {
			t.Fatalf("legacy list kind=%q", sub.ClientKind)
		}
		got, err := store.GetPushSubscription(ctx, sub.ID)
		if err != nil || got == nil || got.ClientKind != PushClientBrowser || got.Status != sub.Status {
			t.Fatalf("legacy get=%+v err=%v", got, err)
		}
	}
	if got, err := store.GetPushSubscription(ctx, "missing"); err != nil || got != nil {
		t.Fatalf("missing legacy get=%+v err=%v", got, err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 66 {
		t.Fatalf("read-only changed marker=%d err=%v", version, err)
	}
}
