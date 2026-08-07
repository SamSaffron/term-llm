package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// GetBranchByIdempotencyKey returns an already-materialized child without
// repeating any helper work associated with the original branch request.
func (s *SQLiteStore) GetBranchByIdempotencyKey(ctx context.Context, sourceSessionID, idempotencyKey string) (BranchResult, bool, error) {
	if s == nil || !s.hasSessionBranches {
		return BranchResult{}, false, ErrBranchingUnsupported
	}
	sourceSessionID = strings.TrimSpace(sourceSessionID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if sourceSessionID == "" || idempotencyKey == "" {
		return BranchResult{}, false, nil
	}
	var childID string
	var forkAfterMessageID sql.NullInt64
	var anchorSequence int
	err := s.db.QueryRowContext(ctx, `
		SELECT child_session_id, fork_after_message_id, fork_after_sequence FROM session_branches
		WHERE parent_session_id = ? AND idempotency_key = ?`, sourceSessionID, idempotencyKey).Scan(&childID, &forkAfterMessageID, &anchorSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return BranchResult{}, false, nil
	}
	if err != nil {
		return BranchResult{}, false, fmt.Errorf("lookup idempotent branch: %w", err)
	}
	child, err := s.Get(ctx, childID)
	if err != nil {
		return BranchResult{}, false, err
	}
	result := BranchResult{Session: child, ForkAfterMessageID: forkAfterMessageID.Int64, Reused: true}
	if anchorSequence >= 0 {
		err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND sequence <= ? AND role IN ('user', 'assistant') ORDER BY sequence DESC, id DESC LIMIT 1`, childID, anchorSequence).Scan(&result.AnchorMessageID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return BranchResult{}, false, fmt.Errorf("load idempotent branch anchor: %w", err)
		}
	}
	return result, true, nil
}

// CreateBranch materializes a normal linear child session containing the source
// transcript prefix through opts.AnchorMessageID. The dedicated edge table is
// metadata only; provider state, redo state, plans, goals, and usage are not copied.
func (s *SQLiteStore) CreateBranch(ctx context.Context, sourceSessionID string, opts CreateBranchOptions) (BranchResult, error) {
	if s == nil || !s.hasSessionBranches || !s.hasTranscriptRev || !s.hasMessageStreamIdentity || !s.hasMessageClientID {
		return BranchResult{}, ErrBranchingUnsupported
	}
	sourceSessionID = strings.TrimSpace(sourceSessionID)
	if sourceSessionID == "" || opts.AnchorMessageID < 0 {
		return BranchResult{}, ErrNotFound
	}
	opts.IdempotencyKey = strings.TrimSpace(opts.IdempotencyKey)

	var childID string
	var forkAfterMessageID int64
	var copiedAnchorID int64
	var reused bool
	err := retryOnBusy(ctx, 5, func() error {
		conn, err := s.beginImmediate(ctx)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				rollbackImmediate(conn)
				return
			}
			_ = conn.Close()
		}()

		if opts.IdempotencyKey != "" {
			var replayForkAfterMessageID sql.NullInt64
			var replayAnchorSequence int
			err := conn.QueryRowContext(ctx, `
				SELECT child_session_id, fork_after_message_id, fork_after_sequence FROM session_branches
				WHERE parent_session_id = ? AND idempotency_key = ?`, sourceSessionID, opts.IdempotencyKey).
				Scan(&childID, &replayForkAfterMessageID, &replayAnchorSequence)
			if err == nil {
				forkAfterMessageID = replayForkAfterMessageID.Int64
				if forkAfterMessageID != opts.AnchorMessageID {
					return ErrBranchIdempotencyConflict
				}
				if replayAnchorSequence >= 0 {
					err := conn.QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND sequence <= ? AND role IN ('user', 'assistant') ORDER BY sequence DESC, id DESC LIMIT 1`, childID, replayAnchorSequence).Scan(&copiedAnchorID)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return fmt.Errorf("resolve idempotent branch anchor: %w", err)
					}
				}
				if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
					return fmt.Errorf("commit branch replay: %w", err)
				}
				committed = true
				reused = true
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("lookup idempotent branch: %w", err)
			}
		}

		actual, err := transcriptMutationState(ctx, conn, sourceSessionID)
		if err != nil {
			return err
		}
		if opts.ExpectedState != nil && actual != *opts.ExpectedState {
			return ErrBranchConflict
		}
		if opts.ExpectedRev != nil && actual.Rev != *opts.ExpectedRev {
			return ErrBranchConflict
		}

		anchorSequence := -1
		var nullableAnchor any
		if opts.AnchorMessageID > 0 {
			var anchorCompactionTail bool
			var anchorRole, anchorText string
			if err := conn.QueryRowContext(ctx, `
				SELECT sequence, role, text_content, COALESCE(compaction_tail, FALSE)
				FROM messages WHERE id = ? AND session_id = ?`, opts.AnchorMessageID, sourceSessionID).
				Scan(&anchorSequence, &anchorRole, &anchorText, &anchorCompactionTail); errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			} else if err != nil {
				return fmt.Errorf("resolve branch anchor: %w", err)
			}
			if anchorCompactionTail || (anchorRole == string(llm.RoleUser) && llm.IsInternalCompactionSummaryText(anchorText)) {
				return ErrNotFound
			}
			nullableAnchor = opts.AnchorMessageID
		}

		var parentCompactionSeq, parentCompactionCount int
		if err := conn.QueryRowContext(ctx, `SELECT compaction_seq, compaction_count FROM sessions WHERE id = ?`, sourceSessionID).Scan(&parentCompactionSeq, &parentCompactionCount); err != nil {
			return fmt.Errorf("load branch source compaction: %w", err)
		}
		childCompactionSeq, childCompactionCount := -1, 0
		preserveCompactionTail := false
		if parentCompactionSeq >= 0 && anchorSequence >= parentCompactionSeq {
			childCompactionSeq = parentCompactionSeq
			childCompactionCount = parentCompactionCount
			preserveCompactionTail = true
		}

		forkAfterMessageID = opts.AnchorMessageID
		childID = NewID()
		now := time.Now()
		insertSession := `
			INSERT INTO sessions (
				id, number, name, summary, generated_short_title, generated_long_title,
				title_source, title_generated_at, title_basis_msg_seq, title_skipped_at,
				provider, provider_key, model, reasoning_effort, reasoning_mode, mode,
				approval_mode, origin, agent, cwd, worktree_dir, created_at, updated_at,
				archived, pinned, parent_id, search, tools, mcp, user_turns, llm_turns,
				tool_calls, input_tokens, cached_input_tokens, cache_write_tokens,
				output_tokens, last_total_tokens, last_message_count, status, tags,
				goal, share, compaction_seq, compaction_count, transcript_rev
			)
			SELECT ?, (SELECT COALESCE(MAX(number), 0) + 1 FROM sessions), '', '', '', '',
			       '', NULL, 0, NULL,
			       provider, provider_key, model, reasoning_effort, reasoning_mode, mode,
			       approval_mode, origin, agent, cwd, worktree_dir, ?, ?,
			       FALSE, FALSE, NULL, search, tools, mcp, 0, 0,
			       0, 0, 0, 0, 0,
			       0, 0, 'active', NULL,
			       NULL, NULL, ?, ?, 0
			FROM sessions WHERE id = ?`
		inserted, err := conn.ExecContext(ctx, insertSession, childID, now, now, childCompactionSeq, childCompactionCount, sourceSessionID)
		if err != nil {
			return fmt.Errorf("insert branch session: %w", err)
		}
		if rows, _ := inserted.RowsAffected(); rows != 1 {
			return ErrNotFound
		}

		if anchorSequence >= 0 {
			compactionTailExpr := "FALSE"
			prefixFilter := ""
			if preserveCompactionTail {
				compactionTailExpr = "compaction_tail"
			} else {
				// A pre-boundary branch becomes uncompacted. Copy only the visible
				// transcript prefix: historical compaction summaries and retained
				// duplicate tails are persistence artifacts, not additional turns.
				prefixFilter = fmt.Sprintf(`
					AND COALESCE(compaction_tail, FALSE) = FALSE
					AND NOT (role = 'user' AND substr(%s, 1, %d) = '%s')`,
					trimmedMessageTextSQL(""), len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix)
			}
			copyMessages := `
				INSERT INTO messages (
					session_id, role, parts, text_content, duration_ms, turn_index,
					created_at, sequence, compaction_tail, client_message_id, response_id,
					assistant_segment_ordinal, segment_start_sequence, segment_end_sequence
				)
				SELECT ?, role, parts, text_content, duration_ms, turn_index,
				       created_at, sequence, ` + compactionTailExpr + `, '', '', -1, 0, 0
				FROM messages
				WHERE session_id = ? AND sequence <= ?` + prefixFilter + `
				ORDER BY sequence, id`
			if _, err := conn.ExecContext(ctx, copyMessages, childID, sourceSessionID, anchorSequence); err != nil {
				return fmt.Errorf("copy branch messages: %w", err)
			}
		}
		pathNoteInserted := false
		if opts.PathNote != nil && strings.TrimSpace(opts.PathNote.Text) != "" {
			provenance := opts.PathNote.Provenance
			provenance.SourceSessionID = sourceSessionID
			provenance.AnchorMessageID = opts.AnchorMessageID
			note := NewPathNoteMessage(childID, opts.PathNote.Text, provenance, -1)
			partsJSON, err := json.Marshal(note.Parts)
			if err != nil {
				return fmt.Errorf("encode branch path note: %w", err)
			}
			var noteSequence int
			if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), -1) + 1 FROM messages WHERE session_id = ?`, childID).Scan(&noteSequence); err != nil {
				return fmt.Errorf("allocate branch path note sequence: %w", err)
			}
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO messages (
					session_id, role, parts, text_content, duration_ms, turn_index,
					created_at, sequence, compaction_tail, client_message_id, response_id,
					assistant_segment_ordinal, segment_start_sequence, segment_end_sequence
				) VALUES (?, ?, ?, ?, 0, 0, ?, ?, FALSE, '', '', -1, 0, 0)`,
				childID, string(note.Role), string(partsJSON), note.TextContent, note.CreatedAt, noteSequence); err != nil {
				return fmt.Errorf("insert branch path note: %w", err)
			}
			pathNoteInserted = true
		}
		if anchorSequence >= 0 || pathNoteInserted {
			if _, err := conn.ExecContext(ctx, `UPDATE sessions SET transcript_rev = 1 WHERE id = ?`, childID); err != nil {
				return fmt.Errorf("initialize branch transcript revision: %w", err)
			}
		}
		if err := s.recomputeTranscriptMetadata(ctx, conn, childID, true); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO session_branches (
				child_session_id, parent_session_id, fork_after_message_id,
				fork_after_sequence, idempotency_key, created_at
			) VALUES (?, ?, ?, ?, ?, ?)`, childID, sourceSessionID, nullableAnchor, anchorSequence, opts.IdempotencyKey, now); err != nil {
			return fmt.Errorf("insert branch edge: %w", err)
		}
		copiedAnchorID = 0
		if anchorSequence >= 0 {
			err := conn.QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND sequence <= ? AND role IN ('user', 'assistant') ORDER BY sequence DESC, id DESC LIMIT 1`, childID, anchorSequence).Scan(&copiedAnchorID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("resolve copied branch anchor: %w", err)
			}
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit branch: %w", err)
		}
		committed = true
		return nil
	})
	if err != nil {
		return BranchResult{}, err
	}
	// Keep reads after COMMIT outside retryOnBusy. Retrying the whole transaction
	// after a transient post-commit read failure could materialize another child
	// when the caller did not supply an idempotency key.
	child, err := s.Get(ctx, childID)
	if err != nil {
		return BranchResult{}, err
	}
	return BranchResult{Session: child, ForkAfterMessageID: forkAfterMessageID, AnchorMessageID: copiedAnchorID, Reused: reused}, nil
}

// MessagesAfterBranchAnchor returns the visible conversation suffix omitted by
// CreateBranch for the given source anchor. Anchor zero selects the full transcript.
func MessagesAfterBranchAnchor(messages []Message, anchorMessageID int64) ([]llm.Message, error) {
	anchorSequence := -1
	if anchorMessageID > 0 {
		found := false
		for _, message := range messages {
			if message.ID == anchorMessageID {
				anchorSequence = message.Sequence
				found = true
				break
			}
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	out := make([]llm.Message, 0, len(messages))
	for i := range messages {
		message := &messages[i]
		if message.Sequence <= anchorSequence || message.CompactionTail || isInternalCompactionSummaryMessage(*message) {
			continue
		}
		switch message.Role {
		case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
			out = append(out, message.ToLLMMessage())
		case llm.RoleDeveloper:
			provenance, ok := message.PathNoteProvenance()
			if !ok {
				continue
			}
			// Preserve only the trusted persistence marker long enough for the
			// isolated path-note helper to distinguish inherited notes from arbitrary
			// developer instructions. Send display text rather than the provider-facing
			// wrapper so nested branches do not repeatedly summarize the safety preamble.
			provenanceCopy := *provenance
			provenanceCopy.ReadFiles = append([]string(nil), provenance.ReadFiles...)
			provenanceCopy.ModifiedFiles = append([]string(nil), provenance.ModifiedFiles...)
			out = append(out, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{
				{Type: llm.PartPathNote, PathNote: &provenanceCopy},
				{Type: llm.PartText, Text: message.PathNoteDisplayText()},
			}})
		}
	}
	return out, nil
}

// GetBranchTree returns all surviving sessions connected to sessionID. Parent
// IDs intentionally are not foreign keys, so a deleted parent simply makes its
// surviving child the root of a smaller tree.
func (s *SQLiteStore) GetBranchTree(ctx context.Context, sessionID string) (BranchTree, error) {
	if s == nil || !s.hasSessionBranches {
		return BranchTree{}, ErrBranchingUnsupported
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return BranchTree{}, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE component(id) AS (
			VALUES (?)
			UNION
			SELECT b.parent_session_id
			FROM session_branches b JOIN component c ON b.child_session_id = c.id
			JOIN sessions parent ON parent.id = b.parent_session_id
			UNION
			SELECT b.child_session_id
			FROM session_branches b JOIN component c ON b.parent_session_id = c.id
			JOIN sessions child ON child.id = b.child_session_id
		)
		SELECT sess.id, COALESCE(sess.number, 0), COALESCE(edge.parent_session_id, ''),
		       COALESCE(edge.fork_after_message_id, 0), COALESCE(edge.fork_after_sequence, -1),
		       COALESCE((SELECT copied.id FROM messages copied
		                 WHERE copied.session_id = sess.id AND copied.sequence <= edge.fork_after_sequence
		                   AND copied.role IN ('user', 'assistant')
		                 ORDER BY copied.sequence DESC, copied.id DESC LIMIT 1), 0),
		       COALESCE(NULLIF(sess.name, ''), NULLIF(sess.generated_short_title, ''), NULLIF(sess.summary, ''), 'Untitled'),
		       COALESCE(anchor.role, ''), COALESCE(anchor.text_content, ''), sess.created_at
		FROM component c
		JOIN sessions sess ON sess.id = c.id
		LEFT JOIN session_branches edge ON edge.child_session_id = sess.id
		LEFT JOIN messages anchor ON anchor.id = edge.fork_after_message_id
		ORDER BY sess.created_at, sess.number`, sessionID)
	if err != nil {
		return BranchTree{}, fmt.Errorf("query branch tree: %w", err)
	}
	defer rows.Close()

	byID := make(map[string]BranchTreeNode)
	for rows.Next() {
		var node BranchTreeNode
		if err := rows.Scan(&node.SessionID, &node.SessionNumber, &node.ParentSessionID,
			&node.ForkAfterMessageID, &node.ForkAfterSequence, &node.CopiedAnchorMessageID, &node.Title,
			&node.AnchorRole, &node.AnchorPreview, &node.CreatedAt); err != nil {
			return BranchTree{}, fmt.Errorf("scan branch tree: %w", err)
		}
		node.AnchorPreview = TruncateSummary(node.AnchorPreview)
		byID[node.SessionID] = node
	}
	if err := rows.Err(); err != nil {
		return BranchTree{}, err
	}
	if _, ok := byID[sessionID]; !ok {
		return BranchTree{}, ErrNotFound
	}

	children := make(map[string][]BranchTreeNode)
	roots := make([]BranchTreeNode, 0, 1)
	for _, node := range byID {
		if _, parentSurvives := byID[node.ParentSessionID]; node.ParentSessionID == "" || !parentSurvives {
			roots = append(roots, node)
		} else {
			children[node.ParentSessionID] = append(children[node.ParentSessionID], node)
		}
	}
	less := func(a, b BranchTreeNode) bool {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.SessionNumber < b.SessionNumber
		}
		return a.CreatedAt.Before(b.CreatedAt)
	}
	sort.Slice(roots, func(i, j int) bool { return less(roots[i], roots[j]) })
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool { return less(children[parent][i], children[parent][j]) })
	}

	tree := BranchTree{ActiveSessionID: sessionID}
	visited := make(map[string]bool, len(byID))
	var appendTree func(BranchTreeNode)
	appendTree = func(node BranchTreeNode) {
		if visited[node.SessionID] {
			return
		}
		visited[node.SessionID] = true
		tree.Nodes = append(tree.Nodes, node)
		for _, child := range children[node.SessionID] {
			appendTree(child)
		}
	}
	for _, root := range roots {
		if tree.RootSessionID == "" {
			tree.RootSessionID = root.SessionID
		}
		appendTree(root)
	}
	tree.PathCount = len(tree.Nodes)
	return tree, nil
}
