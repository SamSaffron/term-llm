package memory

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/sqlitefts"
)

func (s *Store) CreateInsight(ctx context.Context, ins *Insight) error {
	if ins == nil {
		return fmt.Errorf("insight is nil")
	}
	if strings.TrimSpace(ins.Agent) == "" {
		return fmt.Errorf("agent is required")
	}
	if strings.TrimSpace(ins.Content) == "" {
		return fmt.Errorf("content is required")
	}
	now := time.Now()
	if ins.CreatedAt.IsZero() {
		ins.CreatedAt = now
	}
	if ins.LastReinforced.IsZero() {
		ins.LastReinforced = ins.CreatedAt
	}
	if ins.Confidence == 0 {
		ins.Confidence = 0.5
	}
	if ins.ReinforcementCount == 0 {
		ins.ReinforcementCount = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create insight tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		INSERT INTO memory_insights
		    (agent, content, compact_content, category, trigger_desc, confidence, reinforcement_count, created_at, last_reinforced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ins.Agent, ins.Content, ins.CompactContent, ins.Category, ins.TriggerDesc,
		ins.Confidence, ins.ReinforcementCount,
		ins.CreatedAt.UTC().Format(time.RFC3339),
		ins.LastReinforced.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("create insight: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("get insight id: %w", err)
	}
	ins.ID = id

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO memory_insights_fts(rowid, agent, content, category, trigger_desc)
		 VALUES (?, ?, ?, ?, ?)`,
		id, ins.Agent, ins.Content, ins.Category, ins.TriggerDesc); err != nil {
		return fmt.Errorf("sync insight fts: %w", err)
	}
	return tx.Commit()
}

// ListInsights returns insights for an agent, newest first.
func (s *Store) ListInsights(ctx context.Context, agent string, limit int) ([]*Insight, error) {
	q := `SELECT id, agent, content, compact_content, category, trigger_desc, confidence, reinforcement_count,
	             created_at, last_reinforced
	      FROM memory_insights`
	args := []any{}
	if strings.TrimSpace(agent) != "" {
		q += ` WHERE agent = ?`
		args = append(args, agent)
	}
	q += ` ORDER BY confidence DESC, last_reinforced DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list insights: %w", err)
	}
	defer rows.Close()

	var out []*Insight
	for rows.Next() {
		ins, err := scanInsight(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ins)
	}
	return out, rows.Err()
}

// GetInsightByID returns a single insight by its row ID.
func (s *Store) GetInsightByID(ctx context.Context, id int64) (*Insight, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, agent, content, compact_content, category, trigger_desc, confidence, reinforcement_count,
		       created_at, last_reinforced
		FROM memory_insights WHERE id = ?`, id)
	ins, err := scanInsight(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ins, err
}

// UpdateInsight replaces the content (and optionally compact_content) of an insight and resets FTS.
// Pass compact="" to leave the compact form unchanged.
func (s *Store) UpdateInsight(ctx context.Context, id int64, content, compact string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("content is required")
	}
	compact = strings.TrimSpace(compact)

	// Fetch old row before UPDATE so the FTS delete command uses the exact
	// original content (FTS5 delete requires the stored values to match).
	old, err := s.GetInsightByID(ctx, id)
	if err != nil {
		return err
	}
	if old == nil {
		return fmt.Errorf("insight not found: %d", id)
	}

	// Preserve existing compact if caller passes "".
	if compact == "" {
		compact = old.CompactContent
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("update insight tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err = tx.ExecContext(ctx,
		`UPDATE memory_insights SET content = ?, compact_content = ?, last_reinforced = ? WHERE id = ?`,
		content, compact, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("update insight: %w", err)
	}

	// Remove old FTS entry then insert updated one.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO memory_insights_fts(memory_insights_fts, rowid, agent, content, category, trigger_desc)
		 VALUES ('delete', ?, ?, ?, ?, ?)`,
		id, old.Agent, old.Content, old.Category, old.TriggerDesc); err != nil {
		return fmt.Errorf("fts delete insight: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO memory_insights_fts(rowid, agent, content, category, trigger_desc)
		 VALUES (?, ?, ?, ?, ?)`,
		id, old.Agent, content, old.Category, old.TriggerDesc); err != nil {
		return fmt.Errorf("fts insert insight: %w", err)
	}
	return tx.Commit()
}

// DeleteInsight removes an insight and its FTS entry.
func (s *Store) DeleteInsight(ctx context.Context, id int64) (bool, error) {
	ins, err := s.GetInsightByID(ctx, id)
	if err != nil {
		return false, err
	}
	if ins == nil {
		return false, nil
	}

	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO memory_insights_fts(memory_insights_fts, rowid, agent, content, category, trigger_desc)
		 VALUES ('delete', ?, ?, ?, ?, ?)`,
		id, ins.Agent, ins.Content, ins.Category, ins.TriggerDesc)

	res, err := s.db.ExecContext(ctx, `DELETE FROM memory_insights WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete insight: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReinforceInsight bumps the reinforcement count and nudges confidence upward
// using a logarithmic schedule (diminishing returns after the first few observations).
func (s *Store) ReinforceInsight(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE memory_insights
		SET reinforcement_count = reinforcement_count + 1,
		    confidence = MIN(1.0, confidence + (1.0 - confidence) * 0.2),
		    last_reinforced = ?
		WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("reinforce insight: %w", err)
	}
	return nil
}

// SearchInsights finds insights via BM25 full-text search. Used for deduplication
// during extraction (checking if a newly mined insight already exists), not for
// injection — ExpandInsights returns all insights sorted by confidence instead.
func (s *Store) SearchInsights(ctx context.Context, agent, query string, limit int) ([]*Insight, error) {
	if strings.TrimSpace(query) == "" {
		return s.ListInsights(ctx, agent, limit)
	}
	if limit <= 0 {
		limit = 10
	}

	ftsQuery := sqlitefts.PrefixORQuery(query, 3)
	if ftsQuery == "" {
		return s.ListInsights(ctx, agent, limit)
	}
	args := []any{ftsQuery}
	agentClause := ""
	if strings.TrimSpace(agent) != "" {
		agentClause = `AND i.agent = ?`
		args = append(args, agent)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT i.id, i.agent, i.content, i.compact_content, i.category, i.trigger_desc,
		       i.confidence, i.reinforcement_count, i.created_at, i.last_reinforced
		FROM memory_insights_fts f
		JOIN memory_insights i ON i.id = f.rowid
		WHERE memory_insights_fts MATCH ? %s
		ORDER BY rank * (1.0 / MAX(i.confidence, 0.1))
		LIMIT ?`, agentClause), args...)
	if err != nil {
		return nil, fmt.Errorf("search insights: %w", err)
	}
	defer rows.Close()

	var out []*Insight
	for rows.Next() {
		ins, err := scanInsight(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ins)
	}
	return out, rows.Err()
}

// ExpandInsights returns all insights for the agent sorted by confidence,
// formatted as a compact block ready for injection into conversation context.
// maxTokens is a rough token budget (approximated at 4 chars/token).
// Returns an empty string when the bank is empty.
//
// No search/filtering is applied: insight banks are small and curated, so
// returning all of them (within the token cap) is more correct than trying
// to match them against the user's first message via BM25 or embeddings.
func (s *Store) ExpandInsights(ctx context.Context, agent, _ string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = 500
	}
	maxChars := maxTokens * 4

	insights, err := s.ListInsights(ctx, agent, 200)
	if err != nil {
		return "", err
	}
	if len(insights) == 0 {
		return "", nil
	}

	const header = "<insights>\nBehavioral guidelines from past sessions:\n"
	const footer = "</insights>"

	var sb strings.Builder
	sb.WriteString(header)
	used := sb.Len()
	n := 0

	for _, ins := range insights {
		// Only include insights above a minimum confidence threshold.
		if ins.Confidence < 0.4 {
			continue
		}
		if !injectableInsightCategory(ins.Category) {
			continue
		}
		n++
		text := strings.TrimSpace(ins.CompactContent)
		if text == "" {
			text = strings.TrimSpace(ins.Content)
		}
		line := fmt.Sprintf("%d. %s\n", n, text)
		if used+len(line) > maxChars {
			break
		}
		sb.WriteString(line)
		used += len(line)
	}
	sb.WriteString(footer)

	if n == 0 {
		return "", nil
	}
	return sb.String(), nil
}

func injectableInsightCategory(category string) bool {
	switch strings.TrimSpace(category) {
	case "mining", "user-profile", "infrastructure":
		return false
	default:
		return true
	}
}

func scanInsight(scanner interface{ Scan(dest ...any) error }) (*Insight, error) {
	var ins Insight
	var createdAt, lastReinforced string
	err := scanner.Scan(
		&ins.ID, &ins.Agent, &ins.Content, &ins.CompactContent, &ins.Category, &ins.TriggerDesc,
		&ins.Confidence, &ins.ReinforcementCount, &createdAt, &lastReinforced,
	)
	if err != nil {
		return nil, err
	}
	ins.CreatedAt, _ = parseFlexibleTime(createdAt)
	ins.LastReinforced, _ = parseFlexibleTime(lastReinforced)
	return &ins, nil
}

// DecayInsights reduces confidence for insights that haven't been reinforced
// recently, using an exponential half-life model identical to fragment decay.
// For each insight, the new confidence is:
//
//	new = current * 2^(-days_since_reinforced / halfLifeDays)
//
// Returns the number of insights updated.
func (s *Store) DecayInsights(ctx context.Context, agent string, halfLifeDays float64) (int, error) {
	if halfLifeDays <= 0 {
		halfLifeDays = 30
	}
	// Fetch all insights for the agent (or all agents if agent=="").
	query := `SELECT id, confidence, last_reinforced FROM memory_insights`
	var args []any
	if agent != "" {
		query += ` WHERE agent = ?`
		args = append(args, agent)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("decay insights query: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id         int64
		confidence float64
		lastReinf  time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var lastReinfStr string
		if err := rows.Scan(&c.id, &c.confidence, &lastReinfStr); err != nil {
			continue
		}
		c.lastReinf, _ = parseFlexibleTime(lastReinfStr)
		if c.lastReinf.IsZero() {
			continue // Malformed timestamp — skip rather than catastrophic decay.
		}
		candidates = append(candidates, c)
	}
	rows.Close()

	now := time.Now()
	updated := 0
	for _, c := range candidates {
		daysSince := now.Sub(c.lastReinf).Hours() / 24
		if daysSince < 1 {
			continue // No meaningful decay within the first day.
		}
		decayed := c.confidence * math.Pow(2, -daysSince/halfLifeDays)
		if math.Abs(decayed-c.confidence) < 0.001 {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE memory_insights SET confidence = ? WHERE id = ?`,
			decayed, c.id,
		); err != nil {
			continue
		}
		updated++
	}
	return updated, nil
}

// GCInsights deletes insights whose confidence has fallen below minConfidence.
// This is typically called after DecayInsights to prune stale entries.
// Returns the number of insights deleted.
func (s *Store) GCInsights(ctx context.Context, agent string, minConfidence float64) (int, error) {
	if minConfidence <= 0 {
		minConfidence = 0.1
	}

	// Fetch candidates first so we can clean up FTS entries before deleting rows.
	selectQ := `SELECT id, agent, content, category, trigger_desc FROM memory_insights WHERE confidence < ?`
	selectArgs := []any{minConfidence}
	if agent != "" {
		selectQ += ` AND agent = ?`
		selectArgs = append(selectArgs, agent)
	}
	rows, err := s.db.QueryContext(ctx, selectQ, selectArgs...)
	if err != nil {
		return 0, fmt.Errorf("gc insights select: %w", err)
	}
	type gcRow struct {
		id          int64
		agent       string
		content     string
		category    string
		triggerDesc string
	}
	var victims []gcRow
	for rows.Next() {
		var r gcRow
		if err := rows.Scan(&r.id, &r.agent, &r.content, &r.category, &r.triggerDesc); err == nil {
			victims = append(victims, r)
		}
	}
	rows.Close()

	if len(victims) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("gc insights tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	deleted := 0
	for _, v := range victims {
		_, _ = tx.ExecContext(ctx,
			`INSERT INTO memory_insights_fts(memory_insights_fts, rowid, agent, content, category, trigger_desc)
			 VALUES ('delete', ?, ?, ?, ?, ?)`,
			v.id, v.agent, v.content, v.category, v.triggerDesc)
		if res, err := tx.ExecContext(ctx,
			`DELETE FROM memory_insights WHERE id = ?`, v.id); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				deleted++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("gc insights commit: %w", err)
	}
	return deleted, nil
}
