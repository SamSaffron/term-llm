package session

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// Frozen released v67 outbox shape. The other bootstrap objects are unchanged
// by v68; remove all new v68 objects before publishing the faithful old marker.
const notificationOutboxV67 = `CREATE TABLE completion_push_outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 event_id TEXT NOT NULL UNIQUE,
 response_id TEXT NOT NULL,
 subscription_id TEXT NOT NULL,
 payload BLOB NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','delivered','dead')),
 attempt_count INTEGER NOT NULL DEFAULT 0,
 next_attempt_at TEXT NOT NULL DEFAULT(datetime('now')),
 last_error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT(datetime('now')),
 updated_at TEXT NOT NULL DEFAULT(datetime('now')),
 UNIQUE(response_id,subscription_id)
);
CREATE INDEX idx_completion_push_outbox_due ON completion_push_outbox(status,next_attempt_at,id);`

func notificationV67Fixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err = initSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER notification_subscription_deleted;DROP TRIGGER notification_subscription_stale;DROP TRIGGER notification_attention_seen;
 DROP TABLE notification_response_turns;DROP TABLE notification_awaited_agents;DROP TABLE notification_incorporated_agents;
 DROP TABLE notification_pending_calls;DROP TABLE notification_turns;DROP TABLE notification_presence;DROP TABLE notification_documents;
 DROP TABLE notification_steering_origins;DROP TABLE notification_rush_origins;DROP TABLE completion_push_outbox;` + notificationOutboxV67 + `
 UPDATE schema_version SET version=67;
 INSERT INTO push_subscriptions(id,endpoint,key_p256dh,key_auth,client_kind,status) VALUES('sub','https://push.invalid/test','key','auth','app','stale');
 INSERT INTO completion_push_outbox(event_id,response_id,subscription_id,payload,status) VALUES
 ('legacy-pending','r1','sub',X'010203','pending'),('legacy-delivered','r2','sub',X'040506','delivered');`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestNotificationMigrationV68PreservesDataRetiresPendingAndMatchesFresh(t *testing.T) {
	db := notificationV67Fixture(t)
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var pendingStatus, deliveredStatus, kind, status string
	var payload []byte
	if err := db.QueryRow(`SELECT status,payload FROM completion_push_outbox WHERE event_id='legacy-pending'`).Scan(&pendingStatus, &payload); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM completion_push_outbox WHERE event_id='legacy-delivered'`).Scan(&deliveredStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT client_kind,status FROM push_subscriptions WHERE id='sub'`).Scan(&kind, &status); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != "dead" || deliveredStatus != "delivered" || kind != "app" || status != "stale" || !reflect.DeepEqual(payload, []byte{1, 2, 3}) {
		t.Fatalf("preservation pending=%s delivered=%s kind=%s status=%s payload=%v", pendingStatus, deliveredStatus, kind, status, payload)
	}
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err = initSchema(fresh); err != nil {
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
		t.Fatalf("fresh/upgrade differ\ngot=%v\nwant=%v", got, want)
	}
}

func TestNotificationMigrationV68RollsBackMarkerRetirementAndDDLThenRetries(t *testing.T) {
	db := notificationV67Fixture(t)
	original := migrations
	defer func() { migrations = original }()
	migrations = append([]migration(nil), original...)
	last := -1
	for i := range migrations {
		if migrations[i].version == 68 {
			last = i
			break
		}
	}
	if last < 0 {
		t.Fatal("v68 migration not found")
	}
	up := migrations[last].up
	migrations[last].up = func(db schemaExecutor) error {
		if err := up(db); err != nil {
			return err
		}
		return errors.New("injected v68 failure")
	}
	if err := initSchema(db); err == nil || !strings.Contains(err.Error(), "injected v68 failure") {
		t.Fatalf("failure=%v", err)
	}
	var version int
	var status string
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 67 {
		t.Fatalf("marker=%d error=%v", version, err)
	}
	if err := db.QueryRow(`SELECT status FROM completion_push_outbox WHERE event_id='legacy-pending'`).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("retirement escaped rollback=%s %v", status, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "completion_push_outbox", "turn_id"); err != nil || exists {
		t.Fatalf("column escaped rollback=%v %v", exists, err)
	}
	if exists, err := sqliteutil.TableExists(db, "notification_turns"); err != nil || exists {
		t.Fatalf("table escaped rollback=%v %v", exists, err)
	}
	migrations = original
	if err := initSchema(db); err != nil {
		t.Fatalf("retry=%v", err)
	}
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("retry marker=%d error=%v", version, err)
	}
}
