package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *SQLiteStore) FinalizeResponseRun(ctx context.Context, terminal ResponseRunTerminal) (AttentionState, error) {
	if terminal.Outcome != ResponseRunCompleted && terminal.Outcome != ResponseRunFailed && terminal.Outcome != ResponseRunCancelled {
		return AttentionState{}, errors.New("session: invalid response run terminal outcome")
	}
	now := terminal.EndedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	storeID, err := s.StoreInstanceID(ctx)
	if err != nil {
		return AttentionState{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AttentionState{}, fmt.Errorf("begin response run finalization: %w", err)
	}
	defer tx.Rollback()
	var sessionID, state, owner string
	var token, runEpoch, startedRev, storedFinalRev, lifecycleAttentionSeq int64
	var storedCount int
	err = tx.QueryRowContext(ctx, `SELECT session_id, state, owner_instance_id, fencing_token, run_epoch,
		started_rev, final_rev, durable_output_count, attention_seq FROM serve_response_lifecycle WHERE response_id = ?`, terminal.ResponseID).
		Scan(&sessionID, &state, &owner, &token, &runEpoch, &startedRev, &storedFinalRev, &storedCount, &lifecycleAttentionSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return AttentionState{}, ErrNotFound
	}
	if err != nil {
		return AttentionState{}, fmt.Errorf("read response run lifecycle: %w", err)
	}
	if state != string(ResponseRunRunning) || owner != terminal.OwnerInstanceID || token != terminal.FencingToken {
		if state != string(ResponseRunRunning) {
			if state != string(terminal.Outcome) {
				return AttentionState{}, ErrResponseRunLeaseLost
			}
			attention, attentionErr := getAttentionTx(ctx, tx, sessionID)
			if errors.Is(attentionErr, sql.ErrNoRows) {
				return AttentionState{StoreInstanceID: storeID, SessionID: sessionID}, nil
			}
			if attentionErr == nil && (attention.ResponseID != terminal.ResponseID || attention.LatestAttentionSeq != lifecycleAttentionSeq) {
				return AttentionState{StoreInstanceID: storeID, SessionID: sessionID}, nil
			}
			attention.StoreInstanceID = storeID
			return attention, attentionErr
		}
		return AttentionState{}, ErrResponseRunLeaseLost
	}
	finalRev := max(storedFinalRev, terminal.FinalRev)
	outputCount := max(storedCount, terminal.DurableOutputCount)
	result, err := tx.ExecContext(ctx, `UPDATE serve_response_lifecycle SET state = ?, final_rev = ?,
		durable_output_count = ?, ended_at = ?, updated_at = ?, interaction_required_count = 0,
		interaction_required_kinds = '', interaction_required_since = NULL
		WHERE response_id = ? AND state = 'running' AND owner_instance_id = ? AND fencing_token = ?`,
		terminal.Outcome, finalRev, outputCount, unixMilli(now), unixMilli(now), terminal.ResponseID,
		terminal.OwnerInstanceID, terminal.FencingToken)
	if err != nil {
		return AttentionState{}, fmt.Errorf("finalize response run: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return AttentionState{}, ErrResponseRunLeaseLost
	}
	if err := insertLifecycleChange(ctx, tx, sessionID, finalRev); err != nil {
		return AttentionState{}, fmt.Errorf("publish response run terminal state: %w", err)
	}
	// Generation ACK and candidate commit are one durable decision.
	if terminal.Outcome == ResponseRunCompleted {
		if _, err := tx.ExecContext(ctx, `UPDATE session_agent_runs SET notified_at=COALESCE(notified_at,?) WHERE EXISTS
 (SELECT 1 FROM notification_incorporated_agents i WHERE i.response_id=? AND i.child_session_id=session_agent_runs.child_session_id AND i.run_generation=session_agent_runs.run_generation)`, now, terminal.ResponseID); err != nil {
			return AttentionState{}, fmt.Errorf("acknowledge incorporated agent generations: %w", err)
		}
	}
	intermediate, err := prepareNotificationTerminalTx(ctx, tx, terminal)
	if err != nil {
		return AttentionState{}, fmt.Errorf("settle notification turn: %w", err)
	}
	attention := AttentionState{SessionID: sessionID}
	if !intermediate && terminalNeedsAttention(terminal.Outcome, startedRev, finalRev, outputCount) {
		seq, err := insertAttentionChange(ctx, tx, sessionID, finalRev)
		if err != nil {
			return AttentionState{}, fmt.Errorf("allocate attention marker: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_attention(session_id, latest_attention_seq, response_id,
			run_epoch, outcome, started_rev, final_rev, terminal_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET latest_attention_seq = excluded.latest_attention_seq,
			response_id = excluded.response_id, run_epoch = excluded.run_epoch, outcome = excluded.outcome,
			started_rev = excluded.started_rev, final_rev = excluded.final_rev,
			terminal_at = excluded.terminal_at, updated_at = excluded.updated_at
			WHERE excluded.latest_attention_seq > session_attention.latest_attention_seq`, sessionID, seq,
			terminal.ResponseID, runEpoch, terminal.Outcome, startedRev, finalRev, unixMilli(now), unixMilli(now)); err != nil {
			return AttentionState{}, fmt.Errorf("persist attention marker: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE serve_response_lifecycle SET attention_seq = ? WHERE response_id = ?`, seq, terminal.ResponseID); err != nil {
			return AttentionState{}, fmt.Errorf("link attention marker: %w", err)
		}
		attention, err = getAttentionTx(ctx, tx, sessionID)
		if err != nil {
			return AttentionState{}, fmt.Errorf("read finalized attention: %w", err)
		}
		attention.Changed = true
	}
	attention.StoreInstanceID = storeID
	if err := insertNotificationCandidateTx(ctx, tx, terminal, attention, now); err != nil {
		return AttentionState{}, err
	}
	if err := tx.Commit(); err != nil {
		return AttentionState{}, fmt.Errorf("commit response run finalization: %w", err)
	}
	return attention, nil
}
