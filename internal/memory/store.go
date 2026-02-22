package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultSourceMine = "mine"
)

// Config controls memory store initialization.
type Config struct {
	Path string // Optional DB path override (supports :memory:)
}

// Fragment is a durable memory item extracted from sessions.
type Fragment struct {
	ID          string
	Agent       string
	Path        string
	Content     string
	Source      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	AccessedAt  *time.Time
	AccessCount int
	DecayScore  float64
	Pinned      bool
}

// ListOptions configures fragment listing.
type ListOptions struct {
	Agent string
	Since *time.Time
	Limit int
}

// SearchResult is a BM25 result from FTS.
type SearchResult struct {
	Agent   string  `json:"agent"`
	Path    string  `json:"path"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

// MiningState tracks per-session mining progress.
type MiningState struct {
	SessionID       string
	Agent           string
	LastMinedOffset int
	MinedAt         time.Time
}

// Store persists memory fragments and mining state.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS memory_fragments (
    id           TEXT PRIMARY KEY,
    agent        TEXT NOT NULL,
    path         TEXT NOT NULL,
    content      TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT 'mine',
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NOT NULL,
    accessed_at  DATETIME,
    access_count INTEGER NOT NULL DEFAULT 0,
    decay_score  REAL NOT NULL DEFAULT 1.0,
    pinned       BOOLEAN NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_fragments_agent_path ON memory_fragments(agent, path);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    id UNINDEXED,
    agent UNINDEXED,
    path,
    content,
    content='memory_fragments',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TABLE IF NOT EXISTS memory_mining_state (
    session_id         TEXT PRIMARY KEY,
    agent              TEXT NOT NULL,
    last_mined_offset  INTEGER NOT NULL DEFAULT 0,
    mined_at           DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS memory_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// NewStore opens memory.db and initializes schema.
func NewStore(cfg Config) (*Store, error) {
	dbPath, err := ResolveDBPath(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve memory db path: %w", err)
	}

	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			return nil, fmt.Errorf("create memory data directory: %w", err)
		}
	}

	dsn := dbPath
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=mmap_size(134217728)&_pragma=cache_size(-64000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}

	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("initialize memory schema: %w", err)
	}

	if err := ensureFTSInitialized(db); err != nil {
		return fmt.Errorf("initialize memory fts: %w", err)
	}

	return nil
}

func ensureFTSInitialized(db *sql.DB) error {
	var marker string
	err := db.QueryRow(`SELECT value FROM memory_meta WHERE key = 'fts_initialized'`).Scan(&marker)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}

	if _, err := db.Exec(`INSERT INTO memory_fts(memory_fts) VALUES('rebuild')`); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO memory_meta(key, value) VALUES('fts_initialized', '1')`); err != nil {
		return err
	}
	return nil
}

// GetDataDir returns the XDG data directory for term-llm.
func GetDataDir() (string, error) {
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		return filepath.Join(xdgData, "term-llm"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".local", "share", "term-llm"), nil
}

// GetDBPath returns the default memory.db path.
func GetDBPath() (string, error) {
	dataDir, err := GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "memory.db"), nil
}

// ResolveDBPath resolves an optional DB path override.
func ResolveDBPath(pathOverride string) (string, error) {
	pathOverride = strings.TrimSpace(pathOverride)
	if pathOverride == "" {
		return GetDBPath()
	}
	if pathOverride == ":memory:" {
		return pathOverride, nil
	}

	pathOverride = os.ExpandEnv(pathOverride)
	if strings.HasPrefix(pathOverride, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		pathOverride = filepath.Join(homeDir, pathOverride[2:])
	}

	abs, err := filepath.Abs(pathOverride)
	if err != nil {
		return "", fmt.Errorf("resolve db path %q: %w", pathOverride, err)
	}
	return abs, nil
}

// CreateFragment inserts a new fragment and syncs FTS explicitly.
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
// Returns updated=false when no matching fragment exists.
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

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`UPDATE memory_fragments SET content = ?, updated_at = ? WHERE rowid = ?`,
		content, now, rowID)
	if err != nil {
		return false, fmt.Errorf("update fragment: %w", err)
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

// ListFragments returns fragments sorted by updated_at descending.
func (s *Store) ListFragments(ctx context.Context, opts ListOptions) ([]Fragment, error) {
	query := `
		SELECT id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE 1=1`
	args := []any{}

	if strings.TrimSpace(opts.Agent) != "" {
		query += ` AND agent = ?`
		args = append(args, strings.TrimSpace(opts.Agent))
	}
	if opts.Since != nil {
		query += ` AND updated_at >= ?`
		args = append(args, *opts.Since)
	}
	query += ` ORDER BY updated_at DESC`
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

// SearchFragments performs BM25 search over FTS5.
func (s *Store) SearchFragments(ctx context.Context, query string, limit int, agent string) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 6
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT mf.agent,
		       mf.path,
		       snippet(memory_fts, 3, '[', ']', '...', 24) AS snippet,
		       bm25(memory_fts) AS score
		FROM memory_fts
		JOIN memory_fragments mf ON mf.rowid = memory_fts.rowid
		WHERE memory_fts MATCH ?
		  AND (? = '' OR mf.agent = ?)
		ORDER BY bm25(memory_fts)
		LIMIT ?`, query, agent, agent, limit)
	if err != nil {
		return nil, fmt.Errorf("search fragments: %w", err)
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Agent, &r.Path, &r.Snippet, &r.Score); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetState returns mining state for a session.
func (s *Store) GetState(ctx context.Context, sessionID string) (*MiningState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT session_id, agent, last_mined_offset, mined_at
		FROM memory_mining_state
		WHERE session_id = ?`, sessionID)

	var st MiningState
	if err := row.Scan(&st.SessionID, &st.Agent, &st.LastMinedOffset, &st.MinedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get mining state: %w", err)
	}
	return &st, nil
}

// UpsertState inserts or updates mining state.
func (s *Store) UpsertState(ctx context.Context, st *MiningState) error {
	if st == nil {
		return fmt.Errorf("mining state is nil")
	}
	if strings.TrimSpace(st.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(st.Agent) == "" {
		return fmt.Errorf("agent is required")
	}
	if st.MinedAt.IsZero() {
		st.MinedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_mining_state(session_id, agent, last_mined_offset, mined_at)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			agent = excluded.agent,
			last_mined_offset = excluded.last_mined_offset,
			mined_at = excluded.mined_at`,
		st.SessionID, st.Agent, st.LastMinedOffset, st.MinedAt)
	if err != nil {
		return fmt.Errorf("upsert mining state: %w", err)
	}
	return nil
}

// FragmentCountsByAgent returns count(fragment) grouped by agent.
func (s *Store) FragmentCountsByAgent(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent, COUNT(*)
		FROM memory_fragments
		GROUP BY agent`)
	if err != nil {
		return nil, fmt.Errorf("query fragment counts: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var agent string
		var count int
		if err := rows.Scan(&agent, &count); err != nil {
			return nil, fmt.Errorf("scan fragment count: %w", err)
		}
		counts[agent] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// LastMinedByAgent returns MAX(mined_at) grouped by agent.
func (s *Store) LastMinedByAgent(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent, MAX(mined_at)
		FROM memory_mining_state
		GROUP BY agent`)
	if err != nil {
		return nil, fmt.Errorf("query last mined: %w", err)
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var agent string
		var minedAt time.Time
		if err := rows.Scan(&agent, &minedAt); err != nil {
			return nil, fmt.Errorf("scan last mined: %w", err)
		}
		out[agent] = minedAt
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Close closes the underlying DB.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func scanFragment(scanner interface{ Scan(dest ...any) error }) (*Fragment, error) {
	var f Fragment
	var accessedAt sql.NullTime
	err := scanner.Scan(
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
	)
	if err != nil {
		return nil, err
	}
	if accessedAt.Valid {
		f.AccessedAt = &accessedAt.Time
	}
	return &f, nil
}

func getFragmentByAgentPathTx(ctx context.Context, tx *sql.Tx, agent, path string) (*Fragment, int64, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT rowid, id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE agent = ? AND path = ?`,
		agent, path)

	var rowID int64
	var f Fragment
	var accessedAt sql.NullTime
	err := row.Scan(
		&rowID,
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
	)
	if err == sql.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get fragment: %w", err)
	}
	if accessedAt.Valid {
		f.AccessedAt = &accessedAt.Time
	}
	return &f, rowID, nil
}

func syncFTSInsert(ctx context.Context, tx *sql.Tx, rowID int64, f *Fragment) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_fts(rowid, id, agent, path, content) VALUES(?, ?, ?, ?, ?)`,
		rowID, f.ID, f.Agent, f.Path, f.Content)
	return err
}

func syncFTSDelete(ctx context.Context, tx *sql.Tx, rowID int64, f *Fragment) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_fts(memory_fts, rowid, id, agent, path, content) VALUES('delete', ?, ?, ?, ?, ?)`,
		rowID, f.ID, f.Agent, f.Path, f.Content)
	return err
}

func newID() string {
	now := time.Now().Format("20060102-150405")
	randBytes := make([]byte, 3)
	_, _ = rand.Read(randBytes)
	return fmt.Sprintf("mem-%s-%s", now, hex.EncodeToString(randBytes))
}
