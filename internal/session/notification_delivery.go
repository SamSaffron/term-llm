package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/samsaffron/term-llm/internal/session/notificationdb"
)

func (s *SQLiteStore) UpdateNotificationPresence(ctx context.Context, p NotificationPresence) error {
	err := notificationdb.UpdateNotificationPresence(ctx, s.db, p)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
func (s *SQLiteStore) ClaimCompletionNotifications(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]CompletionNotification, error) {
	return notificationdb.ClaimCompletionNotifications(ctx, s.db, owner, now, lease, limit)
}
func (s *SQLiteStore) RecheckCompletionNotification(ctx context.Context, id int64, owner, token string, now time.Time) (bool, error) {
	return notificationdb.RecheckCompletionNotification(ctx, s.db, id, owner, token, now)
}
func (s *SQLiteStore) CompleteCompletionNotification(ctx context.Context, id int64, owner, token, status string, next time.Time, detail string) error {
	return notificationdb.CompleteCompletionNotification(ctx, s.db, id, owner, token, status, next, detail)
}
