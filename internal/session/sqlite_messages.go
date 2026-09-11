package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
)

func (s *SQLiteStore) AddMessage(ctx context.Context, sessionID string, msg *Message) error {
	_, err := s.AddMessageWithTranscriptRev(ctx, sessionID, msg)
	return err
}

// SupportsBatchTranscriptWriter reports whether this store can commit the
// ordered writable transaction required by collaborative terminal activity.
func (s *SQLiteStore) SupportsBatchTranscriptWriter() bool {
	return s != nil && !s.ReadOnly()
}

// AppendMessagesWithTranscriptRev atomically appends messages in input order,
// allocates consecutive sequences, and bumps transcript_rev once.
func (s *SQLiteStore) AppendMessagesWithTranscriptRev(ctx context.Context, sessionID string, messages []*Message) (int64, error) {
	return s.appendMessagesWithRush(ctx, sessionID, messages, nil)
}

func (s *SQLiteStore) appendMessagesWithRush(ctx context.Context, sessionID string, messages []*Message, rush *RushOperation) (int64, error) {
	if len(messages) == 0 {
		state, err := s.GetResponseRunStartState(ctx, sessionID)
		return state.Rev, err
	}
	parts := make([]string, len(messages))
	now := time.Now()
	for i, msg := range messages {
		if msg == nil {
			return 0, fmt.Errorf("append messages: nil message at index %d", i)
		}
		msg.SessionID = sessionID
		if msg.CreatedAt.IsZero() {
			msg.CreatedAt = now
		}
		encoded, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
		if err != nil {
			return 0, fmt.Errorf("serialize message %d parts: %w", i, err)
		}
		parts[i] = encoded
	}
	var committedRev int64
	err := retryOnBusy(ctx, 5, func() error {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("get connection: %w", err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return fmt.Errorf("begin immediate transaction: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			}
		}()
		if rush != nil {
			// This CAS serializes Stop with initial-input/provider authorization.
			result, err := conn.ExecContext(ctx, `UPDATE session_rush_operations SET status='started',revision=revision+1,updated_at=CURRENT_TIMESTAMP WHERE session_id=? AND request_id=? AND status='starting' AND revision=? AND owner_fence=? AND replacement_response_id=?`, sessionID, rush.RequestID, rush.Revision, rush.Fence, rush.ReplacementResponseID)
			if err != nil {
				return err
			}
			n, _ := result.RowsAffected()
			if n != 1 {
				return ErrSteeringConflict
			}
		}
		var maxSeq sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT MAX(sequence) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
			return fmt.Errorf("get max sequence: %w", err)
		}
		firstSequence := 0
		if maxSeq.Valid {
			firstSequence = int(maxSeq.Int64) + 1
		}
		ids := make([]int64, len(messages))
		userTurns := 0
		bumpLast := false
		lastActivity := now
		lastUserActivity := now
		for i, msg := range messages {
			sequence := firstSequence + i
			result, err := conn.ExecContext(ctx, `
				INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail, client_message_id, response_id, assistant_segment_ordinal, segment_start_sequence, segment_end_sequence)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sessionID, string(msg.Role), parts[i], msg.TextContent, msg.DurationMs, msg.TurnIndex, msg.CreatedAt, sequence, msg.CompactionTail,
				msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence)
			if err != nil {
				return fmt.Errorf("insert message %d: %w", i, err)
			}
			ids[i], _ = result.LastInsertId()
			if msg.Role == "user" && !msg.CompactionTail {
				userTurns++
				lastUserActivity = msg.CreatedAt
				if msg.ClientMessageID != "" {
					if _, err := conn.ExecContext(ctx, `DELETE FROM session_pending_steering WHERE session_id = ? AND id = ?`, sessionID, msg.ClientMessageID); err != nil {
						return fmt.Errorf("delete pending steering: %w", err)
					}
				}
			}
			if !msg.CompactionTail && (msg.Role == "user" || msg.Role == "assistant") {
				bumpLast = true
				lastActivity = msg.CreatedAt
			}
		}
		if _, err := conn.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, user_turns = user_turns + ? WHERE id = ?`, now, userTurns, sessionID); err != nil {
			return fmt.Errorf("update session metrics: %w", err)
		}
		if bumpLast && s.hasLastMessageAt {
			if _, err := conn.ExecContext(ctx, `UPDATE sessions SET last_message_at = ? WHERE id = ?`, lastActivity, sessionID); err != nil {
				return fmt.Errorf("update last message time: %w", err)
			}
		}
		if userTurns > 0 && s.hasLastUserMessageAt {
			if _, err := conn.ExecContext(ctx, `UPDATE sessions SET last_user_message_at = ? WHERE id = ?`, lastUserActivity, sessionID); err != nil {
				return fmt.Errorf("update last user message time: %w", err)
			}
		}
		rev, err := s.bumpTranscriptRev(ctx, conn, sessionID)
		if err != nil {
			return err
		}
		if err := checkpointResponseRunFenceTx(ctx, conn, sessionID, rev); err != nil {
			return err
		}
		if rush != nil {
			if _, err := conn.ExecContext(ctx, `UPDATE session_rush_entries SET disposition='committed' WHERE session_id=? AND request_id=?`, sessionID, rush.RequestID); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}
		committed = true
		for i, msg := range messages {
			msg.ID = ids[i]
			msg.Sequence = firstSequence + i
		}
		committedRev = rev
		return nil
	})
	return committedRev, err
}

// AddMessageWithTranscriptRev adds a message and returns the revision bumped by
// the same transaction.
func (s *SQLiteStore) AddMessageWithTranscriptRev(ctx context.Context, sessionID string, msg *Message) (int64, error) {
	msg.SessionID = sessionID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
	if err != nil {
		return 0, fmt.Errorf("serialize parts: %w", err)
	}

	autoSequence := msg.Sequence < 0
	var committedRev int64
	err = retryOnBusy(ctx, 5, func() error {
		sequence := msg.Sequence
		var id int64
		var rev int64
		var err error
		if autoSequence {
			id, sequence, rev, err = s.addMessageAutoSequence(ctx, sessionID, msg, partsJSON)
		} else {
			id, rev, err = s.addMessageExplicitSequence(ctx, sessionID, msg, partsJSON, sequence)
		}
		if err != nil {
			return err
		}
		msg.ID = id
		msg.Sequence = sequence
		committedRev = rev
		return nil
	})
	return committedRev, err
}

func (s *SQLiteStore) addMessageExplicitSequence(ctx context.Context, sessionID string, msg *Message, partsJSON string, sequence int) (int64, int64, error) {
	// Use transaction for atomic insert/session timestamp update.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	id, rev, err := s.insertMessageAndBumpSession(ctx, tx, sessionID, msg, partsJSON, sequence)
	if err != nil {
		return 0, 0, err
	}
	if err := checkpointResponseRunFenceTx(ctx, tx, sessionID, rev); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit transaction: %w", err)
	}
	return id, rev, nil
}

func (s *SQLiteStore) addMessageAutoSequence(ctx context.Context, sessionID string, msg *Message, partsJSON string) (int64, int, int64, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, 0, 0, fmt.Errorf("begin immediate transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var maxSeq sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(sequence) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return 0, 0, 0, fmt.Errorf("get max sequence: %w", err)
	}
	sequence := 0
	if maxSeq.Valid {
		sequence = int(maxSeq.Int64) + 1
	}

	id, rev, err := s.insertMessageAndBumpSession(ctx, conn, sessionID, msg, partsJSON, sequence)
	if err != nil {
		return 0, 0, 0, err
	}
	if err := checkpointResponseRunFenceTx(ctx, conn, sessionID, rev); err != nil {
		return 0, 0, 0, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, 0, 0, fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return id, sequence, rev, nil
}

type sqliteExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type sqliteQueryExecer interface {
	sqliteExecer
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *SQLiteStore) bumpTranscriptRev(ctx context.Context, execer sqliteQueryExecer, sessionID string) (int64, error) {
	if !s.hasTranscriptRev {
		return 0, nil
	}
	// session_redo was introduced after transcript revisions. Keep invalidation
	// below the compatibility guard so old read-only schemas never query a table
	// they cannot have.
	if _, err := execer.ExecContext(ctx, `DELETE FROM session_redo WHERE session_id = ?`, sessionID); err != nil {
		return 0, fmt.Errorf("invalidate transcript redo: %w", err)
	}
	return s.bumpTranscriptRevPreservingRedo(ctx, execer, sessionID)
}

func (s *SQLiteStore) bumpTranscriptRevPreservingRedo(ctx context.Context, execer sqliteQueryExecer, sessionID string) (int64, error) {
	if !s.hasTranscriptRev {
		return 0, nil
	}
	var rev int64
	err := execer.QueryRowContext(ctx, `
		UPDATE sessions
		SET transcript_rev = transcript_rev + 1
		WHERE id = ?
		RETURNING transcript_rev`, sessionID).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("bump transcript revision: %w", err)
	}
	return rev, nil
}

func (s *SQLiteStore) insertMessageAndBumpSession(ctx context.Context, execer sqliteQueryExecer, sessionID string, msg *Message, partsJSON string, sequence int) (int64, int64, error) {
	result, err := execer.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail, client_message_id, response_id, assistant_segment_ordinal, segment_start_sequence, segment_end_sequence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.TurnIndex, msg.CreatedAt, sequence, msg.CompactionTail,
		msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence)
	if err != nil {
		return 0, 0, fmt.Errorf("insert message: %w", err)
	}
	id, _ := result.LastInsertId()

	// Update session's updated_at. Preserve the existing sidebar activity
	// semantics here: user/assistant role rows bump last_message_at, while
	// tool/developer/system/event rows do not. message_count is maintained by
	// triggers with the stricter chat-bubble predicate above, so tool-call-only
	// assistant rows may affect activity ordering without increasing the count.
	bumpLastMessageAt := s.hasLastMessageAt && !msg.CompactionTail && (msg.Role == "user" || msg.Role == "assistant")
	if bumpLastMessageAt {
		_, err = execer.ExecContext(ctx,
			"UPDATE sessions SET updated_at = ?, last_message_at = ? WHERE id = ?",
			time.Now(), msg.CreatedAt, sessionID)
	} else {
		_, err = execer.ExecContext(ctx, "UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now(), sessionID)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("update session timestamp: %w", err)
	}
	rev, err := s.bumpTranscriptRev(ctx, execer, sessionID)
	if err != nil {
		return 0, 0, err
	}
	return id, rev, nil
}

// UpdateMessage replaces the content of an existing message (keyed by msg.ID
// within sessionID). Returns ErrNotFound if no row matches. Used by the
// "persist as we go" upsert path: the caller first calls AddMessage to stamp
// an ID, then subsequent snapshots call UpdateMessage with the same ID.
func (s *SQLiteStore) UpdateMessage(ctx context.Context, sessionID string, msg *Message) error {
	_, err := s.updateMessage(ctx, sessionID, msg, true)
	return err
}

// UpdateStreamingMessage updates an in-progress assistant message while letting
// the caller defer the FTS-backed text_content rewrite until finalization.
func (s *SQLiteStore) UpdateStreamingMessage(ctx context.Context, sessionID string, msg *Message, finalizeText bool) error {
	_, err := s.UpdateStreamingMessageWithTranscriptRev(ctx, sessionID, msg, finalizeText)
	return err
}

func (s *SQLiteStore) UpdateStreamingMessageWithTranscriptRev(ctx context.Context, sessionID string, msg *Message, finalizeText bool) (int64, error) {
	return s.updateMessage(ctx, sessionID, msg, finalizeText)
}

func (s *SQLiteStore) updateMessage(ctx context.Context, sessionID string, msg *Message, updateText bool) (int64, error) {
	if msg == nil {
		return 0, fmt.Errorf("update message: nil msg")
	}
	if msg.ID == 0 {
		return 0, fmt.Errorf("update message: missing id")
	}

	partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
	if err != nil {
		return 0, fmt.Errorf("serialize parts: %w", err)
	}

	query := `
			UPDATE messages
			SET role = ?, parts = ?`
	args := []any{string(msg.Role), partsJSON}
	if updateText {
		query += `, text_content = ?`
		args = append(args, msg.TextContent)
	}
	query += `, duration_ms = ?, turn_index = ?, compaction_tail = ?, client_message_id = ?, response_id = ?, assistant_segment_ordinal = ?, segment_start_sequence = ?, segment_end_sequence = ?
			WHERE id = ? AND session_id = ?`
	args = append(args, msg.DurationMs, msg.TurnIndex, msg.CompactionTail, msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence, msg.ID, sessionID)

	var committedRev int64
	err = retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("update message: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected: %w", err)
		}
		if rowsAffected == 0 {
			return ErrNotFound
		}

		// Bump session updated_at so sidebar sort reflects the snapshot.
		// Intentionally do NOT touch last_message_at — the message was
		// already counted at AddMessage time; updates shouldn't re-order.
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now(), sessionID); err != nil {
			return fmt.Errorf("update session timestamp: %w", err)
		}
		rev, err := s.bumpTranscriptRev(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if err := checkpointResponseRunFenceTx(ctx, tx, sessionID, rev); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}
		committedRev = rev
		return nil
	})
	return committedRev, err
}

func (s *SQLiteStore) PersistCompactionTailHints(ctx context.Context, sessionID string, messageIDs []int64) error {
	if !s.hasMessageCompactionTail || len(messageIDs) == 0 {
		return nil
	}

	args := make([]any, 0, len(messageIDs)+1)
	args = append(args, sessionID)
	placeholders := make([]string, 0, len(messageIDs))
	seen := make(map[int64]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(placeholders) == 0 {
		return nil
	}

	query := `
		UPDATE messages
		SET compaction_tail = TRUE
		WHERE session_id = ?
		  AND COALESCE(compaction_tail, FALSE) = FALSE
		  AND id IN (` + strings.Join(placeholders, ",") + `)`

	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("persist compaction tail hints: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count compaction tail hints: %w", err)
		}
		if changed == 0 {
			return tx.Commit()
		}
		if _, err := s.bumpTranscriptRev(ctx, tx, sessionID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *SQLiteStore) ClearCompactionBoundary(ctx context.Context, id string) error {
	if !s.hasCompactionSeq {
		return nil
	}
	query := "UPDATE sessions SET compaction_seq = -1, updated_at = ? WHERE id = ?"
	if s.hasCompactionCount {
		query = "UPDATE sessions SET compaction_seq = -1, compaction_count = 0, updated_at = ? WHERE id = ?"
	}
	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, query, time.Now(), id)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count cleared compaction boundaries: %w", err)
		}
		if changed > 0 {
			if _, err := s.bumpTranscriptRev(ctx, tx, id); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// ReplaceMessages reconciles the complete persisted history for a session.
// It preserves any unchanged prefix and rewrites only the changed suffix, avoiding
// a DELETE+INSERT of long histories when serve/web persists an appended snapshot.
// Because the replacement snapshot becomes the complete persisted history, any
// previous compaction boundary is cleared.
func (s *SQLiteStore) ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error {
	_, err := s.ReplaceMessagesWithTranscriptRev(ctx, sessionID, messages)
	return err
}

func (s *SQLiteStore) ReplaceMessagesWithTranscriptRev(ctx context.Context, sessionID string, messages []Message) (int64, error) {
	partsJSON, err := prepareReplacementMessageParts(messages, s.cfg.StripImageBase64)
	if err != nil {
		return 0, err
	}

	var committedRev int64
	err = retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		commonPrefix, fullDelete, err := replacementCommonPrefix(ctx, tx, sessionID, messages, partsJSON)
		if err != nil {
			return err
		}

		// Drop the first changed row and any stale suffix. Unchanged prefix rows keep
		// their rowids, FTS entries, and created_at values; triggers update FTS and
		// message_count only for rows that actually changed. If legacy/corrupt rows
		// have duplicate or negative sequence numbers, fall back to the old full
		// rewrite so replacement remains a complete-history operation.
		if fullDelete {
			if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
				return fmt.Errorf("delete existing messages: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ? AND sequence >= ?", sessionID, commonPrefix); err != nil {
			return fmt.Errorf("delete changed messages: %w", err)
		}

		if commonPrefix < len(messages) {
			insertStmt, err := tx.PrepareContext(ctx, `
				INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail, client_message_id, response_id, assistant_segment_ordinal, segment_start_sequence, segment_end_sequence)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return fmt.Errorf("prepare message insert: %w", err)
			}
			defer insertStmt.Close()

			for i := commonPrefix; i < len(messages); i++ {
				msg := messages[i]
				createdAt := msg.CreatedAt
				if createdAt.IsZero() {
					createdAt = time.Now()
				}
				_, err = insertStmt.ExecContext(ctx,
					sessionID, string(msg.Role), partsJSON[i], msg.TextContent, msg.DurationMs, msg.TurnIndex, createdAt, i, false,
					msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence)
				if err != nil {
					return fmt.Errorf("insert message %d: %w", i, err)
				}
			}
		}

		// Update session activity metadata and clear any stale compaction boundary. A
		// replace is a full-history rewrite; keeping an old compaction_seq could make
		// future active-context loads start past the end of the rewritten rows. Compute
		// activity from the persisted rows after the rewrite so unchanged prefixes keep
		// their original created_at values instead of freshly rebuilt snapshot times.
		now := time.Now()
		if err := s.updateReplaceMessagesSessionMetadata(ctx, tx, sessionID, now, true); err != nil {
			return err
		}
		rev, err := s.bumpTranscriptRev(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if err := checkpointResponseRunFenceTx(ctx, tx, sessionID, rev); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		committedRev = rev
		return nil
	})
	return committedRev, err
}

// ReplaceCompactedMessages reconciles the active post-compaction history for a
// session while preserving pre-compaction scrollback and the compaction boundary.
// It must only be used with snapshots that start at the current compaction_seq.
func (s *SQLiteStore) ReplaceCompactedMessages(ctx context.Context, sessionID string, messages []Message) error {
	_, err := s.ReplaceCompactedMessagesWithTranscriptRev(ctx, sessionID, messages)
	return err
}

func (s *SQLiteStore) ReplaceCompactedMessagesWithTranscriptRev(ctx context.Context, sessionID string, messages []Message) (int64, error) {
	partsJSON, err := prepareReplacementMessageParts(messages, s.cfg.StripImageBase64)
	if err != nil {
		return 0, err
	}

	var committedRev int64
	err = retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		startSeq, err := s.compactionStartSeq(ctx, tx, sessionID)
		if err != nil {
			return err
		}

		commonPrefix, fullActiveDelete, err := compactedReplacementCommonPrefix(ctx, tx, sessionID, startSeq, messages, partsJSON)
		if err != nil {
			return err
		}

		deleteFrom := startSeq + commonPrefix
		if fullActiveDelete {
			commonPrefix = 0
			deleteFrom = startSeq
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ? AND sequence >= ?", sessionID, deleteFrom); err != nil {
			return fmt.Errorf("delete compacted messages: %w", err)
		}

		if commonPrefix < len(messages) {
			insertStmt, err := tx.PrepareContext(ctx, `
				INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail, client_message_id, response_id, assistant_segment_ordinal, segment_start_sequence, segment_end_sequence)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return fmt.Errorf("prepare compacted message insert: %w", err)
			}
			defer insertStmt.Close()

			for i := commonPrefix; i < len(messages); i++ {
				msg := messages[i]
				createdAt := msg.CreatedAt
				if createdAt.IsZero() {
					createdAt = time.Now()
				}
				_, err = insertStmt.ExecContext(ctx,
					sessionID, string(msg.Role), partsJSON[i], msg.TextContent, msg.DurationMs, msg.TurnIndex, createdAt, startSeq+i, msg.CompactionTail,
					msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence)
				if err != nil {
					return fmt.Errorf("insert compacted message %d: %w", i, err)
				}
			}
		}

		now := time.Now()
		if err := s.updateReplaceMessagesSessionMetadata(ctx, tx, sessionID, now, false); err != nil {
			return err
		}
		rev, err := s.bumpTranscriptRev(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if err := checkpointResponseRunFenceTx(ctx, tx, sessionID, rev); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		committedRev = rev
		return nil
	})
	return committedRev, err
}

func (s *SQLiteStore) compactionStartSeq(ctx context.Context, tx *sql.Tx, sessionID string) (int, error) {
	if !s.hasCompactionSeq {
		return 0, fmt.Errorf("replace compacted messages: compaction boundary unsupported")
	}

	var startSeq int
	if s.hasCompactionCount {
		var compactionCount int
		err := tx.QueryRowContext(ctx, "SELECT compaction_seq, COALESCE(compaction_count, 0) FROM sessions WHERE id = ?", sessionID).Scan(&startSeq, &compactionCount)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("load compaction boundary: %w", err)
		}
		if startSeq < 0 || (startSeq == 0 && compactionCount <= 0) {
			return 0, fmt.Errorf("replace compacted messages: session %s has no compaction boundary", sessionID)
		}
		return startSeq, nil
	}

	err := tx.QueryRowContext(ctx, "SELECT compaction_seq FROM sessions WHERE id = ?", sessionID).Scan(&startSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("load compaction boundary: %w", err)
	}
	if startSeq < 0 {
		return 0, fmt.Errorf("replace compacted messages: session %s has no compaction boundary", sessionID)
	}
	return startSeq, nil
}

func compactedReplacementCommonPrefix(ctx context.Context, tx *sql.Tx, sessionID string, startSeq int, desired []Message, desiredPartsJSON []string) (prefix int, fullActiveDelete bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, role, parts, text_content, duration_ms, turn_index,
		       COALESCE(client_message_id, ''), COALESCE(response_id, ''), COALESCE(assistant_segment_ordinal, -1),
		       COALESCE(segment_start_sequence, 0), COALESCE(segment_end_sequence, 0)
		FROM messages
		WHERE session_id = ? AND sequence >= ?
		ORDER BY sequence ASC, id ASC`, sessionID, startSeq)
	if err != nil {
		return 0, false, fmt.Errorf("query compacted messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sequence int
		var role, partsJSON string
		var textContent sql.NullString
		var durationMs sql.NullInt64
		var turnIndex sql.NullInt64
		var clientMessageID, responseID string
		var assistantSegmentOrdinal int
		var segmentStartSequence, segmentEndSequence int64
		if err := rows.Scan(&sequence, &role, &partsJSON, &textContent, &durationMs, &turnIndex,
			&clientMessageID, &responseID, &assistantSegmentOrdinal, &segmentStartSequence, &segmentEndSequence); err != nil {
			return 0, false, fmt.Errorf("scan compacted message: %w", err)
		}
		if sequence < startSeq {
			return 0, true, nil
		}

		expectedSeq := startSeq + prefix
		if prefix >= len(desired) {
			if sequence < expectedSeq {
				return 0, true, nil
			}
			break
		}
		if sequence != expectedSeq {
			if sequence < expectedSeq {
				return 0, true, nil
			}
			break
		}

		want := desired[prefix]
		if role != string(want.Role) ||
			partsJSON != desiredPartsJSON[prefix] ||
			nullStringValue(textContent) != want.TextContent ||
			nullInt64Value(durationMs) != want.DurationMs ||
			int(nullInt64Value(turnIndex)) != want.TurnIndex ||
			clientMessageID != want.ClientMessageID ||
			responseID != want.ResponseID ||
			assistantSegmentOrdinal != want.AssistantSegmentOrdinal ||
			segmentStartSequence != want.SegmentStartSequence ||
			segmentEndSequence != want.SegmentEndSequence {
			break
		}
		prefix++
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return prefix, false, nil
}

func (s *SQLiteStore) updateReplaceMessagesSessionMetadata(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time, clearCompactionBoundary bool) error {
	setClauses := []string{
		"updated_at = ?",
	}
	args := []any{now}
	if clearCompactionBoundary {
		setClauses = append(setClauses, "user_turns = (SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'user')")
		args = append(args, sessionID)
	}

	if s.hasLastUserMessageAt {
		// Preserve existing user-activity semantics: every persisted user row counts,
		// including any internal context-summary user rows that ReplaceMessages is
		// asked to make part of the complete transcript.
		setClauses = append(setClauses, "last_user_message_at = (SELECT MAX(created_at) FROM messages WHERE session_id = ? AND role = 'user')")
		args = append(args, sessionID)
	}
	if s.hasLastMessageAt {
		// last_message_at drives visible conversation activity and matches
		// insertMessageAndBumpSession: user/assistant rows count unless they are
		// retained compaction-tail context.
		visibleTailClause := ""
		if s.hasMessageCompactionTail {
			visibleTailClause = " AND COALESCE(compaction_tail, FALSE) = FALSE"
		}
		setClauses = append(setClauses, "last_message_at = (SELECT MAX(created_at) FROM messages WHERE session_id = ? AND role IN ('user', 'assistant')"+visibleTailClause+")")
		args = append(args, sessionID)
	}
	if clearCompactionBoundary {
		if s.hasCompactionSeq {
			setClauses = append(setClauses, "compaction_seq = -1")
		}
		if s.hasCompactionCount {
			setClauses = append(setClauses, "compaction_count = 0")
		}
	}

	args = append(args, sessionID)
	query := "UPDATE sessions SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update session replacement metadata: %w", err)
	}
	return nil
}

func prepareReplacementMessageParts(messages []Message, stripImageBase64 bool) ([]string, error) {
	partsJSON := make([]string, len(messages))
	for i := range messages {
		parts, err := messages[i].PartsJSONForStorage(stripImageBase64)
		if err != nil {
			return nil, fmt.Errorf("serialize parts for message %d: %w", i, err)
		}
		partsJSON[i] = parts
	}
	return partsJSON, nil
}

func replacementCommonPrefix(ctx context.Context, tx *sql.Tx, sessionID string, desired []Message, desiredPartsJSON []string) (prefix int, fullDelete bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, role, parts, text_content, duration_ms, turn_index, COALESCE(compaction_tail, FALSE),
		       COALESCE(client_message_id, ''), COALESCE(response_id, ''), COALESCE(assistant_segment_ordinal, -1),
		       COALESCE(segment_start_sequence, 0), COALESCE(segment_end_sequence, 0)
		FROM messages
		WHERE session_id = ?
		ORDER BY sequence ASC, id ASC`, sessionID)
	if err != nil {
		return 0, false, fmt.Errorf("query existing messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sequence int
		var role, partsJSON string
		var textContent sql.NullString
		var durationMs sql.NullInt64
		var turnIndex sql.NullInt64
		var compactionTail bool
		var clientMessageID, responseID string
		var assistantSegmentOrdinal int
		var segmentStartSequence, segmentEndSequence int64
		if err := rows.Scan(&sequence, &role, &partsJSON, &textContent, &durationMs, &turnIndex, &compactionTail,
			&clientMessageID, &responseID, &assistantSegmentOrdinal, &segmentStartSequence, &segmentEndSequence); err != nil {
			return 0, false, fmt.Errorf("scan existing message: %w", err)
		}
		if sequence < 0 {
			return 0, true, nil
		}

		if prefix >= len(desired) {
			if sequence < prefix {
				return 0, true, nil
			}
			break
		}
		if sequence != prefix {
			// A duplicate/out-of-order sequence below the already accepted prefix
			// cannot be removed by deleting sequence >= prefix, so fall back to a full
			// rewrite. Gaps above the prefix are safely truncated from the gap onward.
			if sequence < prefix {
				return 0, true, nil
			}
			break
		}

		want := desired[prefix]
		if role != string(want.Role) ||
			partsJSON != desiredPartsJSON[prefix] ||
			nullStringValue(textContent) != want.TextContent ||
			nullInt64Value(durationMs) != want.DurationMs ||
			int(nullInt64Value(turnIndex)) != want.TurnIndex ||
			compactionTail ||
			clientMessageID != want.ClientMessageID ||
			responseID != want.ResponseID ||
			assistantSegmentOrdinal != want.AssistantSegmentOrdinal ||
			segmentStartSequence != want.SegmentStartSequence ||
			segmentEndSequence != want.SegmentEndSequence {
			break
		}
		prefix++
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return prefix, false, nil
}

func nullStringValue(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func nullInt64Value(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

// CompactMessages appends compacted messages to the session, preserving old
// history, and updates compaction_seq so that resume loads only post-compaction
// messages. Old messages remain in the database for scrollback/history.
func (s *SQLiteStore) CompactMessages(ctx context.Context, sessionID string, messages []Message) error {
	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		// Find the current max sequence number
		var maxSeq int
		err = tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(sequence), -1) FROM messages WHERE session_id = ?",
			sessionID).Scan(&maxSeq)
		if err != nil {
			return fmt.Errorf("get max sequence: %w", err)
		}
		startSeq := maxSeq + 1

		insertStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail, client_message_id, response_id, assistant_segment_ordinal, segment_start_sequence, segment_end_sequence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare message insert: %w", err)
		}
		defer insertStmt.Close()

		// Insert new messages starting after the existing ones
		for i, msg := range messages {
			msg.SessionID = sessionID
			msg.Sequence = startSeq + i
			if msg.CreatedAt.IsZero() {
				msg.CreatedAt = time.Now()
			}

			partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
			if err != nil {
				return fmt.Errorf("serialize parts for message %d: %w", i, err)
			}

			_, err = insertStmt.ExecContext(ctx,
				sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.TurnIndex, msg.CreatedAt, msg.Sequence, msg.CompactionTail,
				msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence)
			if err != nil {
				return fmt.Errorf("insert message %d: %w", i, err)
			}
		}

		// Update compaction boundary/count and timestamp. Older read-only schemas
		// cannot reach this write path because migrations run before opening.
		now := time.Now()
		if s.hasCompactionCount {
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET compaction_seq = ?, compaction_count = COALESCE(compaction_count, 0) + 1, updated_at = ? WHERE id = ?",
				startSeq, now, sessionID); err != nil {
				return fmt.Errorf("update compaction metrics: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET compaction_seq = ?, updated_at = ? WHERE id = ?",
			startSeq, now, sessionID); err != nil {
			return fmt.Errorf("update compaction_seq: %w", err)
		}
		if _, err := s.bumpTranscriptRev(ctx, tx, sessionID); err != nil {
			return err
		}

		return tx.Commit()
	})
}

func (s *SQLiteStore) messageSelectCols() string {
	compactionTailCol := "FALSE AS compaction_tail"
	if s.hasMessageCompactionTail {
		compactionTailCol = "COALESCE(compaction_tail, FALSE) AS compaction_tail"
	}
	clientMessageIDCol := "'' AS client_message_id"
	if s.hasMessageClientID {
		clientMessageIDCol = "COALESCE(client_message_id, '') AS client_message_id"
	}
	streamIdentityCols := "'' AS response_id, -1 AS assistant_segment_ordinal, 0 AS segment_start_sequence, 0 AS segment_end_sequence"
	if s.hasMessageStreamIdentity {
		streamIdentityCols = "COALESCE(response_id, '') AS response_id, COALESCE(assistant_segment_ordinal, -1) AS assistant_segment_ordinal, COALESCE(segment_start_sequence, 0) AS segment_start_sequence, COALESCE(segment_end_sequence, 0) AS segment_end_sequence"
	}
	return `id, session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, ` + compactionTailCol + `, ` + clientMessageIDCol + `, ` + streamIdentityCols
}

// TranscriptVersioned reports whether this database has durable transcript
// revisions. It is false only for old schemas opened read-only without migration.
func (s *SQLiteStore) TranscriptVersioned() bool {
	return s != nil && s.hasTranscriptRev
}

// SessionSummariesIncludeTranscriptRev reports whether List can read revisions
// from the sessions row without compatibility fallbacks.
func (s *SQLiteStore) SessionSummariesIncludeTranscriptRev() bool {
	return s != nil && s.hasTranscriptRev
}

// TranscriptRev returns the current durable transcript revision. Old read-only
// databases without the revision column are explicitly unversioned (revision 0).
func (s *SQLiteStore) TranscriptRev(ctx context.Context, sessionID string) (int64, error) {
	if !s.hasTranscriptRev {
		var exists int
		err := s.queryDB().QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", sessionID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	var rev int64
	err := s.queryDB().QueryRowContext(ctx, "SELECT transcript_rev FROM sessions WHERE id = ?", sessionID).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get transcript revision: %w", err)
	}
	return rev, nil
}

type undoRedoMetadata struct {
	GeneratedShortTitle string             `json:"generated_short_title,omitempty"`
	GeneratedLongTitle  string             `json:"generated_long_title,omitempty"`
	TitleSource         SessionTitleSource `json:"title_source,omitempty"`
	TitleGeneratedAt    time.Time          `json:"title_generated_at,omitempty"`
	TitleBasisMsgSeq    int                `json:"title_basis_msg_seq,omitempty"`
	TitleSkippedAt      time.Time          `json:"title_skipped_at,omitempty"`
}

type sqliteQueryRowsExecer interface {
	sqliteQueryExecer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func transcriptMutationState(ctx context.Context, q sqliteQueryExecer, sessionID string) (TranscriptMutationState, error) {
	var state TranscriptMutationState
	err := q.QueryRowContext(ctx, `
		SELECT transcript_rev, COALESCE((
			SELECT id FROM messages
			WHERE session_id = sessions.id AND role NOT IN ('system', 'developer')
			ORDER BY sequence DESC, id DESC LIMIT 1
		), 0)
		FROM sessions WHERE id = ?`, sessionID).Scan(&state.Rev, &state.HeadID)
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptMutationState{}, ErrNotFound
	}
	if err != nil {
		return TranscriptMutationState{}, fmt.Errorf("read transcript mutation state: %w", err)
	}
	return state, nil
}

// TranscriptMutationState returns the durable revision and visible transcript
// head used for optimistic undo/redo requests.
func (s *SQLiteStore) TranscriptMutationState(ctx context.Context, sessionID string) (TranscriptMutationState, error) {
	if !s.hasTranscriptRev {
		return TranscriptMutationState{}, ErrTranscriptRevisionUnsupported
	}
	return transcriptMutationState(ctx, s.db, sessionID)
}

func requireTranscriptMutationState(actual, expected TranscriptMutationState) error {
	if actual != expected {
		return ErrTranscriptConflict
	}
	return nil
}

func (s *SQLiteStore) beginImmediate(ctx context.Context) (*sql.Conn, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("begin immediate transaction: %w", err)
	}
	return conn, nil
}

func rollbackImmediate(conn *sql.Conn) {
	if conn != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	}
}

func (s *SQLiteStore) loadUndoRedoMetadata(ctx context.Context, q sqliteQueryExecer, sessionID string) (undoRedoMetadata, error) {
	var metadata undoRedoMetadata
	var titleSource sql.NullString
	var titleGeneratedAt, titleSkippedAt sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(generated_short_title, ''), COALESCE(generated_long_title, ''), title_source,
		       title_generated_at, COALESCE(title_basis_msg_seq, 0), title_skipped_at
		FROM sessions WHERE id = ?`, sessionID).Scan(
		&metadata.GeneratedShortTitle, &metadata.GeneratedLongTitle, &titleSource,
		&titleGeneratedAt, &metadata.TitleBasisMsgSeq, &titleSkippedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return undoRedoMetadata{}, ErrNotFound
	}
	if err != nil {
		return undoRedoMetadata{}, fmt.Errorf("load undo metadata: %w", err)
	}
	metadata.TitleSource = SessionTitleSource(titleSource.String)
	if titleGeneratedAt.Valid {
		metadata.TitleGeneratedAt = titleGeneratedAt.Time
	}
	if titleSkippedAt.Valid {
		metadata.TitleSkippedAt = titleSkippedAt.Time
	}
	return metadata, nil
}

func (s *SQLiteStore) recomputeTranscriptMetadata(ctx context.Context, q sqliteQueryExecer, sessionID string, clearGeneratedTitle bool) error {
	tailClause := ""
	if s.hasMessageCompactionTail {
		tailClause = " AND COALESCE(compaction_tail, FALSE) = FALSE"
	}
	realUser := realUserMessageSQL("", s.hasMessageCompactionTail)
	var firstUserText sql.NullString
	err := q.QueryRowContext(ctx, "SELECT text_content FROM messages WHERE session_id = ? AND "+realUser+" ORDER BY sequence, id LIMIT 1", sessionID).Scan(&firstUserText)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load transcript summary source: %w", err)
	}
	summary := ""
	if firstUserText.Valid {
		summary = TruncateSummary(firstUserText.String)
	}
	set := []string{
		"updated_at = ?",
		"summary = ?",
		"last_total_tokens = 0",
		"last_message_count = 0",
	}
	args := []any{time.Now(), summary}
	// user_turns is an increment-only activity metric. Undo/redo changes the
	// visible transcript, but must not collapse turns represented by compacted
	// history or make the metric decrease when a prompt is temporarily undone.
	if s.hasLastUserMessageAt {
		// Preserve the documented activity semantics: every persisted user row
		// counts, including compaction summaries and retained compaction tails.
		set = append(set, "last_user_message_at = (SELECT MAX(created_at) FROM messages WHERE session_id = ? AND role = 'user')")
		args = append(args, sessionID)
	}
	if s.hasLastMessageAt {
		set = append(set, "last_message_at = (SELECT MAX(created_at) FROM messages WHERE session_id = ? AND role IN ('user', 'assistant')"+tailClause+")")
		args = append(args, sessionID)
	}
	if clearGeneratedTitle && s.hasGeneratedTitles {
		set = append(set,
			"generated_short_title = ''", "generated_long_title = ''",
			"title_source = CASE WHEN title_source = 'user' THEN title_source ELSE '' END",
			"title_generated_at = NULL", "title_basis_msg_seq = 0", "title_skipped_at = NULL")
	}
	args = append(args, sessionID)
	result, err := q.ExecContext(ctx, "UPDATE sessions SET "+strings.Join(set, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return fmt.Errorf("recompute transcript metadata: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count transcript metadata update: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) restoreRedoTitleMetadata(ctx context.Context, q sqliteQueryExecer, sessionID string, metadata undoRedoMetadata) error {
	if !s.hasGeneratedTitles {
		return nil
	}
	var currentSource sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT title_source FROM sessions WHERE id = ?`, sessionID).Scan(&currentSource); err != nil {
		return err
	}
	if SessionTitleSource(currentSource.String) == TitleSourceUser {
		return nil
	}
	_, err := q.ExecContext(ctx, `
		UPDATE sessions SET generated_short_title = ?, generated_long_title = ?, title_source = ?,
		       title_generated_at = ?, title_basis_msg_seq = ?, title_skipped_at = ?
		WHERE id = ?`, metadata.GeneratedShortTitle, metadata.GeneratedLongTitle, metadata.TitleSource,
		nullableTime(metadata.TitleGeneratedAt), metadata.TitleBasisMsgSeq, nullableTime(metadata.TitleSkippedAt), sessionID)
	return err
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (s *SQLiteStore) reconcilePlanProjection(ctx context.Context, q sqliteQueryRowsExecer, sessionID string) error {
	rows, err := q.QueryContext(ctx, `SELECT `+s.messageSelectCols()+` FROM messages WHERE session_id = ? ORDER BY sequence, id`, sessionID)
	if err != nil {
		return fmt.Errorf("load transcript for plan reconciliation: %w", err)
	}
	messages, err := scanMessageRows(rows)
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	llmMessages := make([]llm.Message, 0, len(messages))
	for i := range messages {
		llmMessages = append(llmMessages, messages[i].ToLLMMessage())
	}
	snapshot, represented := planpkg.LatestSuccessfulSnapshot(llmMessages)
	if !represented || len(snapshot.Plan) == 0 {
		if _, err := q.ExecContext(ctx, `DELETE FROM session_plans WHERE session_id = ?`, sessionID); err != nil {
			return fmt.Errorf("clear reconciled plan projection: %w", err)
		}
		return nil
	}
	raw, err := snapshot.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("encode reconciled plan projection: %w", err)
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO session_plans (session_id, snapshot, version, updated_at)
		VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(session_id) DO UPDATE SET
			snapshot = excluded.snapshot,
			version = session_plans.version + 1,
			updated_at = CURRENT_TIMESTAMP`, sessionID, string(raw)); err != nil {
		return fmt.Errorf("save reconciled plan projection: %w", err)
	}
	return nil
}

// UndoLastUserTurn removes the latest real user row at or after the compaction
// boundary and every transcript row after it. The suffix is durably owned by
// SQLite so another client or a restarted process can redo it.
func (s *SQLiteStore) UndoLastUserTurn(ctx context.Context, sessionID string, expected TranscriptMutationState) (TranscriptMutationResult, error) {
	if !s.hasTranscriptRev {
		return TranscriptMutationResult{}, ErrTranscriptRevisionUnsupported
	}
	conn, err := s.beginImmediate(ctx)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	defer rollbackImmediate(conn)

	actual, err := transcriptMutationState(ctx, conn, sessionID)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	if err := requireTranscriptMutationState(actual, expected); err != nil {
		return TranscriptMutationResult{}, err
	}
	metadata, err := s.loadUndoRedoMetadata(ctx, conn, sessionID)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	compactionSeq := -1
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(compaction_seq, -1) FROM sessions WHERE id = ?`, sessionID).Scan(&compactionSeq); err != nil {
		return TranscriptMutationResult{}, err
	}
	if compactionSeq < 0 {
		compactionSeq = 0
	}
	var targetID int64
	var targetSeq int
	var userText string
	realUserPredicate := realUserMessageSQL("", s.hasMessageCompactionTail)
	err = conn.QueryRowContext(ctx, `
		SELECT id, sequence FROM messages
		WHERE session_id = ? AND sequence >= ? AND `+realUserPredicate+`
		ORDER BY sequence DESC, id DESC LIMIT 1`, sessionID, compactionSeq).Scan(&targetID, &targetSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptMutationResult{}, ErrNothingToUndo
	}
	if err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("find undo user turn: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT `+s.messageSelectCols()+` FROM messages
		WHERE session_id = ? AND (sequence > ? OR (sequence = ? AND id >= ?))
		ORDER BY sequence, id`, sessionID, targetSeq, targetSeq, targetID)
	if err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("load undo suffix: %w", err)
	}
	suffix, err := scanMessageRows(rows)
	closeErr := rows.Close()
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	if closeErr != nil {
		return TranscriptMutationResult{}, closeErr
	}
	if len(suffix) == 0 || suffix[0].ID != targetID {
		return TranscriptMutationResult{}, fmt.Errorf("undo suffix lost target row")
	}
	attachmentsOmitted := false
	var composerText strings.Builder
	for _, part := range suffix[0].Parts {
		switch part.Type {
		case llm.PartText:
			composerText.WriteString(part.Text)
		case llm.PartImage, llm.PartFile:
			attachmentsOmitted = true
		}
	}
	userText = composerText.String()
	suffixJSON, err := json.Marshal(suffix)
	if err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("encode undo suffix: %w", err)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("encode undo metadata: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM messages WHERE session_id = ? AND (sequence > ? OR (sequence = ? AND id >= ?))`, sessionID, targetSeq, targetSeq, targetID); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("delete undo suffix: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM session_provider_state WHERE session_id = ?`, sessionID); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("reset provider state for undo: %w", err)
	}
	if err := s.recomputeTranscriptMetadata(ctx, conn, sessionID, true); err != nil {
		return TranscriptMutationResult{}, err
	}
	if err := s.reconcilePlanProjection(ctx, conn, sessionID); err != nil {
		return TranscriptMutationResult{}, err
	}
	rev, err := s.bumpTranscriptRevPreservingRedo(ctx, conn, sessionID)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	post, err := transcriptMutationState(ctx, conn, sessionID)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	if post.Rev != rev {
		return TranscriptMutationResult{}, fmt.Errorf("undo revision mismatch")
	}
	// Successive undo entries contain disjoint removed suffixes, so the durable
	// stack is naturally bounded by the removed transcript rather than an
	// arbitrary entry limit. Preserve every recovery point until redo consumes it
	// or an ordinary transcript mutation invalidates the whole stack.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO session_redo (session_id, stack_pos, suffix, metadata, created_at)
		SELECT ?, COALESCE(MAX(stack_pos), 0) + 1, ?, ?, CURRENT_TIMESTAMP
		FROM session_redo WHERE session_id = ?`,
		sessionID, string(suffixJSON), string(metadataJSON), sessionID); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("push redo suffix: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("commit undo: %w", err)
	}
	return TranscriptMutationResult{TranscriptMutationState: post, UserText: userText, AttachmentsOmitted: attachmentsOmitted}, nil
}

// RedoLastUserTurn restores the exact durable suffix captured by the latest
// undo, including stable message IDs, sequences, timestamps, and structured
// parts. It does not replay tool or external side effects.
func (s *SQLiteStore) RedoLastUserTurn(ctx context.Context, sessionID string, expected TranscriptMutationState) (TranscriptMutationResult, error) {
	if !s.hasTranscriptRev {
		return TranscriptMutationResult{}, ErrTranscriptRevisionUnsupported
	}
	conn, err := s.beginImmediate(ctx)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	defer rollbackImmediate(conn)
	actual, err := transcriptMutationState(ctx, conn, sessionID)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	if err := requireTranscriptMutationState(actual, expected); err != nil {
		return TranscriptMutationResult{}, err
	}
	var stackPos int64
	var suffixRaw, metadataRaw string
	err = conn.QueryRowContext(ctx, `
		SELECT stack_pos, suffix, metadata FROM session_redo
		WHERE session_id = ? ORDER BY stack_pos DESC LIMIT 1`, sessionID).Scan(&stackPos, &suffixRaw, &metadataRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptMutationResult{}, ErrNothingToRedo
	}
	if err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("load redo suffix: %w", err)
	}
	var suffix []Message
	if err := json.Unmarshal([]byte(suffixRaw), &suffix); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("decode redo suffix: %w", err)
	}
	var metadata undoRedoMetadata
	if err := json.Unmarshal([]byte(metadataRaw), &metadata); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("decode redo metadata: %w", err)
	}
	for i := range suffix {
		msg := &suffix[i]
		partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
		if err != nil {
			return TranscriptMutationResult{}, fmt.Errorf("serialize redo message %d: %w", i, err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO messages (id, session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail, client_message_id, response_id, assistant_segment_ordinal, segment_start_sequence, segment_end_sequence)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.ID, sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.TurnIndex, msg.CreatedAt, msg.Sequence,
			msg.CompactionTail, msg.ClientMessageID, msg.ResponseID, msg.AssistantSegmentOrdinal, msg.SegmentStartSequence, msg.SegmentEndSequence); err != nil {
			return TranscriptMutationResult{}, fmt.Errorf("restore redo message %d: %w", i, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM session_provider_state WHERE session_id = ?`, sessionID); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("reset provider state for redo: %w", err)
	}
	if err := s.recomputeTranscriptMetadata(ctx, conn, sessionID, false); err != nil {
		return TranscriptMutationResult{}, err
	}
	if err := s.restoreRedoTitleMetadata(ctx, conn, sessionID, metadata); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("restore redo title metadata: %w", err)
	}
	if err := s.reconcilePlanProjection(ctx, conn, sessionID); err != nil {
		return TranscriptMutationResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM session_redo WHERE session_id = ? AND stack_pos = ?`, sessionID, stackPos); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("consume redo suffix: %w", err)
	}
	if _, err := s.bumpTranscriptRevPreservingRedo(ctx, conn, sessionID); err != nil {
		return TranscriptMutationResult{}, err
	}
	post, err := transcriptMutationState(ctx, conn, sessionID)
	if err != nil {
		return TranscriptMutationResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return TranscriptMutationResult{}, fmt.Errorf("commit redo: %w", err)
	}
	return TranscriptMutationResult{TranscriptMutationState: post}, nil
}

func transcriptRowHasDisplayBody(role llm.Role, parts []llm.Part, planToolCalls map[string]bool) bool {
	if role == llm.RoleEvent {
		msg := llm.Message{Role: role, Parts: parts}
		if _, ok := llm.ParseModelSwapMarker(msg); ok {
			return true
		}
		if _, ok := llm.ParseRunErrorMarker(msg); ok {
			return true
		}
	}
	for _, part := range parts {
		switch part.Type {
		case llm.PartText:
			if part.Text != "" {
				return true
			}
		case llm.PartImage:
			return true
		case llm.PartSkillActivation:
			if role == llm.RoleEvent && part.SkillActivation != nil {
				return true
			}
		case llm.PartToolActivity:
			if part.ToolActivity != nil {
				return true
			}
		case llm.PartToolCall:
			if part.ToolCall != nil {
				return true
			}
		case llm.PartToolResult:
			if part.ToolResult != nil && (part.ToolResult.IsError || len(part.ToolResult.Images) > 0 || len(part.ToolResult.Media) > 0 || len(part.ToolResult.GuardianReviews) > 0 || part.ToolResult.Name == "update_plan" || part.ToolResult.Name == "ask_user" || planToolCalls[part.ToolResult.ID]) {
				return true
			}
		}
	}
	return false
}

func (s *SQLiteStore) queryDB() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// transcriptReadDB is retained for focused transcript callers and tests; all
// ordinary read-only queries share the same WAL reader lane.
func (s *SQLiteStore) transcriptReadDB() *sql.DB {
	return s.queryDB()
}

func (s *SQLiteStore) responseRunStateReadDB() *sql.DB {
	if s.responseRunReadDB != nil {
		return s.responseRunReadDB
	}
	return s.db
}

// GetResponseRunStartState returns the response-run transcript envelope in one
// indexed scalar query. File-backed WAL stores use a dedicated scalar reader so
// startup is queued behind neither long transcript materialization nor the
// single writer connection. In-memory, read-only, and rollback-journal stores
// retain their existing serialized connection behavior.
func (s *SQLiteStore) GetResponseRunStartState(ctx context.Context, sessionID string) (ResponseRunStartState, error) {
	if !s.hasTranscriptRev || !s.hasMessagesTable {
		return ResponseRunStartState{}, ErrTranscriptRevisionUnsupported
	}
	compactionSeqExpr := "-1"
	if s.hasCompactionSeq {
		compactionSeqExpr = "COALESCE(compaction_seq, -1)"
	}
	compactionCountExpr := "0"
	if s.hasCompactionCount {
		compactionCountExpr = "COALESCE(compaction_count, 0)"
	}
	compactionTailFilter := ""
	if s.hasMessageCompactionTail {
		compactionTailFilter = "AND NOT COALESCE(compaction_tail, FALSE)"
	}
	state := ResponseRunStartState{CompactionSeq: -1}
	err := s.responseRunStateReadDB().QueryRowContext(ctx, `
		SELECT transcript_rev, `+compactionSeqExpr+`, `+compactionCountExpr+`, COALESCE((
			SELECT id
			FROM messages
			WHERE session_id = sessions.id
				AND role IN ('user', 'assistant', 'tool')
				`+compactionTailFilter+`
			ORDER BY sequence DESC, id DESC
			LIMIT 1
		), 0)
		FROM sessions
		WHERE id = ?`, sessionID).Scan(
		&state.Rev,
		&state.CompactionSeq,
		&state.CompactionCount,
		&state.DurableBoundaryID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ResponseRunStartState{}, ErrNotFound
	}
	if err != nil {
		return ResponseRunStartState{}, fmt.Errorf("get response run start state: %w", err)
	}
	return state, nil
}

// GetTranscriptSnapshot returns the complete transcript envelope from one
// SQLite read transaction.
func (s *SQLiteStore) GetTranscriptSnapshot(ctx context.Context, sessionID string) (TranscriptSnapshot, error) {
	tx, err := s.transcriptReadDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("begin transcript index read: %w", err)
	}
	defer tx.Rollback()

	revExpr := "0"
	if s.hasTranscriptRev {
		revExpr = "transcript_rev"
	}
	compactionSeqExpr := "-1"
	if s.hasCompactionSeq {
		compactionSeqExpr = "COALESCE(compaction_seq, -1)"
	}
	compactionCountExpr := "0"
	if s.hasCompactionCount {
		compactionCountExpr = "COALESCE(compaction_count, 0)"
	}
	snapshot := TranscriptSnapshot{CompactionSeq: -1}
	if err := tx.QueryRowContext(ctx, "SELECT "+revExpr+", "+compactionSeqExpr+", "+compactionCountExpr+" FROM sessions WHERE id = ?", sessionID).Scan(&snapshot.Rev, &snapshot.CompactionSeq, &snapshot.CompactionCount); errors.Is(err, sql.ErrNoRows) {
		return TranscriptSnapshot{}, ErrNotFound
	} else if err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("read transcript revision: %w", err)
	}
	compactionTailCol := "FALSE"
	if s.hasMessageCompactionTail {
		compactionTailCol = "COALESCE(compaction_tail, FALSE)"
	}
	clientMessageIDCol := "''"
	if s.hasMessageClientID {
		clientMessageIDCol = "COALESCE(client_message_id, '')"
	}
	streamIdentityCols := "'', -1"
	if s.hasMessageStreamIdentity {
		streamIdentityCols = "COALESCE(response_id, ''), COALESCE(assistant_segment_ordinal, -1)"
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, sequence, role, parts, `+compactionTailCol+`, `+clientMessageIDCol+`, `+streamIdentityCols+`
		FROM messages
		WHERE session_id = ? AND role NOT IN ('system', 'developer')
		ORDER BY sequence ASC, id ASC`, sessionID)
	if err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("query transcript index: %w", err)
	}
	defer rows.Close()
	snapshot.Items = make([]TranscriptIndexItem, 0)
	partsByItem := make([][]llm.Part, 0)
	planToolCalls := make(map[string]bool)
	for rows.Next() {
		var item TranscriptIndexItem
		var partsJSON string
		var compactionTail bool
		if err := rows.Scan(&item.ID, &item.Seq, &item.Role, &partsJSON, &compactionTail, &item.ClientMessageID, &item.ResponseID, &item.AssistantSegmentOrdinal); err != nil {
			return TranscriptSnapshot{}, fmt.Errorf("scan transcript index: %w", err)
		}
		if compactionTail {
			item.Flags |= TranscriptFlagCompactionTail
		}
		var parts []llm.Part
		if err := json.Unmarshal([]byte(partsJSON), &parts); err != nil {
			return TranscriptSnapshot{}, fmt.Errorf("decode transcript index parts: %w", err)
		}
		candidate := Message{Role: llm.Role(item.Role), Parts: parts, ClientMessageID: item.ClientMessageID}
		if candidate.IsGoalSteering() {
			continue
		}
		for _, part := range parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.ID != "" && part.ToolCall.Name == "update_plan" {
				planToolCalls[part.ToolCall.ID] = true
			}
		}
		snapshot.Items = append(snapshot.Items, item)
		partsByItem = append(partsByItem, parts)
	}
	if err := rows.Err(); err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("iterate transcript index: %w", err)
	}
	if err := rows.Close(); err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("close transcript index: %w", err)
	}
	for i := range snapshot.Items {
		if !transcriptRowHasDisplayBody(llm.Role(snapshot.Items[i].Role), partsByItem[i], planToolCalls) {
			snapshot.Items[i].Flags |= TranscriptFlagEmptyBody
		}
	}
	if err := tx.Commit(); err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("commit transcript index read: %w", err)
	}
	return snapshot, nil
}

// GetTranscriptIndex returns every durable non-internal row and the revision
// describing it from one SQLite read transaction.
func (s *SQLiteStore) GetTranscriptIndex(ctx context.Context, sessionID string) (int64, []TranscriptIndexItem, error) {
	snapshot, err := s.GetTranscriptSnapshot(ctx, sessionID)
	if err != nil {
		return 0, nil, err
	}
	return snapshot.Rev, snapshot.Items, nil
}

// GetMessagesByTranscriptRanges returns complete durable transcript segments in
// authoritative order and the revision describing them from one SQLite read
// transaction. Each segment uses four bind variables regardless of how many
// durable rows it expands to, so a giant tool turn never approaches SQLite's
// variable limit.
func (s *SQLiteStore) GetMessagesByTranscriptRanges(ctx context.Context, sessionID string, ranges []TranscriptRange) (int64, []Message, error) {
	tx, err := s.transcriptReadDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, nil, fmt.Errorf("begin transcript bodies read: %w", err)
	}
	defer tx.Rollback()

	revExpr := "0"
	if s.hasTranscriptRev {
		revExpr = "transcript_rev"
	}
	var rev int64
	if err := tx.QueryRowContext(ctx, "SELECT "+revExpr+" FROM sessions WHERE id = ?", sessionID).Scan(&rev); errors.Is(err, sql.ErrNoRows) {
		return 0, nil, ErrNotFound
	} else if err != nil {
		return 0, nil, fmt.Errorf("read transcript revision: %w", err)
	}

	if len(ranges) > TranscriptMaterializationMaxRanges {
		return 0, nil, fmt.Errorf("transcript bodies query exceeds %d ranges", TranscriptMaterializationMaxRanges)
	}
	clauses := make([]string, 0, len(ranges))
	args := make([]any, 0, 1+len(ranges)*4)
	args = append(args, sessionID)
	seen := make(map[TranscriptRange]struct{}, len(ranges))
	for _, transcriptRange := range ranges {
		if transcriptRange.StartID <= 0 || transcriptRange.EndID <= 0 {
			continue
		}
		if transcriptRange.EndSeq < transcriptRange.StartSeq || (transcriptRange.EndSeq == transcriptRange.StartSeq && transcriptRange.EndID < transcriptRange.StartID) {
			continue
		}
		if _, ok := seen[transcriptRange]; ok {
			continue
		}
		seen[transcriptRange] = struct{}{}
		clauses = append(clauses, "((sequence, id) >= (?, ?) AND (sequence, id) <= (?, ?))")
		args = append(args, transcriptRange.StartSeq, transcriptRange.StartID, transcriptRange.EndSeq, transcriptRange.EndID)
	}

	messages := make([]Message, 0)
	if len(clauses) > 0 {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+s.messageSelectCols()+`
			FROM messages
			WHERE session_id = ?
				AND role NOT IN ('system', 'developer')
				AND (`+strings.Join(clauses, " OR ")+`)
			ORDER BY sequence ASC, id ASC`, args...)
		if err != nil {
			return 0, nil, fmt.Errorf("query transcript bodies: %w", err)
		}
		messages, err = scanMessageRows(rows)
		closeErr := rows.Close()
		if err != nil {
			return 0, nil, err
		}
		if closeErr != nil {
			return 0, nil, fmt.Errorf("close transcript bodies: %w", closeErr)
		}
		visible := messages[:0]
		for i := range messages {
			if !messages[i].IsGoalSteering() {
				visible = append(visible, messages[i])
			}
		}
		messages = visible
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit transcript bodies read: %w", err)
	}
	return rev, messages, nil
}

// MaxMessageSequences returns the greatest persisted message sequence for each
// requested session. Sessions without messages are returned with sequence -1.
func (s *SQLiteStore) MaxMessageSequences(ctx context.Context, sessionIDs []string) (map[string]int, error) {
	const batchSize = 500

	result := make(map[string]int, len(sessionIDs))
	for start := 0; start < len(sessionIDs); start += batchSize {
		end := min(start+batchSize, len(sessionIDs))
		values := make([]string, 0, end-start)
		args := make([]any, 0, end-start)
		for _, sessionID := range sessionIDs[start:end] {
			result[sessionID] = -1
			values = append(values, "(?)")
			args = append(args, sessionID)
		}

		query := `
			WITH requested(session_id) AS (VALUES ` + strings.Join(values, ",") + `)
			SELECT requested.session_id, COALESCE((
				SELECT MAX(messages.sequence)
				FROM messages
				WHERE messages.session_id = requested.session_id
			), -1)
			FROM requested`
		rows, err := s.queryDB().QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query maximum message sequences: %w", err)
		}
		for rows.Next() {
			var sessionID string
			var maxSequence int
			if err := rows.Scan(&sessionID, &maxSequence); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan maximum message sequence: %w", err)
			}
			result[sessionID] = maxSequence
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate maximum message sequences: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close maximum message sequences: %w", err)
		}
	}
	return result, nil
}

// GetMessagesFrom retrieves messages for a session starting from a given
// sequence number. Used on resume and for keyset-style pagination when walking
// long transcripts.
// When limit <= 0, all rows at/after fromSeq are returned.
func (s *SQLiteStore) GetMessagesFrom(ctx context.Context, sessionID string, fromSeq, limit int) ([]Message, error) {
	query := `
		SELECT ` + s.messageSelectCols() + `
		FROM messages
		WHERE session_id = ? AND sequence >= ?
		ORDER BY sequence ASC`
	args := []any{sessionID, fromSeq}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages from seq %d: %w", fromSeq, err)
	}
	defer rows.Close()

	return scanMessageRows(rows)
}

func scanMessageRows(rows *sql.Rows) ([]Message, error) {
	var messages []Message
	for rows.Next() {
		var msg Message
		var partsJSON string
		var durationMs sql.NullInt64
		err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &partsJSON,
			&msg.TextContent, &durationMs, &msg.TurnIndex, &msg.CreatedAt, &msg.Sequence, &msg.CompactionTail,
			&msg.ClientMessageID, &msg.ResponseID, &msg.AssistantSegmentOrdinal, &msg.SegmentStartSequence, &msg.SegmentEndSequence)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if durationMs.Valid {
			msg.DurationMs = durationMs.Int64
		}
		if err := msg.SetPartsFromJSON(partsJSON); err != nil {
			return nil, fmt.Errorf("deserialize parts: %w", err)
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

// GetMessagesPageDescending retrieves messages for a session in reverse
// sequence order. When beforeSeq > 0, only rows with sequence < beforeSeq are
// returned. Used for reverse tail pagination without loading entire sessions.
func (s *SQLiteStore) GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]Message, error) {
	query := `
		SELECT ` + s.messageSelectCols() + `
		FROM messages
		WHERE session_id = ?`
	args := []any{sessionID}
	if beforeSeq > 0 {
		query += " AND sequence < ?"
		args = append(args, beforeSeq)
	}
	query += " ORDER BY sequence DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages descending: %w", err)
	}
	defer rows.Close()

	return scanMessageRows(rows)
}

// GetMessageByID retrieves a single message by its global message id.
func (s *SQLiteStore) GetMessageByID(ctx context.Context, msgID int64) (*Message, error) {
	row := s.queryDB().QueryRowContext(ctx, `
		SELECT `+s.messageSelectCols()+`
		FROM messages
		WHERE id = ?`, msgID)
	var msg Message
	var partsJSON string
	var durationMs sql.NullInt64
	err := row.Scan(&msg.ID, &msg.SessionID, &msg.Role, &partsJSON,
		&msg.TextContent, &durationMs, &msg.TurnIndex, &msg.CreatedAt, &msg.Sequence, &msg.CompactionTail,
		&msg.ClientMessageID, &msg.ResponseID, &msg.AssistantSegmentOrdinal, &msg.SegmentStartSequence, &msg.SegmentEndSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	if durationMs.Valid {
		msg.DurationMs = durationMs.Int64
	}
	if err := msg.SetPartsFromJSON(partsJSON); err != nil {
		return nil, fmt.Errorf("deserialize parts: %w", err)
	}
	return &msg, nil
}

// GetMessagesByClientMessageIDs resolves a batch through the indexed lookup.
func (s *SQLiteStore) GetMessagesByClientMessageIDs(ctx context.Context, sessionID string, clientMessageIDs []string) (map[string]*Message, error) {
	found := make(map[string]*Message, len(clientMessageIDs))
	for _, rawID := range clientMessageIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		message, err := s.GetMessageByClientMessageID(ctx, sessionID, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found[id] = message
	}
	return found, nil
}

// GetMessageByClientMessageID retrieves a first-party user message by its
// session-scoped stable identity.
func (s *SQLiteStore) GetMessageByClientMessageID(ctx context.Context, sessionID, clientMessageID string) (*Message, error) {
	if !s.hasMessageClientID {
		return nil, ErrNotFound
	}
	row := s.queryDB().QueryRowContext(ctx, `
		SELECT `+s.messageSelectCols()+`
		FROM messages
		WHERE session_id = ? AND client_message_id = ?
		ORDER BY id ASC
		LIMIT 1`, sessionID, clientMessageID)
	var msg Message
	var partsJSON string
	var durationMs sql.NullInt64
	err := row.Scan(&msg.ID, &msg.SessionID, &msg.Role, &partsJSON,
		&msg.TextContent, &durationMs, &msg.TurnIndex, &msg.CreatedAt, &msg.Sequence, &msg.CompactionTail,
		&msg.ClientMessageID, &msg.ResponseID, &msg.AssistantSegmentOrdinal, &msg.SegmentStartSequence, &msg.SegmentEndSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message by client_message_id: %w", err)
	}
	if durationMs.Valid {
		msg.DurationMs = durationMs.Int64
	}
	if err := msg.SetPartsFromJSON(partsJSON); err != nil {
		return nil, fmt.Errorf("deserialize parts: %w", err)
	}
	return &msg, nil
}

// GetLatestVisibleMessageID retrieves the latest persisted user/assistant message id for a session.
func (s *SQLiteStore) GetLatestVisibleMessageID(ctx context.Context, sessionID string) (int64, error) {
	var msgID int64
	compactionTailClause := ""
	if s.hasMessageCompactionTail {
		compactionTailClause = " AND COALESCE(compaction_tail, FALSE) = FALSE"
	}
	err := s.queryDB().QueryRowContext(ctx, `
		SELECT id
		FROM messages
		WHERE session_id = ? AND role IN (?, ?)`+compactionTailClause+`
		ORDER BY sequence DESC, id DESC
		LIMIT 1`, sessionID, string(llm.RoleUser), string(llm.RoleAssistant)).Scan(&msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query latest visible message id: %w", err)
	}
	return msgID, nil
}

// PreviousUserPrompt returns the newest persisted user prompt older than beforeID.
// When beforeID <= 0, traversal starts at the newest prompt. History is global
// across sessions and optionally filtered by agent.
func (s *SQLiteStore) PreviousUserPrompt(ctx context.Context, agent string, beforeID int64) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		JOIN sessions sess ON sess.id = m.session_id
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND (? = '' OR COALESCE(sess.agent, '') = '' OR COALESCE(sess.agent, '') = ?)
		  AND (? <= 0 OR m.id < ?)
		ORDER BY m.id DESC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, agent, agent, beforeID, beforeID)
}

// NextUserPrompt returns the oldest persisted user prompt newer than afterID.
func (s *SQLiteStore) NextUserPrompt(ctx context.Context, agent string, afterID int64) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		JOIN sessions sess ON sess.id = m.session_id
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND (? = '' OR COALESCE(sess.agent, '') = '' OR COALESCE(sess.agent, '') = ?)
		  AND m.id > ?
		ORDER BY m.id ASC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, agent, agent, afterID)
}

// PreviousUserPromptOutsideSession returns the newest persisted user prompt older
// than the cursor, ordered by message timestamp across all agents, excluding the
// current session and machine-generated subagent sessions.
func (s *SQLiteStore) PreviousUserPromptOutsideSession(ctx context.Context, excludeSessionID string, beforeID int64, beforeCreatedAt time.Time) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		JOIN sessions sess ON sess.id = m.session_id
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND sess.parent_id IS NULL
		  AND (? = '' OR m.session_id <> ?)`
	args := []any{len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, excludeSessionID, excludeSessionID}
	if beforeID > 0 {
		query += `
		  AND m.id <> ?
		  AND (m.created_at, m.id) < (?, ?)`
		args = append(args, beforeID, beforeCreatedAt, beforeID)
	}
	query += `
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, args...)
}

// NextUserPromptOutsideSession returns the oldest persisted user prompt newer
// than the cursor, ordered by message timestamp across all agents, excluding the
// current session and machine-generated subagent sessions.
func (s *SQLiteStore) NextUserPromptOutsideSession(ctx context.Context, excludeSessionID string, afterID int64, afterCreatedAt time.Time) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		JOIN sessions sess ON sess.id = m.session_id
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND sess.parent_id IS NULL
		  AND (? = '' OR m.session_id <> ?)`
	args := []any{len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, excludeSessionID, excludeSessionID}
	if afterID > 0 {
		query += `
		  AND m.id <> ?
		  AND (m.created_at, m.id) > (?, ?)`
		args = append(args, afterID, afterCreatedAt, afterID)
	}
	query += `
		ORDER BY m.created_at ASC, m.id ASC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, args...)
}

func (s *SQLiteStore) queryPromptHistoryEntry(ctx context.Context, query string, args ...any) (*PromptHistoryEntry, error) {
	var entry PromptHistoryEntry
	err := s.queryDB().QueryRowContext(ctx, query, args...).Scan(&entry.ID, &entry.Text, &entry.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query prompt history: %w", err)
	}
	return &entry, nil
}

// GetMessages retrieves messages for a session.
func (s *SQLiteStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	query := `
		SELECT ` + s.messageSelectCols() + `
		FROM messages
		WHERE session_id = ?
		ORDER BY sequence ASC`

	args := []any{sessionID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	} else if offset > 0 {
		// SQLite requires LIMIT before OFFSET; use -1 to mean "all rows".
		query += " LIMIT -1"
	}
	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	return scanMessageRows(rows)
}

// GetDiffCommentMessages retrieves only messages whose serialized parts can
// contain typed inline-diff comment metadata. The final typed-part check remains
// with the caller; LIKE is used only as a conservative SQLite row prefilter.
func (s *SQLiteStore) GetDiffCommentMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT `+s.messageSelectCols()+`
		FROM messages
		WHERE session_id = ? AND parts LIKE ?
		ORDER BY sequence ASC`, sessionID, `%"Type":"diff_comment"%`)
	if err != nil {
		return nil, fmt.Errorf("query diff comment messages: %w", err)
	}
	defer rows.Close()

	return scanMessageRows(rows)
}

// SetCurrent marks a session as the current one.
