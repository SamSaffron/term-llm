package session

import (
	"context"
	"fmt"
	"time"
)

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
	if err := incorporateNotificationSteeringTx(ctx, execer, sessionID, msg); err != nil {
		return 0, 0, err
	}
	if msg.Role == "user" && !msg.CompactionTail && msg.ClientMessageID != "" {
		if _, err := execer.ExecContext(ctx, `DELETE FROM session_pending_steering WHERE session_id=? AND id=?`, sessionID, msg.ClientMessageID); err != nil {
			return 0, 0, err
		}
	}

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
