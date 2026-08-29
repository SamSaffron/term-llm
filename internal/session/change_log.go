package session

import (
	"context"
	"database/sql"
	"fmt"
)

const maxStoreChangeBatch = 1024

// StoreChangeCursorError means the requested sequence has fallen behind the
// bounded durable tail and the consumer must perform authoritative recovery.
type StoreChangeCursorError struct {
	After  int64
	Oldest int64
	Latest int64
}

func (e *StoreChangeCursorError) Error() string {
	return fmt.Sprintf("store change cursor %d is outside retained range %d..%d", e.After, e.Oldest, e.Latest)
}

// StoreChangeCursor returns the newest durable change sequence. Watchers take
// this once at startup, then request only rows appended by later commits.
func (s *SQLiteStore) StoreChangeCursor(ctx context.Context) (int64, error) {
	var sequence int64
	if err := s.queryDB().QueryRowContext(ctx, `
		SELECT COALESCE(MAX(sequence), 0) FROM session_change_log`).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("read store change cursor: %w", err)
	}
	return sequence, nil
}

// ListStoreChanges reads the indexed tail of the durable change log. The
// sequence primary key makes an idle poll and a small incremental batch cheap
// regardless of the total number of sessions in the database.
func (s *SQLiteStore) ListStoreChanges(ctx context.Context, after int64, limit int) ([]StoreChange, error) {
	if after < 0 {
		after = 0
	}
	if limit <= 0 || limit > maxStoreChangeBatch {
		limit = maxStoreChangeBatch
	}
	tx, err := s.queryDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin store change read: %w", err)
	}
	defer tx.Rollback()
	var oldest, latest int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE((SELECT sequence FROM session_change_log ORDER BY sequence ASC LIMIT 1), 0),
			COALESCE((SELECT sequence FROM session_change_log ORDER BY sequence DESC LIMIT 1), 0)`).Scan(&oldest, &latest); err != nil {
		return nil, fmt.Errorf("read store change bounds: %w", err)
	}
	if (oldest > 0 && after+1 < oldest) || latest < after {
		return nil, &StoreChangeCursorError{After: after, Oldest: oldest, Latest: latest}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, kind, session_id, project_id, transcript_rev, status
		FROM session_change_log
		WHERE sequence > ?
		ORDER BY sequence
		LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list store changes after %d: %w", after, err)
	}
	defer rows.Close()
	changes := make([]StoreChange, 0)
	for rows.Next() {
		var change StoreChange
		if err := rows.Scan(
			&change.Sequence,
			&change.Kind,
			&change.SessionID,
			&change.ProjectID,
			&change.TranscriptRev,
			&change.Status,
		); err != nil {
			return nil, fmt.Errorf("scan store change: %w", err)
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate store changes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close store changes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit store change read: %w", err)
	}
	return changes, nil
}
