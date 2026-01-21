package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db  *sql.DB
	cfg Config
}

// Schema for the sessions database.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    name TEXT,
    summary TEXT,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    cwd TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived BOOLEAN DEFAULT FALSE,
    parent_id TEXT REFERENCES sessions(id),
    search BOOLEAN DEFAULT FALSE,
    tools TEXT,
    mcp TEXT
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    parts TEXT NOT NULL,
    text_content TEXT,
    duration_ms INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sequence INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, sequence);

-- Metadata table for current session tracking
CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT
);

-- Full-text search on extracted text content
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    text_content,
    content='messages',
    content_rowid='id'
);

-- Triggers to keep FTS in sync
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
    INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
END;
`

// NewSQLiteStore creates a new SQLite-based session store.
func NewSQLiteStore(cfg Config) (*SQLiteStore, error) {
	dbPath, err := GetDBPath()
	if err != nil {
		return nil, fmt.Errorf("get db path: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Initialize schema and run migrations
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}

	store := &SQLiteStore{db: db, cfg: cfg}

	// Run cleanup if configured
	if err := store.cleanup(); err != nil {
		// Log but don't fail
		fmt.Fprintf(os.Stderr, "warning: session cleanup failed: %v\n", err)
	}

	return store, nil
}

// schemaVersion is the current schema version.
// - Fresh databases get the full schema from `schema` const and start at this version
// - Existing databases run migrations to reach this version
// Increment when adding new migrations.
const schemaVersion = 1

// migration represents a schema migration.
type migration struct {
	version     int
	description string
	up          func(db *sql.DB) error
}

// migrations defines schema migrations for upgrading existing databases.
// The base `schema` const always contains the FULL current schema.
// Migrations are only needed for databases created before a schema change.
//
// To add a new migration:
// 1. Update the `schema` const with the new columns/tables
// 2. Increment schemaVersion
// 3. Add a migration that transforms old databases to match the new schema
var migrations = []migration{
	{
		// Migration 1: Add session settings columns
		// Only runs on databases created before these columns existed
		version:     1,
		description: "add session settings columns (search, tools, mcp)",
		up: func(db *sql.DB) error {
			alterStatements := []string{
				"ALTER TABLE sessions ADD COLUMN search BOOLEAN DEFAULT FALSE",
				"ALTER TABLE sessions ADD COLUMN tools TEXT",
				"ALTER TABLE sessions ADD COLUMN mcp TEXT",
			}
			for _, stmt := range alterStatements {
				if _, err := db.Exec(stmt); err != nil {
					if !isDuplicateColumnError(err) {
						return err
					}
				}
			}
			return nil
		},
	},
}

// initSchema initializes the database schema and runs any pending migrations.
// Optimized for the common case: schema already current = single SELECT query.
func initSchema(db *sql.DB) error {
	// Fast path: check if schema is already current
	var currentVersion int
	err := db.QueryRow("SELECT version FROM schema_version").Scan(&currentVersion)
	if err == nil && currentVersion >= schemaVersion {
		// Schema is current, nothing to do
		return nil
	}

	// Slow path: need to initialize or migrate
	return initSchemaFull(db, err, currentVersion)
}

// initSchemaFull handles schema creation and migrations.
// Only called when schema needs initialization or migration.
func initSchemaFull(db *sql.DB, versionErr error, currentVersion int) error {
	// Create base schema (uses IF NOT EXISTS, safe to run multiple times)
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create base schema: %w", err)
	}

	// Create schema_version table if it doesn't exist
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	// Determine current version if we didn't get it earlier
	// versionErr is non-nil if schema_version table doesn't exist or has no rows
	if versionErr != nil && (versionErr == sql.ErrNoRows || strings.Contains(versionErr.Error(), "no such table")) {
		// No version record - check if this is fresh DB or pre-migration DB
		var tableCount int
		err = db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name='sessions'
		`).Scan(&tableCount)
		if err != nil {
			return fmt.Errorf("check sessions table: %w", err)
		}

		if tableCount > 0 {
			// Pre-migration DB - start at version 0, will run all migrations
			currentVersion = 0
		} else {
			// Fresh DB - schema already has all columns, start at latest
			currentVersion = schemaVersion
		}

		// Insert initial version record
		if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", currentVersion); err != nil {
			return fmt.Errorf("insert initial version: %w", err)
		}
	} else if versionErr != nil {
		return fmt.Errorf("get current version: %w", versionErr)
	}

	// Run pending migrations
	for _, m := range migrations {
		if m.version > currentVersion {
			if err := m.up(db); err != nil {
				return fmt.Errorf("migration %d (%s): %w", m.version, m.description, err)
			}

			// Update version
			if _, err := db.Exec("UPDATE schema_version SET version = ?", m.version); err != nil {
				return fmt.Errorf("update version to %d: %w", m.version, err)
			}
		}
	}

	return nil
}

// isDuplicateColumnError checks if an error is due to a column already existing.
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "duplicate column") ||
		strings.Contains(errStr, "already exists")
}

// cleanup removes old sessions based on configuration.
func (s *SQLiteStore) cleanup() error {
	ctx := context.Background()

	// Delete old sessions
	if s.cfg.MaxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -s.cfg.MaxAgeDays)
		_, err := s.db.ExecContext(ctx,
			"DELETE FROM sessions WHERE updated_at < ? AND archived = FALSE",
			cutoff)
		if err != nil {
			return fmt.Errorf("delete old sessions: %w", err)
		}
	}

	// Keep only max_count sessions
	if s.cfg.MaxCount > 0 {
		_, err := s.db.ExecContext(ctx, `
			DELETE FROM sessions WHERE id IN (
				SELECT id FROM sessions
				WHERE archived = FALSE
				ORDER BY updated_at DESC
				LIMIT -1 OFFSET ?
			)`, s.cfg.MaxCount)
		if err != nil {
			return fmt.Errorf("enforce max count: %w", err)
		}
	}

	return nil
}

// Create inserts a new session.
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

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, name, summary, provider, model, cwd, created_at, updated_at, archived, parent_id, search, tools, mcp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Name, sess.Summary, sess.Provider, sess.Model, sess.CWD,
		sess.CreatedAt, sess.UpdatedAt, sess.Archived, nullString(sess.ParentID),
		sess.Search, nullString(sess.Tools), nullString(sess.MCP))
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// Get retrieves a session by ID.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, summary, provider, model, cwd, created_at, updated_at, archived, parent_id, search, tools, mcp
		FROM sessions WHERE id = ?`, id)

	var sess Session
	var parentID, tools, mcp sql.NullString
	err := row.Scan(&sess.ID, &sess.Name, &sess.Summary, &sess.Provider, &sess.Model,
		&sess.CWD, &sess.CreatedAt, &sess.UpdatedAt, &sess.Archived, &parentID,
		&sess.Search, &tools, &mcp)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
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
	return &sess, nil
}

// Update modifies an existing session.
func (s *SQLiteStore) Update(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now()
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET name = ?, summary = ?, provider = ?, model = ?, cwd = ?,
		       updated_at = ?, archived = ?, parent_id = ?, search = ?, tools = ?, mcp = ?
		WHERE id = ?`,
		sess.Name, sess.Summary, sess.Provider, sess.Model, sess.CWD,
		sess.UpdatedAt, sess.Archived, nullString(sess.ParentID),
		sess.Search, nullString(sess.Tools), nullString(sess.MCP), sess.ID)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found: %s", sess.ID)
	}
	return nil
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
func (s *SQLiteStore) List(ctx context.Context, opts ListOptions) ([]SessionSummary, error) {
	query := `
		SELECT s.id, s.name, s.summary, s.provider, s.model, s.created_at, s.updated_at,
		       (SELECT COUNT(*) FROM messages WHERE session_id = s.id) as message_count
		FROM sessions s
		WHERE 1=1`
	args := []any{}

	if opts.Provider != "" {
		query += " AND s.provider = ?"
		args = append(args, opts.Provider)
	}
	if opts.Model != "" {
		query += " AND s.model = ?"
		args = append(args, opts.Model)
	}
	if !opts.Archived {
		query += " AND s.archived = FALSE"
	}

	query += " ORDER BY s.updated_at DESC"

	limit := opts.Limit
	if limit == 0 {
		limit = 50 // Default
	}
	query += fmt.Sprintf(" LIMIT %d", limit)
	if opts.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", opts.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var sum SessionSummary
		err := rows.Scan(&sum.ID, &sum.Name, &sum.Summary, &sum.Provider, &sum.Model,
			&sum.CreatedAt, &sum.UpdatedAt, &sum.MessageCount)
		if err != nil {
			return nil, fmt.Errorf("scan session summary: %w", err)
		}
		results = append(results, sum)
	}
	return results, rows.Err()
}

// Search finds sessions containing the query text using FTS5.
func (s *SQLiteStore) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit == 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.session_id, m.id, s.name, s.summary, snippet(messages_fts, 0, '**', '**', '...', 32),
		       s.provider, s.model, m.created_at
		FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		JOIN sessions s ON s.id = m.session_id
		WHERE messages_fts MATCH ?
		ORDER BY rank
		LIMIT ?`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		err := rows.Scan(&r.SessionID, &r.MessageID, &r.SessionName, &r.Summary,
			&r.Snippet, &r.Provider, &r.Model, &r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// AddMessage adds a message to a session.
func (s *SQLiteStore) AddMessage(ctx context.Context, sessionID string, msg *Message) error {
	msg.SessionID = sessionID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	partsJSON, err := msg.PartsJSON()
	if err != nil {
		return fmt.Errorf("serialize parts: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, parts, text_content, duration_ms, created_at, sequence)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.CreatedAt, msg.Sequence)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}

	// Get the inserted ID
	id, _ := result.LastInsertId()
	msg.ID = id

	// Update session's updated_at
	_, err = s.db.ExecContext(ctx, "UPDATE sessions SET updated_at = ? WHERE id = ?",
		time.Now(), sessionID)
	if err != nil {
		return fmt.Errorf("update session timestamp: %w", err)
	}

	return nil
}

// GetMessages retrieves messages for a session.
func (s *SQLiteStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	query := `
		SELECT id, session_id, role, parts, text_content, duration_ms, created_at, sequence
		FROM messages
		WHERE session_id = ?
		ORDER BY sequence ASC`

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	if offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", offset)
	}

	rows, err := s.db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var partsJSON string
		var durationMs sql.NullInt64
		err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &partsJSON,
			&msg.TextContent, &durationMs, &msg.CreatedAt, &msg.Sequence)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if durationMs.Valid {
			msg.DurationMs = durationMs.Int64
		}
		if err := msg.SetPartsFromJSON(partsJSON); err != nil {
			return nil, fmt.Errorf("deserialize parts: %w", err)
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

// SetCurrent marks a session as the current one.
func (s *SQLiteStore) SetCurrent(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO metadata (key, value) VALUES ('current_session', ?)`,
		sessionID)
	return err
}

// GetCurrent retrieves the current session.
func (s *SQLiteStore) GetCurrent(ctx context.Context) (*Session, error) {
	var sessionID string
	err := s.db.QueryRowContext(ctx,
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

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// nullString converts an empty string to NULL for database storage.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
