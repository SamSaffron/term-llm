package notificationdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const notificationCandidateColumns = `id,event_id,turn_id,response_id,subscription_id,window_id,document_id,source,store_instance_id,session_id,attention_seq,run_epoch,outcome,created_at,expires_at,attempt_count,claim_owner,claim_token,claim_expires_at`

func scanCompletionNotification(q interface{ Scan(...any) error }) (CompletionNotification, error) {
	var n CompletionNotification
	var created string
	var expires, lease int64
	err := q.Scan(&n.ID, &n.EventID, &n.TurnID, &n.ResponseID, &n.SubscriptionID, &n.WindowID, &n.DocumentID, &n.Source, &n.StoreInstanceID, &n.SessionID, &n.AttentionSeq, &n.RunEpoch, &n.Outcome, &created, &expires, &n.AttemptCount, &n.ClaimOwner, &n.ClaimToken, &lease)
	n.CreatedAt, err = parseCandidateTime(created, err)
	n.ExpiresAt = time.UnixMilli(expires).UTC()
	n.LeaseExpiresAt = time.UnixMilli(lease).UTC()
	return n, err
}

// retireInvalidNotificationCandidatesTx intentionally preserves read history;
// reverting an ACK to unseen must not resurrect an already retired OS alert.
func retireInvalidNotificationCandidatesTx(ctx context.Context, q QueryExecer, now time.Time) error {
	_, err := q.ExecContext(ctx, `UPDATE completion_push_outbox SET status='dead',last_error='expired, read, superseded or opted out',claim_owner='',claim_token=''
 WHERE status='pending' AND turn_id!='' AND (expires_at<=? OR NOT EXISTS(SELECT 1 FROM metadata WHERE key='store_instance_id' AND value=completion_push_outbox.store_instance_id) OR NOT EXISTS (
 SELECT 1 FROM notification_turns t JOIN push_subscriptions s ON s.id=t.subscription_id
 JOIN session_attention a ON a.session_id=t.session_id
 WHERE t.turn_id=completion_push_outbox.turn_id AND t.state='settled' AND s.status='active'
 AND t.subscription_id=completion_push_outbox.subscription_id AND a.seen_through_seq<completion_push_outbox.attention_seq))`, now.UnixMilli())
	return err
}

func ClaimCompletionNotifications(ctx context.Context, db *sql.DB, owner string, now time.Time, lease time.Duration, limit int) ([]CompletionNotification, error) {
	if owner == "" {
		return nil, errors.New("session: notification claim owner required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if lease <= 0 || lease > time.Minute {
		lease = 30 * time.Second
	}
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// First mutation serializes competing claimers, including across processes.
	if err = retireInvalidNotificationCandidatesTx(ctx, tx, now); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM completion_push_outbox o WHERE status='pending' AND turn_id!='' AND next_attempt_at<=? AND claim_expires_at<=?
 AND NOT EXISTS(SELECT 1 FROM notification_presence p WHERE p.window_id=o.window_id AND p.subscription_id=o.subscription_id AND p.active=1 AND p.expires_at>?)
 ORDER BY next_attempt_at,id LIMIT ?`, now.UTC().Format("2006-01-02 15:04:05"), now.UnixMilli(), now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var items []CompletionNotification
	for _, id := range ids {
		token := NewID()
		row := tx.QueryRowContext(ctx, `UPDATE completion_push_outbox SET claim_owner=?,claim_token=?,claim_expires_at=? WHERE id=? AND status='pending' AND claim_expires_at<=? RETURNING `+notificationCandidateColumns, owner, token, now.Add(lease).UnixMilli(), id, now.UnixMilli())
		item, err := scanCompletionNotification(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit notification claims: %w", err)
	}
	return items, nil
}

func RecheckCompletionNotification(ctx context.Context, db *sql.DB, id int64, owner, token string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Check the fence before retirement so an old worker cannot mutate a claim.
	var valid int
	err = tx.QueryRowContext(ctx, `UPDATE completion_push_outbox SET updated_at=updated_at WHERE id=? AND claim_owner=? AND claim_token=? AND claim_token!='' AND status='pending' AND claim_expires_at>? RETURNING 1`, id, owner, token, now.UnixMilli()).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotificationClaimLost
	}
	if err != nil {
		return false, err
	}
	if err = retireInvalidNotificationCandidatesTx(ctx, tx, now); err != nil {
		return false, err
	}
	var ready bool
	err = tx.QueryRowContext(ctx, `SELECT status='pending' AND NOT EXISTS(SELECT 1 FROM notification_presence p WHERE p.window_id=o.window_id AND p.subscription_id=o.subscription_id AND p.active=1 AND p.expires_at>?) FROM completion_push_outbox o WHERE id=?`, now.UnixMilli(), id).Scan(&ready)
	if err != nil {
		return false, err
	}
	if !ready {
		// An active owner defers. It is not a read acknowledgement and does not kill
		// this candidate. Clear the claim so later dispatch can retry after absence.
		_, err = tx.ExecContext(ctx, `UPDATE completion_push_outbox SET claim_owner='',claim_token='',claim_expires_at=0 WHERE id=?`, id)
		if err != nil {
			return false, err
		}
	}
	return ready, tx.Commit()
}

func CompleteCompletionNotification(ctx context.Context, db *sql.DB, id int64, owner, token, status string, next time.Time, detail string) error {
	if status != "delivered" && status != "retry" && status != "dead" && status != "defer" {
		return errors.New("session: invalid notification result")
	}
	if len(detail) > 240 {
		detail = detail[:240]
	}
	if next.IsZero() {
		next = time.Now().UTC()
	}
	storedStatus := status
	attempt := 1
	if status == "defer" {
		attempt = 0
	}
	if status == "retry" || status == "defer" {
		storedStatus = "pending"
	}
	result, err := db.ExecContext(ctx, `UPDATE completion_push_outbox SET status=?,next_attempt_at=?,attempt_count=attempt_count+?,last_error=?,claim_owner='',claim_token='',claim_expires_at=0,updated_at=datetime('now')
 WHERE id=? AND status='pending' AND claim_owner=? AND claim_token=? AND claim_token!='' AND claim_expires_at>CAST((julianday('now') - 2440587.5)*86400000 AS INTEGER)`, storedStatus, next.UTC().Format("2006-01-02 15:04:05"), attempt, detail, id, owner, token)
	if err != nil {
		return fmt.Errorf("record notification result: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrNotificationClaimLost
	}
	return nil
}

func parseCandidateTime(value string, scanErr error) (time.Time, error) {
	if scanErr != nil {
		return time.Time{}, scanErr
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errors.New("session: invalid notification candidate timestamp")
}
