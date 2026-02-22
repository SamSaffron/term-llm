package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
    number INTEGER,
    name TEXT,
    summary TEXT,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    mode TEXT DEFAULT 'chat',
    agent TEXT,
    cwd TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived BOOLEAN DEFAULT FALSE,
    parent_id TEXT REFERENCES sessions(id),
    search BOOLEAN DEFAULT FALSE,
    tools TEXT,
    mcp TEXT,
    user_turns INTEGER DEFAULT 0,
    llm_turns INTEGER DEFAULT 0,
    tool_calls INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    cached_input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    tags TEXT
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
CREATE INDEX IF NOT EXISTS idx_sessions_mode ON sessions(mode);
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
	dbPath, err := ResolveDBPath(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("get db path: %w", err)
	}

	// Ensure directory exists for file-backed databases.
	if dbPath != ":memory:" && !cfg.ReadOnly {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}

	// Configure SQLite for concurrent access:
	// - foreign_keys: Enforce referential integrity
	// - journal_mode(WAL): Write-Ahead Logging for better concurrency
	// - busy_timeout(5000): Wait up to 5 seconds when database is locked
	// - synchronous(NORMAL): Balanced durability/performance for WAL mode
	dsn := dbPath
	if cfg.ReadOnly && dbPath != ":memory:" {
		dsn = "file:" + filepath.ToSlash(dbPath) + "?mode=ro"
	}
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=mmap_size(134217728)&_pragma=cache_size(-64000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Initialize schema and run migrations.
	// Read-only mode skips initialization because it cannot write schema changes.
	if !cfg.ReadOnly {
		if err := initSchema(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize schema: %w", err)
		}
	}

	store := &SQLiteStore{db: db, cfg: cfg}

	// Run cleanup if configured (read-write mode only).
	if !cfg.ReadOnly {
		if err := store.cleanup(); err != nil {
			// Log but don't fail
			fmt.Fprintf(os.Stderr, "warning: session cleanup failed: %v\n", err)
		}
	}

	return store, nil
}

// schemaVersion is the current schema version.
// - Fresh databases get the full schema from `schema` const and start at this version
// - Existing databases run migrations to reach this version
// Increment when adding new migrations.
const schemaVersion = 7

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
	{
		// Migration 2: Add session metrics columns
		// Tracks user turns, LLM turns, tool calls, tokens, status, and tags
		version:     2,
		description: "add session metrics columns (user_turns, llm_turns, tool_calls, tokens, status, tags)",
		up: func(db *sql.DB) error {
			alterStatements := []string{
				"ALTER TABLE sessions ADD COLUMN user_turns INTEGER DEFAULT 0",
				"ALTER TABLE sessions ADD COLUMN llm_turns INTEGER DEFAULT 0",
				"ALTER TABLE sessions ADD COLUMN tool_calls INTEGER DEFAULT 0",
				"ALTER TABLE sessions ADD COLUMN input_tokens INTEGER DEFAULT 0",
				"ALTER TABLE sessions ADD COLUMN output_tokens INTEGER DEFAULT 0",
				"ALTER TABLE sessions ADD COLUMN status TEXT DEFAULT 'active'",
				"ALTER TABLE sessions ADD COLUMN tags TEXT",
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
	{
		// Migration 3: Add unique constraint on message sequences and status index
		// Fixes TOCTOU race condition in AddMessage by enforcing uniqueness at DB level.
		// Also adds index on sessions.status for query performance.
		version:     3,
		description: "add unique constraint on message sequences and status index",
		up: func(db *sql.DB) error {
			// First, fix any existing duplicate sequences within sessions.
			// Renumber messages by created_at order within each session.
			rows, err := db.Query(`
				SELECT DISTINCT session_id FROM messages
				WHERE session_id IN (
					SELECT session_id FROM messages
					GROUP BY session_id, sequence
					HAVING COUNT(*) > 1
				)
			`)
			if err != nil {
				return fmt.Errorf("find duplicate sequences: %w", err)
			}
			defer rows.Close()

			var sessionsToFix []string
			for rows.Next() {
				var sid string
				if err := rows.Scan(&sid); err != nil {
					return fmt.Errorf("scan session id: %w", err)
				}
				sessionsToFix = append(sessionsToFix, sid)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate sessions: %w", err)
			}

			// Renumber messages in each affected session
			for _, sid := range sessionsToFix {
				msgRows, err := db.Query(`
				SELECT id FROM messages
				WHERE session_id = ?
				ORDER BY created_at ASC, id ASC
			`, sid)
				if err != nil {
					return fmt.Errorf("get messages for session %s: %w", sid, err)
				}
				var msgIDs []int64
				for msgRows.Next() {
					var id int64
					if err := msgRows.Scan(&id); err != nil {
						msgRows.Close()
						return fmt.Errorf("scan message id: %w", err)
					}
					msgIDs = append(msgIDs, id)
				}
				msgRows.Close()
				if err := msgRows.Err(); err != nil {
					return fmt.Errorf("iterate messages: %w", err)
				}

				// Update sequences
				for seq, msgID := range msgIDs {
					if _, err := db.Exec(`UPDATE messages SET sequence = ? WHERE id = ?`, seq, msgID); err != nil {
						return fmt.Errorf("update message sequence: %w", err)
					}
				}
			}

			// Now add the unique index (also serves as the constraint)
			_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_sequence ON messages(session_id, sequence)`)
			if err != nil {
				return fmt.Errorf("create unique index: %w", err)
			}

			// Add index on sessions.status for query performance
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status)`)
			if err != nil {
				return fmt.Errorf("create status index: %w", err)
			}

			return nil
		},
	},
	{
		// Migration 4: Add session mode column
		// Distinguishes chat, ask, plan, and exec sessions
		version:     4,
		description: "add session mode column",
		up: func(db *sql.DB) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN mode TEXT DEFAULT 'chat'")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			// Add index for mode filtering
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_mode ON sessions(mode)`)
			if err != nil {
				return fmt.Errorf("create mode index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 5: Add agent column
		// Tracks which agent was used for the session
		version:     5,
		description: "add agent column",
		up: func(db *sql.DB) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN agent TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 6: Add sequential session numbers
		// Adds number column for simpler session identification (1, 2, 3...)
		version:     6,
		description: "add session number column",
		up: func(db *sql.DB) error {
			// Add number column
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN number INTEGER")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}

			// Create unique index on number
			_, err = db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_number ON sessions(number)")
			if err != nil {
				return fmt.Errorf("create number index: %w", err)
			}

			// Backfill existing sessions with sequential numbers ordered by created_at
			_, err = db.Exec(`
				WITH numbered AS (
					SELECT id, ROW_NUMBER() OVER (ORDER BY created_at ASC) as num
					FROM sessions
				)
				UPDATE sessions SET number = (
					SELECT num FROM numbered WHERE numbered.id = sessions.id
				)
			`)
			if err != nil {
				return fmt.Errorf("backfill session numbers: %w", err)
			}

			return nil
		},
	},
	{
		// Migration 7: Add cached input token metrics
		// Tracks prompt-cache token reads for cumulative session stats
		version:     7,
		description: "add cached_input_tokens metric column",
		up: func(db *sql.DB) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN cached_input_tokens INTEGER DEFAULT 0")
			if err != nil && !isDuplicateColumnError(err) {
				return err
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

	// Ensure indexes exist (handles fresh DBs where migrations don't run)
	_, err = db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_number ON sessions(number)")
	if err != nil {
		return fmt.Errorf("ensure number index: %w", err)
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
	if sess.Status == "" {
		sess.Status = StatusActive
	}
	if sess.Mode == "" {
		sess.Mode = ModeChat
	}

	err := retryOnBusy(ctx, 5, func() error {
		// Use a single INSERT statement with a subquery to atomically assign the
		// next session number. This avoids race conditions where two concurrent
		// Creates could read the same MAX(number).
		result, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (id, number, name, summary, provider, model, mode, agent, cwd, created_at, updated_at, archived, parent_id, search, tools, mcp,
			                      user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens, output_tokens, status, tags)
			VALUES (?, (SELECT COALESCE(MAX(number), 0) + 1 FROM sessions), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, sess.Name, sess.Summary, sess.Provider, sess.Model, string(sess.Mode), nullString(sess.Agent), sess.CWD,
			sess.CreatedAt, sess.UpdatedAt, sess.Archived, nullString(sess.ParentID),
			sess.Search, nullString(sess.Tools), nullString(sess.MCP),
			sess.UserTurns, sess.LLMTurns, sess.ToolCalls, sess.InputTokens, sess.CachedInputTokens, sess.OutputTokens,
			string(sess.Status), nullString(sess.Tags))
		if err != nil {
			return fmt.Errorf("insert session: %w", err)
		}

		// Fetch the assigned number
		rows, _ := result.RowsAffected()
		if rows == 0 {
			return fmt.Errorf("no rows inserted")
		}

		// Query the assigned number back
		err = s.db.QueryRowContext(ctx, "SELECT number FROM sessions WHERE id = ?", sess.ID).Scan(&sess.Number)
		if err != nil {
			return fmt.Errorf("get assigned number: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Get retrieves a session by ID.
func (s *SQLiteStore) Get(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, number, name, summary, provider, model, mode, agent, cwd, created_at, updated_at, archived, parent_id, search, tools, mcp,
		       user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens, output_tokens, status, tags
		FROM sessions WHERE id = ?`, id)

	var sess Session
	var number sql.NullInt64
	var mode, agent, parentID, tools, mcp, status, tags sql.NullString
	err := row.Scan(&sess.ID, &number, &sess.Name, &sess.Summary, &sess.Provider, &sess.Model, &mode,
		&agent, &sess.CWD, &sess.CreatedAt, &sess.UpdatedAt, &sess.Archived, &parentID,
		&sess.Search, &tools, &mcp,
		&sess.UserTurns, &sess.LLMTurns, &sess.ToolCalls, &sess.InputTokens, &sess.CachedInputTokens, &sess.OutputTokens,
		&status, &tags)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	if number.Valid {
		sess.Number = number.Int64
	}
	if mode.Valid {
		sess.Mode = SessionMode(mode.String)
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
	return &sess, nil
}

// GetByNumber retrieves a session by its sequential number.
func (s *SQLiteStore) GetByNumber(ctx context.Context, number int64) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, number, name, summary, provider, model, mode, agent, cwd, created_at, updated_at, archived, parent_id, search, tools, mcp,
		       user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens, output_tokens, status, tags
		FROM sessions WHERE number = ?`, number)

	var sess Session
	var num sql.NullInt64
	var mode, agent, parentID, tools, mcp, status, tags sql.NullString
	err := row.Scan(&sess.ID, &num, &sess.Name, &sess.Summary, &sess.Provider, &sess.Model, &mode,
		&agent, &sess.CWD, &sess.CreatedAt, &sess.UpdatedAt, &sess.Archived, &parentID,
		&sess.Search, &tools, &mcp,
		&sess.UserTurns, &sess.LLMTurns, &sess.ToolCalls, &sess.InputTokens, &sess.CachedInputTokens, &sess.OutputTokens,
		&status, &tags)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	if num.Valid {
		sess.Number = num.Int64
	}
	if mode.Valid {
		sess.Mode = SessionMode(mode.String)
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
	return &sess, nil
}

// GetByPrefix retrieves a session by number (with # prefix), exact ID, or by short ID prefix match.
// It tries in order: #number (e.g., #42), exact ID match, short ID prefix match.
func (s *SQLiteStore) GetByPrefix(ctx context.Context, prefix string) (*Session, error) {
	// Check for #number format (e.g., "#42" or "42" after stripping #)
	if strings.HasPrefix(prefix, "#") {
		numStr := strings.TrimPrefix(prefix, "#")
		if num, err := strconv.ParseInt(numStr, 10, 64); err == nil {
			sess, err := s.GetByNumber(ctx, num)
			if err != nil {
				return nil, err
			}
			if sess != nil {
				return sess, nil
			}
		}
	}

	// Also support plain numbers for convenience (but only if it's purely numeric)
	// This maintains backward compatibility while preferring # prefix
	if num, err := strconv.ParseInt(prefix, 10, 64); err == nil {
		sess, err := s.GetByNumber(ctx, num)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess, nil
		}
		// If no session found by number, fall through to ID matching
		// (in case someone has numeric-prefixed IDs)
	}

	// Try exact ID match
	sess, err := s.Get(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		return sess, nil
	}

	// Try prefix match using expanded short ID
	pattern := ExpandShortID(prefix)
	row := s.db.QueryRowContext(ctx, `
		SELECT id, number, name, summary, provider, model, mode, agent, cwd, created_at, updated_at, archived, parent_id, search, tools, mcp,
		       user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens, output_tokens, status, tags
		FROM sessions WHERE id LIKE ? ORDER BY created_at DESC LIMIT 1`, pattern)

	var prefixSess Session
	var number sql.NullInt64
	var mode, agent, parentID, tools, mcp, status, tags sql.NullString
	err = row.Scan(&prefixSess.ID, &number, &prefixSess.Name, &prefixSess.Summary, &prefixSess.Provider, &prefixSess.Model, &mode,
		&agent, &prefixSess.CWD, &prefixSess.CreatedAt, &prefixSess.UpdatedAt, &prefixSess.Archived, &parentID,
		&prefixSess.Search, &tools, &mcp,
		&prefixSess.UserTurns, &prefixSess.LLMTurns, &prefixSess.ToolCalls, &prefixSess.InputTokens, &prefixSess.CachedInputTokens, &prefixSess.OutputTokens,
		&status, &tags)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	if number.Valid {
		prefixSess.Number = number.Int64
	}
	if mode.Valid {
		prefixSess.Mode = SessionMode(mode.String)
	}
	if agent.Valid {
		prefixSess.Agent = agent.String
	}
	if parentID.Valid {
		prefixSess.ParentID = parentID.String
	}
	if tools.Valid {
		prefixSess.Tools = tools.String
	}
	if mcp.Valid {
		prefixSess.MCP = mcp.String
	}
	if status.Valid {
		prefixSess.Status = SessionStatus(status.String)
	}
	if tags.Valid {
		prefixSess.Tags = tags.String
	}
	return &prefixSess, nil
}

// Update modifies an existing session.
func (s *SQLiteStore) Update(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now()
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET name = ?, summary = ?, provider = ?, model = ?, mode = ?, agent = ?, cwd = ?,
		       updated_at = ?, archived = ?, parent_id = ?, search = ?, tools = ?, mcp = ?,
		       user_turns = ?, llm_turns = ?, tool_calls = ?, input_tokens = ?, cached_input_tokens = ?, output_tokens = ?,
		       status = ?, tags = ?
		WHERE id = ?`,
		sess.Name, sess.Summary, sess.Provider, sess.Model, string(sess.Mode), nullString(sess.Agent), sess.CWD,
		sess.UpdatedAt, sess.Archived, nullString(sess.ParentID),
		sess.Search, nullString(sess.Tools), nullString(sess.MCP),
		sess.UserTurns, sess.LLMTurns, sess.ToolCalls, sess.InputTokens, sess.CachedInputTokens, sess.OutputTokens,
		string(sess.Status), nullString(sess.Tags), sess.ID)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found: %s", sess.ID)
	}
	return nil
}

// UpdateMetrics updates just the metrics fields (used for incremental saves).
func (s *SQLiteStore) UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens int) error {
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET
			       llm_turns = llm_turns + ?,
			       tool_calls = tool_calls + ?,
			       input_tokens = input_tokens + ?,
			       cached_input_tokens = cached_input_tokens + ?,
			       output_tokens = output_tokens + ?,
			       updated_at = ?
			WHERE id = ?`,
			llmTurns, toolCalls, inputTokens, cachedInputTokens, outputTokens, time.Now(), id)
		return err
	})
}

// UpdateStatus updates just the session status.
func (s *SQLiteStore) UpdateStatus(ctx context.Context, id string, status SessionStatus) error {
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET status = ?, updated_at = ?
			WHERE id = ?`,
			string(status), time.Now(), id)
		return err
	})
}

// IncrementUserTurns increments the user turn count.
func (s *SQLiteStore) IncrementUserTurns(ctx context.Context, id string) error {
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET user_turns = user_turns + 1, updated_at = ?
			WHERE id = ?`,
			time.Now(), id)
		return err
	})
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
		SELECT s.id, s.number, s.name, s.summary, s.provider, s.model, s.mode, s.created_at, s.updated_at,
		       (SELECT COUNT(*) FROM messages WHERE session_id = s.id) as message_count,
		       s.user_turns, s.llm_turns, s.tool_calls, s.input_tokens, s.cached_input_tokens, s.output_tokens, s.status, s.tags
		FROM sessions s
		WHERE 1=1`
	args := []any{}

	if opts.Name != "" {
		query += " AND s.name = ?"
		args = append(args, opts.Name)
	}
	if opts.Provider != "" {
		query += " AND s.provider = ?"
		args = append(args, opts.Provider)
	}
	if opts.Model != "" {
		query += " AND s.model = ?"
		args = append(args, opts.Model)
	}
	if opts.Mode != "" {
		query += " AND s.mode = ?"
		args = append(args, string(opts.Mode))
	}
	if opts.Status != "" {
		query += " AND s.status = ?"
		args = append(args, string(opts.Status))
	}
	if opts.Tag != "" {
		// Substring match on comma-separated tags
		query += " AND (',' || s.tags || ',' LIKE '%,' || ? || ',%')"
		args = append(args, opts.Tag)
	}
	if !opts.Archived {
		query += " AND s.archived = FALSE"
	}

	query += " ORDER BY s.updated_at DESC"

	limit := opts.Limit
	if limit == 0 {
		limit = 50 // Default
	}
	query += " LIMIT ?"
	args = append(args, limit)
	if opts.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, opts.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var results []SessionSummary
	for rows.Next() {
		var sum SessionSummary
		var number sql.NullInt64
		var mode, status, tags sql.NullString
		err := rows.Scan(&sum.ID, &number, &sum.Name, &sum.Summary, &sum.Provider, &sum.Model, &mode,
			&sum.CreatedAt, &sum.UpdatedAt, &sum.MessageCount,
			&sum.UserTurns, &sum.LLMTurns, &sum.ToolCalls, &sum.InputTokens, &sum.CachedInputTokens, &sum.OutputTokens,
			&status, &tags)
		if err != nil {
			return nil, fmt.Errorf("scan session summary: %w", err)
		}
		if number.Valid {
			sum.Number = number.Int64
		}
		if mode.Valid {
			sum.Mode = SessionMode(mode.String)
		}
		if status.Valid {
			sum.Status = SessionStatus(status.String)
		}
		if tags.Valid {
			sum.Tags = tags.String
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
		SELECT m.session_id, s.number, m.id, s.name, s.summary, snippet(messages_fts, 0, '**', '**', '...', 32),
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
		var number sql.NullInt64
		err := rows.Scan(&r.SessionID, &number, &r.MessageID, &r.SessionName, &r.Summary,
			&r.Snippet, &r.Provider, &r.Model, &r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		if number.Valid {
			r.SessionNumber = number.Int64
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// AddMessage adds a message to a session.
// If msg.Sequence < 0, the sequence number is auto-allocated atomically.
func (s *SQLiteStore) AddMessage(ctx context.Context, sessionID string, msg *Message) error {
	msg.SessionID = sessionID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	partsJSON, err := msg.PartsJSON()
	if err != nil {
		return fmt.Errorf("serialize parts: %w", err)
	}

	// Track whether we need to auto-allocate sequence
	autoSequence := msg.Sequence < 0

	// Retry the entire transaction on SQLITE_BUSY
	return retryOnBusy(ctx, 5, func() error {
		// Use transaction for atomic sequence allocation
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		// Auto-allocate sequence if not specified (Sequence < 0)
		if autoSequence {
			var maxSeq sql.NullInt64
			err = tx.QueryRowContext(ctx,
				`SELECT MAX(sequence) FROM messages WHERE session_id = ?`,
				sessionID).Scan(&maxSeq)
			if err != nil {
				return fmt.Errorf("get max sequence: %w", err)
			}
			if maxSeq.Valid {
				msg.Sequence = int(maxSeq.Int64) + 1
			} else {
				msg.Sequence = 0
			}
		}

		result, err := tx.ExecContext(ctx, `
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
		_, err = tx.ExecContext(ctx, "UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now(), sessionID)
		if err != nil {
			return fmt.Errorf("update session timestamp: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}

		return nil
	})
}

// ReplaceMessages deletes all existing messages for the session and inserts
// the new set in a single transaction. Used after context compaction.
func (s *SQLiteStore) ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error {
	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		// Delete all existing messages for this session
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return fmt.Errorf("delete existing messages: %w", err)
		}

		// Insert new messages with sequential sequence numbers
		for i, msg := range messages {
			msg.SessionID = sessionID
			msg.Sequence = i
			if msg.CreatedAt.IsZero() {
				msg.CreatedAt = time.Now()
			}

			partsJSON, err := msg.PartsJSON()
			if err != nil {
				return fmt.Errorf("serialize parts for message %d: %w", i, err)
			}

			_, err = tx.ExecContext(ctx, `
				INSERT INTO messages (session_id, role, parts, text_content, duration_ms, created_at, sequence)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.CreatedAt, msg.Sequence)
			if err != nil {
				return fmt.Errorf("insert message %d: %w", i, err)
			}
		}

		// Update session's updated_at
		if _, err := tx.ExecContext(ctx, "UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now(), sessionID); err != nil {
			return fmt.Errorf("update session timestamp: %w", err)
		}

		return tx.Commit()
	})
}

// GetMessages retrieves messages for a session.
func (s *SQLiteStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	query := `
		SELECT id, session_id, role, parts, text_content, duration_ms, created_at, sequence
		FROM messages
		WHERE session_id = ?
		ORDER BY sequence ASC`

	args := []any{sessionID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
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
