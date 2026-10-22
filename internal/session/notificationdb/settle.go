package notificationdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// prepareNotificationTerminalTx computes logical completion from durable lineage,
// never from a protocol terminal alone. Invalid wait snapshots grant no authority.
func PrepareTerminalTx(ctx context.Context, tx *sql.Tx, t ResponseRunTerminal) (bool, error) {
	var turnID, sessionID string
	err := tx.QueryRowContext(ctx, `SELECT n.turn_id,n.session_id FROM notification_turns n JOIN notification_response_turns r ON r.turn_id=n.turn_id WHERE r.response_id=? AND n.current_response_id=? AND n.state='running'`, t.ResponseID, t.ResponseID).Scan(&turnID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return t.PendingClientCalls && t.Outcome == ResponseRunCompleted, nil
	}
	if err != nil {
		return false, err
	}
	if t.Outcome == ResponseRunFailed && t.Recoverable {
		_, err = tx.ExecContext(ctx, `UPDATE notification_turns SET state='waiting_join' WHERE turn_id=?`, turnID)
		return true, err
	}
	if t.Outcome != ResponseRunCompleted {
		state := "settled"
		if t.Outcome == ResponseRunCancelled {
			state = "cancelled"
		}
		_, err = tx.ExecContext(ctx, `UPDATE notification_turns SET state=? WHERE turn_id=?`, state, turnID)
		return false, err
	}
	if t.AwaitingAgentJoin {
		for _, event := range t.AwaitedEvents {
			// Direct parentage must resolve to this exact logical turn, even if the
			// child was spawned on an earlier client-tool hop. ID+generation are exact.
			_, err = tx.ExecContext(ctx, `INSERT INTO notification_awaited_agents(turn_id,response_id,child_session_id,run_generation)
    SELECT ?,?,a.child_session_id,a.run_generation FROM session_agent_runs a
    JOIN notification_response_turns r ON r.response_id=a.parent_response_id AND r.turn_id=?
    WHERE a.child_session_id=? AND a.run_generation=? AND a.parent_session_id=?
    AND a.collected_at IS NULL AND a.notified_at IS NULL AND a.wake_suppressed=0 AND a.stop_reason!='parent_stopped'
    ON CONFLICT(turn_id,child_session_id) DO UPDATE SET response_id=excluded.response_id,run_generation=excluded.run_generation`, turnID, t.ResponseID, turnID, event.ID, event.RunGeneration, sessionID)
			if err != nil {
				return false, err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM notification_awaited_agents WHERE turn_id=? AND NOT EXISTS
  (SELECT 1 FROM session_agent_runs a WHERE a.child_session_id=notification_awaited_agents.child_session_id
   AND a.run_generation=notification_awaited_agents.run_generation AND a.collected_at IS NULL AND a.notified_at IS NULL
   AND a.wake_suppressed=0 AND a.stop_reason!='parent_stopped')`, turnID); err != nil {
		return false, err
	}
	var waiting int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_awaited_agents WHERE turn_id=?`, turnID).Scan(&waiting); err != nil {
		return false, err
	}
	state := "settled"
	if waiting > 0 {
		state = "waiting_join"
	}
	if t.PendingClientCalls {
		for _, id := range t.PendingClientCallIDs {
			if id == "" {
				continue
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO notification_pending_calls(turn_id,response_id,call_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, turnID, t.ResponseID, id); err != nil {
				return false, err
			}
		}
		state = "waiting_client"
	}
	_, err = tx.ExecContext(ctx, `UPDATE notification_turns SET state=? WHERE turn_id=?`, state, turnID)
	return state != "settled", err
}

func InsertCandidateTx(ctx context.Context, tx *sql.Tx, t ResponseRunTerminal, a AttentionState, now time.Time) error {
	if !a.Changed || t.Outcome == ResponseRunCancelled {
		return nil
	}
	// Target captured at admission/committed steering, not resampled from an open
	// tab. Delivery rechecks subscription, presence, read and supersession.
	_, err := tx.ExecContext(ctx, `INSERT INTO completion_push_outbox
  (event_id,response_id,subscription_id,payload,turn_id,window_id,document_id,source,store_instance_id,session_id,attention_seq,run_epoch,outcome,expires_at,next_attempt_at,created_at)
  SELECT 'completion:'||n.turn_id||':'||n.subscription_id,?,n.subscription_id,X'',n.turn_id,n.window_id,n.document_id,n.source,?,n.session_id,?,?,?, ?,?,?
  FROM notification_turns n JOIN notification_response_turns r ON r.turn_id=n.turn_id
  JOIN push_subscriptions s ON s.id=n.subscription_id AND s.status='active'
  WHERE r.response_id=? AND n.current_response_id=? AND n.state='settled' AND n.subscription_id!=''
  ON CONFLICT(event_id) DO NOTHING`, t.ResponseID, a.StoreInstanceID, a.LatestAttentionSeq, a.RunEpoch, t.Outcome, now.Add(24*time.Hour).UnixMilli(), now.Add(3*time.Second).Format("2006-01-02 15:04:05"), now.Truncate(time.Millisecond).Format(time.RFC3339Nano), t.ResponseID, t.ResponseID)
	if err != nil {
		return fmt.Errorf("persist completion notification candidate: %w", err)
	}
	return nil
}
