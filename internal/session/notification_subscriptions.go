package session

import (
	"context"
	"fmt"
	"strings"
)

// DeletePushSubscription removes a Web Push subscription by endpoint.
func (s *SQLiteStore) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	if err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeletePushSubscriptionByID(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE id = ?", strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("delete push subscription by id: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkPushSubscriptionStale(ctx context.Context, id, code, detail string) error {
	if len(detail) > 240 {
		detail = detail[:240]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions
		SET status = 'stale', updated_at = datetime('now'), last_failure_code = ?,
		    last_failure = ?, last_failure_at = datetime('now') WHERE id = ?`, code, detail, id)
	return err
}

func (s *SQLiteStore) MarkPushSubscriptionUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions
		SET updated_at = datetime('now'), last_used_at = datetime('now'),
		    last_failure_code = '', last_failure = '', last_failure_at = NULL WHERE id = ? AND status = 'active'`, id)
	return err
}
