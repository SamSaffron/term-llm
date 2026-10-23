package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/engine"
)

func (s *SQLiteStore) SavePendingSteering(ctx context.Context, entry PendingSteering) error {
	entry.SessionID = strings.TrimSpace(entry.SessionID)
	entry.ID = strings.TrimSpace(entry.ID)
	if entry.SessionID == "" || entry.ID == "" {
		return fmt.Errorf("save pending steering: session and message IDs are required")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.Origin == "" {
		entry.Origin = engine.SteeringOriginLegacy
	}
	messageJSON, err := json.Marshal(entry.Message)
	if err != nil {
		return err
	}
	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// Allocate on the writer connection. The counter survives an empty queue.
		if _, err = tx.ExecContext(ctx, `INSERT INTO session_steering_sequence(session_id,sequence) VALUES (?,1)
   ON CONFLICT(session_id) DO UPDATE SET sequence=sequence+1`, entry.SessionID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO session_pending_steering
   (session_id,id,message,display_text,attachment_summary,created_at,acceptance_sequence,origin)
   SELECT ?,?,?,?,?,?,sequence,? FROM session_steering_sequence WHERE session_id=?
   ON CONFLICT(session_id,id) DO NOTHING`, entry.SessionID, entry.ID, string(messageJSON), entry.DisplayText,
			entry.AttachmentSummary, entry.CreatedAt, entry.Origin, entry.SessionID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			var content, display, attachments, origin string
			if err := tx.QueryRowContext(ctx, `SELECT message,display_text,attachment_summary,origin FROM session_pending_steering WHERE session_id=? AND id=?`, entry.SessionID, entry.ID).Scan(&content, &display, &attachments, &origin); err != nil {
				return err
			}
			if content != string(messageJSON) || display != entry.DisplayText || attachments != entry.AttachmentSummary || origin != string(entry.Origin) {
				return ErrSteeringConflict
			}
		}
		if err := saveNotificationSteeringOriginTx(ctx, tx, entry); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) DeletePendingSteering(ctx context.Context, sessionID, id string) error {
	sessionID = strings.TrimSpace(sessionID)
	id = strings.TrimSpace(id)
	if sessionID == "" || id == "" {
		return nil
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM session_pending_steering WHERE session_id = ? AND id = ? AND owner_kind = ''`,
		sessionID, id)
	if err != nil {
		return fmt.Errorf("delete pending steering: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		var owner string
		err := s.queryDB().QueryRowContext(ctx, `SELECT owner_kind FROM session_pending_steering WHERE session_id=? AND id=?`, sessionID, id).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if owner != "" {
			return ErrSteeringConflict
		}
	}
	return nil
}

func (s *SQLiteStore) ListPendingSteering(ctx context.Context, sessionID string) ([]PendingSteering, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT pending.id, pending.message, pending.display_text, pending.attachment_summary, pending.created_at, pending.acceptance_sequence, pending.origin, pending.owner_kind, pending.owner_id, pending.owner_fence, COALESCE(n.origin,'')
		FROM session_pending_steering pending LEFT JOIN notification_steering_origins n ON n.session_id=pending.session_id AND n.steering_id=pending.id
		WHERE pending.session_id = ?
		  AND NOT EXISTS (
			SELECT 1 FROM messages committed
			WHERE committed.session_id = pending.session_id
			  AND committed.client_message_id = pending.id
		  )
		ORDER BY pending.acceptance_sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list pending steering: %w", err)
	}
	defer rows.Close()
	var entries []PendingSteering
	for rows.Next() {
		entry := PendingSteering{SessionID: sessionID}
		var messageJSON, notificationJSON string
		if err := rows.Scan(&entry.ID, &messageJSON, &entry.DisplayText, &entry.AttachmentSummary, &entry.CreatedAt, &entry.AcceptanceSequence, &entry.Origin, &entry.OwnerKind, &entry.OwnerID, &entry.OwnerFence, &notificationJSON); err != nil {
			return nil, fmt.Errorf("scan pending steering: %w", err)
		}
		if err := json.Unmarshal([]byte(messageJSON), &entry.Message); err != nil {
			return nil, fmt.Errorf("decode pending steering %q: %w", entry.ID, err)
		}
		if notificationJSON != "" {
			if err := json.Unmarshal([]byte(notificationJSON), &entry.NotificationOrigin); err != nil {
				return nil, err
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending steering: %w", err)
	}
	return entries, nil
}
