package notificationdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// UpdateNotificationPresence records authenticated attendance evidence. It never
// changes attention watermarks. A conflicting live document must rotate its
// window identity rather than stealing a sibling's completion destination.
func UpdateNotificationPresence(ctx context.Context, db *sql.DB, p NotificationPresence) error {
	now := time.Now().UTC()
	u, sourceErr := url.ParseRequestURI(p.Source)
	if sourceErr != nil || len(p.Source) > 256 || !strings.HasPrefix(p.Source, "/") || strings.HasPrefix(p.Source, "//") || strings.ContainsAny(p.Source, "?#") || u.RawQuery != "" || u.Fragment != "" || u.Path != path.Clean(u.Path) || strings.ContainsAny(u.Path, "\\\x00") {
		return errors.New("session: invalid notification source")
	}

	if p.Version != 2 || p.WindowID == "" || p.DocumentID == "" || p.SubscriptionID == "" || p.Sequence <= 0 || !p.ExpiresAt.After(now) || p.ExpiresAt.After(now.Add(65*time.Second)) {
		return errors.New("session: invalid notification presence lease")
	}
	if len(p.WindowID) > 256 || len(p.DocumentID) > 256 || len(p.SubscriptionID) > 256 || len(p.PreviousDocumentID) > 256 {
		return errors.New("session: notification identity too long")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire SQLite's write lock before reading the current incarnation.
	result, err := tx.ExecContext(ctx, `UPDATE push_subscriptions SET updated_at=updated_at WHERE id=? AND status='active'`, p.SubscriptionID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return sql.ErrNoRows
	}
	var window, subscription string
	var seq int64
	var retired bool
	err = tx.QueryRowContext(ctx, `SELECT window_id,subscription_id,sequence,retired FROM notification_documents WHERE document_id=?`, p.DocumentID).Scan(&window, &subscription, &seq, &retired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (retired || window != p.WindowID || seq >= p.Sequence) {
		return ErrNotificationPresenceConflict
	}
	// Explicit re-enrollment can reuse this live document after opt-out. It
	// cannot take a document still enrolled under another active subscription,
	// nor revive an incarnation retired by a newer document.
	if err == nil && subscription != p.SubscriptionID {
		var live int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM push_subscriptions WHERE id=? AND status='active'`, subscription).Scan(&live)
		if err == nil {
			return ErrNotificationPresenceConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	var doc string
	var active bool
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT document_id,active,expires_at FROM notification_presence WHERE window_id=?`, p.WindowID).Scan(&doc, &active, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && doc != p.DocumentID {
		if p.PreviousDocumentID != "" && p.PreviousDocumentID != doc {
			return ErrNotificationPresenceConflict
		}
		if active && expires > now.UnixMilli() && p.PreviousDocumentID != doc {
			return ErrNotificationPresenceConflict
		}
		if _, err = tx.ExecContext(ctx, `UPDATE notification_documents SET retired=1 WHERE document_id=?`, doc); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO notification_documents(document_id,window_id,subscription_id,sequence) VALUES(?,?,?,?)
 ON CONFLICT(document_id) DO UPDATE SET sequence=excluded.sequence,subscription_id=excluded.subscription_id`, p.DocumentID, p.WindowID, p.SubscriptionID, p.Sequence); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_presence(window_id,document_id,subscription_id,sequence,active,expires_at,source) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(window_id) DO UPDATE SET document_id=excluded.document_id,subscription_id=excluded.subscription_id,sequence=excluded.sequence,active=excluded.active,expires_at=excluded.expires_at,source=excluded.source`, p.WindowID, p.DocumentID, p.SubscriptionID, p.Sequence, p.Active, p.ExpiresAt.UnixMilli(), p.Source)
	if err != nil {
		return fmt.Errorf("persist notification presence: %w", err)
	}
	return tx.Commit()
}
