package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ExecHandoff is an explicit, one-shot hot-reload intent, not crash recovery.
// Request contains orchestration options, never an executable stack or pending
// tool calls. The source remains leased until exec; failed exec can discard the
// intent without cancelling or fencing the parked invocation.
type ExecHandoff struct {
	ID, ServiceID, SourceOwnerID, SessionID, SourceResponseID string
	SourceFence, CheckpointRev                                int64
	Request                                                   []byte
}

var ErrExecHandoffConflict = errors.New("session: stale or consumed self-exec handoff")

type ExecHandoffStore interface {
	PrepareExecHandoff(context.Context, []ExecHandoff) error
	ReadExecHandoff(context.Context, string, string) ([]ExecHandoff, error)
	DiscardExecHandoff(context.Context, string, string) error
	ExecContinuation(context.Context, string, string) (string, error)
}

func AsExecHandoffStore(store Store) (ExecHandoffStore, bool) {
	if logging, ok := store.(*LoggingStore); ok {
		return AsExecHandoffStore(logging.Store)
	}
	if sqlite, ok := store.(*SQLiteStore); ok && (sqlite.cfg.ReadOnly || strings.TrimSpace(sqlite.cfg.Path) == ":memory:") {
		return nil, false
	}
	result, ok := store.(ExecHandoffStore)
	return result, ok
}

const execHandoffSchemaV58 = `
CREATE TABLE IF NOT EXISTS serve_exec_handoffs (
 restart_id TEXT NOT NULL,
 service_id TEXT NOT NULL,
 source_owner_id TEXT NOT NULL,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 source_response_id TEXT NOT NULL UNIQUE,
 source_fence INTEGER NOT NULL,
 checkpoint_rev INTEGER NOT NULL,
 request BLOB NOT NULL,
 consumed INTEGER NOT NULL DEFAULT 0,
 replacement_response_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 PRIMARY KEY(restart_id, session_id)
);
`

func (s *SQLiteStore) PrepareExecHandoff(ctx context.Context, entries []ExecHandoff) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire the writer lock before validating any transcript/lease.
	if _, err = tx.ExecContext(ctx, `UPDATE metadata SET value=value WHERE key='serve_response_fencing_token'`); err != nil {
		return err
	}
	for _, h := range entries {
		first := entries[0]
		if h.ID != first.ID || h.ServiceID != first.ServiceID || h.SourceOwnerID != first.SourceOwnerID || !json.Valid(h.Request) {
			return ErrExecHandoffConflict
		}
		if h.ID == "" || h.ServiceID == "" || h.SourceOwnerID == "" {
			return ErrExecHandoffConflict
		}
		if err = validateExecSource(ctx, tx, h); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO serve_exec_handoffs(restart_id,service_id,source_owner_id,session_id,source_response_id,source_fence,checkpoint_rev,request,created_at) VALUES(?,?,?,?,?,?,?,?,CAST((julianday('now')-2440587.5)*86400000 AS INTEGER))`, h.ID, h.ServiceID, h.SourceOwnerID, h.SessionID, h.SourceResponseID, h.SourceFence, h.CheckpointRev, h.Request); err != nil {
			return fmt.Errorf("prepare self-exec: %w", err)
		}
	}
	return tx.Commit()
}

func validateExecSource(ctx context.Context, tx *sql.Tx, h ExecHandoff) error {
	var valid int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions s JOIN serve_response_lifecycle r ON r.session_id=s.id WHERE s.id=? AND s.transcript_rev=? AND r.response_id=? AND r.owner_instance_id=? AND r.fencing_token=? AND r.state='running' AND r.lease_expires_at>CAST((julianday('now')-2440587.5)*86400000 AS INTEGER) AND NOT EXISTS(SELECT 1 FROM session_pending_steering p WHERE p.session_id=s.id) AND NOT EXISTS(SELECT 1 FROM session_rush_operations q WHERE q.session_id=s.id AND q.status IN ('interrupting','waiting_for_settlement','starting')) AND NOT EXISTS(SELECT 1 FROM serve_response_lifecycle n WHERE n.session_id=s.id AND n.fencing_token>r.fencing_token)`, h.SessionID, h.CheckpointRev, h.SourceResponseID, h.SourceOwnerID, h.SourceFence).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExecHandoffConflict
	}
	return err
}

func (s *SQLiteStore) ReadExecHandoff(ctx context.Context, id, service string) ([]ExecHandoff, error) {
	rows, err := s.queryDB().QueryContext(ctx, `SELECT restart_id,service_id,source_owner_id,session_id,source_response_id,source_fence,checkpoint_rev,request FROM serve_exec_handoffs WHERE restart_id=? AND service_id=? AND consumed=0 AND created_at>CAST((julianday('now')-2440587.5)*86400000 AS INTEGER)-300000`, id, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ExecHandoff
	for rows.Next() {
		var h ExecHandoff
		if err := rows.Scan(&h.ID, &h.ServiceID, &h.SourceOwnerID, &h.SessionID, &h.SourceResponseID, &h.SourceFence, &h.CheckpointRev, &h.Request); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) DiscardExecHandoff(ctx context.Context, id, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM serve_exec_handoffs WHERE restart_id=? AND source_owner_id=? AND consumed=0`, id, owner)
	return err
}

// consumeExecHandoffTx is called inside response admission's writer transaction.
// An absent environment hint never calls this path; neither a PID nor a lookup
// UUID alone authorizes adoption. Service, boot, lease and transcript must match.
func consumeExecHandoffTx(ctx context.Context, tx *sql.Tx, a ResponseRunAdmission) error {
	var h ExecHandoff
	err := tx.QueryRowContext(ctx, `SELECT restart_id,service_id,source_owner_id,session_id,source_response_id,source_fence,checkpoint_rev FROM serve_exec_handoffs WHERE restart_id=? AND service_id=? AND session_id=? AND consumed=0 AND created_at>CAST((julianday('now')-2440587.5)*86400000 AS INTEGER)-300000`, a.ExecRestartID, a.ExecServiceID, a.SessionID).Scan(&h.ID, &h.ServiceID, &h.SourceOwnerID, &h.SessionID, &h.SourceResponseID, &h.SourceFence, &h.CheckpointRev)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrExecHandoffConflict
	}
	if err != nil {
		return err
	}
	if h.SourceOwnerID == a.OwnerInstanceID {
		return ErrExecHandoffConflict
	}
	if err = validateExecSource(ctx, tx, h); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE serve_exec_handoffs SET consumed=1,replacement_response_id=? WHERE restart_id=? AND session_id=?`, a.ResponseID, h.ID, h.SessionID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE serve_response_lifecycle SET state='orphaned',lease_expires_at=0 WHERE response_id=?`, h.SourceResponseID)
	return err
}

// ExecContinuation follows only explicit accepted replacement edges. It never
// resolves to an unrelated newer user response in the same session.
func (s *SQLiteStore) ExecContinuation(ctx context.Context, source, service string) (string, error) {
	for depth := 0; depth < 64; depth++ {
		var next string
		err := s.queryDB().QueryRowContext(ctx, `SELECT replacement_response_id FROM serve_exec_handoffs WHERE source_response_id=? AND service_id=? AND consumed=1`, source, service).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return source, nil
		}
		if err != nil {
			return "", err
		}
		if next == "" || next == source {
			return "", ErrExecHandoffConflict
		}
		source = next
	}
	return "", ErrExecHandoffConflict
}
