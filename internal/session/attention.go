package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	defaultResponseRunLease = 30 * time.Second
	responseRunOrphanGrace  = 5 * time.Second
	maxAttentionPageSize    = 500
)

var _ ServeResponseLifecycleStore = (*SQLiteStore)(nil)
var _ ResponseRunInteractionStore = (*SQLiteStore)(nil)
var _ AttentionStore = (*SQLiteStore)(nil)

func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func timeFromMillis(value sql.NullInt64) time.Time {
	if !value.Valid || value.Int64 <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value.Int64).UTC()
}

func randomStoreInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "store_" + hex.EncodeToString(value[:]), nil
}

func (s *SQLiteStore) StoreInstanceID(ctx context.Context) (string, error) {
	if s.cfg.ReadOnly {
		return "", errors.New("session: attention unavailable for read-only store")
	}
	s.storeInstanceMu.Lock()
	defer s.storeInstanceMu.Unlock()
	if s.storeInstanceID != "" {
		return s.storeInstanceID, nil
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = 'store_instance_id'`).Scan(&value)
	if err == nil && strings.TrimSpace(value) != "" {
		s.storeInstanceID = value
		return value, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read store instance id: %w", err)
	}
	value, err = randomStoreInstanceID()
	if err != nil {
		return "", fmt.Errorf("generate store instance id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES ('store_instance_id', ?)
		ON CONFLICT(key) DO NOTHING`, value); err != nil {
		return "", fmt.Errorf("persist store instance id: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = 'store_instance_id'`).Scan(&value); err != nil {
		return "", fmt.Errorf("re-read store instance id: %w", err)
	}
	s.storeInstanceID = value
	return value, nil
}

func nextFencingToken(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES ('serve_response_fencing_token', '0')
		ON CONFLICT(key) DO UPDATE SET value = CAST(CAST(value AS INTEGER) + 1 AS TEXT)`); err != nil {
		return 0, err
	}
	var token int64
	if err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM metadata WHERE key = 'serve_response_fencing_token'`).Scan(&token); err != nil {
		return 0, err
	}
	// The first insert starts at zero; allocate one rather than exposing zero as a token.
	if token == 0 {
		token = 1
		if _, err := tx.ExecContext(ctx, `UPDATE metadata SET value = '1' WHERE key = 'serve_response_fencing_token'`); err != nil {
			return 0, err
		}
	}
	return token, nil
}

func (s *SQLiteStore) AdmitResponseRun(ctx context.Context, admission ResponseRunAdmission) (ResponseRunLease, error) {
	if strings.TrimSpace(admission.ResponseID) == "" || strings.TrimSpace(admission.SessionID) == "" || strings.TrimSpace(admission.OwnerInstanceID) == "" {
		return ResponseRunLease{}, errors.New("session: invalid response run admission")
	}
	leaseDuration := admission.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultResponseRunLease
	}
	now := time.Now().UTC()
	startedAt := admission.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = now
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ResponseRunLease{}, fmt.Errorf("begin response run admission: %w", err)
	}
	defer tx.Rollback()
	token, err := nextFencingToken(ctx, tx)
	if err != nil {
		return ResponseRunLease{}, fmt.Errorf("allocate response run fence: %w", err)
	}
	leaseExpiresAt := now.Add(leaseDuration)
	result, err := tx.ExecContext(ctx, `INSERT INTO serve_response_lifecycle(
		response_id, session_id, run_epoch, state, owner_instance_id, fencing_token,
		lease_expires_at, started_rev, started_at, updated_at)
		VALUES (?, ?, ?, 'running', ?, ?, CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) + ?, ?, ?,
		CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER))`, admission.ResponseID, admission.SessionID,
		admission.RunEpoch, admission.OwnerInstanceID, token, leaseDuration.Milliseconds(),
		admission.StartedRev, unixMilli(startedAt))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "foreign key constraint") {
			return ResponseRunLease{}, ErrNotFound
		}
		return ResponseRunLease{}, fmt.Errorf("admit response run: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ResponseRunLease{}, errors.New("session: response run was not admitted")
	}
	var leaseExpiresMillis int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_expires_at FROM serve_response_lifecycle WHERE response_id = ?`, admission.ResponseID).Scan(&leaseExpiresMillis); err != nil {
		return ResponseRunLease{}, fmt.Errorf("read response run lease: %w", err)
	}
	leaseExpiresAt = time.UnixMilli(leaseExpiresMillis).UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_change_log(kind, session_id, transcript_rev)
		VALUES (?, ?, ?)`, StoreChangeSessionLifecycleChanged, admission.SessionID, admission.StartedRev); err != nil {
		return ResponseRunLease{}, fmt.Errorf("publish response run admission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ResponseRunLease{}, fmt.Errorf("commit response run admission: %w", err)
	}
	return ResponseRunLease{ResponseID: admission.ResponseID, FencingToken: token, LeaseExpiresAt: leaseExpiresAt}, nil
}

func (s *SQLiteStore) RenewResponseRunLease(ctx context.Context, responseID, ownerInstanceID string, fencingToken int64) (ResponseRunLease, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE serve_response_lifecycle
		SET lease_expires_at = CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) + ?,
		    updated_at = CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)
		WHERE response_id = ? AND state = 'running' AND owner_instance_id = ? AND fencing_token = ?
		  AND lease_expires_at + ? > CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`,
		defaultResponseRunLease.Milliseconds(), responseID, ownerInstanceID, fencingToken, responseRunOrphanGrace.Milliseconds())
	if err != nil {
		return ResponseRunLease{}, fmt.Errorf("renew response run lease: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ResponseRunLease{}, ErrResponseRunLeaseLost
	}
	var expiresMillis int64
	if err := s.db.QueryRowContext(ctx, `SELECT lease_expires_at FROM serve_response_lifecycle
		WHERE response_id = ? AND owner_instance_id = ? AND fencing_token = ?`, responseID, ownerInstanceID, fencingToken).Scan(&expiresMillis); err != nil {
		return ResponseRunLease{}, fmt.Errorf("read renewed response run lease: %w", err)
	}
	return ResponseRunLease{ResponseID: responseID, FencingToken: fencingToken, LeaseExpiresAt: time.UnixMilli(expiresMillis).UTC()}, nil
}

func (s *SQLiteStore) ValidateResponseRunLease(ctx context.Context, responseID, ownerInstanceID string, fencingToken int64) error {
	var valid int
	err := s.queryDB().QueryRowContext(ctx, `SELECT 1 FROM serve_response_lifecycle
		WHERE response_id = ? AND state = 'running' AND owner_instance_id = ? AND fencing_token = ?
		  AND lease_expires_at > CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`,
		responseID, ownerInstanceID, fencingToken).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrResponseRunLeaseLost
	}
	if err != nil {
		return fmt.Errorf("validate response run lease: %w", err)
	}
	return nil
}

func (s *SQLiteStore) CheckpointResponseRun(ctx context.Context, checkpoint ResponseRunCheckpoint) error {
	result, err := s.db.ExecContext(ctx, `UPDATE serve_response_lifecycle
		SET final_rev = MAX(final_rev, ?), durable_output_count = MAX(durable_output_count, ?), updated_at = ?
		WHERE response_id = ? AND state = 'running' AND owner_instance_id = ? AND fencing_token = ?
		  AND lease_expires_at > CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`,
		checkpoint.FinalRev, checkpoint.DurableOutputCount, time.Now().UTC().UnixMilli(), checkpoint.ResponseID,
		checkpoint.OwnerInstanceID, checkpoint.FencingToken)
	if err != nil {
		return fmt.Errorf("checkpoint response run: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrResponseRunLeaseLost
	}
	return nil
}

func normalizeInteractionKinds(values []string, count int) ([]string, string) {
	if count <= 0 {
		return nil, ""
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	encoded, _ := json.Marshal(result)
	return result, string(encoded)
}

func (s *SQLiteStore) SetResponseRunInteractionState(ctx context.Context, value ResponseRunInteractionState) error {
	if strings.TrimSpace(value.ResponseID) == "" || strings.TrimSpace(value.OwnerInstanceID) == "" || value.FencingToken <= 0 || value.Revision < 0 {
		return errors.New("session: invalid response interaction state")
	}
	if value.Count < 0 {
		value.Count = 0
	}
	_, kindsJSON := normalizeInteractionKinds(value.Kinds, value.Count)
	var since any
	if value.Count > 0 {
		requiredSince := value.RequiredSince.UTC()
		if requiredSince.IsZero() {
			requiredSince = time.Now().UTC()
		}
		since = unixMilli(requiredSince)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID, currentKinds string
	var currentCount int
	var currentRevision, finalRev int64
	var currentSince sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT session_id, interaction_required_count, interaction_required_kinds,
		interaction_required_since, interaction_state_rev, final_rev FROM serve_response_lifecycle
		WHERE response_id = ? AND state = 'running' AND owner_instance_id = ? AND fencing_token = ?
		  AND lease_expires_at + ? > CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`,
		value.ResponseID, value.OwnerInstanceID, value.FencingToken, responseRunOrphanGrace.Milliseconds()).
		Scan(&sessionID, &currentCount, &currentKinds, &currentSince, &currentRevision, &finalRev)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrResponseRunLeaseLost
	}
	if err != nil {
		return fmt.Errorf("read response interaction state: %w", err)
	}
	if value.Revision < currentRevision {
		return tx.Commit()
	}
	incomingSince := int64(0)
	if millis, ok := since.(int64); ok {
		incomingSince = millis
	}
	currentSinceMillis := int64(0)
	if currentSince.Valid {
		currentSinceMillis = currentSince.Int64
	}
	if currentCount == value.Count && currentKinds == kindsJSON && currentSinceMillis == incomingSince && currentRevision == value.Revision {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE serve_response_lifecycle
		SET interaction_required_count = ?, interaction_required_kinds = ?, interaction_required_since = ?,
		    interaction_state_rev = ?, updated_at = CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)
		WHERE response_id = ? AND state = 'running' AND owner_instance_id = ? AND fencing_token = ?
		  AND interaction_state_rev <= ?`, value.Count, kindsJSON, since, value.Revision,
		value.ResponseID, value.OwnerInstanceID, value.FencingToken, value.Revision)
	if err != nil {
		return fmt.Errorf("update response interaction state: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrResponseRunLeaseLost
	}
	if err := insertLifecycleChange(ctx, tx, sessionID, finalRev); err != nil {
		return fmt.Errorf("publish response interaction state: %w", err)
	}
	return tx.Commit()
}

type responseRunFenceExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func checkpointResponseRunFenceTx(ctx context.Context, exec responseRunFenceExecutor, sessionID string, finalRev int64) error {
	fence, ok := ResponseRunFenceFromContext(ctx)
	if !ok {
		return nil
	}
	result, err := exec.ExecContext(ctx, `UPDATE serve_response_lifecycle
		SET final_rev = MAX(final_rev, ?), durable_output_count = MAX(durable_output_count, ?),
		    updated_at = CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)
		WHERE response_id = ? AND session_id = ? AND state = 'running'
		  AND owner_instance_id = ? AND fencing_token = ?
		  AND lease_expires_at > CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`,
		finalRev, fence.DurableOutputCount, fence.ResponseID, sessionID, fence.OwnerInstanceID, fence.FencingToken)
	if err != nil {
		return fmt.Errorf("fence response transcript write: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrResponseRunLeaseLost
	}
	return nil
}

func terminalNeedsAttention(outcome ResponseRunState, startedRev, finalRev int64, outputCount int) bool {
	switch outcome {
	case ResponseRunCompleted, ResponseRunFailed, ResponseRunOrphaned:
		return true
	case ResponseRunCancelled:
		return outputCount > 0 || finalRev > startedRev
	default:
		return false
	}
}

func insertLifecycleChange(ctx context.Context, tx *sql.Tx, sessionID string, rev int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO session_change_log(kind, session_id, transcript_rev) VALUES (?, ?, ?)`,
		StoreChangeSessionLifecycleChanged, sessionID, rev)
	return err
}

func insertAttentionChange(ctx context.Context, tx *sql.Tx, sessionID string, rev int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO session_change_log(kind, session_id, transcript_rev) VALUES (?, ?, ?)`,
		StoreChangeSessionAttentionChanged, sessionID, rev)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func scanAttentionState(row interface{ Scan(...any) error }) (AttentionState, error) {
	var state AttentionState
	var terminalAt, seenAt sql.NullInt64
	if err := row.Scan(&state.SessionID, &state.LatestAttentionSeq, &state.ResponseID, &state.RunEpoch,
		&state.Outcome, &state.StartedRev, &state.FinalRev, &terminalAt, &state.SeenThroughSeq, &seenAt); err != nil {
		return AttentionState{}, err
	}
	state.TerminalAt = timeFromMillis(terminalAt)
	state.SeenAt = timeFromMillis(seenAt)
	state.Unseen = state.LatestAttentionSeq > state.SeenThroughSeq
	return state, nil
}

func getAttentionTx(ctx context.Context, tx *sql.Tx, sessionID string) (AttentionState, error) {
	return scanAttentionState(tx.QueryRowContext(ctx, `SELECT session_id, latest_attention_seq, response_id, run_epoch,
		outcome, started_rev, final_rev, terminal_at, seen_through_seq, seen_at
		FROM session_attention WHERE session_id = ?`, sessionID))
}

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
	attention := AttentionState{SessionID: sessionID}
	if terminalNeedsAttention(terminal.Outcome, startedRev, finalRev, outputCount) {
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
	if err := tx.Commit(); err != nil {
		return AttentionState{}, fmt.Errorf("commit response run finalization: %w", err)
	}
	attention.StoreInstanceID = storeID
	return attention, nil
}

func (s *SQLiteStore) RecoverExpiredResponseRuns(ctx context.Context, limit int) ([]AttentionState, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var databaseNow int64
	if err := s.queryDB().QueryRowContext(ctx, `SELECT CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`).Scan(&databaseNow); err != nil {
		return nil, fmt.Errorf("read database time for orphan recovery: %w", err)
	}
	cutoff := databaseNow - responseRunOrphanGrace.Milliseconds()
	rows, err := s.queryDB().QueryContext(ctx, `SELECT response_id FROM serve_response_lifecycle
		WHERE state = 'running' AND lease_expires_at < ? ORDER BY lease_expires_at LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired response runs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	storeID, err := s.StoreInstanceID(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]AttentionState, 0, len(ids))
	for _, id := range ids {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		var sessionID string
		var runEpoch, startedRev, finalRev int64
		var outputCount int
		err = tx.QueryRowContext(ctx, `SELECT session_id, run_epoch, started_rev, final_rev, durable_output_count
			FROM serve_response_lifecycle WHERE response_id = ? AND state = 'running' AND lease_expires_at < ?`, id, cutoff).
			Scan(&sessionID, &runEpoch, &startedRev, &finalRev, &outputCount)
		if errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			continue
		}
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		now := time.Now().UTC()
		updated, err := tx.ExecContext(ctx, `UPDATE serve_response_lifecycle SET state = 'orphaned', ended_at = ?,
			updated_at = ?, interaction_required_count = 0, interaction_required_kinds = '', interaction_required_since = NULL
			WHERE response_id = ? AND state = 'running' AND lease_expires_at < ?`, unixMilli(now), unixMilli(now), id, cutoff)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if affected, _ := updated.RowsAffected(); affected != 1 {
			tx.Rollback()
			continue
		}
		if err := insertLifecycleChange(ctx, tx, sessionID, finalRev); err != nil {
			tx.Rollback()
			return nil, err
		}
		seq, err := insertAttentionChange(ctx, tx, sessionID, finalRev)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_attention(session_id, latest_attention_seq, response_id,
			run_epoch, outcome, started_rev, final_rev, terminal_at, updated_at)
			VALUES (?, ?, ?, ?, 'orphaned', ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET latest_attention_seq=excluded.latest_attention_seq,
			response_id=excluded.response_id, run_epoch=excluded.run_epoch, outcome='orphaned',
			started_rev=excluded.started_rev, final_rev=excluded.final_rev, terminal_at=excluded.terminal_at,
			updated_at=excluded.updated_at WHERE excluded.latest_attention_seq > session_attention.latest_attention_seq`,
			sessionID, seq, id, runEpoch, startedRev, finalRev, unixMilli(now), unixMilli(now)); err != nil {
			tx.Rollback()
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE serve_response_lifecycle SET attention_seq = ? WHERE response_id = ?`, seq, id); err != nil {
			tx.Rollback()
			return nil, err
		}
		attention, err := getAttentionTx(ctx, tx, sessionID)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		attention.StoreInstanceID = storeID
		attention.Changed = true
		result = append(result, attention)
	}
	retentionCutoff := databaseNow - (30 * 24 * time.Hour).Milliseconds()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM serve_response_lifecycle WHERE response_id IN (
		SELECT l.response_id FROM serve_response_lifecycle l
		LEFT JOIN session_attention a ON a.session_id = l.session_id
		WHERE l.state <> 'running' AND l.ended_at < ?
		  AND (l.attention_seq = 0 OR a.seen_through_seq >= l.attention_seq)
		LIMIT 1000)`, retentionCutoff)
	return result, nil
}

func (s *SQLiteStore) GetAttentionBatch(ctx context.Context, sessionIDs []string) (map[string]AttentionState, error) {
	result := make(map[string]AttentionState)
	if len(sessionIDs) == 0 {
		return result, nil
	}
	storeID, err := s.StoreInstanceID(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sessionIDs))
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, id := range sessionIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += maxAttentionPageSize {
		end := min(start+maxAttentionPageSize, len(ids))
		args := make([]any, 0, end-start)
		placeholders := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
			placeholders = append(placeholders, "?")
		}
		rows, queryErr := s.queryDB().QueryContext(ctx, `SELECT session_id, latest_attention_seq, response_id, run_epoch,
			outcome, started_rev, final_rev, terminal_at, seen_through_seq, seen_at
			FROM session_attention WHERE session_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if queryErr != nil {
			return nil, fmt.Errorf("get session attention batch: %w", queryErr)
		}
		for rows.Next() {
			state, scanErr := scanAttentionState(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			state.StoreInstanceID = storeID
			result[state.SessionID] = state
		}
		if rowsErr := rows.Close(); rowsErr != nil {
			return nil, rowsErr
		}
	}
	return result, nil
}

func (s *SQLiteStore) GetAttention(ctx context.Context, sessionID string) (AttentionState, error) {
	state, err := scanAttentionState(s.queryDB().QueryRowContext(ctx, `SELECT session_id, latest_attention_seq, response_id,
		run_epoch, outcome, started_rev, final_rev, terminal_at, seen_through_seq, seen_at
		FROM session_attention WHERE session_id = ?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if existsErr := s.queryDB().QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); errors.Is(existsErr, sql.ErrNoRows) {
			return AttentionState{}, ErrNotFound
		} else if existsErr != nil {
			return AttentionState{}, existsErr
		}
		state = AttentionState{SessionID: sessionID}
	} else if err != nil {
		return AttentionState{}, fmt.Errorf("get session attention: %w", err)
	}
	state.StoreInstanceID, err = s.StoreInstanceID(ctx)
	return state, err
}

func (s *SQLiteStore) MarkAttentionSeen(ctx context.Context, sessionID, storeInstanceID string, throughSeq int64) (AttentionState, error) {
	if throughSeq <= 0 {
		return AttentionState{}, ErrAttentionConflict
	}
	currentStoreID, err := s.StoreInstanceID(ctx)
	if err != nil {
		return AttentionState{}, err
	}
	if storeInstanceID != currentStoreID {
		return AttentionState{}, ErrAttentionConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AttentionState{}, err
	}
	defer tx.Rollback()
	state, err := getAttentionTx(ctx, tx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if existsErr := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&exists); errors.Is(existsErr, sql.ErrNoRows) {
			return AttentionState{}, ErrNotFound
		}
		return AttentionState{}, ErrAttentionConflict
	}
	if err != nil {
		return AttentionState{}, err
	}
	if throughSeq > state.LatestAttentionSeq {
		return AttentionState{}, ErrAttentionConflict
	}
	if throughSeq > state.SeenThroughSeq {
		var exactMarker int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM serve_response_lifecycle WHERE session_id = ? AND attention_seq = ?`, sessionID, throughSeq).Scan(&exactMarker); errors.Is(err, sql.ErrNoRows) {
			return AttentionState{}, ErrAttentionConflict
		} else if err != nil {
			return AttentionState{}, err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE session_attention SET seen_through_seq = MAX(seen_through_seq, ?),
			seen_at = ?, updated_at = ? WHERE session_id = ?`, throughSeq, unixMilli(now), unixMilli(now), sessionID); err != nil {
			return AttentionState{}, err
		}
		if _, err := insertAttentionChange(ctx, tx, sessionID, state.FinalRev); err != nil {
			return AttentionState{}, err
		}
		state, err = getAttentionTx(ctx, tx, sessionID)
		if err != nil {
			return AttentionState{}, err
		}
		state.Changed = true
	}
	if err := tx.Commit(); err != nil {
		return AttentionState{}, err
	}
	state.StoreInstanceID = currentStoreID
	return state, nil
}

type attentionCursor struct {
	Seq           int64  `json:"s,omitempty"`
	StartedAt     int64  `json:"t,omitempty"`
	RequiredSince int64  `json:"u,omitempty"`
	ResponseID    string `json:"r,omitempty"`
}

func encodeAttentionCursor(cursor attentionCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeAttentionCursor(value string) (attentionCursor, error) {
	if strings.TrimSpace(value) == "" {
		return attentionCursor{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return attentionCursor{}, ErrAttentionConflict
	}
	var cursor attentionCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return attentionCursor{}, ErrAttentionConflict
	}
	return cursor, nil
}

func attentionSnapshotVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var version int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM session_change_log WHERE kind IN (
		'session.lifecycle_changed', 'session.attention_changed', 'session.created', 'session.deleted',
		'session.metadata_changed', 'project.membership_changed', 'project.created', 'project.updated', 'project.deleted')`).Scan(&version)
	return version, err
}

func (s *SQLiteStore) ListAttention(ctx context.Context, opts AttentionListOptions) (AttentionPage, error) {
	if opts.Kind != AttentionKindUnseen && opts.Kind != AttentionKindRunning && opts.Kind != AttentionKindInputRequired {
		return AttentionPage{}, ErrAttentionConflict
	}
	if opts.Limit <= 0 || opts.Limit > maxAttentionPageSize {
		opts.Limit = 200
	}
	cursor, err := decodeAttentionCursor(opts.Cursor)
	if err != nil {
		return AttentionPage{}, err
	}
	storeID, err := s.StoreInstanceID(ctx)
	if err != nil {
		return AttentionPage{}, err
	}
	tx, err := s.queryDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AttentionPage{}, err
	}
	defer tx.Rollback()
	version, err := attentionSnapshotVersion(ctx, tx)
	if err != nil {
		return AttentionPage{}, err
	}
	if opts.SnapshotVersion > 0 && opts.SnapshotVersion != version {
		return AttentionPage{}, ErrAttentionConflict
	}
	var rows *sql.Rows
	switch opts.Kind {
	case AttentionKindUnseen:
		rows, err = tx.QueryContext(ctx, `SELECT a.session_id, a.response_id, COALESCE(l.state, a.outcome), a.latest_attention_seq,
			a.started_rev, a.final_rev, COALESCE(s.generated_short_title, s.name, ''),
			COALESCE(s.generated_long_title, ''), COALESCE(s.project_id, ''), a.outcome,
			COALESCE(l.started_at, a.terminal_at, 0), a.terminal_at, l.lease_expires_at, 0, '', NULL, 0
			FROM session_attention a JOIN sessions s ON s.id = a.session_id
			LEFT JOIN serve_response_lifecycle l ON l.response_id = a.response_id
			WHERE a.latest_attention_seq > a.seen_through_seq AND s.parent_id IS NULL
			  AND (? = 0 OR a.latest_attention_seq < ?)
			ORDER BY a.latest_attention_seq DESC LIMIT ?`, cursor.Seq, cursor.Seq, opts.Limit+1)
	case AttentionKindRunning:
		rows, err = tx.QueryContext(ctx, `SELECT l.session_id, l.response_id, l.state, 0, l.started_rev, l.final_rev,
			COALESCE(s.generated_short_title, s.name, ''), COALESCE(s.generated_long_title, ''),
			COALESCE(s.project_id, ''), '', l.started_at, NULL, l.lease_expires_at, 0, '', NULL, 0
			FROM serve_response_lifecycle l JOIN sessions s ON s.id = l.session_id
			WHERE l.state = 'running' AND s.parent_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM serve_response_lifecycle newer
			      WHERE newer.session_id = l.session_id AND newer.state = 'running'
			        AND (newer.started_at > l.started_at OR (newer.started_at = l.started_at AND newer.response_id > l.response_id)))
			  AND (? = 0 OR l.started_at > ? OR (l.started_at = ? AND l.response_id > ?))
			ORDER BY l.started_at, l.response_id LIMIT ?`, cursor.StartedAt, cursor.StartedAt, cursor.StartedAt, cursor.ResponseID, opts.Limit+1)
	case AttentionKindInputRequired:
		rows, err = tx.QueryContext(ctx, `SELECT l.session_id, l.response_id, l.state, 0, l.started_rev, l.final_rev,
			COALESCE(s.generated_short_title, s.name, ''), COALESCE(s.generated_long_title, ''),
			COALESCE(s.project_id, ''), '', l.started_at, NULL, l.lease_expires_at,
			l.interaction_required_count, l.interaction_required_kinds, l.interaction_required_since, l.interaction_state_rev
			FROM serve_response_lifecycle l JOIN sessions s ON s.id = l.session_id
			WHERE l.state = 'running' AND l.interaction_required_count > 0 AND s.parent_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM serve_response_lifecycle newer
			      WHERE newer.session_id = l.session_id AND newer.state = 'running'
			        AND (newer.started_at > l.started_at OR (newer.started_at = l.started_at AND newer.response_id > l.response_id)))
			  AND (? = 0 OR COALESCE(l.interaction_required_since, l.started_at) > ?
			       OR (COALESCE(l.interaction_required_since, l.started_at) = ? AND l.response_id > ?))
			ORDER BY COALESCE(l.interaction_required_since, l.started_at), l.response_id LIMIT ?`,
			cursor.RequiredSince, cursor.RequiredSince, cursor.RequiredSince, cursor.ResponseID, opts.Limit+1)
	}
	if err != nil {
		return AttentionPage{}, err
	}
	defer rows.Close()
	items := make([]AttentionItem, 0, opts.Limit+1)
	for rows.Next() {
		var item AttentionItem
		var startedAt int64
		var terminalAt, leaseExpires, requiredSince sql.NullInt64
		var kindsJSON string
		if err := rows.Scan(&item.SessionID, &item.ResponseID, &item.LifecycleState, &item.AttentionSeq,
			&item.StartedRev, &item.FinalRev, &item.ShortTitle, &item.LongTitle, &item.ProjectID,
			&item.Outcome, &startedAt, &terminalAt, &leaseExpires, &item.PendingInteractionCount,
			&kindsJSON, &requiredSince, &item.InteractionStateRev); err != nil {
			return AttentionPage{}, err
		}
		item.Kind = opts.Kind
		item.StartedAt = time.UnixMilli(startedAt).UTC()
		item.TerminalAt = timeFromMillis(terminalAt)
		item.LeaseExpiresAt = timeFromMillis(leaseExpires)
		item.InteractionRequiredSince = timeFromMillis(requiredSince)
		item.InteractionRequired = item.PendingInteractionCount > 0
		if kindsJSON != "" {
			_ = json.Unmarshal([]byte(kindsJSON), &item.PendingInteractionKinds)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return AttentionPage{}, err
	}
	hasMore := len(items) > opts.Limit
	if hasMore {
		items = items[:opts.Limit]
	}
	next := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		if opts.Kind == AttentionKindUnseen {
			next = encodeAttentionCursor(attentionCursor{Seq: last.AttentionSeq})
		} else if opts.Kind == AttentionKindInputRequired {
			requiredSince := last.InteractionRequiredSince
			if requiredSince.IsZero() {
				requiredSince = last.StartedAt
			}
			next = encodeAttentionCursor(attentionCursor{RequiredSince: requiredSince.UnixMilli(), ResponseID: last.ResponseID})
		} else {
			next = encodeAttentionCursor(attentionCursor{StartedAt: last.StartedAt.UnixMilli(), ResponseID: last.ResponseID})
		}
	}
	if err := tx.Commit(); err != nil {
		return AttentionPage{}, err
	}
	protocolVersion := 1
	if opts.Kind == AttentionKindInputRequired {
		protocolVersion = 2
	}
	return AttentionPage{ProtocolVersion: protocolVersion, StoreInstanceID: storeID, SnapshotVersion: version,
		Items: items, NextCursor: next, HasMore: hasMore}, nil
}
