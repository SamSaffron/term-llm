package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *SQLiteStore) SetCurrent(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO metadata (key, value) VALUES ('current_session', ?)`,
		sessionID)
	return err
}

// GetCurrent retrieves the current session.
func (s *SQLiteStore) GetCurrent(ctx context.Context) (*Session, error) {
	var sessionID string
	err := s.queryDB().QueryRowContext(ctx,
		"SELECT value FROM metadata WHERE key = 'current_session'").Scan(&sessionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, sessionID)
}

// ClearCurrent removes the current session marker.
func (s *SQLiteStore) ClearCurrent(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM metadata WHERE key = 'current_session'")
	return err
}

// SavePushSubscription upserts a Web Push subscription.
func (s *SQLiteStore) SavePushSubscription(ctx context.Context, sub *PushSubscription) error {
	stored, err := s.UpsertPushSubscription(ctx, sub)
	if err == nil && stored != nil {
		*sub = *stored
	}
	return err
}

func scanPushSubscription(scanner interface{ Scan(...any) error }) (*PushSubscription, error) {
	var sub PushSubscription
	var updated, lastUsed, failureAt sql.NullString
	if err := scanner.Scan(
		&sub.ID, &sub.Endpoint, &sub.KeyP256DH, &sub.KeyAuth, &sub.Status, &sub.VAPIDKeyID,
		&updated, &lastUsed, &sub.LastFailureCode, &sub.LastFailure, &failureAt,
	); err != nil {
		return nil, err
	}
	parse := func(value sql.NullString) time.Time {
		if !value.Valid {
			return time.Time{}
		}
		for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano} {
			if parsed, err := time.Parse(layout, value.String); err == nil {
				return parsed.UTC()
			}
		}
		return time.Time{}
	}
	sub.UpdatedAt = parse(updated)
	sub.LastUsedAt = parse(lastUsed)
	sub.LastFailureAt = parse(failureAt)
	return &sub, nil
}

func (s *SQLiteStore) UpsertPushSubscription(ctx context.Context, sub *PushSubscription) (*PushSubscription, error) {
	if sub == nil || strings.TrimSpace(sub.Endpoint) == "" || strings.TrimSpace(sub.KeyP256DH) == "" || strings.TrimSpace(sub.KeyAuth) == "" {
		return nil, fmt.Errorf("save push subscription: endpoint and keys are required")
	}
	if sub.ID == "" {
		sub.ID = NewID()
	}
	if sub.Status == "" {
		sub.Status = "active"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO push_subscriptions (id, endpoint, key_p256dh, key_auth, status, vapid_key_id, updated_at, last_used_at)
		VALUES (?, ?, ?, ?, 'active', ?, datetime('now'), datetime('now'))
		ON CONFLICT(endpoint) DO UPDATE SET
			key_p256dh = excluded.key_p256dh,
			key_auth = excluded.key_auth,
			status = 'active',
			vapid_key_id = excluded.vapid_key_id,
			updated_at = datetime('now'),
			last_used_at = datetime('now'),
			last_failure_code = '',
			last_failure = '',
			last_failure_at = NULL`,
		sub.ID, strings.TrimSpace(sub.Endpoint), strings.TrimSpace(sub.KeyP256DH), strings.TrimSpace(sub.KeyAuth), strings.TrimSpace(sub.VAPIDKeyID))
	if err != nil {
		return nil, fmt.Errorf("save push subscription: %w", err)
	}
	stored, err := scanPushSubscription(s.queryDB().QueryRowContext(ctx, `
		SELECT id, endpoint, key_p256dh, key_auth, status, vapid_key_id, updated_at,
		       last_used_at, last_failure_code, last_failure, last_failure_at
		FROM push_subscriptions WHERE endpoint = ?`, strings.TrimSpace(sub.Endpoint)))
	if err != nil {
		return nil, fmt.Errorf("read saved push subscription: %w", err)
	}
	return stored, nil
}

func (s *SQLiteStore) GetPushSubscription(ctx context.Context, id string) (*PushSubscription, error) {
	sub, err := scanPushSubscription(s.queryDB().QueryRowContext(ctx, `
		SELECT id, endpoint, key_p256dh, key_auth, status, vapid_key_id, updated_at,
		       last_used_at, last_failure_code, last_failure, last_failure_at
		FROM push_subscriptions WHERE id = ?`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get push subscription: %w", err)
	}
	return sub, nil
}

// DeletePushSubscription removes a Web Push subscription by endpoint.
func (s *SQLiteStore) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	if err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeletePushSubscriptionByID(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE id = ?", strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("delete push subscription by id: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkPushSubscriptionStale(ctx context.Context, id, code, detail string) error {
	if len(detail) > 240 {
		detail = detail[:240]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions
		SET status = 'stale', updated_at = datetime('now'), last_failure_code = ?,
		    last_failure = ?, last_failure_at = datetime('now') WHERE id = ?`, code, detail, id)
	return err
}

func (s *SQLiteStore) MarkPushSubscriptionUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions
		SET status = 'active', updated_at = datetime('now'), last_used_at = datetime('now'),
		    last_failure_code = '', last_failure = '', last_failure_at = NULL WHERE id = ?`, id)
	return err
}

// ListPushSubscriptions returns all stored Web Push subscriptions.
func (s *SQLiteStore) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT id, endpoint, key_p256dh, key_auth, status, vapid_key_id, updated_at,
		       last_used_at, last_failure_code, last_failure, last_failure_at
		FROM push_subscriptions
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []PushSubscription
	for rows.Next() {
		sub, err := scanPushSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("scan push subscription: %w", err)
		}
		subs = append(subs, *sub)
	}
	return subs, rows.Err()
}

func (s *SQLiteStore) EnqueueCompletionPush(ctx context.Context, item CompletionPushOutboxItem) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO completion_push_outbox
		(event_id, response_id, subscription_id, payload) VALUES (?, ?, ?, ?)
		ON CONFLICT(response_id, subscription_id) DO NOTHING`, item.EventID, item.ResponseID, item.SubscriptionID, item.Payload)
	if err != nil {
		return false, fmt.Errorf("enqueue completion push: %w", err)
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

func (s *SQLiteStore) ListDueCompletionPushes(ctx context.Context, now time.Time, limit int) ([]CompletionPushOutboxItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := s.queryDB().QueryContext(ctx, `SELECT id, event_id, response_id, subscription_id, payload, attempt_count
		FROM completion_push_outbox WHERE status = 'pending' AND next_attempt_at <= ?
		ORDER BY next_attempt_at, id LIMIT ?`, now.UTC().Format("2006-01-02 15:04:05"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []CompletionPushOutboxItem
	for rows.Next() {
		var item CompletionPushOutboxItem
		if err := rows.Scan(&item.ID, &item.EventID, &item.ResponseID, &item.SubscriptionID, &item.Payload, &item.AttemptCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *SQLiteStore) MarkCompletionPushDelivered(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE completion_push_outbox SET status = 'delivered',
		attempt_count = attempt_count + 1, updated_at = datetime('now'), last_error = '' WHERE id = ?`, id)
	return err
}

func (s *SQLiteStore) RetryCompletionPush(ctx context.Context, id int64, next time.Time, lastError string) error {
	if len(lastError) > 240 {
		lastError = lastError[:240]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE completion_push_outbox SET attempt_count = attempt_count + 1,
		next_attempt_at = ?, updated_at = datetime('now'), last_error = ? WHERE id = ?`, next.UTC().Format("2006-01-02 15:04:05"), lastError, id)
	return err
}

func (s *SQLiteStore) MarkCompletionPushDead(ctx context.Context, id int64, lastError string) error {
	if len(lastError) > 240 {
		lastError = lastError[:240]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE completion_push_outbox SET status = 'dead',
		attempt_count = attempt_count + 1, updated_at = datetime('now'), last_error = ? WHERE id = ?`, lastError, id)
	return err
}

func (s *SQLiteStore) PruneCompletionPushOutbox(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM completion_push_outbox
		WHERE status IN ('delivered', 'dead') AND updated_at < ?`, before.UTC().Format("2006-01-02 15:04:05"))
	return err
}

func (s *SQLiteStore) ReadOnly() bool {
	return s != nil && s.cfg.ReadOnly
}

// Close closes the database connections.
func (s *SQLiteStore) Close() error {
	var readErr error
	if s.readDB != nil {
		readErr = s.readDB.Close()
	}
	var responseRunReadErr error
	if s.responseRunReadDB != nil {
		responseRunReadErr = s.responseRunReadDB.Close()
	}
	return errors.Join(readErr, responseRunReadErr, s.db.Close())
}

// setCurrentColumns records optional columns that are guaranteed to exist after
// read-write initialization/migration reaches the current schema.
func (s *SQLiteStore) setCurrentColumns() {
	s.hasGeneratedTitles = true
	s.hasCompactionSeq = true
	s.hasCompactionCount = true
	s.hasCacheWriteTokens = true
	s.hasOrigin = true
	s.hasPinned = true
	s.hasTitleSkippedAt = true
	s.hasLastUserMessageAt = true
	s.hasLastMessageAt = true
	s.hasLastTotalTokens = true
	s.hasLastMessageCount = true
	s.hasMessageCount = true
	s.hasReasoningEffort = true
	s.hasReasoningMode = true
	s.hasApprovalMode = true
	s.hasWorktreeDir = true
	s.hasGoal = true
	s.hasShare = true
	s.hasTranscriptRev = true
	s.hasMessagesTable = true
	s.hasMessageCompactionTail = true
	s.hasMessageStreamIdentity = true
	s.hasMessageClientID = true
	s.hasSessionBranches = true
	s.hasProjectID = true
	s.hasProjectsTable = true
}

func (s *SQLiteStore) probeProjectsTable() {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'projects'`).Scan(&count); err == nil {
		s.hasProjectsTable = count > 0
	}
}

func (s *SQLiteStore) probeBranchTable() {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_branches'`).Scan(&count); err == nil {
		s.hasSessionBranches = count > 0
	}
}

// probeSessionColumns checks optional session columns in a single PRAGMA scan.
// Read-only mode skips migrations and may open a database created before one or
// more optional columns existed, so callers use these flags to build compatible
// SELECT/UPDATE statements.
func (s *SQLiteStore) probeSessionColumns() {
	rows, err := s.db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return
		}
		switch name {
		case "generated_short_title":
			s.hasGeneratedTitles = true
		case "compaction_seq":
			s.hasCompactionSeq = true
		case "compaction_count":
			s.hasCompactionCount = true
		case "cache_write_tokens":
			s.hasCacheWriteTokens = true
		case "origin":
			s.hasOrigin = true
		case "pinned":
			s.hasPinned = true
		case "title_skipped_at":
			s.hasTitleSkippedAt = true
		case "last_user_message_at":
			s.hasLastUserMessageAt = true
		case "last_message_at":
			s.hasLastMessageAt = true
		case "last_total_tokens":
			s.hasLastTotalTokens = true
		case "last_message_count":
			s.hasLastMessageCount = true
		case "message_count":
			s.hasMessageCount = true
		case "reasoning_effort":
			s.hasReasoningEffort = true
		case "reasoning_mode":
			s.hasReasoningMode = true
		case "approval_mode":
			s.hasApprovalMode = true
		case "worktree_dir":
			s.hasWorktreeDir = true
		case "goal":
			s.hasGoal = true
		case "share":
			s.hasShare = true
		case "transcript_rev":
			s.hasTranscriptRev = true
		case "project_id":
			s.hasProjectID = true
		}
	}
}

func (s *SQLiteStore) probeMessageColumns() {
	rows, err := s.db.Query("PRAGMA table_info(messages)")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return
		}
		s.hasMessagesTable = true
		switch name {
		case "compaction_tail":
			s.hasMessageCompactionTail = true
		case "response_id":
			s.hasMessageStreamIdentity = true
		case "client_message_id":
			s.hasMessageClientID = true
		}
	}
}

// sessionSelectCols returns the SELECT column list for session queries.
// Excludes compaction_seq when the column doesn't exist (old DB in read-only mode).
func (s *SQLiteStore) sessionSelectCols() string {
	base := `id, number, name, summary`
	if s.hasGeneratedTitles {
		base += ", generated_short_title, generated_long_title, title_source, title_generated_at, title_basis_msg_seq"
		if s.hasTitleSkippedAt {
			base += ", title_skipped_at"
		} else {
			base += ", NULL AS title_skipped_at"
		}
	}
	base += `,
	       provider, provider_key, model`
	if s.hasReasoningEffort {
		base += ", reasoning_effort"
	} else {
		base += ", NULL AS reasoning_effort"
	}
	if s.hasReasoningMode {
		base += ", reasoning_mode"
	} else {
		base += ", NULL AS reasoning_mode"
	}
	base += `, mode`
	if s.hasApprovalMode {
		base += ", approval_mode"
	} else {
		base += ", NULL AS approval_mode"
	}
	if s.hasOrigin {
		base += ", origin"
	} else {
		base += ", 'tui' AS origin"
	}
	if s.hasPinned {
		base += ", pinned"
	} else {
		base += ", FALSE AS pinned"
	}
	base += `, agent, cwd`
	if s.hasWorktreeDir {
		base += ", worktree_dir"
	} else {
		base += ", NULL AS worktree_dir"
	}
	if s.hasProjectID {
		base += ", project_id"
	} else {
		base += ", NULL AS project_id"
	}
	base += `, created_at, updated_at, archived, parent_id, search, tools, mcp,
	       user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens`
	if s.hasCacheWriteTokens {
		base += ", cache_write_tokens"
	}
	base += ", output_tokens"
	if s.hasLastTotalTokens {
		base += ", last_total_tokens"
	}
	if s.hasLastMessageCount {
		base += ", last_message_count"
	}
	base += ", " + s.conversationMessageCountSelectSQL("sessions") + " AS message_count"
	base += ", status, tags"
	if s.hasGoal {
		base += ", goal"
	} else {
		base += ", NULL AS goal"
	}
	if s.hasShare {
		base += ", share"
	} else {
		base += ", NULL AS share"
	}
	if s.hasCompactionSeq {
		base += ", compaction_seq"
	}
	if s.hasCompactionCount {
		base += ", compaction_count"
	}
	return base
}

// scanSessionRow scans a session row into a Session struct. The flags
// determine which optional columns are present in the result set.
func scanSessionRow(row *sql.Row, hasGeneratedTitles, hasCacheWriteTokens, hasCompactionSeq, hasCompactionCount, hasTitleSkippedAt, hasLastTotalTokens, hasLastMessageCount bool) (*Session, error) {
	var sess Session
	var number sql.NullInt64
	var name, summary, cwd, worktreeDir, projectID sql.NullString
	var generatedShortTitle, generatedLongTitle, titleSource sql.NullString
	var titleGeneratedAt, titleSkippedAt sql.NullTime
	var mode, approvalMode, origin, agent, parentID, tools, mcp, status, tags, providerKey, reasoningEffort, reasoningMode, goalRaw, shareRaw sql.NullString

	var scanArgs []any
	scanArgs = append(scanArgs, &sess.ID, &number, &name, &summary)
	if hasGeneratedTitles {
		scanArgs = append(scanArgs, &generatedShortTitle, &generatedLongTitle, &titleSource, &titleGeneratedAt, &sess.TitleBasisMsgSeq)
		if hasTitleSkippedAt {
			scanArgs = append(scanArgs, &titleSkippedAt)
		}
	}
	scanArgs = append(scanArgs,
		&sess.Provider, &providerKey, &sess.Model, &reasoningEffort, &reasoningMode, &mode, &approvalMode, &origin, &sess.Pinned,
		&agent, &cwd, &worktreeDir, &projectID, &sess.CreatedAt, &sess.UpdatedAt, &sess.Archived, &parentID,
		&sess.Search, &tools, &mcp,
		&sess.UserTurns, &sess.LLMTurns, &sess.ToolCalls, &sess.InputTokens, &sess.CachedInputTokens,
	)
	if hasCacheWriteTokens {
		scanArgs = append(scanArgs, &sess.CacheWriteTokens)
	}
	scanArgs = append(scanArgs, &sess.OutputTokens)
	if hasLastTotalTokens {
		scanArgs = append(scanArgs, &sess.LastTotalTokens)
	}
	if hasLastMessageCount {
		scanArgs = append(scanArgs, &sess.LastMessageCount)
	}
	scanArgs = append(scanArgs, &sess.MessageCount)
	scanArgs = append(scanArgs, &status, &tags, &goalRaw, &shareRaw)
	if hasCompactionSeq {
		scanArgs = append(scanArgs, &sess.CompactionSeq)
	}
	if hasCompactionCount {
		scanArgs = append(scanArgs, &sess.CompactionCount)
	}

	err := row.Scan(scanArgs...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	// Default compaction_seq when column is absent
	if !hasCompactionSeq {
		sess.CompactionSeq = -1
	}
	if number.Valid {
		sess.Number = number.Int64
	}
	if name.Valid {
		sess.Name = name.String
	}
	if summary.Valid {
		sess.Summary = summary.String
	}
	if hasGeneratedTitles {
		if generatedShortTitle.Valid {
			sess.GeneratedShortTitle = generatedShortTitle.String
		}
		if generatedLongTitle.Valid {
			sess.GeneratedLongTitle = generatedLongTitle.String
		}
		if titleSource.Valid {
			sess.TitleSource = SessionTitleSource(titleSource.String)
		}
		if titleGeneratedAt.Valid {
			sess.TitleGeneratedAt = titleGeneratedAt.Time
		}
		if hasTitleSkippedAt && titleSkippedAt.Valid {
			sess.TitleSkippedAt = titleSkippedAt.Time
		}
	}
	if cwd.Valid {
		sess.CWD = cwd.String
	}
	if worktreeDir.Valid {
		sess.WorktreeDir = worktreeDir.String
	}
	if projectID.Valid {
		sess.ProjectID = projectID.String
	}
	if mode.Valid {
		sess.Mode = SessionMode(mode.String)
	}
	if approvalMode.Valid {
		sess.ApprovalMode = SessionApprovalMode(approvalMode.String)
	}
	if origin.Valid {
		sess.Origin = SessionOrigin(origin.String)
	} else {
		sess.Origin = OriginTUI
	}
	if providerKey.Valid {
		sess.ProviderKey = providerKey.String
	}
	if reasoningEffort.Valid {
		sess.ReasoningEffort = reasoningEffort.String
	}
	if reasoningMode.Valid {
		sess.ReasoningMode = reasoningMode.String
	}
	if agent.Valid {
		sess.Agent = agent.String
	}
	if parentID.Valid {
		sess.ParentID = parentID.String
	}
	if tools.Valid {
		sess.Tools = tools.String
	}
	if mcp.Valid {
		sess.MCP = mcp.String
	}
	if status.Valid {
		sess.Status = SessionStatus(status.String)
	}
	if tags.Valid {
		sess.Tags = tags.String
	}
	if goal := parseGoalJSONString(goalRaw); goal != nil {
		sess.Goal = goal
	}
	if share := parseShareJSONString(shareRaw); share != nil {
		sess.Share = share
	}
	return &sess, nil
}

func goalJSONString(goal *Goal) sql.NullString {
	if goal == nil || !goal.Exists() {
		return sql.NullString{}
	}
	clone := goal.Clone()
	clone.Normalize(time.Now())
	data, err := json.Marshal(clone)
	if err != nil || len(data) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(data), Valid: true}
}

func parseGoalJSONString(raw sql.NullString) *Goal {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var goal Goal
	if err := json.Unmarshal([]byte(raw.String), &goal); err != nil {
		return nil
	}
	goal.Normalize(time.Now())
	if !goal.Exists() {
		return nil
	}
	return &goal
}

func shareJSONString(share *ShareState) sql.NullString {
	if share == nil || !share.Exists() {
		return sql.NullString{}
	}
	data, err := json.Marshal(share.Clone())
	if err != nil || len(data) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(data), Valid: true}
}

func parseShareJSONString(raw sql.NullString) *ShareState {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var share ShareState
	if err := json.Unmarshal([]byte(raw.String), &share); err != nil || !share.Exists() {
		return nil
	}
	share.Normalize()
	return &share
}

// nullString converts an empty string to NULL for database storage.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// isBusyError checks if an error is a SQLite BUSY error
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "SQLITE_BUSY") ||
		strings.Contains(errStr, "database is locked")
}

// retryOnBusy retries an operation with exponential backoff on SQLITE_BUSY errors.
// This provides additional resilience beyond the busy_timeout pragma for high-contention scenarios.
func retryOnBusy(ctx context.Context, maxRetries int, op func() error) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = op()
		if err == nil || !isBusyError(err) {
			return err
		}
		// Exponential backoff: 10ms, 20ms, 40ms, 80ms, 160ms
		d := time.Duration(10*(1<<i)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return err
}
