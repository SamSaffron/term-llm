package notificationdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func ValidOriginTx(ctx context.Context, q QueryExecer, origin NotificationOrigin) (NotificationOrigin, error) {
	if origin.SubscriptionID == "" || origin.WindowID == "" || origin.DocumentID == "" || origin.Source == "" {
		return NotificationOrigin{}, nil
	}
	var valid int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM notification_presence p JOIN push_subscriptions s ON s.id=p.subscription_id
 WHERE p.window_id=? AND p.document_id=? AND p.subscription_id=? AND p.source=? AND p.expires_at>? AND s.status='active'`, origin.WindowID, origin.DocumentID, origin.SubscriptionID, origin.Source, time.Now().UnixMilli()).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationOrigin{}, nil
	}
	return origin, err
}

func SupersedeTx(ctx context.Context, q QueryExecer, sessionID string) error {
	if _, err := q.ExecContext(ctx, `UPDATE completion_push_outbox SET status='dead',last_error='human superseded',claim_owner='',claim_token='' WHERE session_id=? AND status='pending'`, sessionID); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `UPDATE notification_turns SET state='superseded' WHERE session_id=? AND state!='superseded'`, sessionID)
	return err
}

func NewTurnTx(ctx context.Context, q QueryExecer, sessionID, responseID string, origin NotificationOrigin) error {
	origin, err := ValidOriginTx(ctx, q, origin)
	if err != nil {
		return err
	}
	return capturedTurnTx(ctx, q, sessionID, responseID, origin)
}

// A queued human origin is validated when accepted, not resampled at commit.
// Its attendance lease may naturally expire while the provider incorporates it.
func capturedTurnTx(ctx context.Context, q QueryExecer, sessionID, responseID string, origin NotificationOrigin) error {
	if origin.SubscriptionID != "" {
		var active int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM push_subscriptions WHERE id=? AND status='active'`, origin.SubscriptionID).Scan(&active)
		if errors.Is(err, sql.ErrNoRows) {
			origin = NotificationOrigin{}
		} else if err != nil {
			return err
		}
	}
	var err error
	if err := SupersedeTx(ctx, q, sessionID); err != nil {
		return err
	}
	turnID := NewID()
	if _, err = q.ExecContext(ctx, `INSERT INTO notification_turns(turn_id,session_id,current_response_id,subscription_id,window_id,document_id,source,created_at) VALUES(?,?,?,?,?,?,?,?)`, turnID, sessionID, responseID, origin.SubscriptionID, origin.WindowID, origin.DocumentID, origin.Source, time.Now().UnixMilli()); err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO notification_response_turns(response_id,turn_id) VALUES(?,?) ON CONFLICT(response_id) DO UPDATE SET turn_id=excluded.turn_id`, responseID, turnID)
	return err
}

func AdmitTx(ctx context.Context, tx *sql.Tx, a ResponseRunAdmission) error {
	if err := recordIncorporatedNotificationAgentsTx(ctx, tx, a); err != nil {
		return err
	}
	if a.Turn != nil && a.Turn.Human {
		return NewTurnTx(ctx, tx, a.SessionID, a.ResponseID, a.Turn.Origin)
	}
	previous := ""
	events := a.AgentEvents
	if a.Turn != nil {
		previous = a.Turn.PreviousResponseID
		if len(a.Turn.AwaitedEvents) > 0 {
			events = a.Turn.AwaitedEvents
		}
	}
	var turnID string
	if previous != "" && len(events) == 0 {
		err := tx.QueryRowContext(ctx, `SELECT t.turn_id FROM notification_turns t JOIN notification_response_turns r ON r.turn_id=t.turn_id JOIN serve_response_lifecycle l ON l.response_id=r.response_id
   WHERE r.response_id=? AND t.session_id=? AND t.current_response_id=? AND t.state='waiting_client' AND l.state='completed'`, previous, a.SessionID, previous).Scan(&turnID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if turnID == "" && len(events) > 0 {
		var err error
		turnID, err = awaitedNotificationTurnTx(ctx, tx, a.SessionID, "", events)
		if err != nil {
			return err
		}
	}
	if turnID != "" && len(events) == 0 {
		if err := validateClientResultsTx(ctx, tx, turnID, a.Turn.ClientResultIDs); err != nil {
			return err
		}
	}
	if turnID == "" {
		return nil
	}
	// Optional turn-only callers may supply joined generations without the
	// broader host-wake ledger. Persist only proven awaited terminal members;
	// arbitrary or still-running records never become ACK authority.
	if len(a.AgentEvents) == 0 {
		for _, event := range events {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notification_incorporated_agents(response_id,child_session_id,run_generation)
    SELECT ?,w.child_session_id,w.run_generation FROM notification_awaited_agents w JOIN session_agent_runs c
    ON c.child_session_id=w.child_session_id AND c.run_generation=w.run_generation
    WHERE w.turn_id=? AND w.child_session_id=? AND w.run_generation=? AND c.run_status IN ('completed','turn_limit','cancelled','interrupted','failed')
    AND c.collected_at IS NULL AND c.notified_at IS NULL AND c.wake_suppressed=0 AND c.stop_reason!='parent_stopped' ON CONFLICT DO NOTHING`, a.ResponseID, turnID, event.ID, event.RunGeneration); err != nil {
				return err
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE notification_turns SET current_response_id=?,state='running' WHERE turn_id=? AND state IN ('waiting_client','waiting_join')`, a.ResponseID, turnID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrNotificationLineage
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO notification_response_turns(response_id,turn_id) VALUES(?,?)`, a.ResponseID, turnID); err != nil {
		return err
	}
	// Remaining awaited generations follow the durable response chain.
	if _, err = tx.ExecContext(ctx, `UPDATE notification_awaited_agents SET response_id=? WHERE turn_id=?`, a.ResponseID, turnID); err != nil {
		return err
	}

	return nil
}

func awaitedNotificationTurnTx(ctx context.Context, tx *sql.Tx, sessionID, previous string, events []AgentRun) (string, error) {
	var turnID string
	for _, event := range events {
		var candidate string
		err := tx.QueryRowContext(ctx, `SELECT t.turn_id FROM notification_turns t JOIN notification_awaited_agents w ON w.turn_id=t.turn_id
   JOIN session_agent_runs a ON a.child_session_id=w.child_session_id AND a.run_generation=w.run_generation
   JOIN notification_response_turns r ON r.response_id=a.parent_response_id AND r.turn_id=t.turn_id
   WHERE t.session_id=? AND t.state='waiting_join' AND w.response_id=t.current_response_id AND (?='' OR t.current_response_id=?)
   AND w.child_session_id=? AND w.run_generation=? AND a.parent_session_id=t.session_id
   AND a.notified_at IS NULL AND a.collected_at IS NULL AND a.wake_suppressed=0 AND a.stop_reason!='parent_stopped'
   AND a.run_status IN ('completed','turn_limit','cancelled','interrupted','failed')`, sessionID, previous, previous, event.ID, event.RunGeneration).Scan(&candidate)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		if turnID != "" && turnID != candidate {
			return "", ErrNotificationLineage
		}
		turnID = candidate
	}
	return turnID, nil
}

func recordIncorporatedNotificationAgentsTx(ctx context.Context, tx *sql.Tx, a ResponseRunAdmission) error {
	for _, event := range a.AgentEvents {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_incorporated_agents(response_id,child_session_id,run_generation) VALUES(?,?,?) ON CONFLICT DO NOTHING`, a.ResponseID, event.ID, event.RunGeneration); err != nil {
			return err
		}
	}
	return nil
}

func validateClientResultsTx(ctx context.Context, tx *sql.Tx, turnID string, ids []string) error {
	if len(ids) == 0 {
		return ErrNotificationLineage
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			return ErrNotificationLineage
		}
		seen[id] = true
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM notification_pending_calls WHERE turn_id=? AND call_id=?`, turnID, id).Scan(&found); errors.Is(err, sql.ErrNoRows) {
			return ErrNotificationLineage
		} else if err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_pending_calls WHERE turn_id=?`, turnID).Scan(&count); err != nil {
		return err
	}
	if count != len(ids) {
		return ErrNotificationLineage
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM notification_pending_calls WHERE turn_id=?`, turnID)
	return err
}

// CancelTurn is response-scoped: a stale Stop cannot cancel a later human turn.
func CancelTurn(ctx context.Context, db *sql.DB, responseID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var linked int
	if err = tx.QueryRowContext(ctx, `UPDATE notification_response_turns SET response_id=response_id WHERE response_id=? RETURNING 1`, responseID).Scan(&linked); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE notification_turns SET state='cancelled' WHERE current_response_id=? AND state!='superseded' AND turn_id IN(SELECT turn_id FROM notification_response_turns WHERE response_id=?)`, responseID, responseID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE completion_push_outbox SET status='dead',last_error='cancelled',claim_owner='',claim_token='' WHERE status='pending' AND turn_id IN(SELECT turn_id FROM notification_turns WHERE current_response_id=? AND state='cancelled')`, responseID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func TransferTurn(ctx context.Context, db *sql.DB, responseID, owner string, token int64, origin NotificationOrigin) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID string
	// UPDATE acquires the writer lock before inspecting/retiring ownership.
	err = tx.QueryRowContext(ctx, `UPDATE serve_response_lifecycle SET updated_at=updated_at WHERE response_id=? AND owner_instance_id=? AND fencing_token=? AND state='running' AND lease_expires_at>? RETURNING session_id`, responseID, owner, token, time.Now().UnixMilli()).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if err = NewTurnTx(ctx, tx, sessionID, responseID, origin); err != nil {
		return fmt.Errorf("transfer human notification origin: %w", err)
	}
	return tx.Commit()
}
