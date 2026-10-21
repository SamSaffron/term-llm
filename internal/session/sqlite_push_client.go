package session

import "github.com/samsaffron/term-llm/internal/sqliteutil"

// Read-only stores skip migrations, so pre-v67 rows are browser enrollments.
func (s *SQLiteStore) probePushClientKind() {
	s.hasPushClientKind, _ = sqliteutil.ColumnExists(s.db, "push_subscriptions", "client_kind")
}

func (s *SQLiteStore) pushSubscriptionColumns() string {
	kind := "'browser'"
	if s.hasPushClientKind {
		kind = "client_kind"
	}
	return `id, endpoint, key_p256dh, key_auth, status, vapid_key_id, updated_at,
		last_used_at, last_failure_code, last_failure, last_failure_at, ` + kind
}
