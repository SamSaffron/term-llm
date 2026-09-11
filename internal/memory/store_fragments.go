package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/sqlitefts"
)

func (s *Store) CreateFragment(ctx context.Context, f *Fragment) error {
	if f == nil {
		return fmt.Errorf("fragment is nil")
	}
	if strings.TrimSpace(f.Agent) == "" {
		return fmt.Errorf("agent is required")
	}
	if strings.TrimSpace(f.Path) == "" {
		return fmt.Errorf("path is required")
	}
	if strings.TrimSpace(f.Content) == "" {
		return fmt.Errorf("content is required")
	}
	if f.ID == "" {
		f.ID = newID()
	}
	if f.Source == "" {
		f.Source = DefaultSourceMine
	}
	now := time.Now()
	if f.CreatedAt.IsZero() {
		f.CreatedAt = now
	}
	if f.UpdatedAt.IsZero() {
		f.UpdatedAt = f.CreatedAt
	}
	if f.DecayScore == 0 {
		f.DecayScore = 1.0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_fragments (
			id, agent, path, content, source,
			created_at, updated_at, accessed_at,
			access_count, decay_score, pinned
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.Agent, f.Path, f.Content, f.Source,
		f.CreatedAt, f.UpdatedAt, f.AccessedAt,
		f.AccessCount, f.DecayScore, f.Pinned)
	if err != nil {
		return fmt.Errorf("insert fragment: %w", err)
	}

	var rowID int64
	if err := tx.QueryRowContext(ctx, `SELECT rowid FROM memory_fragments WHERE id = ?`, f.ID).Scan(&rowID); err != nil {
		return fmt.Errorf("get fragment rowid: %w", err)
	}

	if err := syncFTSInsert(ctx, tx, rowID, f); err != nil {
		return fmt.Errorf("sync fts insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create fragment: %w", err)
	}
	return nil
}

// UpdateFragment updates content for an existing (agent,path) fragment and syncs FTS.
// Returns updated=false when no matching fragment exists or content is identical (no-op).
// updated_at is only bumped when content actually changes.
func (s *Store) UpdateFragment(ctx context.Context, agent, path, content string) (updated bool, err error) {
	agent = strings.TrimSpace(agent)
	path = strings.TrimSpace(path)
	if agent == "" || path == "" {
		return false, fmt.Errorf("agent and path are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	oldFrag, rowID, err := getFragmentByAgentPathTx(ctx, tx, agent, path)
	if err != nil {
		return false, err
	}
	if oldFrag == nil {
		return false, nil
	}

	// No-op: content unchanged — don't bump updated_at or invalidate embeddings.
	if oldFrag.Content == content {
		return false, nil
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`UPDATE memory_fragments SET content = ?, updated_at = ? WHERE rowid = ?`,
		content, now, rowID)
	if err != nil {
		return false, fmt.Errorf("update fragment: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE fragment_id = ?`, oldFrag.ID); err != nil {
		return false, fmt.Errorf("delete stale embeddings: %w", err)
	}

	if err := syncFTSDelete(ctx, tx, rowID, oldFrag); err != nil {
		return false, fmt.Errorf("sync fts delete: %w", err)
	}

	newFrag := *oldFrag
	newFrag.Content = content
	newFrag.UpdatedAt = now
	if err := syncFTSInsert(ctx, tx, rowID, &newFrag); err != nil {
		return false, fmt.Errorf("sync fts insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit update fragment: %w", err)
	}

	return true, nil
}

// UpdateFragmentByRowID updates a fragment looked up by its SQLite rowid.
// Returns updated=false if no fragment with that rowid exists, or content is unchanged.
func (s *Store) UpdateFragmentByRowID(ctx context.Context, rowID int64, content string) (updated bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var oldFrag Fragment
	err = tx.QueryRowContext(ctx, `
		SELECT rowid, id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments WHERE rowid = ?`, rowID).Scan(
		&oldFrag.RowID, &oldFrag.ID, &oldFrag.Agent, &oldFrag.Path,
		&oldFrag.Content, &oldFrag.Source, &oldFrag.CreatedAt, &oldFrag.UpdatedAt,
		&oldFrag.AccessedAt, &oldFrag.AccessCount, &oldFrag.DecayScore, &oldFrag.Pinned,
	)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get fragment by rowid: %w", err)
	}

	if oldFrag.Content == content {
		return false, nil
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`UPDATE memory_fragments SET content = ?, updated_at = ? WHERE rowid = ?`,
		content, now, rowID)
	if err != nil {
		return false, fmt.Errorf("update fragment: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_embeddings WHERE fragment_id = ?`, oldFrag.ID); err != nil {
		return false, fmt.Errorf("delete stale embeddings: %w", err)
	}

	if err := syncFTSDelete(ctx, tx, rowID, &oldFrag); err != nil {
		return false, fmt.Errorf("sync fts delete: %w", err)
	}

	newFrag := oldFrag
	newFrag.Content = content
	newFrag.UpdatedAt = now
	if err := syncFTSInsert(ctx, tx, rowID, &newFrag); err != nil {
		return false, fmt.Errorf("sync fts insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit update fragment: %w", err)
	}
	return true, nil
}

// DeleteFragment removes a fragment and syncs FTS.
// Returns deleted=false when no matching fragment exists.
func (s *Store) DeleteFragment(ctx context.Context, agent, path string) (deleted bool, err error) {
	agent = strings.TrimSpace(agent)
	path = strings.TrimSpace(path)
	if agent == "" || path == "" {
		return false, fmt.Errorf("agent and path are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	f, rowID, err := getFragmentByAgentPathTx(ctx, tx, agent, path)
	if err != nil {
		return false, err
	}
	if f == nil {
		return false, nil
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM memory_fragments WHERE rowid = ?`, rowID)
	if err != nil {
		return false, fmt.Errorf("delete fragment: %w", err)
	}

	if err := syncFTSDelete(ctx, tx, rowID, f); err != nil {
		return false, fmt.Errorf("sync fts delete: %w", err)
	}

	if err := s.deleteFragmentSources(ctx, tx, f.Agent, f.Path); err != nil {
		return false, fmt.Errorf("delete fragment sources: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit delete fragment: %w", err)
	}

	return true, nil
}

// DeleteFragmentByRowID removes a fragment by its SQLite rowid.
// Returns deleted=false if no fragment with that rowid exists.
func (s *Store) DeleteFragmentByRowID(ctx context.Context, rowID int64) (deleted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var f Fragment
	err = tx.QueryRowContext(ctx, `
		SELECT rowid, id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments WHERE rowid = ?`, rowID).Scan(
		&f.RowID, &f.ID, &f.Agent, &f.Path,
		&f.Content, &f.Source, &f.CreatedAt, &f.UpdatedAt,
		&f.AccessedAt, &f.AccessCount, &f.DecayScore, &f.Pinned,
	)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get fragment by rowid: %w", err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM memory_fragments WHERE rowid = ?`, rowID)
	if err != nil {
		return false, fmt.Errorf("delete fragment: %w", err)
	}

	if err := syncFTSDelete(ctx, tx, rowID, &f); err != nil {
		return false, fmt.Errorf("sync fts delete: %w", err)
	}

	if err := s.deleteFragmentSources(ctx, tx, f.Agent, f.Path); err != nil {
		return false, fmt.Errorf("delete fragment sources: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit delete fragment: %w", err)
	}
	return true, nil
}

// GetFragment fetches a fragment by (agent,path).
func (s *Store) GetFragment(ctx context.Context, agent, path string) (*Fragment, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE agent = ? AND path = ?`,
		agent, path)

	f, err := scanFragment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get fragment: %w", err)
	}
	return f, nil
}

// GetFragmentByRowID fetches a fragment by its SQLite rowid.
func (s *Store) GetFragmentByRowID(ctx context.Context, rowID int64) (*Fragment, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE rowid = ?`, rowID)

	f, err := scanFragment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get fragment by rowid: %w", err)
	}
	return f, nil
}

// FindFragmentsByPath fetches fragments for a path across all agents.
func (s *Store) FindFragmentsByPath(ctx context.Context, path string) ([]Fragment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE path = ?
		ORDER BY updated_at DESC`, path)
	if err != nil {
		return nil, fmt.Errorf("query fragments by path: %w", err)
	}
	defer rows.Close()

	var out []Fragment
	for rows.Next() {
		f, err := scanFragment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan fragment: %w", err)
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFragments returns fragments sorted by created_at descending (most recently learned first).
// updated_at is only bumped when content actually changes, so ordering by it would give
// misleading results when many fragments happen to be updated in the same batch.
// RowID is populated on each returned Fragment.
func (s *Store) ListFragments(ctx context.Context, opts ListOptions) ([]Fragment, error) {
	query := `
		SELECT rowid, id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE 1=1`
	args := []any{}

	if strings.TrimSpace(opts.Agent) != "" {
		query += ` AND agent = ?`
		args = append(args, strings.TrimSpace(opts.Agent))
	}
	if opts.Since != nil {
		query += ` AND created_at >= ?`
		args = append(args, *opts.Since)
	}
	if strings.TrimSpace(opts.PathFilter) != "" {
		query += ` AND path LIKE ? ESCAPE '\'`
		args = append(args, "%"+sqliteLikeEscape(strings.TrimSpace(opts.PathFilter))+"%")
	}
	query += ` ORDER BY created_at DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list fragments: %w", err)
	}
	defer rows.Close()

	var out []Fragment
	for rows.Next() {
		var f Fragment
		var accessedAt sql.NullTime
		if err := rows.Scan(
			&f.RowID,
			&f.ID,
			&f.Agent,
			&f.Path,
			&f.Content,
			&f.Source,
			&f.CreatedAt,
			&f.UpdatedAt,
			&accessedAt,
			&f.AccessCount,
			&f.DecayScore,
			&f.Pinned,
		); err != nil {
			return nil, fmt.Errorf("scan fragment: %w", err)
		}
		if accessedAt.Valid {
			f.AccessedAt = &accessedAt.Time
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListFragmentPaths returns only fragment paths for lightweight listings.
// Results preserve ListFragments' newest-first ordering, but avoid reading large
// content bodies when callers only need path names (for example, mining tools).
func (s *Store) ListFragmentPaths(ctx context.Context, agent, prefix string, limit int) ([]string, error) {
	agent = strings.TrimSpace(agent)
	prefix = strings.TrimSpace(prefix)

	query := `
		SELECT path
		FROM memory_fragments
		WHERE 1=1`
	args := []any{}
	if agent != "" {
		query += ` AND agent = ?`
		args = append(args, agent)
	}
	if prefix != "" {
		query += ` AND path >= ?`
		args = append(args, prefix)
		if end := sqlitePrefixRangeEnd(prefix); end != "" {
			query += ` AND path < ?`
			args = append(args, end)
		}
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list fragment paths: %w", err)
	}
	defer rows.Close()

	paths := []string{}
	if limit > 0 {
		paths = make([]string, 0, limit)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan fragment path: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

// ListAgents returns distinct agent names that have stored fragments.
func (s *Store) ListAgents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT agent FROM memory_fragments ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var agent string
		if err := rows.Scan(&agent); err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

// SearchBM25 performs BM25 search over FTS5 and returns fragment details.
func (s *Store) SearchBM25(ctx context.Context, query string, limit int, agent string) ([]ScoredFragment, error) {
	if limit <= 0 {
		limit = 6
	}

	ftsQuery := sqlitefts.LiteralQuery(query)
	if ftsQuery == "" {
		return []ScoredFragment{}, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT mf.id,
		       mf.agent,
		       mf.path,
		       mf.content,
		       mf.source,
		       mf.created_at,
		       mf.updated_at,
		       mf.accessed_at,
		       mf.access_count,
		       mf.decay_score,
		       mf.pinned,
		       snippet(memory_fts, 3, '[', ']', '...', 24) AS snippet,
		       bm25(memory_fts) AS score
		FROM memory_fts
		JOIN memory_fragments mf ON mf.rowid = memory_fts.rowid
		WHERE memory_fts MATCH ?
		  AND (? = '' OR mf.agent = ?)
		ORDER BY bm25(memory_fts)
		LIMIT ?`, ftsQuery, agent, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("search fragments: %w", err)
	}
	defer rows.Close()

	var out []ScoredFragment
	for rows.Next() {
		var r ScoredFragment
		var accessedAt sql.NullTime
		var rawScore float64
		if err := rows.Scan(
			&r.ID,
			&r.Agent,
			&r.Path,
			&r.Content,
			&r.Source,
			&r.CreatedAt,
			&r.UpdatedAt,
			&accessedAt,
			&r.AccessCount,
			&r.DecayScore,
			&r.Pinned,
			&r.Snippet,
			&rawScore,
		); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		if accessedAt.Valid {
			at := accessedAt.Time
			r.AccessedAt = &at
		}
		// SQLite FTS5 bm25() returns negative values (more negative = more relevant).
		r.Score = -rawScore
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SearchFragments performs BM25 search over FTS5.
func (s *Store) SearchFragments(ctx context.Context, query string, limit int, agent string) ([]SearchResult, error) {
	scored, err := s.SearchBM25(ctx, query, limit, agent)
	if err != nil {
		return nil, err
	}

	out := make([]SearchResult, 0, len(scored))
	for _, r := range scored {
		out = append(out, SearchResult{
			Agent:   r.Agent,
			Path:    r.Path,
			Snippet: r.Snippet,
			Score:   r.Score,
		})
	}
	return out, nil
}

const embeddingVectorBinaryMagic = "tllmf64\x01"
