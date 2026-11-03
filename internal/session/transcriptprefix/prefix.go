// Package transcriptprefix compares persisted transcript prefixes for replacement and compaction.
package transcriptprefix

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ReplacementRow is the comparable persistence metadata used by transcript replacement.
type ReplacementRow struct {
	core.Message
	TextContent string
	DurationMs  int64
	TurnIndex   int
}

func CompactedReplacementCommonPrefix(ctx context.Context, tx *sql.Tx, sessionID string, startSeq int, desiredCount int, desired func(int) ReplacementRow, desiredPartsJSON []string) (prefix int, fullActiveDelete bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, role, parts, text_content, duration_ms, turn_index,
		       COALESCE(author_id, ''), COALESCE(client_message_id, ''), COALESCE(response_id, ''), COALESCE(assistant_segment_ordinal, -1),
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
		var authorID, clientMessageID, responseID string
		var assistantSegmentOrdinal int
		var segmentStartSequence, segmentEndSequence int64
		if err := rows.Scan(&sequence, &role, &partsJSON, &textContent, &durationMs, &turnIndex,
			&authorID, &clientMessageID, &responseID, &assistantSegmentOrdinal, &segmentStartSequence, &segmentEndSequence); err != nil {
			return 0, false, fmt.Errorf("scan compacted message: %w", err)
		}
		if sequence < startSeq {
			return 0, true, nil
		}

		expectedSeq := startSeq + prefix
		if prefix >= desiredCount {
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

		want := desired(prefix)
		if role != string(want.Role) ||
			partsJSON != desiredPartsJSON[prefix] ||
			nullStringValue(textContent) != want.TextContent ||
			nullInt64Value(durationMs) != want.DurationMs ||
			int(nullInt64Value(turnIndex)) != want.TurnIndex ||
			authorID != want.AuthorID || clientMessageID != want.ClientMessageID ||
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

func ReplacementCommonPrefix(ctx context.Context, tx *sql.Tx, sessionID string, desiredCount int, desired func(int) ReplacementRow, desiredPartsJSON []string) (prefix int, fullDelete bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, role, parts, text_content, duration_ms, turn_index, COALESCE(compaction_tail, FALSE),
		       COALESCE(author_id, ''), COALESCE(client_message_id, ''), COALESCE(response_id, ''), COALESCE(assistant_segment_ordinal, -1),
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
		var authorID, clientMessageID, responseID string
		var assistantSegmentOrdinal int
		var segmentStartSequence, segmentEndSequence int64
		if err := rows.Scan(&sequence, &role, &partsJSON, &textContent, &durationMs, &turnIndex, &compactionTail,
			&authorID, &clientMessageID, &responseID, &assistantSegmentOrdinal, &segmentStartSequence, &segmentEndSequence); err != nil {
			return 0, false, fmt.Errorf("scan existing message: %w", err)
		}
		if sequence < 0 {
			return 0, true, nil
		}

		if prefix >= desiredCount {
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

		want := desired(prefix)
		if role != string(want.Role) ||
			partsJSON != desiredPartsJSON[prefix] ||
			nullStringValue(textContent) != want.TextContent ||
			nullInt64Value(durationMs) != want.DurationMs ||
			int(nullInt64Value(turnIndex)) != want.TurnIndex ||
			compactionTail ||
			authorID != want.AuthorID || clientMessageID != want.ClientMessageID ||
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
