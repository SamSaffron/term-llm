package memory

import (
	"container/heap"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/embedding"
	"github.com/samsaffron/term-llm/internal/sqlitefts"
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
	RowID       int64
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

// ScoredFragment is a fragment paired with a relevance score.
type ScoredFragment struct {
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	Path        string    `json:"path"`
	Content     string    `json:"-"`
	Source      string    `json:"-"`
	CreatedAt   time.Time `json:"-"`
	UpdatedAt   time.Time `json:"-"`
	AccessedAt  *time.Time
	AccessCount int       `json:"-"`
	DecayScore  float64   `json:"-"`
	Pinned      bool      `json:"-"`
	Snippet     string    `json:"snippet"`
	Score       float64   `json:"score"`
	Vector      []float64 `json:"-"`
}

// ListOptions configures fragment listing.
type ListOptions struct {
	Agent      string
	Since      *time.Time
	Limit      int
	PathFilter string // substring match on path (case-insensitive)
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
CREATE INDEX IF NOT EXISTS idx_fragments_agent_created_at ON memory_fragments(agent, created_at DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    id UNINDEXED,
    agent UNINDEXED,
    path,
    content,
    content='memory_fragments',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TABLE IF NOT EXISTS memory_embeddings (
    fragment_id         TEXT NOT NULL REFERENCES memory_fragments(id) ON DELETE CASCADE,
    provider            TEXT NOT NULL,
    model               TEXT NOT NULL,
    dimensions          INTEGER NOT NULL,
    vector              BLOB NOT NULL,
    search_prefix_0     REAL NOT NULL DEFAULT 0,
    search_prefix_1     REAL NOT NULL DEFAULT 0,
    search_prefix_2     REAL NOT NULL DEFAULT 0,
    search_prefix_3     REAL NOT NULL DEFAULT 0,
    search_prefix_4     REAL NOT NULL DEFAULT 0,
    search_prefix_5     REAL NOT NULL DEFAULT 0,
    search_prefix_6     REAL NOT NULL DEFAULT 0,
    search_prefix_7     REAL NOT NULL DEFAULT 0,
    search_tail_norm    REAL NOT NULL DEFAULT -1,
    search_vector_norm  REAL NOT NULL DEFAULT -1,
    embedded_at         DATETIME NOT NULL,
    PRIMARY KEY (fragment_id, provider, model)
);

CREATE INDEX IF NOT EXISTS idx_memory_embeddings_provider_model_dimensions ON memory_embeddings(provider, model, dimensions);

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

CREATE TABLE IF NOT EXISTS generated_images (
    id          TEXT PRIMARY KEY,
    agent       TEXT NOT NULL DEFAULT '',
    session_id  TEXT NOT NULL DEFAULT '',
    prompt      TEXT NOT NULL,
    output_path TEXT NOT NULL,
    mime_type   TEXT NOT NULL DEFAULT 'image/png',
    provider    TEXT NOT NULL DEFAULT '',
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    file_size   INTEGER NOT NULL DEFAULT 0,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_generated_images_agent ON generated_images(agent);
CREATE INDEX IF NOT EXISTS idx_generated_images_created_at ON generated_images(created_at DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS generated_images_fts USING fts5(
    prompt,
    output_path,
    content='generated_images',
    content_rowid='rowid',
    tokenize='unicode61'
);

CREATE TABLE IF NOT EXISTS memory_fragment_sources (
    id         INTEGER PRIMARY KEY,
    agent      TEXT NOT NULL,
    path       TEXT NOT NULL,
    session_id TEXT NOT NULL,
    turn_start INTEGER NOT NULL,
    turn_end   INTEGER NOT NULL,
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_fragment_sources_agent_path ON memory_fragment_sources(agent, path);
CREATE INDEX IF NOT EXISTS idx_fragment_sources_session_id ON memory_fragment_sources(session_id);

CREATE TABLE IF NOT EXISTS memory_insights (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    agent               TEXT NOT NULL,
    content             TEXT NOT NULL,
    compact_content     TEXT NOT NULL DEFAULT '',
    category            TEXT NOT NULL DEFAULT '',
    trigger_desc        TEXT NOT NULL DEFAULT '',
    confidence          REAL NOT NULL DEFAULT 0.5,
    reinforcement_count INTEGER NOT NULL DEFAULT 1,
    created_at          DATETIME NOT NULL DEFAULT (datetime('now')),
    last_reinforced     DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_insights_agent ON memory_insights(agent);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_insights_fts USING fts5(
    agent UNINDEXED,
    content,
    category,
    trigger_desc,
    content='memory_insights',
    content_rowid='id',
    tokenize='unicode61'
);

CREATE TABLE IF NOT EXISTS memory_insight_mining_state (
    session_id TEXT PRIMARY KEY,
    agent      TEXT NOT NULL,
    mined_at   DATETIME NOT NULL
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
	if dbPath == ":memory:" {
		dsn = fmt.Sprintf("file:term-llm-memory-%d?mode=memory&cache=shared", time.Now().UnixNano())
	}
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=mmap_size(134217728)&_pragma=cache_size(-64000)"

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

	// Idempotent column migrations for existing databases.
	// SQLite returns "duplicate column name" when the column already exists;
	// we suppress that specific error so initSchema is safe to run on any DB.
	migrations := []string{
		`ALTER TABLE memory_insights ADD COLUMN compact_content TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_0 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_1 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_2 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_3 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_4 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_5 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_6 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_prefix_7 REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_tail_norm REAL NOT NULL DEFAULT -1`,
		`ALTER TABLE memory_embeddings ADD COLUMN search_vector_norm REAL NOT NULL DEFAULT -1`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("schema migration %q: %w", m, err)
		}
	}

	if err := createVectorSearchIndex(db); err != nil {
		return err
	}

	if err := ensureFTSInitialized(db); err != nil {
		return fmt.Errorf("initialize memory fts: %w", err)
	}

	return nil
}

func createVectorSearchIndex(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_memory_embeddings_vector_search_cover ON memory_embeddings(
			provider,
			model,
			dimensions,
			fragment_id,
			search_vector_norm,
			search_tail_norm,
			search_prefix_0,
			search_prefix_1,
			search_prefix_2,
			search_prefix_3,
			search_prefix_4,
			search_prefix_5,
			search_prefix_6,
			search_prefix_7
		)`)
	if err != nil {
		return fmt.Errorf("create vector search index: %w", err)
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

func hasEmbeddingVectorBinaryMagic(payload []byte) bool {
	if len(payload) < len(embeddingVectorBinaryMagic) {
		return false
	}
	for i := 0; i < len(embeddingVectorBinaryMagic); i++ {
		if payload[i] != embeddingVectorBinaryMagic[i] {
			return false
		}
	}
	return true
}

func encodeEmbeddingVector(vec []float64) []byte {
	headerLen := len(embeddingVectorBinaryMagic) + 4
	payload := make([]byte, headerLen+len(vec)*8)
	copy(payload, embeddingVectorBinaryMagic)
	binary.LittleEndian.PutUint32(payload[len(embeddingVectorBinaryMagic):headerLen], uint32(len(vec)))

	offset := headerLen
	for _, v := range vec {
		binary.LittleEndian.PutUint64(payload[offset:offset+8], math.Float64bits(v))
		offset += 8
	}
	return payload
}

func decodeEmbeddingVector(payload []byte) ([]float64, error) {
	if hasEmbeddingVectorBinaryMagic(payload) {
		return decodeBinaryEmbeddingVector(payload)
	}

	var vec []float64
	if err := json.Unmarshal(payload, &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

func decodeBinaryEmbeddingVector(payload []byte) ([]float64, error) {
	dims, offset, err := parseBinaryEmbeddingVectorHeader(payload)
	if err != nil {
		return nil, err
	}

	vec := make([]float64, dims)
	for i := range vec {
		vec[i] = math.Float64frombits(binary.LittleEndian.Uint64(payload[offset : offset+8]))
		offset += 8
	}
	return vec, nil
}

func parseBinaryEmbeddingVectorHeader(payload []byte) (dims, offset int, err error) {
	headerLen := len(embeddingVectorBinaryMagic) + 4
	if len(payload) < headerLen {
		return 0, 0, fmt.Errorf("truncated binary embedding vector header")
	}
	encodedDims := binary.LittleEndian.Uint32(payload[len(embeddingVectorBinaryMagic):headerLen])
	if encodedDims > uint32((len(payload)-headerLen)/8) {
		return 0, 0, fmt.Errorf("binary embedding vector declares %d dimensions but payload has %d value bytes", encodedDims, len(payload)-headerLen)
	}
	dims = int(encodedDims)
	wantLen := headerLen + dims*8
	if len(payload) != wantLen {
		return 0, 0, fmt.Errorf("binary embedding vector length = %d, want %d for %d dimensions", len(payload), wantLen, dims)
	}
	return dims, headerLen, nil
}

const (
	vectorSearchPrefixDims     = 8
	vectorSearchExactBatchSize = 128
	vectorSearchUpperBoundEps  = 1e-9
)

type embeddingSearchSummary struct {
	prefix     [vectorSearchPrefixDims]float64
	tailNorm   float64
	vectorNorm float64
}

func summarizeEmbeddingSearchVector(vec []float64) embeddingSearchSummary {
	var s embeddingSearchSummary
	var tailNormSq float64
	for i, v := range vec {
		s.vectorNorm += v * v
		if i < vectorSearchPrefixDims {
			s.prefix[i] = v
		} else {
			tailNormSq += v * v
		}
	}
	if s.vectorNorm > 0 {
		s.vectorNorm = math.Sqrt(s.vectorNorm)
	}
	if tailNormSq > 0 {
		s.tailNorm = math.Sqrt(tailNormSq)
	}
	return s
}

func cosineSimilarityPayload(queryVec []float64, payload []byte) (float64, error) {
	if hasEmbeddingVectorBinaryMagic(payload) {
		return cosineSimilarityBinaryPayload(queryVec, payload)
	}

	vec, err := decodeEmbeddingVector(payload)
	if err != nil {
		return 0, err
	}
	return embedding.CosineSimilarity(queryVec, vec), nil
}

func cosineSimilarityBinaryPayload(queryVec []float64, payload []byte) (float64, error) {
	dims, offset, err := parseBinaryEmbeddingVectorHeader(payload)
	if err != nil {
		return 0, err
	}
	if dims != len(queryVec) || dims == 0 {
		return 0, nil
	}

	var dotProduct, normA, normB float64
	for _, a := range queryVec {
		b := math.Float64frombits(binary.LittleEndian.Uint64(payload[offset : offset+8]))
		offset += 8
		dotProduct += a * b
		normA += a * a
		normB += b * b
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0, nil
	}
	return dotProduct / denom, nil
}

// UpsertEmbedding inserts or updates an embedding vector for a fragment.
func (s *Store) UpsertEmbedding(ctx context.Context, fragmentID, provider, model string, dims int, vec []float64) error {
	fragmentID = strings.TrimSpace(fragmentID)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if fragmentID == "" {
		return fmt.Errorf("fragment_id is required")
	}
	if provider == "" {
		return fmt.Errorf("provider is required")
	}
	if model == "" {
		return fmt.Errorf("model is required")
	}
	if len(vec) == 0 {
		return fmt.Errorf("vector cannot be empty")
	}
	if dims <= 0 {
		dims = len(vec)
	}
	if len(vec) != dims {
		return fmt.Errorf("vector dimensions mismatch: got %d values, dims=%d", len(vec), dims)
	}

	payload := encodeEmbeddingVector(vec)
	summary := summarizeEmbeddingSearchVector(vec)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_embeddings(
			fragment_id,
			provider,
			model,
			dimensions,
			vector,
			search_prefix_0,
			search_prefix_1,
			search_prefix_2,
			search_prefix_3,
			search_prefix_4,
			search_prefix_5,
			search_prefix_6,
			search_prefix_7,
			search_tail_norm,
			search_vector_norm,
			embedded_at
		)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fragment_id, provider, model) DO UPDATE SET
			dimensions = excluded.dimensions,
			vector = excluded.vector,
			search_prefix_0 = excluded.search_prefix_0,
			search_prefix_1 = excluded.search_prefix_1,
			search_prefix_2 = excluded.search_prefix_2,
			search_prefix_3 = excluded.search_prefix_3,
			search_prefix_4 = excluded.search_prefix_4,
			search_prefix_5 = excluded.search_prefix_5,
			search_prefix_6 = excluded.search_prefix_6,
			search_prefix_7 = excluded.search_prefix_7,
			search_tail_norm = excluded.search_tail_norm,
			search_vector_norm = excluded.search_vector_norm,
			embedded_at = excluded.embedded_at`,
		fragmentID,
		provider,
		model,
		dims,
		payload,
		summary.prefix[0],
		summary.prefix[1],
		summary.prefix[2],
		summary.prefix[3],
		summary.prefix[4],
		summary.prefix[5],
		summary.prefix[6],
		summary.prefix[7],
		summary.tailNorm,
		summary.vectorNorm,
		time.Now())
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}
	return nil
}

// GetEmbedding fetches a stored embedding vector for a fragment+provider+model.
func (s *Store) GetEmbedding(ctx context.Context, fragmentID, provider, model string) ([]float64, error) {
	fragmentID = strings.TrimSpace(fragmentID)
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if fragmentID == "" || provider == "" || model == "" {
		return nil, fmt.Errorf("fragment_id, provider, and model are required")
	}

	var payload []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT vector
		FROM memory_embeddings
		WHERE fragment_id = ? AND provider = ? AND model = ?`,
		fragmentID, provider, model).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}

	vec, err := decodeEmbeddingVector(payload)
	if err != nil {
		return nil, fmt.Errorf("decode embedding vector: %w", err)
	}
	return vec, nil
}

func (s *Store) GetEmbeddingsByIDs(ctx context.Context, fragmentIDs []string, provider, model string) (map[string][]float64, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("provider and model are required")
	}
	if len(fragmentIDs) == 0 {
		return map[string][]float64{}, nil
	}

	seen := map[string]struct{}{}
	ids := make([]string, 0, len(fragmentIDs))
	for _, id := range fragmentIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return map[string][]float64{}, nil
	}

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = strings.TrimSuffix(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT fragment_id, vector
		FROM memory_embeddings
		WHERE provider = ? AND model = ? AND fragment_id IN (%s)`, placeholders)

	args := make([]any, 0, len(ids)+2)
	args = append(args, provider, model)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get embeddings by ids: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]float64, len(ids))
	for rows.Next() {
		var id string
		var payload sql.RawBytes
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("scan embedding row: %w", err)
		}
		vec, err := decodeEmbeddingVector(payload)
		if err != nil {
			return nil, fmt.Errorf("decode embedding vector: %w", err)
		}
		out[id] = vec
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type embeddingSearchBackfillRow struct {
	rowID   int64
	summary embeddingSearchSummary
}

func (s *Store) backfillVectorSearchColumns(ctx context.Context, provider, model string, dimensions int) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, vector
		FROM memory_embeddings
		WHERE provider = ? AND model = ? AND dimensions = ? AND search_vector_norm < 0`,
		provider, model, dimensions)
	if err != nil {
		return fmt.Errorf("query pending vector search metadata: %w", err)
	}
	defer rows.Close()

	var pending []embeddingSearchBackfillRow
	for rows.Next() {
		var row embeddingSearchBackfillRow
		var payload sql.RawBytes
		if err := rows.Scan(&row.rowID, &payload); err != nil {
			return fmt.Errorf("scan pending vector search metadata: %w", err)
		}
		vec, err := decodeEmbeddingVector(payload)
		if err != nil {
			return fmt.Errorf("decode pending vector search vector row %d: %w", row.rowID, err)
		}
		row.summary = summarizeEmbeddingSearchVector(vec)
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin vector search metadata backfill: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE memory_embeddings
		SET search_prefix_0 = ?,
		    search_prefix_1 = ?,
		    search_prefix_2 = ?,
		    search_prefix_3 = ?,
		    search_prefix_4 = ?,
		    search_prefix_5 = ?,
		    search_prefix_6 = ?,
		    search_prefix_7 = ?,
		    search_tail_norm = ?,
		    search_vector_norm = ?
		WHERE rowid = ?`)
	if err != nil {
		return fmt.Errorf("prepare vector search metadata backfill: %w", err)
	}
	defer stmt.Close()

	for _, row := range pending {
		if _, err := stmt.ExecContext(ctx,
			row.summary.prefix[0],
			row.summary.prefix[1],
			row.summary.prefix[2],
			row.summary.prefix[3],
			row.summary.prefix[4],
			row.summary.prefix[5],
			row.summary.prefix[6],
			row.summary.prefix[7],
			row.summary.tailNorm,
			row.summary.vectorNorm,
			row.rowID,
		); err != nil {
			return fmt.Errorf("update vector search metadata row %d: %w", row.rowID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit vector search metadata backfill: %w", err)
	}
	return nil
}

// GetFragmentsNeedingEmbedding returns fragments missing an embedding row for provider+model.
func (s *Store) GetFragmentsNeedingEmbedding(ctx context.Context, agent, provider, model string) ([]Fragment, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("provider and model are required")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.agent, f.path, f.content, f.source, f.created_at, f.updated_at,
		       f.accessed_at, f.access_count, f.decay_score, f.pinned
		FROM memory_fragments f
		LEFT JOIN memory_embeddings e
		  ON e.fragment_id = f.id AND e.provider = ? AND e.model = ?
		WHERE e.fragment_id IS NULL
		  AND (? = '' OR f.agent = ?)
		ORDER BY f.updated_at DESC`, provider, model, strings.TrimSpace(agent), strings.TrimSpace(agent))
	if err != nil {
		return nil, fmt.Errorf("query fragments needing embedding: %w", err)
	}
	defer rows.Close()

	var out []Fragment
	for rows.Next() {
		frag, scanErr := scanFragment(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan fragment: %w", scanErr)
		}
		out = append(out, *frag)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

const vectorSearchSQL = `
		SELECT e.fragment_id,
		       ((? * e.search_prefix_0) +
		        (? * e.search_prefix_1) +
		        (? * e.search_prefix_2) +
		        (? * e.search_prefix_3) +
		        (? * e.search_prefix_4) +
		        (? * e.search_prefix_5) +
		        (? * e.search_prefix_6) +
		        (? * e.search_prefix_7) +
		        (? * e.search_tail_norm)) /
		       CASE
		         WHEN e.search_vector_norm > 0 THEN e.search_vector_norm
		         ELSE 1
		       END AS upper_bound
		FROM memory_embeddings e
		JOIN memory_fragments f ON f.id = e.fragment_id
		WHERE e.provider = ? AND e.model = ? AND e.dimensions = ?
		  AND e.search_vector_norm >= 0
		  AND (? = '' OR f.agent = ?)
		ORDER BY upper_bound DESC, f.updated_at DESC`

type vectorSearchCandidate struct {
	id        string
	updatedAt time.Time
	score     float64
	vector    []float64
}

type vectorSearchCandidateHeap []vectorSearchCandidate

func (h vectorSearchCandidateHeap) Len() int { return len(h) }
func (h vectorSearchCandidateHeap) Less(i, j int) bool {
	return vectorSearchCandidateWorse(h[i], h[j])
}
func (h vectorSearchCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *vectorSearchCandidateHeap) Push(x any) {
	*h = append(*h, x.(vectorSearchCandidate))
}
func (h *vectorSearchCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func buildVectorSearchBatchSQL(ids []string) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	return fmt.Sprintf(`
		SELECT e.fragment_id,
		       f.updated_at,
		       e.vector
		FROM memory_embeddings e
		JOIN memory_fragments f ON f.id = e.fragment_id
		WHERE e.provider = ? AND e.model = ? AND e.dimensions = ?
		  AND e.fragment_id IN (%s)`, placeholders)
}

func (s *Store) scoreVectorSearchBatch(ctx context.Context, provider, model string, dimensions int, queryVec []float64, ids []string, limit int, top *vectorSearchCandidateHeap) error {
	if len(ids) == 0 {
		return nil
	}

	query := buildVectorSearchBatchSQL(ids)
	args := make([]any, 0, len(ids)+3)
	args = append(args, provider, model, dimensions)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query exact vector search batch: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c vectorSearchCandidate
		var payload sql.RawBytes
		if err := rows.Scan(&c.id, &c.updatedAt, &payload); err != nil {
			return fmt.Errorf("scan exact vector search row: %w", err)
		}

		score, err := cosineSimilarityPayload(queryVec, payload)
		if err != nil {
			return fmt.Errorf("score stored vector for fragment %s: %w", c.id, err)
		}
		c.score = score

		if top.Len() < limit {
			c.vector, err = decodeEmbeddingVector(payload)
			if err != nil {
				return fmt.Errorf("decode stored vector for fragment %s: %w", c.id, err)
			}
			heap.Push(top, c)
			continue
		}

		if vectorSearchCandidateBetter(c, (*top)[0]) {
			c.vector, err = decodeEmbeddingVector(payload)
			if err != nil {
				return fmt.Errorf("decode stored vector for fragment %s: %w", c.id, err)
			}
			(*top)[0] = c
			heap.Fix(top, 0)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// VectorSearch performs an exact cosine similarity search while using SQL-side
// prefix/tail upper bounds to avoid loading every embedding blob into Go.
func (s *Store) VectorSearch(ctx context.Context, agent, provider, model string, queryVec []float64, limit int) ([]ScoredFragment, error) {
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("query vector cannot be empty")
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	agent = strings.TrimSpace(agent)
	if provider == "" || model == "" {
		return nil, fmt.Errorf("provider and model are required")
	}
	if limit <= 0 {
		limit = 24
	}

	if err := s.backfillVectorSearchColumns(ctx, provider, model, len(queryVec)); err != nil {
		return nil, err
	}

	querySummary := summarizeEmbeddingSearchVector(queryVec)
	if querySummary.vectorNorm > 0 {
		for i := range querySummary.prefix {
			querySummary.prefix[i] /= querySummary.vectorNorm
		}
		querySummary.tailNorm /= querySummary.vectorNorm
	}
	rows, err := s.db.QueryContext(ctx, vectorSearchSQL,
		querySummary.prefix[0],
		querySummary.prefix[1],
		querySummary.prefix[2],
		querySummary.prefix[3],
		querySummary.prefix[4],
		querySummary.prefix[5],
		querySummary.prefix[6],
		querySummary.prefix[7],
		querySummary.tailNorm,
		provider,
		model,
		len(queryVec),
		agent,
		agent,
	)
	if err != nil {
		return nil, fmt.Errorf("vector search query: %w", err)
	}
	defer rows.Close()

	top := make(vectorSearchCandidateHeap, 0, limit)
	pending := make([]string, 0, vectorSearchExactBatchSize)
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := s.scoreVectorSearchBatch(ctx, provider, model, len(queryVec), queryVec, pending, limit, &top); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for rows.Next() {
		var id string
		var upperBound float64
		if err := rows.Scan(&id, &upperBound); err != nil {
			return nil, fmt.Errorf("scan vector search candidate: %w", err)
		}
		if top.Len() >= limit && upperBound+vectorSearchUpperBoundEps < top[0].score {
			break
		}

		pending = append(pending, id)
		if len(pending) >= vectorSearchExactBatchSize {
			if err := flushPending(); err != nil {
				return nil, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := flushPending(); err != nil {
		return nil, err
	}
	if len(top) == 0 {
		return []ScoredFragment{}, nil
	}

	sort.Slice(top, func(i, j int) bool {
		return vectorSearchCandidateBetter(top[i], top[j])
	})

	ids := make([]string, 0, len(top))
	for _, c := range top {
		ids = append(ids, c.id)
	}
	fragments, err := s.getFragmentsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]ScoredFragment, 0, len(top))
	for _, c := range top {
		r, ok := fragments[c.id]
		if !ok {
			continue
		}
		r.Score = c.score
		r.Vector = c.vector
		out = append(out, r)
	}
	return out, nil
}

func vectorSearchCandidateBetter(a, b vectorSearchCandidate) bool {
	if a.score == b.score {
		return a.updatedAt.After(b.updatedAt)
	}
	return a.score > b.score
}

func vectorSearchCandidateWorse(a, b vectorSearchCandidate) bool {
	if a.score == b.score {
		return a.updatedAt.Before(b.updatedAt)
	}
	return a.score < b.score
}

const fragmentByIDBatchSize = 500

func (s *Store) getFragmentsByIDs(ctx context.Context, ids []string) (map[string]ScoredFragment, error) {
	out := make(map[string]ScoredFragment, len(ids))
	for start := 0; start < len(ids); start += fragmentByIDBatchSize {
		end := start + fragmentByIDBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := s.getFragmentsByIDBatch(ctx, ids[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) getFragmentsByIDBatch(ctx context.Context, ids []string, out map[string]ScoredFragment) error {
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.Repeat("?,", len(ids))
	placeholders = strings.TrimSuffix(placeholders, ",")
	query := fmt.Sprintf(`
		SELECT id, agent, path, content, source, created_at, updated_at,
		       accessed_at, access_count, decay_score, pinned
		FROM memory_fragments
		WHERE id IN (%s)`, placeholders)

	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("get fragments by ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r ScoredFragment
		var accessedAt sql.NullTime
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
		); err != nil {
			return fmt.Errorf("scan fragment by id: %w", err)
		}
		if accessedAt.Valid {
			at := accessedAt.Time
			r.AccessedAt = &at
		}
		out[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// BumpAccess marks a fragment as recently accessed and increments access_count.
// It intentionally does not modify decay_score; recency/freshness is applied as
// an explicit, non-persistent search-time option.
func (s *Store) BumpAccess(ctx context.Context, fragmentID string) error {
	fragmentID = strings.TrimSpace(fragmentID)
	if fragmentID == "" {
		return fmt.Errorf("fragment_id is required")
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE memory_fragments
		SET accessed_at = ?,
		    access_count = access_count + 1
		WHERE id = ?`, time.Now(), fragmentID)
	if err != nil {
		return fmt.Errorf("bump fragment access: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("fragment %s not found", fragmentID)
	}
	return nil
}

const (
	DefaultDecayHalfLifeDays = 30.0
	MinDecayScore            = 0.04
	DefaultDecayGCThreshold  = 0.05
)

// DecayPreview is a non-persistent age-based decay calculation for a fragment.
type DecayPreview struct {
	ID           string
	Agent        string
	Path         string
	UpdatedAt    time.Time
	AccessedAt   *time.Time
	CurrentScore float64
	PreviewScore float64
}

// ComputeDecayScore returns the age-based decay score that would apply for the
// supplied timestamps. It is pure: callers decide whether to use it for ranking,
// reporting, or persistence.
func ComputeDecayScore(updatedAt time.Time, accessedAt *time.Time, halfLifeDays float64, now time.Time) float64 {
	if halfLifeDays <= 0 {
		halfLifeDays = DefaultDecayHalfLifeDays
	}
	if now.IsZero() {
		now = time.Now()
	}

	lastActive := updatedAt
	if accessedAt != nil && accessedAt.After(lastActive) {
		lastActive = *accessedAt
	}
	if lastActive.IsZero() || lastActive.After(now) {
		return 1.0
	}

	ageDays := now.Sub(lastActive).Hours() / 24.0
	if ageDays < 0 {
		ageDays = 0
	}
	decay := math.Pow(0.5, ageDays/halfLifeDays)
	return math.Max(decay, MinDecayScore)
}

// PreviewDecayScores calculates age-based decay scores without writing them to
// the database. Results are sorted from lowest preview score to highest.
func (s *Store) PreviewDecayScores(ctx context.Context, agent string, halfLifeDays float64, limit int) ([]DecayPreview, error) {
	agent = strings.TrimSpace(agent)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, updated_at, accessed_at, decay_score
		FROM memory_fragments
		WHERE pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent)
	if err != nil {
		return nil, fmt.Errorf("query fragments for decay preview: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	out := []DecayPreview{}
	for rows.Next() {
		var p DecayPreview
		var accessedAt sql.NullTime
		if err := rows.Scan(&p.ID, &p.Agent, &p.Path, &p.UpdatedAt, &accessedAt, &p.CurrentScore); err != nil {
			return nil, fmt.Errorf("scan fragment for decay preview: %w", err)
		}
		if accessedAt.Valid {
			at := accessedAt.Time
			p.AccessedAt = &at
		}
		p.PreviewScore = ComputeDecayScore(p.UpdatedAt, p.AccessedAt, halfLifeDays, now)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fragments for decay preview: %w", err)
	}

	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].PreviewScore-out[j].PreviewScore) < 1e-12 {
			if out[i].Agent == out[j].Agent {
				return out[i].Path < out[j].Path
			}
			return out[i].Agent < out[j].Agent
		}
		return out[i].PreviewScore < out[j].PreviewScore
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CountDecayCandidates counts fragments that would fall below threshold if
// age-based decay were applied, without mutating decay_score.
func (s *Store) CountDecayCandidates(ctx context.Context, agent string, halfLifeDays, threshold float64) (int, error) {
	if threshold <= 0 {
		threshold = DefaultDecayGCThreshold
	}
	previews, err := s.PreviewDecayScores(ctx, agent, halfLifeDays, 0)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, preview := range previews {
		if preview.PreviewScore < threshold {
			count++
		}
	}
	return count, nil
}

// RecalcDecayScores recalculates decay_score for non-pinned fragments.
// halfLifeDays defaults to 30 when <= 0.
// Deprecated: prefer PreviewDecayScores or ComputeDecayScore; routine memory
// maintenance should not rewrite decay scores.
func (s *Store) RecalcDecayScores(ctx context.Context, agent string, halfLifeDays float64) (int, error) {
	agent = strings.TrimSpace(agent)
	if halfLifeDays <= 0 {
		halfLifeDays = DefaultDecayHalfLifeDays
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin decay recalculation transaction: %w", err)
	}
	defer tx.Rollback()

	type decayUpdate struct {
		id    string
		score float64
	}
	updates := []decayUpdate{}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, updated_at, accessed_at
		FROM memory_fragments
		WHERE pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent)
	if err != nil {
		return 0, fmt.Errorf("query fragments for decay recalculation: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	for rows.Next() {
		var id string
		var updatedAt time.Time
		var accessedAt sql.NullTime
		if err := rows.Scan(&id, &updatedAt, &accessedAt); err != nil {
			return 0, fmt.Errorf("scan fragment for decay recalculation: %w", err)
		}

		var accessedAtPtr *time.Time
		if accessedAt.Valid {
			at := accessedAt.Time
			accessedAtPtr = &at
		}
		finalDecay := ComputeDecayScore(updatedAt, accessedAtPtr, halfLifeDays, now)
		updates = append(updates, decayUpdate{id: id, score: finalDecay})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate fragments for decay recalculation: %w", err)
	}

	if len(updates) == 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit decay recalculation: %w", err)
		}
		return 0, nil
	}

	stmt, err := tx.PrepareContext(ctx, `UPDATE memory_fragments SET decay_score = ? WHERE id = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare decay recalculation update: %w", err)
	}
	defer stmt.Close()

	updatedCount := 0
	for _, update := range updates {
		res, err := stmt.ExecContext(ctx, update.score, update.id)
		if err != nil {
			return 0, fmt.Errorf("update decay score for fragment %s: %w", update.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updatedCount += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit decay recalculation: %w", err)
	}
	return updatedCount, nil
}

// CountGCCandidates counts fragments eligible for GC.
func (s *Store) CountGCCandidates(ctx context.Context, agent string) (int, error) {
	agent = strings.TrimSpace(agent)

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM memory_fragments
		WHERE decay_score < 0.05
		  AND pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count gc candidates: %w", err)
	}
	return count, nil
}

// GCFragments deletes decayed, non-pinned fragments and keeps FTS in sync.
func (s *Store) GCFragments(ctx context.Context, agent string) (int, error) {
	agent = strings.TrimSpace(agent)

	type gcCandidate struct {
		rowID   int64
		id      string
		agent   string
		path    string
		content string
	}
	candidates := []gcCandidate{}

	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, id, agent, path, content
		FROM memory_fragments
		WHERE decay_score < 0.05
		  AND pinned = 0
		  AND (? = '' OR agent = ?)`, agent, agent)
	if err != nil {
		return 0, fmt.Errorf("query gc candidates: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c gcCandidate
		if err := rows.Scan(&c.rowID, &c.id, &c.agent, &c.path, &c.content); err != nil {
			return 0, fmt.Errorf("scan gc candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate gc candidates: %w", err)
	}

	if len(candidates) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin gc transaction: %w", err)
	}
	defer tx.Rollback()

	removed := 0
	for _, c := range candidates {
		frag := &Fragment{ID: c.id, Agent: c.agent, Path: c.path, Content: c.content}
		if err := syncFTSDelete(ctx, tx, c.rowID, frag); err != nil {
			return 0, fmt.Errorf("sync fts delete during gc for fragment %s: %w", c.id, err)
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM memory_fragments WHERE rowid = ?`, c.rowID)
		if err != nil {
			return 0, fmt.Errorf("delete fragment during gc %s: %w", c.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit gc fragments: %w", err)
	}
	return removed, nil
}

// GetMeta returns a value from memory_meta. Missing keys return "", nil.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("meta key is required")
	}

	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM memory_meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get meta value: %w", err)
	}
	return value, nil
}

// SetMeta upserts a key/value pair in memory_meta.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("meta key is required")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_meta(key, value)
		VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set meta value: %w", err)
	}
	return nil
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

// InsightMinedAt returns the time a session's insights were last extracted,
// or (time.Time{}, nil) if the session has never been insight-mined.
func (s *Store) InsightMinedAt(ctx context.Context, sessionID string) (time.Time, error) {
	var raw string
	err := s.db.QueryRowContext(ctx,
		`SELECT mined_at FROM memory_insight_mining_state WHERE session_id = ?`,
		sessionID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := parseFlexibleTime(raw)
	return t, nil
}

// MarkInsightMined records that insight extraction has run for a session.
func (s *Store) MarkInsightMined(ctx context.Context, sessionID, agent string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_insight_mining_state(session_id, agent, mined_at)
		VALUES(?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			agent    = excluded.agent,
			mined_at = excluded.mined_at`,
		sessionID, agent, time.Now().UTC().Format(time.RFC3339),
	)
	return err
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
//
// SQLite aggregate functions like MAX() return the underlying value as a plain
// string, bypassing the driver's column-type inference that makes direct column
// scans into time.Time work. We therefore scan as a string and parse manually,
// accepting both RFC3339 (current format written by UpsertState) and the legacy
// Go time.String() format that older rows may contain.
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
		var minedAtStr string
		if err := rows.Scan(&agent, &minedAtStr); err != nil {
			return nil, fmt.Errorf("scan last mined: %w", err)
		}
		t, err := parseFlexibleTime(minedAtStr)
		if err != nil {
			// Unparseable timestamp: skip rather than hard-fail.
			continue
		}
		out[agent] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseFlexibleTime parses a time string in either RFC3339 (the current
// UpsertState format) or the legacy Go time.String() layout that older rows
// may contain (e.g. "2006-01-02 15:04:05.999999999 -0700 MST m=+0.000").
func parseFlexibleTime(s string) (time.Time, error) {
	// RFC3339 / RFC3339Nano — written by current UpsertState.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Go time.String() layout, with or without the monotonic "m=+…" suffix.
	// Strip the suffix first so the layout is fixed-width.
	cleaned := s
	if idx := strings.Index(s, " m="); idx != -1 {
		cleaned = s[:idx]
	}
	const goTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"
	if t, err := time.Parse(goTimeLayout, cleaned); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time format: %q", s)
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

// ── Fragment sources (L1→L2 backpointers) ────────────────────────────────────

// FragmentSource records a link between a memory fragment (L1) and the raw
// session turn range (L2) that the fragment was mined from.  One fragment can
// accumulate many sources over time as the miner revisits sessions.
type FragmentSource struct {
	ID        int64
	Agent     string
	Path      string
	SessionID string
	TurnStart int
	TurnEnd   int
	CreatedAt time.Time
}

// AddFragmentSource records that the fragment at (agent, path) was derived from
// messages [turnStart, turnEnd) of sessionID.  Duplicate rows (same agent,
// path, session, and turn range) are silently ignored.
func (s *Store) AddFragmentSource(ctx context.Context, agent, path, sessionID string, turnStart, turnEnd int) error {
	agent = strings.TrimSpace(agent)
	path = strings.TrimSpace(path)
	sessionID = strings.TrimSpace(sessionID)
	if agent == "" || path == "" || sessionID == "" {
		return fmt.Errorf("agent, path, and session_id are required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_fragment_sources (agent, path, session_id, turn_start, turn_end)
		SELECT ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_fragment_sources
			WHERE agent = ? AND path = ? AND session_id = ? AND turn_start = ? AND turn_end = ?
		)`,
		agent, path, sessionID, turnStart, turnEnd,
		agent, path, sessionID, turnStart, turnEnd,
	)
	if err != nil {
		return fmt.Errorf("add fragment source: %w", err)
	}
	return nil
}

// GetFragmentSources returns all source records for a given (agent, path)
// fragment, ordered oldest first.
func (s *Store) GetFragmentSources(ctx context.Context, agent, path string) ([]FragmentSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, session_id, turn_start, turn_end, created_at
		FROM memory_fragment_sources
		WHERE agent = ? AND path = ?
		ORDER BY created_at ASC, id ASC`, agent, path)
	if err != nil {
		return nil, fmt.Errorf("get fragment sources: %w", err)
	}
	defer rows.Close()
	return scanFragmentSources(rows)
}

// GetSourcesForSession returns all fragment sources that were derived from a
// given session, ordered by path.  Useful for auditing what a session produced.
func (s *Store) GetSourcesForSession(ctx context.Context, sessionID string) ([]FragmentSource, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, agent, path, session_id, turn_start, turn_end, created_at
		FROM memory_fragment_sources
		WHERE session_id = ?
		ORDER BY path ASC, turn_start ASC, created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get sources for session: %w", err)
	}
	defer rows.Close()
	return scanFragmentSources(rows)
}

// deleteFragmentSources removes all source records for (agent, path).
// Called internally by DeleteFragment / DeleteFragmentByRowID.
func (s *Store) deleteFragmentSources(ctx context.Context, tx *sql.Tx, agent, path string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM memory_fragment_sources WHERE agent = ? AND path = ?`, agent, path)
	return err
}

func scanFragmentSources(rows *sql.Rows) ([]FragmentSource, error) {
	var out []FragmentSource
	for rows.Next() {
		var fs FragmentSource
		if err := rows.Scan(&fs.ID, &fs.Agent, &fs.Path, &fs.SessionID,
			&fs.TurnStart, &fs.TurnEnd, &fs.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan fragment source: %w", err)
		}
		out = append(out, fs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ── Image tracking ───────────────────────────────────────────────────────────

// ImageRecord is a record of a generated image.
type ImageRecord struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	SessionID  string    `json:"session_id"`
	Prompt     string    `json:"prompt"`
	OutputPath string    `json:"output_path"`
	MimeType   string    `json:"mime_type"`
	Provider   string    `json:"provider"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	FileSize   int       `json:"file_size"`
	CreatedAt  time.Time `json:"created_at"`
}

// ImageListOptions controls listing of generated images.
type ImageListOptions struct {
	Agent  string
	Limit  int
	Offset int
}

// RecordImage inserts a generated image record into the store.
// Non-fatal in callers: errors do not stop image generation.
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
