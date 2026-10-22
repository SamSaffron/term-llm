package notificationdb

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// Immutable v68 objects are shared by fresh bootstrap and migration.
const SchemaV68 = `
CREATE TABLE IF NOT EXISTS notification_turns (
 turn_id TEXT PRIMARY KEY,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 current_response_id TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'running',
 subscription_id TEXT NOT NULL DEFAULT '',
 window_id TEXT NOT NULL DEFAULT '',
 document_id TEXT NOT NULL DEFAULT '',
 source TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notification_turn_session ON notification_turns(session_id,state);
CREATE TABLE IF NOT EXISTS notification_response_turns (
 response_id TEXT PRIMARY KEY REFERENCES serve_response_lifecycle(response_id) ON DELETE CASCADE,
 turn_id TEXT NOT NULL REFERENCES notification_turns(turn_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS notification_awaited_agents (
 turn_id TEXT NOT NULL REFERENCES notification_turns(turn_id) ON DELETE CASCADE,
 response_id TEXT NOT NULL,
 child_session_id TEXT NOT NULL,
 run_generation INTEGER NOT NULL,
 PRIMARY KEY(turn_id,child_session_id)
);
CREATE TABLE IF NOT EXISTS notification_incorporated_agents (
 response_id TEXT NOT NULL REFERENCES serve_response_lifecycle(response_id) ON DELETE CASCADE,
 child_session_id TEXT NOT NULL,
 run_generation INTEGER NOT NULL,
 PRIMARY KEY(response_id,child_session_id)
);
CREATE TABLE IF NOT EXISTS notification_pending_calls (
 turn_id TEXT NOT NULL REFERENCES notification_turns(turn_id) ON DELETE CASCADE,
 response_id TEXT NOT NULL,
 call_id TEXT NOT NULL,
 PRIMARY KEY(turn_id,call_id)
);
CREATE TABLE IF NOT EXISTS notification_rush_origins (
 session_id TEXT NOT NULL,
 request_id TEXT NOT NULL,
 origin TEXT NOT NULL,
 PRIMARY KEY(session_id,request_id),
 FOREIGN KEY(session_id,request_id) REFERENCES session_rush_operations(session_id,request_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS notification_presence (
 window_id TEXT PRIMARY KEY,
 source TEXT NOT NULL,
 document_id TEXT NOT NULL,
 subscription_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 active INTEGER NOT NULL,
 expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notification_documents (
 document_id TEXT PRIMARY KEY,
 window_id TEXT NOT NULL,
 subscription_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 retired INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS notification_steering_origins (
 session_id TEXT NOT NULL,
 steering_id TEXT NOT NULL,
 origin TEXT NOT NULL,
 validated INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(session_id,steering_id),
 FOREIGN KEY(session_id,steering_id) REFERENCES session_pending_steering(session_id,id) ON DELETE CASCADE
);
CREATE TRIGGER IF NOT EXISTS notification_attention_seen AFTER UPDATE OF seen_through_seq ON session_attention BEGIN
 UPDATE completion_push_outbox SET status='dead',last_error='read',claim_token='',claim_owner='' WHERE session_id=NEW.session_id AND attention_seq<=NEW.seen_through_seq AND status='pending' AND turn_id!='';
END;
CREATE TRIGGER IF NOT EXISTS notification_subscription_deleted AFTER DELETE ON push_subscriptions BEGIN
 UPDATE completion_push_outbox SET status='dead',last_error='subscription deleted',claim_token='',claim_owner='' WHERE subscription_id=OLD.id AND status='pending';
 UPDATE notification_turns SET subscription_id='' WHERE subscription_id=OLD.id;
 DELETE FROM notification_presence WHERE subscription_id=OLD.id;
END;
CREATE TRIGGER IF NOT EXISTS notification_subscription_stale AFTER UPDATE OF status ON push_subscriptions WHEN NEW.status!='active' BEGIN
 UPDATE completion_push_outbox SET status='dead',last_error='subscription inactive',claim_token='',claim_owner='' WHERE subscription_id=NEW.id AND status='pending';
 UPDATE notification_turns SET subscription_id='' WHERE subscription_id=NEW.id;
 DELETE FROM notification_presence WHERE subscription_id=NEW.id;
END;
`

const OutboxColumnsV68 = `
 turn_id TEXT NOT NULL DEFAULT '',
 window_id TEXT NOT NULL DEFAULT '',
 document_id TEXT NOT NULL DEFAULT '',
 source TEXT NOT NULL DEFAULT '',
 store_instance_id TEXT NOT NULL DEFAULT '',
 session_id TEXT NOT NULL DEFAULT '',
 attention_seq INTEGER NOT NULL DEFAULT 0,
 run_epoch INTEGER NOT NULL DEFAULT 0,
 outcome TEXT NOT NULL DEFAULT '',
 expires_at INTEGER NOT NULL DEFAULT 0,
 claim_owner TEXT NOT NULL DEFAULT '',
 claim_token TEXT NOT NULL DEFAULT '',
 claim_expires_at INTEGER NOT NULL DEFAULT 0,
`

func MigrateV68(db sqliteutil.Executor) error {
	// These columns have one owner migration; SQLite rolls back every ALTER and
	// retirement if any later statement fails. Do not reconcile at startup.
	columns := []string{
		"turn_id TEXT NOT NULL DEFAULT ''", "window_id TEXT NOT NULL DEFAULT ''", "document_id TEXT NOT NULL DEFAULT ''",
		"source TEXT NOT NULL DEFAULT ''", "store_instance_id TEXT NOT NULL DEFAULT ''", "session_id TEXT NOT NULL DEFAULT ''",
		"attention_seq INTEGER NOT NULL DEFAULT 0", "run_epoch INTEGER NOT NULL DEFAULT 0", "outcome TEXT NOT NULL DEFAULT ''",
		"expires_at INTEGER NOT NULL DEFAULT 0", "claim_owner TEXT NOT NULL DEFAULT ''", "claim_token TEXT NOT NULL DEFAULT ''", "claim_expires_at INTEGER NOT NULL DEFAULT 0",
	}
	for _, column := range columns {
		exists, err := sqliteutil.ColumnExists(db, "completion_push_outbox", strings.Fields(column)[0])
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := db.Exec("ALTER TABLE completion_push_outbox ADD COLUMN " + column); err != nil {
			return fmt.Errorf("add notification candidate column: %w", err)
		}
	}
	if _, err := db.Exec(SchemaV68); err != nil {
		return fmt.Errorf("create notification ownership: %w", err)
	}
	_, err := db.Exec(`UPDATE completion_push_outbox SET status='dead',last_error='legacy automatic notification retired' WHERE status='pending'`)
	return err
}
