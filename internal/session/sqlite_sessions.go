package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
	"github.com/samsaffron/term-llm/internal/sqlitefts"
)

func (s *SQLiteStore) Create(ctx context.Context, sess *Session) error {
	if sess.ID == "" {
		sess.ID = NewID()
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now()
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = sess.CreatedAt
	}
	if sess.Status == "" {
		sess.Status = StatusActive
	}
	if sess.Mode == "" {
		sess.Mode = ModeChat
	}
	if sess.Origin == "" {
		sess.Origin = OriginTUI
	}

	err := retryOnBusy(ctx, 5, func() error {
		// Use a single INSERT statement with a subquery to atomically assign the
		// next session number. This avoids race conditions where two concurrent
		// Creates could read the same MAX(number).
		reasoningEffortCol := ""
		reasoningEffortPlaceholder := ""
		var reasoningEffortArgs []any
		if s.hasReasoningEffort {
			reasoningEffortCol = ", reasoning_effort"
			reasoningEffortPlaceholder = ", ?"
			reasoningEffortArgs = []any{nullString(sess.ReasoningEffort)}
		}
		reasoningModeCol := ""
		reasoningModePlaceholder := ""
		var reasoningModeArgs []any
		if s.hasReasoningMode {
			reasoningModeCol = ", reasoning_mode"
			reasoningModePlaceholder = ", ?"
			reasoningModeArgs = []any{nullString(sess.ReasoningMode)}
		}
		approvalModeCol := ""
		approvalModePlaceholder := ""
		var approvalModeArgs []any
		if s.hasApprovalMode {
			approvalModeCol = ", approval_mode"
			approvalModePlaceholder = ", ?"
			approvalModeArgs = []any{nullString(string(sess.ApprovalMode))}
		}
		worktreeDirCol := ""
		worktreeDirPlaceholder := ""
		var worktreeDirArgs []any
		if s.hasWorktreeDir {
			worktreeDirCol = ", worktree_dir"
			worktreeDirPlaceholder = ", ?"
			worktreeDirArgs = []any{nullString(sess.WorktreeDir)}
		}
		projectIDCol := ""
		projectIDPlaceholder := ""
		var projectIDArgs []any
		if s.hasProjectID {
			projectIDCol = ", project_id"
			projectIDPlaceholder = ", ?"
			projectIDArgs = []any{nullString(sess.ProjectID)}
		}
		goalCol := ""
		goalPlaceholder := ""
		var goalArgs []any
		if s.hasGoal {
			goalCol = ", goal"
			goalPlaceholder = ", ?"
			goalArgs = []any{goalJSONString(sess.Goal)}
		}
		shareCol := ""
		sharePlaceholder := ""
		var shareArgs []any
		if s.hasShare {
			shareCol = ", share"
			sharePlaceholder = ", ?"
			shareArgs = []any{shareJSONString(sess.Share)}
		}
		insertArgs := []any{
			sess.ID, sess.Name, sess.Summary, nullString(sess.GeneratedShortTitle), nullString(sess.GeneratedLongTitle), nullString(string(sess.TitleSource)), nullTime(sess.TitleGeneratedAt), sess.TitleBasisMsgSeq, nullTime(sess.TitleSkippedAt),
			sess.Provider, nullString(sess.ProviderKey), sess.Model, string(sess.Mode),
		}
		insertArgs = append(insertArgs, approvalModeArgs...)
		insertArgs = append(insertArgs,
			nullString(string(sess.Origin)), nullString(sess.Agent), sess.CWD,
		)
		insertArgs = append(insertArgs, worktreeDirArgs...)
		insertArgs = append(insertArgs, projectIDArgs...)
		insertArgs = append(insertArgs,
			sess.CreatedAt, sess.UpdatedAt, sess.Archived, sess.Pinned, nullString(sess.ParentID),
			sess.Search, nullString(sess.Tools), nullString(sess.MCP),
			sess.UserTurns, sess.LLMTurns, sess.ToolCalls, sess.InputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.OutputTokens,
			sess.LastTotalTokens, sess.LastMessageCount, string(sess.Status), nullString(sess.Tags),
		)
		insertArgs = append(insertArgs, goalArgs...)
		insertArgs = append(insertArgs, shareArgs...)
		insertArgs = append(insertArgs, reasoningEffortArgs...)
		insertArgs = append(insertArgs, reasoningModeArgs...)
		result, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (id, number, name, summary, generated_short_title, generated_long_title, title_source, title_generated_at, title_basis_msg_seq, title_skipped_at,
				                      provider, provider_key, model, mode`+approvalModeCol+`, origin, agent, cwd`+worktreeDirCol+projectIDCol+`, created_at, updated_at, archived, pinned, parent_id, search, tools, mcp,
			                      user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
				                      last_total_tokens, last_message_count, status, tags`+goalCol+shareCol+reasoningEffortCol+reasoningModeCol+`)
			VALUES (?, (SELECT COALESCE(MAX(number), 0) + 1 FROM sessions), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`+approvalModePlaceholder+`, ?, ?, ?`+worktreeDirPlaceholder+projectIDPlaceholder+`, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`+goalPlaceholder+sharePlaceholder+reasoningEffortPlaceholder+reasoningModePlaceholder+`)`,
			insertArgs...)
		if err != nil {
			return fmt.Errorf("insert session: %w", err)
		}

		// Fetch the assigned number
		rows, _ := result.RowsAffected()
		if rows == 0 {
			return fmt.Errorf("no rows inserted")
		}

		// Query the assigned number back
		err = s.db.QueryRowContext(ctx, "SELECT number FROM sessions WHERE id = ?", sess.ID).Scan(&sess.Number)
		if err != nil {
			return fmt.Errorf("get assigned number: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) enrichSessionProject(ctx context.Context, sess *Session) *Session {
	if sess == nil || sess.ProjectID == "" || !s.hasProjectsTable {
		return sess
	}
	_ = s.queryDB().QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, sess.ProjectID).Scan(&sess.ProjectName)
	return sess
}

// Get retrieves a session by ID.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Session, error) {
	row := s.queryDB().QueryRowContext(ctx,
		"SELECT "+s.sessionSelectCols()+" FROM sessions WHERE id = ?", id)
	sess, err := scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
	return s.enrichSessionProject(ctx, sess), err
}

// GetByNumber retrieves a session by its sequential number.
func (s *SQLiteStore) GetByNumber(ctx context.Context, number int64) (*Session, error) {
	row := s.queryDB().QueryRowContext(ctx,
		"SELECT "+s.sessionSelectCols()+" FROM sessions WHERE number = ?", number)
	sess, err := scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
	return s.enrichSessionProject(ctx, sess), err
}

// GetByPrefix retrieves a session by number (with # prefix), exact ID, or by short ID prefix match.
// It tries in order: #number (e.g., #42), exact ID match, short ID prefix match.
func (s *SQLiteStore) GetByPrefix(ctx context.Context, prefix string) (*Session, error) {
	// Check for #number format (e.g., "#42" or "42" after stripping #)
	if strings.HasPrefix(prefix, "#") {
		numStr := strings.TrimPrefix(prefix, "#")
		if num, err := strconv.ParseInt(numStr, 10, 64); err == nil {
			sess, err := s.GetByNumber(ctx, num)
			if err != nil {
				return nil, err
			}
			if sess != nil {
				return sess, nil
			}
		}
	}

	// Also support plain numbers for convenience (but only if it's purely numeric)
	// This maintains backward compatibility while preferring # prefix
	if num, err := strconv.ParseInt(prefix, 10, 64); err == nil {
		sess, err := s.GetByNumber(ctx, num)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess, nil
		}
		// If no session found by number, fall through to ID matching
		// (in case someone has numeric-prefixed IDs)
	}

	// Try exact ID match
	sess, err := s.Get(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		return sess, nil
	}

	// Try prefix match using expanded short ID
	pattern := ExpandShortID(prefix)
	row := s.queryDB().QueryRowContext(ctx,
		"SELECT "+s.sessionSelectCols()+" FROM sessions WHERE id LIKE ? ORDER BY created_at DESC LIMIT 1", pattern)
	sess, err = scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
	return s.enrichSessionProject(ctx, sess), err
}

// Update modifies an existing session's metadata fields.
// Token metrics (input_tokens, cached_input_tokens, cache_write_tokens, output_tokens)
// and turn counters (user_turns, llm_turns, tool_calls) are intentionally excluded — they are
// managed exclusively by atomic update paths to prevent stale in-memory values from clobbering
// accumulated totals.
func (s *SQLiteStore) Update(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now()
	if sess.Origin == "" {
		sess.Origin = OriginTUI
	}

	titleSkippedAtClause := ""
	if s.hasTitleSkippedAt {
		titleSkippedAtClause = ", title_skipped_at = ?"
	}
	reasoningEffortClause := ""
	if s.hasReasoningEffort {
		reasoningEffortClause = ", reasoning_effort = ?"
	}
	reasoningModeClause := ""
	if s.hasReasoningMode {
		reasoningModeClause = ", reasoning_mode = ?"
	}
	approvalModeClause := ""
	if s.hasApprovalMode {
		approvalModeClause = ", approval_mode = ?"
	}
	worktreeDirClause := ""
	if s.hasWorktreeDir {
		worktreeDirClause = ", worktree_dir = ?"
	}
	cwdAssignment := "cwd = ?"
	if s.hasProjectID {
		// Once a session has a project, its execution snapshot is immutable. This
		// database-side guard also prevents a stale full-row metadata update from
		// racing a successful BindSessionWorkspace and restoring old paths.
		cwdAssignment = "cwd = CASE WHEN COALESCE(project_id, '') <> '' THEN cwd ELSE ? END"
		if s.hasWorktreeDir {
			worktreeDirClause = ", worktree_dir = CASE WHEN COALESCE(project_id, '') <> '' THEN worktree_dir ELSE ? END"
		}
	}
	goalClause := ""
	if s.hasGoal {
		goalClause = ", goal = ?"
	}
	shareClause := ""
	if s.hasShare {
		shareClause = ", share = ?"
	}
	query := `
		UPDATE sessions SET name = ?, summary = ?, generated_short_title = ?, generated_long_title = ?, title_source = ?, title_generated_at = ?, title_basis_msg_seq = ?` +
		titleSkippedAtClause + `,
		       provider = ?, provider_key = ?, model = ?` + reasoningEffortClause + reasoningModeClause + `, mode = ?` + approvalModeClause + `, origin = ?,
		       agent = CASE WHEN COALESCE(agent, '') <> '' AND COALESCE(?, '') = '' THEN agent ELSE ? END, ` + cwdAssignment + worktreeDirClause + `,
		       updated_at = ?, archived = ?, pinned = ?, parent_id = ?, search = ?, tools = ?, mcp = ?,
		       status = ?, tags = ?` + goalClause + shareClause + `
		WHERE id = ?`

	args := []any{
		sess.Name, sess.Summary, nullString(sess.GeneratedShortTitle), nullString(sess.GeneratedLongTitle), nullString(string(sess.TitleSource)), nullTime(sess.TitleGeneratedAt), sess.TitleBasisMsgSeq,
	}
	if s.hasTitleSkippedAt {
		args = append(args, nullTime(sess.TitleSkippedAt))
	}
	args = append(args,
		sess.Provider, nullString(sess.ProviderKey), sess.Model,
	)
	if s.hasReasoningEffort {
		args = append(args, nullString(sess.ReasoningEffort))
	}
	if s.hasReasoningMode {
		args = append(args, nullString(sess.ReasoningMode))
	}
	args = append(args,
		string(sess.Mode),
	)
	if s.hasApprovalMode {
		args = append(args, nullString(string(sess.ApprovalMode)))
	}
	args = append(args,
		nullString(string(sess.Origin)), nullString(sess.Agent), nullString(sess.Agent), sess.CWD,
	)
	if s.hasWorktreeDir {
		args = append(args, nullString(sess.WorktreeDir))
	}
	args = append(args,
		sess.UpdatedAt, sess.Archived, sess.Pinned, nullString(sess.ParentID),
		sess.Search, nullString(sess.Tools), nullString(sess.MCP),
		string(sess.Status), nullString(sess.Tags),
	)
	if s.hasGoal {
		args = append(args, goalJSONString(sess.Goal))
	}
	if s.hasShare {
		args = append(args, shareJSONString(sess.Share))
	}
	args = append(args, sess.ID)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found: %s", sess.ID)
	}
	return nil
}

// UpdateGoal updates only the persisted goal state for a session.
func (s *SQLiteStore) UpdateGoal(ctx context.Context, id string, goal *Goal) error {
	if !s.hasGoal {
		return fmt.Errorf("goal column is unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is empty")
	}
	goal = goal.Clone()
	if goal != nil {
		goal.Normalize(time.Now())
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET goal = ?, updated_at = ?
		WHERE id = ?`, goalJSONString(goal), time.Now(), id)
	if err != nil {
		return fmt.Errorf("update goal: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	return nil
}

// UpdateShare updates only persisted share metadata for a session.
func (s *SQLiteStore) UpdateShare(ctx context.Context, id string, share *ShareState) error {
	if !s.hasShare {
		return fmt.Errorf("share column is unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is empty")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET share = ?, updated_at = ? WHERE id = ?`, shareJSONString(share), time.Now(), id)
	if err != nil {
		return fmt.Errorf("update share: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	return nil
}

// UpdateGeneratedTitle updates only generated title columns for a session.
func (s *SQLiteStore) UpdateGeneratedTitle(ctx context.Context, id, shortTitle, longTitle string, generatedAt time.Time, basisMsgSeq int) error {
	if !s.hasGeneratedTitles {
		return nil
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is empty")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET
		       generated_short_title = ?,
		       generated_long_title = ?,
		       title_source = CASE
		           WHEN title_source = ? OR TRIM(COALESCE(name, '')) <> '' THEN ?
		           ELSE ?
		       END,
		       title_generated_at = ?,
		       title_basis_msg_seq = ?,
		       updated_at = ?
		WHERE id = ?`,
		nullString(shortTitle), nullString(longTitle), string(TitleSourceUser), nullString(string(TitleSourceUser)), nullString(string(TitleSourceGenerated)), nullTime(generatedAt), basisMsgSeq, time.Now(), id)
	if err != nil {
		return fmt.Errorf("update generated title: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	return nil
}

// MarkTitleSkipped sets title_skipped_at on a session without bumping updated_at.
// This lets the autotitle job skip trivial sessions until real new messages arrive.
func (s *SQLiteStore) MarkTitleSkipped(ctx context.Context, id string, t time.Time) error {
	if !s.hasTitleSkippedAt {
		return nil // column absent on old schema; skip silently
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET title_skipped_at = ? WHERE id = ?", t, id)
	if err != nil {
		return fmt.Errorf("mark title skipped: %w", err)
	}
	return nil
}

// LoadPlanSnapshot loads the authoritative latest update_plan snapshot.
func (s *SQLiteStore) LoadPlanSnapshot(ctx context.Context, sessionID string) (planpkg.Snapshot, int64, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return planpkg.Snapshot{}, 0, nil
	}
	var raw string
	var version int64
	err := retryOnBusy(ctx, 5, func() error {
		return s.queryDB().QueryRowContext(ctx, `
			SELECT snapshot, version FROM session_plans WHERE session_id = ?
		`, sessionID).Scan(&raw, &version)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return planpkg.Snapshot{}, 0, nil
	}
	if err != nil {
		return planpkg.Snapshot{}, 0, fmt.Errorf("load plan snapshot: %w", err)
	}
	snapshot, err := planpkg.Parse(json.RawMessage(raw))
	if err != nil {
		decodeErr := fmt.Errorf("decode stored plan snapshot: %w", err)
		deleted, deleteErr := s.deletePlanSnapshotVersion(ctx, sessionID, version)
		if deleteErr != nil {
			return planpkg.Snapshot{}, 0, fmt.Errorf("%v; discard invalid plan snapshot: %w", decodeErr, deleteErr)
		}
		if deleted {
			slog.Warn("discarded invalid stored plan snapshot", "session_id", sessionID, "version", version, "error", decodeErr)
		} else {
			slog.Warn("stored plan snapshot was invalid; cleanup skipped because the version changed", "session_id", sessionID, "version", version, "error", decodeErr)
		}
		return planpkg.Snapshot{}, 0, nil
	}
	return snapshot, version, nil
}

// SavePlanSnapshot atomically replaces the current snapshot and increments the
// existing row's version. Saving after a clear creates a new row at version 1.
func (s *SQLiteStore) SavePlanSnapshot(ctx context.Context, sessionID string, snapshot planpkg.Snapshot) (int64, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0, nil
	}
	raw, err := snapshot.CanonicalJSON()
	if err != nil {
		return 0, fmt.Errorf("encode plan snapshot: %w", err)
	}
	var version int64
	err = retryOnBusy(ctx, 5, func() error {
		return s.db.QueryRowContext(ctx, `
			INSERT INTO session_plans (session_id, snapshot, version, updated_at)
			VALUES (?, ?, 1, CURRENT_TIMESTAMP)
			ON CONFLICT(session_id) DO UPDATE SET
			    snapshot = excluded.snapshot,
			    version = session_plans.version + 1,
			    updated_at = CURRENT_TIMESTAMP
			RETURNING version
		`, sessionID, string(raw)).Scan(&version)
	})
	if err != nil {
		return 0, fmt.Errorf("save plan snapshot: %w", err)
	}
	return version, nil
}

func (s *SQLiteStore) deletePlanSnapshotVersion(ctx context.Context, sessionID string, version int64) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || version <= 0 {
		return false, nil
	}
	var deleted bool
	err := retryOnBusy(ctx, 5, func() error {
		result, err := s.db.ExecContext(ctx, `
			DELETE FROM session_plans WHERE session_id = ? AND version = ?
		`, sessionID, version)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		deleted = rows > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("delete plan snapshot version: %w", err)
	}
	return deleted, nil
}

// DeletePlanSnapshot clears the current snapshot row for a session.
func (s *SQLiteStore) DeletePlanSnapshot(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	return retryOnBusy(ctx, 5, func() error {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM session_plans WHERE session_id = ?`, sessionID); err != nil {
			return fmt.Errorf("delete plan snapshot: %w", err)
		}
		return nil
	})
}

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
		entry.Origin = llm.SteeringOriginLegacy
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
		SELECT pending.id, pending.message, pending.display_text, pending.attachment_summary, pending.created_at, pending.acceptance_sequence, pending.origin, pending.owner_kind, pending.owner_id, pending.owner_fence
		FROM session_pending_steering pending
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
		var messageJSON string
		if err := rows.Scan(&entry.ID, &messageJSON, &entry.DisplayText, &entry.AttachmentSummary, &entry.CreatedAt, &entry.AcceptanceSequence, &entry.Origin, &entry.OwnerKind, &entry.OwnerID, &entry.OwnerFence); err != nil {
			return nil, fmt.Errorf("scan pending steering: %w", err)
		}
		if err := json.Unmarshal([]byte(messageJSON), &entry.Message); err != nil {
			return nil, fmt.Errorf("decode pending steering %q: %w", entry.ID, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending steering: %w", err)
	}
	return entries, nil
}

// SaveProviderState stores opaque provider-owned resume state for a session.
func (s *SQLiteStore) SaveProviderState(ctx context.Context, sessionID, providerKey string, state []byte) error {
	sessionID = strings.TrimSpace(sessionID)
	providerKey = strings.TrimSpace(providerKey)
	if sessionID == "" || providerKey == "" {
		return nil
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO session_provider_state (session_id, provider_key, state, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(session_id, provider_key) DO UPDATE SET
			    state = excluded.state,
			    updated_at = CURRENT_TIMESTAMP
		`, sessionID, providerKey, state)
		if err != nil {
			return fmt.Errorf("save provider state: %w", err)
		}
		return nil
	})
}

// LoadProviderState returns opaque provider-owned resume state for a session.
func (s *SQLiteStore) LoadProviderState(ctx context.Context, sessionID, providerKey string) ([]byte, error) {
	sessionID = strings.TrimSpace(sessionID)
	providerKey = strings.TrimSpace(providerKey)
	if sessionID == "" || providerKey == "" {
		return nil, nil
	}
	var state []byte
	err := s.queryDB().QueryRowContext(ctx, `
		SELECT state FROM session_provider_state
		WHERE session_id = ? AND provider_key = ?
	`, sessionID, providerKey).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load provider state: %w", err)
	}
	return append([]byte(nil), state...), nil
}

// DeleteProviderState removes provider-owned resume state for a session.
func (s *SQLiteStore) DeleteProviderState(ctx context.Context, sessionID, providerKey string) error {
	sessionID = strings.TrimSpace(sessionID)
	providerKey = strings.TrimSpace(providerKey)
	if sessionID == "" || providerKey == "" {
		return nil
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			DELETE FROM session_provider_state
			WHERE session_id = ? AND provider_key = ?
		`, sessionID, providerKey)
		if err != nil {
			return fmt.Errorf("delete provider state: %w", err)
		}
		return nil
	})
}

// UpdateMetrics atomically increments the metrics fields for a session.
// All token counters use += to avoid clobbering concurrent accumulation.
func (s *SQLiteStore) UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int) error {
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET
			       llm_turns = llm_turns + ?,
			       tool_calls = tool_calls + ?,
			       input_tokens = input_tokens + ?,
			       cached_input_tokens = cached_input_tokens + ?,
			       cache_write_tokens = cache_write_tokens + ?,
			       output_tokens = output_tokens + ?,
			       updated_at = ?
			WHERE id = ?`,
			llmTurns, toolCalls, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens, time.Now(), id)
		return err
	})
}

// UpdateContextEstimate persists the last observed provider context estimate so
// resumed sessions can display a realistic context meter before the next turn.
func (s *SQLiteStore) UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error {
	if !s.hasLastTotalTokens || !s.hasLastMessageCount {
		return nil
	}
	if lastTotalTokens < 0 {
		lastTotalTokens = 0
	}
	if lastMessageCount < 0 {
		lastMessageCount = 0
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET
			       last_total_tokens = ?,
			       last_message_count = ?,
			       updated_at = ?
			WHERE id = ?`,
			lastTotalTokens, lastMessageCount, time.Now(), id)
		return err
	})
}

// UpdateStatus updates just the session status.
func (s *SQLiteStore) UpdateStatus(ctx context.Context, id string, status SessionStatus) error {
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET status = ?, updated_at = ?
			WHERE id = ?`,
			string(status), time.Now(), id)
		return err
	})
}

// IncrementUserTurns increments the user turn count and updates last_user_message_at.
func (s *SQLiteStore) IncrementUserTurns(ctx context.Context, id string) error {
	return retryOnBusy(ctx, 5, func() error {
		now := time.Now()
		lastUserMsgClause := ""
		if s.hasLastUserMessageAt {
			lastUserMsgClause = ", last_user_message_at = ?"
		}
		query := `UPDATE sessions SET user_turns = user_turns + 1, updated_at = ?` + lastUserMsgClause + ` WHERE id = ?`
		args := []any{now}
		if s.hasLastUserMessageAt {
			args = append(args, now)
		}
		args = append(args, id)
		_, err := s.db.ExecContext(ctx, query, args...)
		return err
	})
}

// Delete removes a session and its messages.
func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	// Foreign key cascade handles messages
	result, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found: %s", id)
	}
	return nil
}

// List returns sessions matching the options.
func (s *SQLiteStore) sessionCategoryFilter(categories []string) (string, []any) {
	if len(categories) == 0 {
		return "", nil
	}
	clauses := make([]string, 0, len(categories))
	args := make([]any, 0, len(categories))
	sawSpecificCategory := false
	for _, raw := range categories {
		category := strings.ToLower(strings.TrimSpace(raw))
		switch category {
		case "", "all":
			clauses = nil
		case "chat":
			sawSpecificCategory = true
			if s.hasOrigin {
				clauses = append(clauses, "(s.mode = 'chat' AND COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') = 'tui')")
			} else {
				clauses = append(clauses, "(s.mode = 'chat')")
			}
		case "web":
			sawSpecificCategory = true
			if s.hasOrigin {
				clauses = append(clauses, "(COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') = 'web')")
			}
		case "ask", "plan", "exec":
			sawSpecificCategory = true
			clauses = append(clauses, "(s.mode = ?)")
			args = append(args, category)
		}
		if clauses == nil {
			break
		}
	}
	if len(clauses) > 0 {
		return " AND (" + strings.Join(clauses, " OR ") + ")", args
	}
	if sawSpecificCategory {
		return " AND 1 = 0", nil
	}
	return "", nil
}

func (s *SQLiteStore) List(ctx context.Context, opts ListOptions) ([]SessionSummary, error) {
	cacheWriteCol := "0"
	if s.hasCacheWriteTokens {
		cacheWriteCol = "s.cache_write_tokens"
	}
	originCol := "'tui'"
	if s.hasOrigin {
		originCol = "COALESCE(NULLIF(TRIM(s.origin), ''), 'tui')"
	}
	pinnedCol := "FALSE"
	if s.hasPinned {
		pinnedCol = "COALESCE(s.pinned, FALSE)"
	}
	generatedShortCol := "''"
	generatedLongCol := "''"
	titleSourceCol := "''"
	if s.hasGeneratedTitles {
		generatedShortCol = "s.generated_short_title"
		generatedLongCol = "s.generated_long_title"
		titleSourceCol = "s.title_source"
	}
	lastMessageAtCol := "NULL"
	if s.hasLastMessageAt {
		lastMessageAtCol = "s.last_message_at"
	}
	lastUserMessageAtCol := "NULL"
	if s.hasLastUserMessageAt {
		lastUserMessageAtCol = "s.last_user_message_at"
	}
	goalCol := "NULL"
	if s.hasGoal {
		goalCol = "s.goal"
	}
	shareCol := "NULL"
	if s.hasShare {
		shareCol = "s.share"
	}
	messageCountCol := s.conversationMessageCountSelectSQL("s")
	worktreeDirCol := "''"
	if s.hasWorktreeDir {
		worktreeDirCol = "COALESCE(s.worktree_dir, '')"
	}
	projectIDCol := "''"
	projectNameCol := "''"
	projectJoin := ""
	if s.hasProjectID {
		projectIDCol = "COALESCE(s.project_id, '')"
		if s.hasProjectsTable {
			projectNameCol = "COALESCE(p.name, '')"
			projectJoin = " LEFT JOIN projects p ON p.id = s.project_id"
		}
	}
	transcriptRevCol := "0"
	if s.hasTranscriptRev {
		transcriptRevCol = "COALESCE(s.transcript_rev, 0)"
	}
	fromClause := "FROM sessions s"
	if opts.SortByNumberDesc {
		// Completed-session walks page by descending session number. Force the
		// number index so SQLite can continue scanning from the last seen number
		// instead of picking a filter-only index and re-sorting each page.
		fromClause = "FROM sessions s INDEXED BY idx_sessions_number"
	}
	query := `
		SELECT s.id, s.number, s.name, s.summary, ` + generatedShortCol + `, ` + generatedLongCol + `, ` + titleSourceCol + `,
		       s.provider, COALESCE(s.provider_key, ''), s.model, s.mode, ` + originCol + `, COALESCE(s.agent, ''), s.archived, ` + pinnedCol + `, s.created_at, s.updated_at, ` + lastMessageAtCol + `, ` + lastUserMessageAtCol + `,
		       ` + messageCountCol + ` as message_count, ` + transcriptRevCol + ` as transcript_rev,
		       s.user_turns, s.llm_turns, s.tool_calls, s.input_tokens, s.cached_input_tokens, ` + cacheWriteCol + `, s.output_tokens, s.status, s.tags, COALESCE(s.cwd, ''), ` + worktreeDirCol + `, ` + projectIDCol + `, ` + projectNameCol + `, ` + goalCol + `, ` + shareCol + `
		` + fromClause + projectJoin + `
		WHERE 1=1`
	args := []any{}

	if len(opts.IDs) > 0 {
		seen := make(map[string]struct{}, len(opts.IDs))
		placeholders := make([]string, 0, len(opts.IDs))
		for _, rawID := range opts.IDs {
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		if len(placeholders) == 0 {
			return []SessionSummary{}, nil
		}
		query += " AND s.id IN (" + strings.Join(placeholders, ",") + ")"
	}

	if opts.ParentID != "" {
		query += " AND s.parent_id = ?"
		args = append(args, opts.ParentID)
	} else if opts.ExcludeSubagents {
		query += " AND s.parent_id IS NULL"
	}
	if opts.Name != "" {
		query += " AND s.name = ?"
		args = append(args, opts.Name)
	}
	if opts.Provider != "" {
		query += " AND s.provider = ?"
		args = append(args, opts.Provider)
	}
	if opts.Model != "" {
		query += " AND s.model = ?"
		args = append(args, opts.Model)
	}
	if opts.Mode != "" {
		query += " AND s.mode = ?"
		args = append(args, string(opts.Mode))
	}
	if agent := strings.TrimSpace(opts.Agent); agent != "" {
		query += " AND COALESCE(NULLIF(TRIM(s.agent), ''), 'default') = ?"
		args = append(args, agent)
	}
	if opts.Status != "" {
		query += " AND s.status = ?"
		args = append(args, string(opts.Status))
	}
	if opts.Tag != "" {
		// Substring match on comma-separated tags
		query += " AND (',' || s.tags || ',' LIKE '%,' || ? || ',%')"
		args = append(args, opts.Tag)
	}
	if opts.ProjectID != "" {
		if !s.hasProjectID {
			return []SessionSummary{}, nil
		}
		query += " AND s.project_id = ?"
		args = append(args, opts.ProjectID)
	} else if opts.NoProject && s.hasProjectID {
		query += " AND s.project_id IS NULL"
	}
	if cursor := opts.ProjectCursor; cursor != nil {
		if !opts.SortByActivity || !s.hasPinned || !s.hasLastMessageAt || !s.hasLastUserMessageAt {
			return nil, fmt.Errorf("project cursor requires activity sorting on the current schema")
		}
		activityExpr := "COALESCE(s.last_message_at, s.last_user_message_at, s.created_at)"
		query += ` AND (
			COALESCE(s.pinned, FALSE) < ? OR
			(COALESCE(s.pinned, FALSE) = ? AND (
				` + activityExpr + ` < ? OR
				(` + activityExpr + ` = ? AND s.number < ?)
			)))`
		args = append(args, cursor.Pinned, cursor.Pinned, cursor.ActivityAt, cursor.ActivityAt, cursor.Number)
	}
	categoryClause, categoryArgs := s.sessionCategoryFilter(opts.Categories)
	query += categoryClause
	args = append(args, categoryArgs...)
	if opts.BeforeNumber > 0 {
		query += " AND s.number < ?"
		args = append(args, opts.BeforeNumber)
	}
	if !opts.Archived {
		query += " AND s.archived = FALSE"
	}

	if opts.SortByNumberDesc {
		query += " ORDER BY s.number DESC"
	} else {
		// Sort by last user message time (when the user last interacted), falling back
		// to created_at for sessions with no user messages yet. This prevents background
		// activity (autotitle, mining, status changes) from reordering the sidebar.
		// Web sidebar callers set SortByActivity to use last_message_at instead so
		// assistant-only turns also surface (keeps the top-N window aligned with the
		// client-side "any-message" ordering).
		sortCol := "s.updated_at"
		if opts.SortByActivity && s.hasLastMessageAt {
			sortCol = "COALESCE(s.last_message_at, s.last_user_message_at, s.created_at)"
		} else if s.hasLastUserMessageAt {
			sortCol = "COALESCE(s.last_user_message_at, s.created_at)"
		}
		if s.hasPinned {
			query += " ORDER BY COALESCE(s.pinned, FALSE) DESC, " + sortCol + " DESC, s.number DESC"
		} else {
			query += " ORDER BY " + sortCol + " DESC, s.number DESC"
		}
	}

	limit := opts.Limit
	if limit == 0 {
		limit = 50 // Default
	}
	if limit < 0 {
		query += " LIMIT -1"
	} else {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	if opts.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, opts.Offset)
	}

	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var sum SessionSummary
		var number sql.NullInt64
		var mode, status, tags, generatedShortTitle, generatedLongTitle, titleSource, origin, cwd, worktreeDir, projectID, projectName, goalRaw, shareRaw sql.NullString
		var lastMessageAt, lastUserMessageAt sql.NullTime
		err := rows.Scan(&sum.ID, &number, &sum.Name, &sum.Summary, &generatedShortTitle, &generatedLongTitle, &titleSource, &sum.Provider, &sum.ProviderKey, &sum.Model, &mode,
			&origin, &sum.Agent, &sum.Archived, &sum.Pinned, &sum.CreatedAt, &sum.UpdatedAt, &lastMessageAt, &lastUserMessageAt, &sum.MessageCount, &sum.TranscriptRev,
			&sum.UserTurns, &sum.LLMTurns, &sum.ToolCalls, &sum.InputTokens, &sum.CachedInputTokens, &sum.CacheWriteTokens, &sum.OutputTokens,
			&status, &tags, &cwd, &worktreeDir, &projectID, &projectName, &goalRaw, &shareRaw)
		if err != nil {
			return nil, fmt.Errorf("scan session summary: %w", err)
		}
		if lastMessageAt.Valid {
			sum.LastMessageAt = lastMessageAt.Time
		} else if opts.SortByActivity && lastUserMessageAt.Valid {
			// Cursor encoding must receive the exact fallback value used by the
			// activity ORDER BY and keyset predicate.
			sum.LastMessageAt = lastUserMessageAt.Time
		} else if opts.SortByActivity {
			sum.LastMessageAt = sum.CreatedAt
		}
		if number.Valid {
			sum.Number = number.Int64
		}
		if generatedShortTitle.Valid {
			sum.GeneratedShortTitle = generatedShortTitle.String
		}
		if generatedLongTitle.Valid {
			sum.GeneratedLongTitle = generatedLongTitle.String
		}
		if titleSource.Valid {
			sum.TitleSource = SessionTitleSource(titleSource.String)
		}
		if mode.Valid {
			sum.Mode = SessionMode(mode.String)
		}
		if origin.Valid {
			sum.Origin = SessionOrigin(origin.String)
		} else {
			sum.Origin = OriginTUI
		}
		if status.Valid {
			sum.Status = SessionStatus(status.String)
		}
		if tags.Valid {
			sum.Tags = tags.String
		}
		if cwd.Valid {
			sum.CWD = cwd.String
		}
		if worktreeDir.Valid {
			sum.WorktreeDir = worktreeDir.String
		}
		if projectID.Valid {
			sum.ProjectID = projectID.String
		}
		if projectName.Valid {
			sum.ProjectName = projectName.String
		}
		if goal := parseGoalJSONString(goalRaw); goal != nil {
			sum.Goal = goal
		}
		if share := parseShareJSONString(shareRaw); share != nil {
			sum.Share = share
		}
		results = append(results, sum)
	}
	return results, rows.Err()
}

// Search finds sessions containing the query text using FTS5.
func (s *SQLiteStore) Search(ctx context.Context, opts SearchOptions) ([]SearchResult, error) {
	if opts.Limit == 0 {
		opts.Limit = 20
	}

	ftsQuery := sqlitefts.LiteralQuery(opts.Query)
	if ftsQuery == "" {
		return []SearchResult{}, nil
	}

	messageCountCol := s.conversationMessageCountSelectSQL("s")
	compactionTailClause := ""
	if s.hasMessageCompactionTail {
		compactionTailClause = " AND COALESCE(m.compaction_tail, FALSE) = FALSE"
	}

	originCol := "'tui'"
	if s.hasOrigin {
		originCol = "COALESCE(NULLIF(TRIM(s.origin), ''), 'tui')"
	}
	pinnedCol := "FALSE"
	if s.hasPinned {
		pinnedCol = "COALESCE(s.pinned, FALSE)"
	}
	generatedShortCol := "''"
	generatedLongCol := "''"
	titleSourceCol := "''"
	if s.hasGeneratedTitles {
		generatedShortCol = "s.generated_short_title"
		generatedLongCol = "s.generated_long_title"
		titleSourceCol = "s.title_source"
	}
	lastMessageAtCol := "NULL"
	if s.hasLastMessageAt {
		lastMessageAtCol = "s.last_message_at"
	}

	projectIDCol := "''"
	projectNameCol := "''"
	projectJoin := ""
	if s.hasProjectID {
		projectIDCol = "COALESCE(s.project_id, '')"
		if s.hasProjectsTable {
			projectNameCol = "COALESCE(p.name, '')"
			projectJoin = " LEFT JOIN projects p ON p.id = s.project_id"
		}
	}

	filterClause := ""
	args := []any{ftsQuery}
	categoryClause, categoryArgs := s.sessionCategoryFilter(opts.Categories)
	filterClause += categoryClause
	args = append(args, categoryArgs...)
	if !opts.Archived {
		filterClause += " AND s.archived = FALSE"
	}
	if opts.ExcludeSubagents {
		filterClause += " AND s.parent_id IS NULL"
	}
	if opts.ProjectID != "" {
		if !s.hasProjectID {
			return []SearchResult{}, nil
		}
		filterClause += " AND s.project_id = ?"
		args = append(args, opts.ProjectID)
	}
	args = append(args, opts.Limit)

	rows, err := s.queryDB().QueryContext(ctx, `
		WITH raw_matches AS (
			SELECT rowid AS message_id, rank AS match_rank
			FROM messages_fts
			WHERE messages_fts MATCH ?
		), ranked_matches AS (
			SELECT m.id AS message_id, m.session_id, raw_matches.match_rank,
			       ROW_NUMBER() OVER (PARTITION BY m.session_id ORDER BY raw_matches.match_rank, m.id) AS session_row
			FROM raw_matches
			JOIN messages m ON m.id = raw_matches.message_id
			JOIN sessions s ON s.id = m.session_id
			WHERE 1=1`+compactionTailClause+filterClause+`
		), session_matches AS (
			SELECT message_id, session_id, match_rank
			FROM ranked_matches
			WHERE session_row = 1
			ORDER BY match_rank, message_id
			LIMIT ?
		)
		SELECT m.session_id, s.number, m.id, s.name, s.summary, `+generatedShortCol+` AS generated_short_title,
		       `+generatedLongCol+` AS generated_long_title, `+titleSourceCol+` AS title_source,
		       snippet(messages_fts, 0, '**', '**', '...', 32) AS snippet, s.provider, COALESCE(s.provider_key, '') AS provider_key,
		       s.model, s.mode, `+originCol+` AS origin, s.archived, `+pinnedCol+` AS pinned, s.status,
		       `+messageCountCol+` AS message_count, `+projectIDCol+` AS project_id, `+projectNameCol+` AS project_name,
		       s.created_at, s.updated_at, `+lastMessageAtCol+` AS last_message_at,
		       m.created_at AS message_created_at
		FROM session_matches sm
		JOIN messages m ON m.id = sm.message_id
		JOIN sessions s ON s.id = sm.session_id`+projectJoin+`
		JOIN messages_fts ON messages_fts.rowid = sm.message_id
		ORDER BY sm.match_rank, sm.message_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var number sql.NullInt64
		var generatedShortTitle, generatedLongTitle, titleSource, providerKey, mode, origin, status, projectID, projectName sql.NullString
		var lastMessageAt sql.NullTime
		err := rows.Scan(&r.SessionID, &number, &r.MessageID, &r.SessionName, &r.Summary,
			&generatedShortTitle, &generatedLongTitle, &titleSource, &r.Snippet, &r.Provider, &providerKey,
			&r.Model, &mode, &origin, &r.Archived, &r.Pinned, &status, &r.MessageCount, &projectID, &projectName,
			&r.SessionCreatedAt, &r.UpdatedAt, &lastMessageAt, &r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		if number.Valid {
			r.SessionNumber = number.Int64
		}
		if generatedShortTitle.Valid {
			r.GeneratedShortTitle = generatedShortTitle.String
		}
		if generatedLongTitle.Valid {
			r.GeneratedLongTitle = generatedLongTitle.String
		}
		if titleSource.Valid {
			r.TitleSource = SessionTitleSource(titleSource.String)
		}
		if providerKey.Valid {
			r.ProviderKey = providerKey.String
		}
		if mode.Valid {
			r.Mode = SessionMode(mode.String)
		}
		if origin.Valid {
			r.Origin = SessionOrigin(origin.String)
		} else {
			r.Origin = OriginTUI
		}
		if status.Valid {
			r.Status = SessionStatus(status.String)
		}
		if projectID.Valid {
			r.ProjectID = projectID.String
		}
		if projectName.Valid {
			r.ProjectName = projectName.String
		}
		if lastMessageAt.Valid {
			r.LastMessageAt = lastMessageAt.Time
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// AddMessage adds a message to a session.
// If msg.Sequence < 0, the sequence number is auto-allocated atomically.
