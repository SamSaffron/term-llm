package session

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

// schemaVersion is the current schema version.
// - Fresh databases get the full schema from `schema` const and start at this version
// - Existing databases run migrations to reach this version
// Increment when adding new migrations.
const (
	projectSchemaVersion = 47
	schemaVersion        = 68
)

// migration represents a schema migration.
type migration struct {
	version     int
	description string
	up          func(db schemaExecutor) error
}

type schemaExecutor = sqliteutil.Executor

func migratePushClientKindV67(db schemaExecutor) error {
	exists, err := sqliteutil.ColumnExists(db, "push_subscriptions", "client_kind")
	if err != nil {
		return fmt.Errorf("inspect push_subscriptions.client_kind: %w", err)
	}
	if !exists {
		if _, err := db.Exec(`ALTER TABLE push_subscriptions ADD COLUMN client_kind TEXT NOT NULL DEFAULT 'browser' CHECK (client_kind IN ('browser', 'app'))`); err != nil {
			return fmt.Errorf("add push_subscriptions.client_kind: %w", err)
		}
	}
	return nil
}
