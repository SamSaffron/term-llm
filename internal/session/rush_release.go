package session

import "context"

// ReleaseRush returns a settled, unconsumed terminal operation to recoverable
// pending input. Payload stays in the ledger and only this owner's rows move.
func (s *SQLiteStore) ReleaseRush(ctx context.Context, op *RushOperation) error {
	if op == nil || op.Status.Active() || op.Status == RushStarted {
		return ErrSteeringConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status RushStatus
	var fence, revision int64
	if err := tx.QueryRowContext(ctx, `SELECT status,owner_fence,revision FROM session_rush_operations WHERE session_id=? AND request_id=?`, op.SessionID, op.RequestID).Scan(&status, &fence, &revision); err != nil {
		return err
	}
	if status != op.Status || fence != op.Fence || revision != op.Revision {
		return ErrSteeringConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE session_pending_steering SET owner_kind='',owner_id='',owner_fence=0 WHERE session_id=? AND owner_kind='rush' AND owner_id=? AND owner_fence=?`, op.SessionID, op.RequestID, op.Fence); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE session_rush_entries SET disposition='released' WHERE session_id=? AND request_id=? AND disposition='claimed'`, op.SessionID, op.RequestID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *LoggingStore) ReleaseRush(ctx context.Context, op *RushOperation) error {
	store, ok := AsRushStore(s.Store)
	if !ok {
		return ErrSteeringConflict
	}
	err := store.ReleaseRush(ctx, op)
	s.logOnce("ReleaseRush", err)
	return err
}
