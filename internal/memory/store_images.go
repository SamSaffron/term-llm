package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/sqlitefts"
)

func (s *Store) RecordImage(ctx context.Context, r *ImageRecord) error {
	if r == nil {
		return nil
	}
	if r.ID == "" {
		randBytes := make([]byte, 6)
		_, _ = rand.Read(randBytes)
		r.ID = fmt.Sprintf("img-%s-%s", time.Now().Format("20060102-150405"), hex.EncodeToString(randBytes))
	}
	if r.MimeType == "" {
		r.MimeType = "image/png"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx, `
		INSERT INTO generated_images
			(id, agent, session_id, prompt, output_path, mime_type, provider, width, height, file_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Agent, r.SessionID, r.Prompt, r.OutputPath, r.MimeType, r.Provider, r.Width, r.Height, r.FileSize,
	)
	if err != nil {
		return fmt.Errorf("insert generated_images: %w", err)
	}

	rowID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO generated_images_fts(rowid, prompt, output_path) VALUES (?, ?, ?)`,
		rowID, r.Prompt, r.OutputPath,
	)
	if err != nil {
		return fmt.Errorf("insert generated_images_fts: %w", err)
	}

	return tx.Commit()
}

// ListImages returns generated images ordered by creation time (newest first).
func (s *Store) ListImages(ctx context.Context, opts ImageListOptions) ([]ImageRecord, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}

	query := `
		SELECT id, agent, session_id, prompt, output_path, mime_type, provider, width, height, file_size, created_at
		FROM generated_images`
	args := []interface{}{}

	if opts.Agent != "" {
		query += ` WHERE agent = ?`
		args = append(args, opts.Agent)
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, opts.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	defer rows.Close()

	var out []ImageRecord
	for rows.Next() {
		var r ImageRecord
		if err := rows.Scan(&r.ID, &r.Agent, &r.SessionID, &r.Prompt, &r.OutputPath,
			&r.MimeType, &r.Provider, &r.Width, &r.Height, &r.FileSize, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan image row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListImageAgents returns distinct agent names for generated images.
func (s *Store) ListImageAgents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT agent FROM generated_images ORDER BY agent`)
	if err != nil {
		return nil, fmt.Errorf("list image agents: %w", err)
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var agent string
		if err := rows.Scan(&agent); err != nil {
			return nil, fmt.Errorf("scan image agent: %w", err)
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// ListImagesSince returns generated images for an agent created after the given time.
func (s *Store) ListImagesSince(ctx context.Context, agent string, since time.Time) ([]ImageRecord, error) {
	query := `
		SELECT id, agent, session_id, prompt, output_path, mime_type, provider, width, height, file_size, created_at
		FROM generated_images
		WHERE agent = ?`
	args := []interface{}{agent}
	if !since.IsZero() {
		query += ` AND created_at > ?`
		args = append(args, since)
	}
	query += ` ORDER BY created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list images since: %w", err)
	}
	defer rows.Close()

	var out []ImageRecord
	for rows.Next() {
		var r ImageRecord
		if err := rows.Scan(&r.ID, &r.Agent, &r.SessionID, &r.Prompt, &r.OutputPath,
			&r.MimeType, &r.Provider, &r.Width, &r.Height, &r.FileSize, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan image row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SearchImages searches generated images by prompt/path using FTS5.
func (s *Store) SearchImages(ctx context.Context, query, agent string, limit int) ([]ImageRecord, error) {
	if limit <= 0 {
		limit = 10
	}

	ftsQuery := sqlitefts.LiteralQuery(query)
	if ftsQuery == "" {
		return []ImageRecord{}, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.agent, g.session_id, g.prompt, g.output_path, g.mime_type,
		       g.provider, g.width, g.height, g.file_size, g.created_at
		FROM generated_images_fts f
		JOIN generated_images g ON g.rowid = f.rowid
		WHERE generated_images_fts MATCH ?
		  AND (? = '' OR g.agent = ?)
		ORDER BY rank
		LIMIT ?`,
		ftsQuery, agent, agent, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search images: %w", err)
	}
	defer rows.Close()

	var out []ImageRecord
	for rows.Next() {
		var r ImageRecord
		if err := rows.Scan(&r.ID, &r.Agent, &r.SessionID, &r.Prompt, &r.OutputPath,
			&r.MimeType, &r.Provider, &r.Width, &r.Height, &r.FileSize, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan image search row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sqlitePrefixRangeEnd returns the exclusive upper bound for a bytewise SQLite
// range scan over strings with the provided prefix. Empty means there is no
// representable upper bound.
func sqlitePrefixRangeEnd(prefix string) string {
	if prefix == "" {
		return ""
	}
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}

// sqliteLikeEscape escapes SQLite LIKE special characters (%, _, \) in a literal string.
func sqliteLikeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ── Insights ──────────────────────────────────────────────────────────────────

// Insight is a generalized behavioral rule extracted from past sessions.
// Unlike fragments (which store facts), insights store actionable patterns
// that change how the agent behaves in future similar situations.
type Insight struct {
	ID                 int64
	Agent              string
	Content            string
	CompactContent     string // short form for injection; falls back to Content if empty
	Category           string
	TriggerDesc        string
	Confidence         float64
	ReinforcementCount int
	CreatedAt          time.Time
	LastReinforced     time.Time
}

// CreateInsight inserts a new insight and syncs the FTS index.
