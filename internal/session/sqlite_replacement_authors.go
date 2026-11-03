package session

import (
	"context"
	"database/sql"
	"github.com/samsaffron/term-llm/internal/llm/core"

	"github.com/samsaffron/term-llm/internal/session/transcriptprefix"
)

// Compare only persistence metadata, without copying large structured message bodies.
func replacementAuthorRow(m Message) transcriptprefix.ReplacementRow {
	return transcriptprefix.ReplacementRow{Message: core.Message{Role: m.Role, AuthorID: userAuthorID(m.Role, m.AuthorID), ClientMessageID: m.ClientMessageID, ResponseID: m.ResponseID, AssistantSegmentOrdinal: m.AssistantSegmentOrdinal, SegmentStartSequence: m.SegmentStartSequence, SegmentEndSequence: m.SegmentEndSequence}, TextContent: m.TextContent, DurationMs: m.DurationMs, TurnIndex: m.TurnIndex}
}
func compactedReplacementCommonPrefix(ctx context.Context, tx *sql.Tx, id string, start int, messages []Message, parts []string) (int, bool, error) {
	return transcriptprefix.CompactedReplacementCommonPrefix(ctx, tx, id, start, len(messages), func(i int) transcriptprefix.ReplacementRow { return replacementAuthorRow(messages[i]) }, parts)
}
func replacementCommonPrefix(ctx context.Context, tx *sql.Tx, id string, messages []Message, parts []string) (int, bool, error) {
	return transcriptprefix.ReplacementCommonPrefix(ctx, tx, id, len(messages), func(i int) transcriptprefix.ReplacementRow { return replacementAuthorRow(messages[i]) }, parts)
}
