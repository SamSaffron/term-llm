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
)

var (
	ErrOpenSideExists = errors.New("session: an open side conversation already exists")
	ErrSideClosed     = errors.New("session: side conversation is closed")
	ErrNestedSide     = errors.New("session: nested side conversations are not supported")
)

// SideStore is the optional persistence capability used by surfaces that support
// side conversations. Keeping it separate leaves lightweight and custom Store
// implementations unchanged.
type SideStore interface {
	ForkSide(ctx context.Context, parentID string, origin SessionOrigin) (*Session, error)
	GetOpenSide(ctx context.Context, rootID string) (*Session, error)
	ListSides(ctx context.Context, rootID string) ([]SessionSummary, error)
	GetSideContext(ctx context.Context, sideID string) ([]llm.Message, error)
	ConsumeSideContext(ctx context.Context, sideID string) ([]llm.Message, error)
	CloseSide(ctx context.Context, sideID string) error
	ReopenSide(ctx context.Context, sideID string) (*Session, error)
}

// PrepareForkContext returns a provider-safe point-in-time copy of model
// context. It strips cache anchors and incomplete tool protocol fragments while
// retaining structured content and opaque provider replay for completed turns.
func PrepareForkContext(messages []llm.Message) []llm.Message {
	data, err := json.Marshal(messages)
	if err != nil {
		return nil
	}
	var copied []llm.Message
	if err := json.Unmarshal(data, &copied); err != nil {
		return nil
	}

	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, msg := range copied {
		for _, part := range msg.Parts {
			if part.ToolCall != nil && strings.TrimSpace(part.ToolCall.ID) != "" {
				calls[part.ToolCall.ID] = struct{}{}
			}
			if part.ToolResult != nil && strings.TrimSpace(part.ToolResult.ID) != "" {
				results[part.ToolResult.ID] = struct{}{}
			}
		}
	}
	complete := make(map[string]struct{})
	for id := range calls {
		if _, ok := results[id]; ok {
			complete[id] = struct{}{}
		}
	}

	out := make([]llm.Message, 0, len(copied))
	for _, msg := range copied {
		msg.CacheAnchor = false
		partial := false
		parts := make([]llm.Part, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				if _, ok := complete[part.ToolCall.ID]; !ok {
					partial = true
					continue
				}
			}
			if part.ToolResult != nil {
				if _, ok := complete[part.ToolResult.ID]; !ok {
					partial = true
					continue
				}
			}
			parts = append(parts, part)
		}
		if partial {
			clean := parts[:0]
			for _, part := range parts {
				if part.Type != llm.PartProviderReplay {
					clean = append(clean, part)
				}
			}
			parts = clean
		}
		msg.Parts = parts
		if len(parts) > 0 {
			out = append(out, msg)
		}
	}
	return out
}

func (s *SQLiteStore) ForkSide(ctx context.Context, parentID string, origin SessionOrigin) (*Session, error) {
	if !s.hasSideMetadata {
		return nil, fmt.Errorf("side conversations require a current session database")
	}
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		return nil, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin side fork: %w", err)
	}
	defer tx.Rollback()

	var parent Session
	var kind, rootID string
	var parentOrigin string
	err = tx.QueryRowContext(ctx, `SELECT id, provider, COALESCE(provider_key,''), model, mode, COALESCE(approval_mode,''),
		COALESCE(origin,'tui'), COALESCE(agent,''), COALESCE(cwd,''), COALESCE(worktree_dir,''), search,
		COALESCE(tools,''), COALESCE(mcp,''), COALESCE(kind,'root'), COALESCE(root_id,id)
		FROM sessions WHERE id = ?`, parentID).Scan(&parent.ID, &parent.Provider, &parent.ProviderKey, &parent.Model,
		&parent.Mode, &parent.ApprovalMode, &parentOrigin, &parent.Agent, &parent.CWD, &parent.WorktreeDir,
		&parent.Search, &parent.Tools, &parent.MCP, &kind, &rootID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load fork parent: %w", err)
	}
	if SessionKind(kind) == KindSide {
		return nil, ErrNestedSide
	}
	if origin == "" {
		origin = SessionOrigin(parentOrigin)
	}

	fromSeq := 0
	var compactionSeq, compactionCount int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(compaction_seq,-1), COALESCE(compaction_count,0) FROM sessions WHERE id = ?", parentID).Scan(&compactionSeq, &compactionCount); err != nil {
		return nil, fmt.Errorf("load fork boundary: %w", err)
	}
	if compactionSeq >= 0 && (compactionCount > 0 || compactionSeq > 0) {
		fromSeq = compactionSeq
	}
	rows, err := tx.QueryContext(ctx, `SELECT role, parts FROM messages WHERE session_id = ? AND sequence >= ? ORDER BY sequence`, parentID, fromSeq)
	if err != nil {
		return nil, fmt.Errorf("load fork context: %w", err)
	}
	var inherited []llm.Message
	for rows.Next() {
		var role string
		var raw []byte
		if err := rows.Scan(&role, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan fork context: %w", err)
		}
		var parts []llm.Part
		if err := json.Unmarshal(raw, &parts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode fork context: %w", err)
		}
		inherited = append(inherited, llm.Message{Role: llm.Role(role), Parts: parts})
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close fork context rows: %w", err)
	}
	inherited = PrepareForkContext(inherited)

	now := time.Now()
	child := &Session{
		ID:            NewID(),
		Provider:      parent.Provider,
		ProviderKey:   parent.ProviderKey,
		Model:         parent.Model,
		Mode:          parent.Mode,
		ApprovalMode:  ApprovalModePrompt,
		Origin:        origin,
		Agent:         parent.Agent,
		CWD:           parent.CWD,
		WorktreeDir:   parent.WorktreeDir,
		ParentID:      parent.ID,
		RootID:        rootID,
		Kind:          KindSide,
		SideState:     SideOpen,
		Search:        parent.Search,
		Tools:         parent.Tools,
		MCP:           parent.MCP,
		Status:        StatusActive,
		CompactionSeq: -1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions
		(id, number, provider, provider_key, model, mode, approval_mode, origin, agent, cwd, worktree_dir,
		 created_at, updated_at, parent_id, root_id, kind, side_state, search, tools, mcp, status, compaction_seq, compaction_count)
		VALUES (?, (SELECT COALESCE(MAX(number),0)+1 FROM sessions), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'side', 'open', ?, ?, ?, 'active', -1, 0)`,
		child.ID, child.Provider, nullString(child.ProviderKey), child.Model, child.Mode, child.ApprovalMode, child.Origin,
		nullString(child.Agent), child.CWD, nullString(child.WorktreeDir), now, now, child.ParentID, child.RootID,
		child.Search, nullString(child.Tools), nullString(child.MCP))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrOpenSideExists
		}
		return nil, fmt.Errorf("insert side session: %w", err)
	}
	for i, msg := range inherited {
		raw, err := json.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("encode side context: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO side_context_messages(side_session_id, sequence, message) VALUES (?, ?, ?)`, child.ID, i, raw); err != nil {
			return nil, fmt.Errorf("insert side context: %w", err)
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT number FROM sessions WHERE id = ?", child.ID).Scan(&child.Number); err != nil {
		return nil, fmt.Errorf("load side number: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit side fork: %w", err)
	}
	return child, nil
}

func (s *SQLiteStore) GetSideContext(ctx context.Context, sideID string) ([]llm.Message, error) {
	return sideContextQuery(ctx, s.db, sideID)
}

func (s *SQLiteStore) ConsumeSideContext(ctx context.Context, sideID string) ([]llm.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	messages, err := sideContextQuery(ctx, tx, sideID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM side_context_messages WHERE side_session_id = ?", sideID); err != nil {
		return nil, fmt.Errorf("consume side context: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return messages, nil
}

type sideContextQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func sideContextQuery(ctx context.Context, q sideContextQuerier, sideID string) ([]llm.Message, error) {
	rows, err := q.QueryContext(ctx, "SELECT message FROM side_context_messages WHERE side_session_id = ? ORDER BY sequence", sideID)
	if err != nil {
		return nil, fmt.Errorf("get side context: %w", err)
	}
	defer rows.Close()
	var messages []llm.Message
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var msg llm.Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("decode side context: %w", err)
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (s *SQLiteStore) GetOpenSide(ctx context.Context, rootID string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+s.sessionSelectCols()+" FROM sessions WHERE root_id = ? AND kind = 'side' AND side_state = 'open' LIMIT 1", rootID)
	return scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
}

func (s *SQLiteStore) CloseSide(ctx context.Context, sideID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET side_state = 'closed', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND kind = 'side' AND side_state = 'open'`, sideID)
	if err != nil {
		return fmt.Errorf("close side: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrSideClosed
	}
	return nil
}

func (s *SQLiteStore) ReopenSide(ctx context.Context, sideID string) (*Session, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET side_state = 'open', updated_at = CURRENT_TIMESTAMP WHERE id = ? AND kind = 'side' AND side_state = 'closed'`, sideID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrOpenSideExists
		}
		return nil, fmt.Errorf("reopen side: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, sideID)
}

func (s *SQLiteStore) ListSides(ctx context.Context, rootID string) ([]SessionSummary, error) {
	return s.List(ctx, ListOptions{RootID: rootID, IncludeSides: true, OnlySides: true, Archived: true, SortByActivity: true})
}
