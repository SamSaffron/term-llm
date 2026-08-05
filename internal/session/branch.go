package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

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

	var result BranchResult
	err := retryOnBusy(ctx, 5, func() error {
		conn, err := s.beginImmediate(ctx)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				rollbackImmediate(conn)
			}
		}()

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

		if opts.IdempotencyKey != "" {
			var childID string
			var replayAnchorSequence int
			err := conn.QueryRowContext(ctx, `
				SELECT child_session_id, fork_after_sequence FROM session_branches
				WHERE parent_session_id = ? AND idempotency_key = ?`, sourceSessionID, opts.IdempotencyKey).Scan(&childID, &replayAnchorSequence)
			if err == nil {
				if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
					return fmt.Errorf("commit branch replay: %w", err)
				}
				committed = true
				_ = conn.Close()
				child, err := s.Get(ctx, childID)
				if err != nil {
					return err
				}
				result = BranchResult{Session: child, Reused: true}
				if replayAnchorSequence >= 0 {
					_ = s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND sequence = ? ORDER BY id DESC LIMIT 1`, childID, replayAnchorSequence).Scan(&result.AnchorMessageID)
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("lookup idempotent branch: %w", err)
			}
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

		childID := NewID()
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
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit branch: %w", err)
		}
		committed = true
		_ = conn.Close()

		child, err := s.Get(ctx, childID)
		if err != nil {
			return err
		}
		result = BranchResult{Session: child}
		if anchorSequence >= 0 {
			if err := s.db.QueryRowContext(ctx, `SELECT id FROM messages WHERE session_id = ? AND sequence = ? ORDER BY id DESC LIMIT 1`, childID, anchorSequence).Scan(&result.AnchorMessageID); err != nil {
				return fmt.Errorf("load copied branch anchor: %w", err)
			}
		}
		return nil
	})
	return result, err
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
			&node.ForkAfterMessageID, &node.ForkAfterSequence, &node.Title,
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
