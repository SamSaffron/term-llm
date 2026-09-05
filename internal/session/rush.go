package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm"
	"time"
)

var ErrSteeringConflict = errors.New("steering identity, content or ownership conflict")

type RushStatus string

const (
	RushInterrupting RushStatus = "interrupting"
	RushWaiting      RushStatus = "waiting_for_settlement"
	RushStarting     RushStatus = "starting"
	RushStarted      RushStatus = "started"
	RushBlocked      RushStatus = "blocked"
	RushCancelled    RushStatus = "cancelled"
	RushFailed       RushStatus = "failed"
	RushNoop         RushStatus = "noop"
)

func (s RushStatus) Active() bool {
	return s == RushInterrupting || s == RushWaiting || s == RushStarting
}

type RushEntry struct {
	Steering    PendingSteering `json:"steering"`
	Disposition string          `json:"disposition"`
}
type RushOperation struct {
	SessionID             string      `json:"session_id"`
	RequestID             string      `json:"rush_id"`
	SourceResponseID      string      `json:"source_response_id"`
	SourceEpoch           int64       `json:"source_run_epoch"`
	Status                RushStatus  `json:"status"`
	Revision              int64       `json:"revision"`
	ReplacementResponseID string      `json:"replacement_response_id,omitempty"`
	Fence                 int64       `json:"-"`
	Reason                string      `json:"reason,omitempty"`
	Entries               []RushEntry `json:"entries"`
	SteeringIDs           []string    `json:"steering_ids"`
	CreatedAt             time.Time   `json:"created_at"`
	UpdatedAt             time.Time   `json:"updated_at"`
}

type RushStore interface {
	ReleaseRush(context.Context, *RushOperation) error
	AdmitRush(context.Context, RushOperation, []PendingSteering) (*RushOperation, error)
	GetRush(context.Context, string, string) (*RushOperation, error)
	ActiveRush(context.Context, string) (*RushOperation, error)
	LatestRush(context.Context, string) (*RushOperation, error)
	AdvanceRush(context.Context, *RushOperation, RushStatus, string) (*RushOperation, error)
	CommitRushInitialInput(context.Context, *RushOperation, []*Message) (int64, error)
}

func AsRushStore(store Store) (RushStore, bool) {
	if logging, ok := store.(*LoggingStore); ok {
		if _, ok := AsRushStore(logging.Store); !ok {
			return nil, false
		}
		return logging, true
	}
	s, ok := store.(RushStore)
	return s, ok
}

// Admission is idempotent by session/request/source, and snapshots exactly the
// frozen engine batch. Missing, changed or differently owned rows abort it all.
func (s *SQLiteStore) AdmitRush(ctx context.Context, op RushOperation, entries []PendingSteering) (*RushOperation, error) {
	if op.SessionID == "" || op.RequestID == "" || op.SourceResponseID == "" || op.SourceEpoch <= 0 || op.Fence <= 0 {
		return nil, ErrSteeringConflict
	}
	if existing, err := s.GetRush(ctx, op.SessionID, op.RequestID); err == nil {
		if existing.SourceResponseID != op.SourceResponseID || existing.SourceEpoch != op.SourceEpoch {
			return nil, ErrSteeringConflict
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	op.Status = RushInterrupting
	eligible := false
	for _, entry := range entries {
		eligible = eligible || entry.Origin.EligibleForRush()
	}
	if !eligible {
		op.Status = RushNoop
		entries = nil
		op.ReplacementResponseID = ""
	}
	var replacement any
	if op.ReplacementResponseID != "" {
		replacement = op.ReplacementResponseID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_rush_operations(session_id,request_id,source_response_id,source_epoch,status,replacement_response_id,owner_fence) VALUES (?,?,?,?,?,?,?)`, op.SessionID, op.RequestID, op.SourceResponseID, op.SourceEpoch, op.Status, replacement, op.Fence)
	if err != nil {
		return nil, fmt.Errorf("admit rush: %w", err)
	}
	for _, entry := range entries {
		result, err := tx.ExecContext(ctx, `UPDATE session_pending_steering SET owner_kind='rush',owner_id=?,owner_fence=? WHERE session_id=? AND id=? AND owner_kind='' AND NOT EXISTS (SELECT 1 FROM messages WHERE session_id=? AND client_message_id=?)`, op.RequestID, op.Fence, op.SessionID, entry.ID, op.SessionID, entry.ID)
		if err != nil {
			return nil, err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return nil, ErrSteeringConflict
		}
		var content, display, attachments, origin string
		var sequence int64
		err = tx.QueryRowContext(ctx, `SELECT message,display_text,attachment_summary,origin,acceptance_sequence FROM session_pending_steering WHERE session_id=? AND id=?`, op.SessionID, entry.ID).Scan(&content, &display, &attachments, &origin, &sequence)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(entry.Message)
		if err != nil {
			return nil, err
		}
		if content != string(encoded) || display != entry.DisplayText || attachments != llm.MessageAttachmentSummary(entry.Message) || (entry.Origin != "" && origin != string(entry.Origin)) || (entry.AcceptanceSequence > 0 && sequence != entry.AcceptanceSequence) {
			return nil, ErrSteeringConflict
		}
		entry.SessionID = op.SessionID
		entry.AcceptanceSequence = sequence
		entry.Origin = llm.SteeringOrigin(origin)
		entry.AttachmentSummary = attachments
		payload, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_rush_entries(session_id,request_id,steering_id,acceptance_sequence,payload) VALUES (?,?,?,?,?)`, op.SessionID, op.RequestID, entry.ID, sequence, string(payload))
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetRush(ctx, op.SessionID, op.RequestID)
}

func (s *SQLiteStore) GetRush(ctx context.Context, sessionID, requestID string) (*RushOperation, error) {
	op := &RushOperation{SessionID: sessionID, RequestID: requestID}
	err := s.queryDB().QueryRowContext(ctx, `SELECT source_response_id,source_epoch,status,revision,COALESCE(replacement_response_id,''),owner_fence,reason,created_at,updated_at FROM session_rush_operations WHERE session_id=? AND request_id=?`, sessionID, requestID).Scan(&op.SourceResponseID, &op.SourceEpoch, &op.Status, &op.Revision, &op.ReplacementResponseID, &op.Fence, &op.Reason, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.queryDB().QueryContext(ctx, `SELECT payload,disposition FROM session_rush_entries WHERE session_id=? AND request_id=? ORDER BY acceptance_sequence`, sessionID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry RushEntry
		var payload string
		if err = rows.Scan(&payload, &entry.Disposition); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(payload), &entry.Steering); err != nil {
			return nil, err
		}
		op.Entries = append(op.Entries, entry)
		op.SteeringIDs = append(op.SteeringIDs, entry.Steering.ID)
	}
	return op, rows.Err()
}
func (s *SQLiteStore) ActiveRush(ctx context.Context, sessionID string) (*RushOperation, error) {
	var id string
	err := s.queryDB().QueryRowContext(ctx, `SELECT request_id FROM session_rush_operations WHERE session_id=? AND status IN ('interrupting','waiting_for_settlement','starting')`, sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetRush(ctx, sessionID, id)
}
func (s *SQLiteStore) AdvanceRush(ctx context.Context, op *RushOperation, status RushStatus, reason string) (*RushOperation, error) {
	if op == nil || !op.Status.Active() {
		return nil, ErrSteeringConflict
	}
	switch status {
	case RushCancelled, RushBlocked, RushFailed, RushNoop:
	case RushWaiting:
		if op.Status != RushInterrupting {
			return nil, ErrSteeringConflict
		}
	case RushStarting:
		if op.Status != RushInterrupting && op.Status != RushWaiting {
			return nil, ErrSteeringConflict
		}
	default:
		return nil, ErrSteeringConflict // started is owned only by atomic input commit
	}

	result, err := s.db.ExecContext(ctx, `UPDATE session_rush_operations SET status=?,reason=?,revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE session_id=? AND request_id=? AND revision=? AND owner_fence=? AND status=?`, status, reason, op.SessionID, op.RequestID, op.Revision, op.Fence, op.Status)
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil, ErrSteeringConflict
	}
	return s.GetRush(ctx, op.SessionID, op.RequestID)
}

// CommitRushInitialInput shares the ordinary append transaction, but adds a
// fenced operation CAS before any insert and commits dispositions before COMMIT.
func (s *SQLiteStore) CommitRushInitialInput(ctx context.Context, op *RushOperation, messages []*Message) (int64, error) {
	if op == nil || op.Status != RushStarting || op.ReplacementResponseID == "" {
		return 0, ErrSteeringConflict
	}

	index := 0
	for _, msg := range messages {
		if msg == nil {
			return 0, ErrSteeringConflict
		}
		if msg.Role != llm.RoleUser {
			if msg.ClientMessageID != "" || (msg.Role != llm.RoleDeveloper && msg.Role != llm.RoleSystem) {
				return 0, ErrSteeringConflict
			}
			continue
		}
		if index >= len(op.Entries) || msg.ClientMessageID != op.Entries[index].Steering.ID {
			return 0, ErrSteeringConflict
		}
		expected := NewMessage(op.SessionID, op.Entries[index].Steering.Message, -1)
		a, err := expected.PartsJSONForStorage(s.cfg.StripImageBase64)
		if err != nil {
			return 0, err
		}
		b, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
		if err != nil {
			return 0, err
		}
		if a != b {
			return 0, ErrSteeringConflict
		}
		index++
	}
	if index != len(op.Entries) {
		return 0, ErrSteeringConflict
	}

	return s.appendMessagesWithRush(ctx, op.SessionID, messages, op)
}

func (s *SQLiteStore) LatestRush(ctx context.Context, sessionID string) (*RushOperation, error) {
	var id string
	err := s.queryDB().QueryRowContext(ctx, `SELECT request_id FROM session_rush_operations WHERE session_id=? ORDER BY rowid DESC LIMIT 1`, sessionID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetRush(ctx, sessionID, id)
}
