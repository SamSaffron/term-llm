package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/samsaffron/term-llm/internal/llm"
)

// RefreshSessionInputs does not insert history, normalize equal tool settings,
// or touch any other session setting. All reads are repeated on busy retry.
func (s *SQLiteStore) RefreshSessionInputs(ctx context.Context, id, prompt, selectedTools string, equalTools func(string, string) bool) (SessionInputRefreshResult, error) {
	var result SessionInputRefreshResult
	err := retryOnBusy(ctx, 5, func() error {
		result = SessionInputRefreshResult{}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var boundary Session
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(tools,''), COALESCE(compaction_seq,-1), COALESCE(compaction_count,0) FROM sessions WHERE id=?`, id).Scan(&result.Tools, &boundary.CompactionSeq, &boundary.CompactionCount)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		starts := []int{-1}
		if HasCompactionBoundary(&boundary) {
			starts = append(starts, boundary.CompactionSeq)
		}
		seen := make(map[int64]bool)
		parts := llm.SystemText(prompt).Parts
		encoded, err := json.Marshal(parts)
		if err != nil {
			return err
		}
		for index, start := range starts {
			var msg Message
			var raw string
			query := `SELECT id, sequence, role, parts, COALESCE(text_content,'') FROM messages WHERE session_id=?`
			args := []any{id}
			if index > 0 {
				query += ` AND sequence>=?`
				args = append(args, start)
			}
			query += ` ORDER BY sequence,id LIMIT 1`
			err := tx.QueryRowContext(ctx, query, args...).Scan(&msg.ID, &msg.Sequence, &msg.Role, &raw, &msg.TextContent)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if seen[msg.ID] || msg.Role != llm.RoleSystem {
				continue
			}
			seen[msg.ID] = true
			if err := json.Unmarshal([]byte(raw), &msg.Parts); err != nil {
				return fmt.Errorf("decode session prompt: %w", err)
			}
			if msg.TextContent == prompt && reflect.DeepEqual(msg.Parts, parts) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET text_content=?, parts=? WHERE id=? AND session_id=?`, prompt, string(encoded), msg.ID, id); err != nil {
				return err
			}
			msg.SessionID = id
			msg.TextContent = prompt
			msg.Parts = parts
			result.Messages = append(result.Messages, msg)
		}
		result.ToolsChanged = result.Tools != selectedTools
		if equalTools != nil {
			result.ToolsChanged = !equalTools(result.Tools, selectedTools)
		}
		if result.ToolsChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET tools=? WHERE id=?`, selectedTools, id); err != nil {
				return err
			}
			result.Tools = selectedTools
		}
		if len(result.Messages) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET transcript_rev=transcript_rev+1 WHERE id=?`, id); err != nil {
				return err
			}
		}
		if result.Changed() {
			if _, err := tx.ExecContext(ctx, `DELETE FROM session_provider_state WHERE session_id=?`, id); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return SessionInputRefreshResult{}, fmt.Errorf("refresh session inputs: %w", err)
	}
	return result, nil
}
