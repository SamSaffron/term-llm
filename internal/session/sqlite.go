package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/sqlitefts"
	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db                       *sql.DB
	cfg                      Config
	hasGeneratedTitles       bool // true if sessions table has generated title columns
	hasCompactionSeq         bool // true if sessions table has compaction_seq column
	hasCompactionCount       bool // true if sessions table has compaction_count column
	hasCacheWriteTokens      bool // true if sessions table has cache_write_tokens column
	hasOrigin                bool // true if sessions table has origin column
	hasPinned                bool // true if sessions table has pinned column
	hasTitleSkippedAt        bool // true if sessions table has title_skipped_at column
	hasLastUserMessageAt     bool // true if sessions table has last_user_message_at column
	hasLastMessageAt         bool // true if sessions table has last_message_at column
	hasLastTotalTokens       bool // true if sessions table has last_total_tokens column
	hasLastMessageCount      bool // true if sessions table has last_message_count column
	hasMessageCount          bool // true if sessions table has message_count column
	hasReasoningEffort       bool // true if sessions table has reasoning_effort column
	hasReasoningMode         bool // true if sessions table has reasoning_mode column
	hasApprovalMode          bool // true if sessions table has approval_mode column
	hasWorktreeDir           bool // true if sessions table has worktree_dir column
	hasGoal                  bool // true if sessions table has goal column
	hasShare                 bool // true if sessions table has share column
	hasMessageCompactionTail bool // true if messages table has compaction_tail column
}

// Schema for the sessions database.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    number INTEGER,
    name TEXT,
    summary TEXT,
    generated_short_title TEXT,
    generated_long_title TEXT,
    title_source TEXT,
    title_generated_at TIMESTAMP,
    title_basis_msg_seq INTEGER DEFAULT 0,
    title_skipped_at TIMESTAMP,
	provider TEXT NOT NULL,
	provider_key TEXT,
	model TEXT NOT NULL,
	reasoning_effort TEXT,
	reasoning_mode TEXT,
	mode TEXT DEFAULT 'chat',
	approval_mode TEXT,
	origin TEXT DEFAULT 'tui',
    agent TEXT,
    cwd TEXT,
    worktree_dir TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_user_message_at TIMESTAMP,
    last_message_at TIMESTAMP,
    archived BOOLEAN DEFAULT FALSE,
    pinned BOOLEAN DEFAULT FALSE,
    parent_id TEXT REFERENCES sessions(id),
    search BOOLEAN DEFAULT FALSE,
    tools TEXT,
    mcp TEXT,
    user_turns INTEGER DEFAULT 0,
    llm_turns INTEGER DEFAULT 0,
    tool_calls INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    cached_input_tokens INTEGER DEFAULT 0,
    cache_write_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    last_total_tokens INTEGER DEFAULT 0,
    last_message_count INTEGER DEFAULT 0,
    message_count INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    tags TEXT,
    goal TEXT,
    share TEXT,
    compaction_seq INTEGER DEFAULT -1,
    compaction_count INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool', 'developer', 'event')),
    parts TEXT NOT NULL,
    text_content TEXT,
    duration_ms INTEGER,
    turn_index INTEGER DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sequence INTEGER NOT NULL,
    compaction_tail BOOLEAN DEFAULT FALSE
);

CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_mode ON sessions(mode);
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, sequence);
CREATE INDEX IF NOT EXISTS idx_messages_session_role ON messages(session_id, role);
CREATE INDEX IF NOT EXISTS idx_messages_role_id ON messages(role, id);
CREATE INDEX IF NOT EXISTS idx_messages_role_created_id ON messages(role, created_at, id);

CREATE TABLE IF NOT EXISTS session_provider_state (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    provider_key TEXT NOT NULL,
    state BLOB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (session_id, provider_key)
);

-- Metadata table for current session tracking
CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT
);

CREATE TABLE IF NOT EXISTS push_subscriptions (
    id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL UNIQUE,
    key_p256dh TEXT NOT NULL,
    key_auth TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    last_used_at TEXT
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

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE OF text_content ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
    INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
END;
`

const messagesTableSchema = `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool', 'developer', 'event')),
    parts TEXT NOT NULL,
    text_content TEXT,
    duration_ms INTEGER,
    turn_index INTEGER DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sequence INTEGER NOT NULL,
    compaction_tail BOOLEAN DEFAULT FALSE
)`

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
	// SQLite has a single-writer model. Keep all session store traffic on one
	// pooled connection so file-backed stores do not fan out into concurrent
	// writer connections, and so raw :memory: databases keep their schema/data
	// visible to every operation.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Initialize schema and run migrations.
	// Read-only mode skips initialization because it cannot write schema changes.
	if !cfg.ReadOnly {
		if err := initSchema(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize schema: %w", err)
		}
		if err := createMessageCountTriggers(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize message_count triggers: %w", err)
		}
	}

	store := &SQLiteStore{db: db, cfg: cfg}

	// Read-write stores have just created or migrated the schema above, so the
	// current optional columns are known to be present. Read-only stores skip
	// migrations and may be pointed at an older DB, so probe the tables once and
	// derive compatibility flags from those scans.
	if cfg.ReadOnly {
		store.probeSessionColumns()
		store.probeMessageColumns()
	} else {
		store.setCurrentColumns()
	}

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
const schemaVersion = 37

// migration represents a schema migration.
type migration struct {
	version     int
	description string
	up          func(db schemaExecutor) error
}

type schemaExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

type connSchemaExecutor struct {
	ctx  context.Context
	conn *sql.Conn
}

func (e connSchemaExecutor) Exec(query string, args ...any) (sql.Result, error) {
	return e.conn.ExecContext(e.ctx, query, args...)
}

func (e connSchemaExecutor) Query(query string, args ...any) (*sql.Rows, error) {
	return e.conn.QueryContext(e.ctx, query, args...)
}

func withImmediateMigrationTx(ctx context.Context, db *sql.DB, fn func(schemaExecutor) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := fn(connSchemaExecutor{ctx: ctx, conn: conn}); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	committed = true
	return nil
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
		up: func(db schemaExecutor) error {
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
		up: func(db schemaExecutor) error {
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
		up: func(db schemaExecutor) error {
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
		up: func(db schemaExecutor) error {
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
		up: func(db schemaExecutor) error {
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
		up: func(db schemaExecutor) error {
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
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN cached_input_tokens INTEGER DEFAULT 0")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 8: Add canonical provider key for reliable resume behavior
		version:     8,
		description: "add provider_key column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN provider_key TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 9: Create push_subscriptions table for Web Push notifications
		version:     9,
		description: "create push_subscriptions table",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`
				CREATE TABLE IF NOT EXISTS push_subscriptions (
					id TEXT PRIMARY KEY,
					endpoint TEXT NOT NULL UNIQUE,
					key_p256dh TEXT NOT NULL,
					key_auth TEXT NOT NULL,
					created_at TEXT NOT NULL DEFAULT (datetime('now')),
					last_used_at TEXT
				)
			`)
			return err
		},
	},
	{
		// Migration 10: Add compaction_seq to track compaction boundary
		version:     10,
		description: "add compaction_seq column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN compaction_seq INTEGER DEFAULT -1")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 11: Add cache_write_tokens for accurate cost accounting
		// Tracks cache-creation input tokens (Anthropic cache_creation_input_tokens,
		// distinct from cache reads and non-cached input). Without this column,
		// the largest cost bucket on cold-cache turns was silently dropped.
		version:     11,
		description: "add cache_write_tokens column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN cache_write_tokens INTEGER DEFAULT 0")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 12: Add generated session title fields for autotitling experiments.
		version:     12,
		description: "add generated title columns",
		up: func(db schemaExecutor) error {
			alterStatements := []string{
				"ALTER TABLE sessions ADD COLUMN generated_short_title TEXT",
				"ALTER TABLE sessions ADD COLUMN generated_long_title TEXT",
				"ALTER TABLE sessions ADD COLUMN title_source TEXT",
				"ALTER TABLE sessions ADD COLUMN title_generated_at TIMESTAMP",
				"ALTER TABLE sessions ADD COLUMN title_basis_msg_seq INTEGER DEFAULT 0",
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
		// Migration 13: Add session origin column for UI/source filtering.
		version:     13,
		description: "add session origin column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN origin TEXT DEFAULT 'tui'")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err = db.Exec(`UPDATE sessions SET origin = 'tui' WHERE origin IS NULL OR TRIM(origin) = ''`)
			if err != nil {
				return fmt.Errorf("backfill session origin: %w", err)
			}
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_origin ON sessions(origin)`)
			if err != nil {
				return fmt.Errorf("create origin index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 14: Add pinned flag for promoting sessions in sidebars.
		version:     14,
		description: "add session pinned column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN pinned BOOLEAN DEFAULT FALSE")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err = db.Exec(`UPDATE sessions SET pinned = FALSE WHERE pinned IS NULL`)
			if err != nil {
				return fmt.Errorf("backfill pinned sessions: %w", err)
			}
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_pinned ON sessions(pinned)`)
			if err != nil {
				return fmt.Errorf("create pinned index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 15: Add title_skipped_at for autotitle skip-until-changed logic.
		version:     15,
		description: "add title_skipped_at column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN title_skipped_at TIMESTAMP")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			// Index covers: WHERE archived=FALSE AND (title_skipped_at IS NULL OR title_skipped_at < updated_at)
			// ORDER BY updated_at DESC — ready for SQL-level autotitle filtering.
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_title_skipped ON sessions(archived, title_skipped_at, updated_at DESC)`)
			if err != nil {
				return fmt.Errorf("create title_skipped index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 16: Add last_user_message_at for sorting sessions by user activity.
		// Sessions sorted by updated_at bubble up when background jobs (autotitle, mining)
		// touch them. Sorting by last user message time reflects actual user engagement.
		version:     16,
		description: "add last_user_message_at column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN last_user_message_at TIMESTAMP")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			// Backfill from messages table: set to the most recent user message time per session.
			_, err = db.Exec(`
				UPDATE sessions SET last_user_message_at = (
					SELECT MAX(m.created_at) FROM messages m
					WHERE m.session_id = sessions.id AND m.role = 'user'
				)
				WHERE EXISTS (
					SELECT 1 FROM messages m
					WHERE m.session_id = sessions.id AND m.role = 'user'
				)`)
			if err != nil {
				return fmt.Errorf("backfill last_user_message_at: %w", err)
			}
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_last_user_msg ON sessions(last_user_message_at DESC)`)
			if err != nil {
				return fmt.Errorf("create last_user_message_at index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 17: Allow developer-role messages in persisted chat history.
		// Platform developer messages were added above the session layer, but the
		// messages table still rejected role='developer', causing silent drops.
		version:     17,
		description: "allow developer messages in messages table",
		up: func(db schemaExecutor) error {
			return rebuildMessagesTableForCurrentRoles(db)
		},
	},
	{
		// Migration 18: Persist last observed context estimate baseline for resumes.
		version:     18,
		description: "add last context estimate columns",
		up: func(db schemaExecutor) error {
			alterStatements := []string{
				"ALTER TABLE sessions ADD COLUMN last_total_tokens INTEGER DEFAULT 0",
				"ALTER TABLE sessions ADD COLUMN last_message_count INTEGER DEFAULT 0",
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
		// Migration 19: Persist reasoning effort on sessions so web sessions
		// can lock provider/model/effort after the first message.
		version:     19,
		description: "add reasoning_effort column for locking web session config",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN reasoning_effort TEXT"); err != nil {
				if !isDuplicateColumnError(err) {
					return err
				}
			}
			return nil
		},
	},
	{
		// Migration 20: Add last_message_at for sorting the web sidebar by
		// conversation activity. This remains role-based (user or assistant rows)
		// and is distinct from updated_at, which bumps on background work like
		// autotitle. Tool, developer, and system rows are excluded.
		version:     20,
		description: "add last_message_at column for visible-message activity sort",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN last_message_at TIMESTAMP")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err = db.Exec(`
				UPDATE sessions SET last_message_at = (
					SELECT MAX(m.created_at) FROM messages m
					WHERE m.session_id = sessions.id
					  AND m.role IN ('user', 'assistant')
				)
				WHERE EXISTS (
					SELECT 1 FROM messages m
					WHERE m.session_id = sessions.id
					  AND m.role IN ('user', 'assistant')
				)`)
			if err != nil {
				return fmt.Errorf("backfill last_message_at: %w", err)
			}
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_last_message ON sessions(last_message_at DESC)`)
			if err != nil {
				return fmt.Errorf("create last_message_at index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 21: Add a covering index for List()'s per-session visible
		// message counts. The existing (session_id, sequence) index is ideal for
		// ordered history reads, but COUNT(*) WHERE session_id=? AND role IN (...)
		// otherwise has to visit every row for the session and read role from the
		// table. Keeping role in the index lets SQLite satisfy the sidebar count
		// subquery from the index alone.
		version:     21,
		description: "add covering index for visible message counts",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_session_role ON messages(session_id, role)`)
			if err != nil {
				return fmt.Errorf("create messages session role index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 22: Allow durable event-role timeline markers (for example
		// model-switch separators) in persisted chat history. These rows are
		// rendered by clients but filtered before provider requests.
		version:     22,
		description: "allow event messages in messages table",
		up: func(db schemaExecutor) error {
			return rebuildMessagesTableForCurrentRoles(db)
		},
	},
	{
		// Migration 23: Track the total number of compactions per session.
		version:     23,
		description: "add compaction_count column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN compaction_count INTEGER DEFAULT 0")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 24: Add turn_index for debugging and per-turn stats.
		version:     24,
		description: "add message turn_index column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE messages ADD COLUMN turn_index INTEGER DEFAULT 0")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 25: Persist conversation message counts on the
		// sessions row so sidebar/session listings can read them directly instead
		// of running a COUNT(*) subquery per returned session.
		version:     25,
		description: "add persisted session message_count column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN message_count INTEGER DEFAULT 0")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err = db.Exec(fmt.Sprintf(`
				UPDATE sessions
				SET message_count = COALESCE((
					SELECT COUNT(*) FROM messages m
					WHERE m.session_id = sessions.id
					  AND %s
				), 0)`, countableConversationMessageSQL("m", false)))
			if err != nil {
				return fmt.Errorf("backfill message_count: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 26: Persist display suppression metadata for retained
		// post-compaction tail rows. These rows remain active model context, but
		// transcript renderers hide them because the same raw messages are already
		// visible before the compaction marker.
		version:     26,
		description: "add message compaction_tail display flag",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE messages ADD COLUMN compaction_tail BOOLEAN DEFAULT FALSE")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			if err := createMessageCountTriggers(db); err != nil {
				return fmt.Errorf("recreate message_count triggers: %w", err)
			}
			_, err = db.Exec(fmt.Sprintf(`
				UPDATE sessions
				SET message_count = COALESCE((
					SELECT COUNT(*) FROM messages m
					WHERE m.session_id = sessions.id
					  AND %s
				), 0)`, countableConversationMessageSQL("m", true)))
			if err != nil {
				return fmt.Errorf("backfill visible message_count: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 27: Align sidebar/message_count semantics with rendered chat
		// bubbles. Tool-only assistant rows are stored with role='assistant' but
		// render as tool groups, not assistant chat bubbles, so exclude assistant
		// rows without display text. Also exclude internal compaction summary user
		// rows hidden from normal chat.
		version:     27,
		description: "exclude non-chat-bubble rows from message_count",
		up: func(db schemaExecutor) error {
			if err := createMessageCountTriggers(db); err != nil {
				return fmt.Errorf("recreate message_count triggers: %w", err)
			}
			_, err := db.Exec(fmt.Sprintf(`
				UPDATE sessions
				SET message_count = COALESCE((
					SELECT COUNT(*) FROM messages m
					WHERE m.session_id = sessions.id
					  AND %s
				), 0)`, countableConversationMessageSQL("m", true)))
			if err != nil {
				return fmt.Errorf("backfill chat-bubble message_count: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 28: Add an index for cross-session prompt history recall.
		version:     28,
		description: "add prompt history role/id index",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_role_id ON messages(role, id)`)
			return err
		},
	},
	{
		// Migration 29: Add a timestamp index for date-ordered prompt history recall.
		version:     29,
		description: "add prompt history role/created index",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_role_created_id ON messages(role, created_at, id)`)
			return err
		},
	},
	{
		// Migration 30: Add composite expression indexes matching the web sidebar
		// sort orders. These avoid scanning and temp-sorting every visible session on
		// refresh for both any-message activity and last-user-activity orderings.
		version:     30,
		description: "add sidebar activity ordering indexes",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_sidebar_activity ON sessions(archived, COALESCE(pinned, FALSE) DESC, COALESCE(last_message_at, last_user_message_at, created_at) DESC)`); err != nil {
				return fmt.Errorf("create sidebar activity index: %w", err)
			}
			if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_sidebar_last_user_activity ON sessions(archived, COALESCE(pinned, FALSE) DESC, COALESCE(last_user_message_at, created_at) DESC)`); err != nil {
				return fmt.Errorf("create sidebar last-user activity index: %w", err)
			}
			return nil
		},
	},
	{
		// Migration 31: Persist provider-owned resume state outside the transcript.
		version:     31,
		description: "create session provider state table",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_provider_state (
				session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				provider_key TEXT NOT NULL,
				state BLOB NOT NULL,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (session_id, provider_key)
			)`)
			return err
		},
	},
	{
		// Migration 32: Avoid rewriting the FTS row when streaming updates only
		// mutate assistant parts/duration. Only text_content changes need FTS sync.
		version:     32,
		description: "narrow messages FTS update trigger to text_content",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec(`DROP TRIGGER IF EXISTS messages_au`); err != nil {
				return err
			}
			_, err := db.Exec(`CREATE TRIGGER messages_au AFTER UPDATE OF text_content ON messages BEGIN
				INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
				INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
			END`)
			return err
		},
	},
	{
		// Migration 33: Persist per-session tool approval mode for chat resumes.
		version:     33,
		description: "add session approval_mode column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN approval_mode TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 34: Persist bound git worktree directory for per-session BaseDir.
		version:     34,
		description: "add session worktree_dir column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN worktree_dir TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 35: Persist per-session /goal state.
		version:     35,
		description: "add session goal column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN goal TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		// Migration 36: Persist the explicit GPT-5.6 reasoning mode override.
		version:     36,
		description: "add session reasoning_mode column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN reasoning_mode TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
	{
		version:     37,
		description: "add session share column",
		up: func(db schemaExecutor) error {
			_, err := db.Exec("ALTER TABLE sessions ADD COLUMN share TEXT")
			if err != nil && !isDuplicateColumnError(err) {
				return err
			}
			return nil
		},
	},
}

// Keep in sync with llm.IsInternalCompactionSummaryText. SQLite migrations and
// triggers need a SQL form of the same prefix check.
const internalCompactionSummarySQLPrefix = "[Context Compaction]"

func countableConversationMessageSQL(alias string, includeCompactionTail bool) string {
	roleCol := qualifiedMessageColumn(alias, "role")
	// SQLite's one-argument TRIM only removes spaces. Include common ASCII
	// whitespace; persisted model/tool text that is only whitespace is expected to
	// use these characters.
	textCol := qualifiedMessageColumn(alias, "text_content")
	trimChars := "char(9) || char(10) || char(11) || char(12) || char(13) || char(32)"
	textExpr := "TRIM(COALESCE(" + textCol + ", ''), " + trimChars + ")"

	userPredicate := fmt.Sprintf("(%s = 'user' AND substr(%s, 1, %d) <> '%s')",
		roleCol, textExpr, len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix)
	assistantPredicate := fmt.Sprintf("(%s = 'assistant' AND %s <> '')", roleCol, textExpr)
	predicate := "(" + userPredicate + " OR " + assistantPredicate + ")"
	if includeCompactionTail {
		predicate = "(COALESCE(" + qualifiedMessageColumn(alias, "compaction_tail") + ", FALSE) = FALSE AND " + predicate + ")"
	}
	return predicate
}

func qualifiedMessageColumn(alias, column string) string {
	if alias == "" {
		return column
	}
	return alias + "." + column
}

func createMessageCountTriggers(db schemaExecutor) error {
	oldCountable := countableConversationMessageSQL("old", true)
	newCountable := countableConversationMessageSQL("new", true)
	stmts := []string{
		`DROP TRIGGER IF EXISTS messages_count_ai`,
		`DROP TRIGGER IF EXISTS messages_count_ad`,
		`DROP TRIGGER IF EXISTS messages_count_au`,
		fmt.Sprintf(`CREATE TRIGGER messages_count_ai AFTER INSERT ON messages
		WHEN %s
		BEGIN
		    UPDATE sessions
		    SET message_count = COALESCE(message_count, 0) + 1
		    WHERE id = new.session_id;
		END;`, newCountable),
		fmt.Sprintf(`CREATE TRIGGER messages_count_ad AFTER DELETE ON messages
		WHEN %s
		BEGIN
		    UPDATE sessions
		    SET message_count = COALESCE(message_count, 0) - 1
		    WHERE id = old.session_id;
		END;`, oldCountable),
		fmt.Sprintf(`CREATE TRIGGER messages_count_au AFTER UPDATE ON messages
		WHEN old.session_id <> new.session_id
		  OR (%s <> %s)
		BEGIN
		    UPDATE sessions
		    SET message_count = COALESCE(message_count, 0) - CASE
		        WHEN %s THEN 1
		        ELSE 0
		    END
		    WHERE id = old.session_id;
		    UPDATE sessions
		    SET message_count = COALESCE(message_count, 0) + CASE
		        WHEN %s THEN 1
		        ELSE 0
		    END
		    WHERE id = new.session_id;
		END;`, oldCountable, newCountable, oldCountable, newCountable),
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func rebuildMessagesTableForCurrentRoles(db schemaExecutor) error {
	stmts := []string{
		`DROP TRIGGER IF EXISTS messages_ai`,
		`DROP TRIGGER IF EXISTS messages_ad`,
		`DROP TRIGGER IF EXISTS messages_au`,
		`DROP INDEX IF EXISTS idx_messages_session_id`,
		`DROP INDEX IF EXISTS idx_messages_session_sequence`,
		`DROP INDEX IF EXISTS idx_messages_session_role`,
		`DROP TABLE IF EXISTS messages_fts`,
		`ALTER TABLE messages RENAME TO messages_old`,
		messagesTableSchema,
		`INSERT INTO messages (id, session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence)
		 SELECT id, session_id, role, parts, text_content, duration_ms, 0, created_at, sequence
		 FROM messages_old`,
		`DROP TABLE messages_old`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, sequence)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_sequence ON messages(session_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_session_role ON messages(session_id, role)`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(
			text_content,
			content='messages',
			content_rowid='id'
		)`,
		`INSERT INTO messages_fts(rowid, text_content) SELECT id, text_content FROM messages`,
		`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
		END`,
		`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
		END`,
		`CREATE TRIGGER messages_au AFTER UPDATE OF text_content ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
			INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
		END`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
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
	needsBootstrapVersion := versionErr != nil && (versionErr == sql.ErrNoRows || strings.Contains(versionErr.Error(), "no such table"))
	preExistingSessionsTable := false
	if needsBootstrapVersion {
		var tableCount int
		err := db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name='sessions'
		`).Scan(&tableCount)
		if err != nil {
			return fmt.Errorf("check sessions table before schema init: %w", err)
		}
		preExistingSessionsTable = tableCount > 0
	}

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

	// Determine current version if we didn't get it earlier.
	// versionErr is non-nil if schema_version table doesn't exist or has no rows.
	// For fresh DBs we must use the pre-schema check above; after db.Exec(schema)
	// the sessions table always exists and cannot distinguish fresh installs.
	if needsBootstrapVersion {
		if preExistingSessionsTable {
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

	// Run pending migrations. Each migration and its schema_version bump commit
	// atomically under BEGIN IMMEDIATE. The current migration list contains only
	// transaction-safe DDL/DML; future transaction-unsafe PRAGMA migrations should
	// be handled explicitly outside this helper.
	for _, m := range migrations {
		if m.version > currentVersion {
			if err := runSessionMigration(db, m); err != nil {
				return err
			}
			currentVersion = m.version
		}
	}

	// Ensure indexes exist (handles fresh DBs where migrations don't run)
	_, err = db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_sequence ON messages(session_id, sequence)")
	if err != nil {
		return fmt.Errorf("ensure message sequence index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status)")
	if err != nil {
		return fmt.Errorf("ensure status index: %w", err)
	}
	_, err = db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_number ON sessions(number)")
	if err != nil {
		return fmt.Errorf("ensure number index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_origin ON sessions(origin)")
	if err != nil {
		return fmt.Errorf("ensure origin index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_pinned ON sessions(pinned)")
	if err != nil {
		return fmt.Errorf("ensure pinned index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_title_skipped ON sessions(archived, title_skipped_at, updated_at DESC)")
	if err != nil {
		return fmt.Errorf("ensure title_skipped index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_last_user_msg ON sessions(last_user_message_at DESC)")
	if err != nil {
		return fmt.Errorf("ensure last_user_message_at index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_last_message ON sessions(last_message_at DESC)")
	if err != nil {
		return fmt.Errorf("ensure last_message_at index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_sidebar_activity ON sessions(archived, COALESCE(pinned, FALSE) DESC, COALESCE(last_message_at, last_user_message_at, created_at) DESC)")
	if err != nil {
		return fmt.Errorf("ensure sidebar activity index: %w", err)
	}
	_, err = db.Exec("CREATE INDEX IF NOT EXISTS idx_sessions_sidebar_last_user_activity ON sessions(archived, COALESCE(pinned, FALSE) DESC, COALESCE(last_user_message_at, created_at) DESC)")
	if err != nil {
		return fmt.Errorf("ensure sidebar last-user activity index: %w", err)
	}

	return nil
}

func runSessionMigration(db *sql.DB, m migration) error {
	ctx := context.Background()
	return withImmediateMigrationTx(ctx, db, func(tx schemaExecutor) error {
		if err := m.up(tx); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.description, err)
		}
		if _, err := tx.Exec("UPDATE schema_version SET version = ?", m.version); err != nil {
			return fmt.Errorf("update version to %d: %w", m.version, err)
		}
		return nil
	})
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
	if sess.Origin == "" {
		sess.Origin = OriginTUI
	}

	err := retryOnBusy(ctx, 5, func() error {
		// Use a single INSERT statement with a subquery to atomically assign the
		// next session number. This avoids race conditions where two concurrent
		// Creates could read the same MAX(number).
		reasoningEffortCol := ""
		reasoningEffortPlaceholder := ""
		var reasoningEffortArgs []any
		if s.hasReasoningEffort {
			reasoningEffortCol = ", reasoning_effort"
			reasoningEffortPlaceholder = ", ?"
			reasoningEffortArgs = []any{nullString(sess.ReasoningEffort)}
		}
		reasoningModeCol := ""
		reasoningModePlaceholder := ""
		var reasoningModeArgs []any
		if s.hasReasoningMode {
			reasoningModeCol = ", reasoning_mode"
			reasoningModePlaceholder = ", ?"
			reasoningModeArgs = []any{nullString(sess.ReasoningMode)}
		}
		approvalModeCol := ""
		approvalModePlaceholder := ""
		var approvalModeArgs []any
		if s.hasApprovalMode {
			approvalModeCol = ", approval_mode"
			approvalModePlaceholder = ", ?"
			approvalModeArgs = []any{nullString(string(sess.ApprovalMode))}
		}
		worktreeDirCol := ""
		worktreeDirPlaceholder := ""
		var worktreeDirArgs []any
		if s.hasWorktreeDir {
			worktreeDirCol = ", worktree_dir"
			worktreeDirPlaceholder = ", ?"
			worktreeDirArgs = []any{nullString(sess.WorktreeDir)}
		}
		goalCol := ""
		goalPlaceholder := ""
		var goalArgs []any
		if s.hasGoal {
			goalCol = ", goal"
			goalPlaceholder = ", ?"
			goalArgs = []any{goalJSONString(sess.Goal)}
		}
		shareCol := ""
		sharePlaceholder := ""
		var shareArgs []any
		if s.hasShare {
			shareCol = ", share"
			sharePlaceholder = ", ?"
			shareArgs = []any{shareJSONString(sess.Share)}
		}
		insertArgs := []any{
			sess.ID, sess.Name, sess.Summary, nullString(sess.GeneratedShortTitle), nullString(sess.GeneratedLongTitle), nullString(string(sess.TitleSource)), nullTime(sess.TitleGeneratedAt), sess.TitleBasisMsgSeq, nullTime(sess.TitleSkippedAt),
			sess.Provider, nullString(sess.ProviderKey), sess.Model, string(sess.Mode),
		}
		insertArgs = append(insertArgs, approvalModeArgs...)
		insertArgs = append(insertArgs,
			nullString(string(sess.Origin)), nullString(sess.Agent), sess.CWD,
		)
		insertArgs = append(insertArgs, worktreeDirArgs...)
		insertArgs = append(insertArgs,
			sess.CreatedAt, sess.UpdatedAt, sess.Archived, sess.Pinned, nullString(sess.ParentID),
			sess.Search, nullString(sess.Tools), nullString(sess.MCP),
			sess.UserTurns, sess.LLMTurns, sess.ToolCalls, sess.InputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.OutputTokens,
			sess.LastTotalTokens, sess.LastMessageCount, string(sess.Status), nullString(sess.Tags),
		)
		insertArgs = append(insertArgs, goalArgs...)
		insertArgs = append(insertArgs, shareArgs...)
		insertArgs = append(insertArgs, reasoningEffortArgs...)
		insertArgs = append(insertArgs, reasoningModeArgs...)
		result, err := s.db.ExecContext(ctx, `
			INSERT INTO sessions (id, number, name, summary, generated_short_title, generated_long_title, title_source, title_generated_at, title_basis_msg_seq, title_skipped_at,
			                      provider, provider_key, model, mode`+approvalModeCol+`, origin, agent, cwd`+worktreeDirCol+`, created_at, updated_at, archived, pinned, parent_id, search, tools, mcp,
			                      user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens, cache_write_tokens, output_tokens,
				                      last_total_tokens, last_message_count, status, tags`+goalCol+shareCol+reasoningEffortCol+reasoningModeCol+`)
			VALUES (?, (SELECT COALESCE(MAX(number), 0) + 1 FROM sessions), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`+approvalModePlaceholder+`, ?, ?, ?`+worktreeDirPlaceholder+`, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?`+goalPlaceholder+sharePlaceholder+reasoningEffortPlaceholder+reasoningModePlaceholder+`)`,
			insertArgs...)
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
	row := s.db.QueryRowContext(ctx,
		"SELECT "+s.sessionSelectCols()+" FROM sessions WHERE id = ?", id)
	return scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
}

// GetByNumber retrieves a session by its sequential number.
func (s *SQLiteStore) GetByNumber(ctx context.Context, number int64) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+s.sessionSelectCols()+" FROM sessions WHERE number = ?", number)
	return scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
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
	row := s.db.QueryRowContext(ctx,
		"SELECT "+s.sessionSelectCols()+" FROM sessions WHERE id LIKE ? ORDER BY created_at DESC LIMIT 1", pattern)
	return scanSessionRow(row, s.hasGeneratedTitles, s.hasCacheWriteTokens, s.hasCompactionSeq, s.hasCompactionCount, s.hasTitleSkippedAt, s.hasLastTotalTokens, s.hasLastMessageCount)
}

// Update modifies an existing session's metadata fields.
// Token metrics (input_tokens, cached_input_tokens, cache_write_tokens, output_tokens)
// and turn counters (user_turns, llm_turns, tool_calls) are intentionally excluded — they are
// managed exclusively by atomic update paths to prevent stale in-memory values from clobbering
// accumulated totals.
func (s *SQLiteStore) Update(ctx context.Context, sess *Session) error {
	sess.UpdatedAt = time.Now()
	if sess.Origin == "" {
		sess.Origin = OriginTUI
	}

	titleSkippedAtClause := ""
	if s.hasTitleSkippedAt {
		titleSkippedAtClause = ", title_skipped_at = ?"
	}
	reasoningEffortClause := ""
	if s.hasReasoningEffort {
		reasoningEffortClause = ", reasoning_effort = ?"
	}
	reasoningModeClause := ""
	if s.hasReasoningMode {
		reasoningModeClause = ", reasoning_mode = ?"
	}
	approvalModeClause := ""
	if s.hasApprovalMode {
		approvalModeClause = ", approval_mode = ?"
	}
	worktreeDirClause := ""
	if s.hasWorktreeDir {
		worktreeDirClause = ", worktree_dir = ?"
	}
	goalClause := ""
	if s.hasGoal {
		goalClause = ", goal = ?"
	}
	shareClause := ""
	if s.hasShare {
		shareClause = ", share = ?"
	}
	query := `
		UPDATE sessions SET name = ?, summary = ?, generated_short_title = ?, generated_long_title = ?, title_source = ?, title_generated_at = ?, title_basis_msg_seq = ?` +
		titleSkippedAtClause + `,
		       provider = ?, provider_key = ?, model = ?` + reasoningEffortClause + reasoningModeClause + `, mode = ?` + approvalModeClause + `, origin = ?, agent = ?, cwd = ?` + worktreeDirClause + `,
		       updated_at = ?, archived = ?, pinned = ?, parent_id = ?, search = ?, tools = ?, mcp = ?,
		       status = ?, tags = ?` + goalClause + shareClause + `
		WHERE id = ?`

	args := []any{
		sess.Name, sess.Summary, nullString(sess.GeneratedShortTitle), nullString(sess.GeneratedLongTitle), nullString(string(sess.TitleSource)), nullTime(sess.TitleGeneratedAt), sess.TitleBasisMsgSeq,
	}
	if s.hasTitleSkippedAt {
		args = append(args, nullTime(sess.TitleSkippedAt))
	}
	args = append(args,
		sess.Provider, nullString(sess.ProviderKey), sess.Model,
	)
	if s.hasReasoningEffort {
		args = append(args, nullString(sess.ReasoningEffort))
	}
	if s.hasReasoningMode {
		args = append(args, nullString(sess.ReasoningMode))
	}
	args = append(args,
		string(sess.Mode),
	)
	if s.hasApprovalMode {
		args = append(args, nullString(string(sess.ApprovalMode)))
	}
	args = append(args,
		nullString(string(sess.Origin)), nullString(sess.Agent), sess.CWD,
	)
	if s.hasWorktreeDir {
		args = append(args, nullString(sess.WorktreeDir))
	}
	args = append(args,
		sess.UpdatedAt, sess.Archived, sess.Pinned, nullString(sess.ParentID),
		sess.Search, nullString(sess.Tools), nullString(sess.MCP),
		string(sess.Status), nullString(sess.Tags),
	)
	if s.hasGoal {
		args = append(args, goalJSONString(sess.Goal))
	}
	if s.hasShare {
		args = append(args, shareJSONString(sess.Share))
	}
	args = append(args, sess.ID)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found: %s", sess.ID)
	}
	return nil
}

// UpdateGoal updates only the persisted goal state for a session.
func (s *SQLiteStore) UpdateGoal(ctx context.Context, id string, goal *Goal) error {
	if !s.hasGoal {
		return fmt.Errorf("goal column is unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is empty")
	}
	goal = goal.Clone()
	if goal != nil {
		goal.Normalize(time.Now())
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET goal = ?, updated_at = ?
		WHERE id = ?`, goalJSONString(goal), time.Now(), id)
	if err != nil {
		return fmt.Errorf("update goal: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	return nil
}

// UpdateShare updates only persisted share metadata for a session.
func (s *SQLiteStore) UpdateShare(ctx context.Context, id string, share *ShareState) error {
	if !s.hasShare {
		return fmt.Errorf("share column is unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is empty")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET share = ?, updated_at = ? WHERE id = ?`, shareJSONString(share), time.Now(), id)
	if err != nil {
		return fmt.Errorf("update share: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	return nil
}

// UpdateGeneratedTitle updates only generated title columns for a session.
func (s *SQLiteStore) UpdateGeneratedTitle(ctx context.Context, id, shortTitle, longTitle string, generatedAt time.Time, basisMsgSeq int) error {
	if !s.hasGeneratedTitles {
		return nil
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("session id is empty")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET
		       generated_short_title = ?,
		       generated_long_title = ?,
		       title_source = ?,
		       title_generated_at = ?,
		       title_basis_msg_seq = ?,
		       updated_at = ?
		WHERE id = ?`,
		nullString(shortTitle), nullString(longTitle), nullString(string(TitleSourceGenerated)), nullTime(generatedAt), basisMsgSeq, time.Now(), id)
	if err != nil {
		return fmt.Errorf("update generated title: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	return nil
}

// MarkTitleSkipped sets title_skipped_at on a session without bumping updated_at.
// This lets the autotitle job skip trivial sessions until real new messages arrive.
func (s *SQLiteStore) MarkTitleSkipped(ctx context.Context, id string, t time.Time) error {
	if !s.hasTitleSkippedAt {
		return nil // column absent on old schema; skip silently
	}
	_, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET title_skipped_at = ? WHERE id = ?", t, id)
	if err != nil {
		return fmt.Errorf("mark title skipped: %w", err)
	}
	return nil
}

// SaveProviderState stores opaque provider-owned resume state for a session.
func (s *SQLiteStore) SaveProviderState(ctx context.Context, sessionID, providerKey string, state []byte) error {
	sessionID = strings.TrimSpace(sessionID)
	providerKey = strings.TrimSpace(providerKey)
	if sessionID == "" || providerKey == "" {
		return nil
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO session_provider_state (session_id, provider_key, state, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(session_id, provider_key) DO UPDATE SET
			    state = excluded.state,
			    updated_at = CURRENT_TIMESTAMP
		`, sessionID, providerKey, state)
		if err != nil {
			return fmt.Errorf("save provider state: %w", err)
		}
		return nil
	})
}

// LoadProviderState returns opaque provider-owned resume state for a session.
func (s *SQLiteStore) LoadProviderState(ctx context.Context, sessionID, providerKey string) ([]byte, error) {
	sessionID = strings.TrimSpace(sessionID)
	providerKey = strings.TrimSpace(providerKey)
	if sessionID == "" || providerKey == "" {
		return nil, nil
	}
	var state []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT state FROM session_provider_state
		WHERE session_id = ? AND provider_key = ?
	`, sessionID, providerKey).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load provider state: %w", err)
	}
	return append([]byte(nil), state...), nil
}

// DeleteProviderState removes provider-owned resume state for a session.
func (s *SQLiteStore) DeleteProviderState(ctx context.Context, sessionID, providerKey string) error {
	sessionID = strings.TrimSpace(sessionID)
	providerKey = strings.TrimSpace(providerKey)
	if sessionID == "" || providerKey == "" {
		return nil
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			DELETE FROM session_provider_state
			WHERE session_id = ? AND provider_key = ?
		`, sessionID, providerKey)
		if err != nil {
			return fmt.Errorf("delete provider state: %w", err)
		}
		return nil
	})
}

// UpdateMetrics atomically increments the metrics fields for a session.
// All token counters use += to avoid clobbering concurrent accumulation.
func (s *SQLiteStore) UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int) error {
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET
			       llm_turns = llm_turns + ?,
			       tool_calls = tool_calls + ?,
			       input_tokens = input_tokens + ?,
			       cached_input_tokens = cached_input_tokens + ?,
			       cache_write_tokens = cache_write_tokens + ?,
			       output_tokens = output_tokens + ?,
			       updated_at = ?
			WHERE id = ?`,
			llmTurns, toolCalls, inputTokens, cachedInputTokens, cacheWriteTokens, outputTokens, time.Now(), id)
		return err
	})
}

// UpdateContextEstimate persists the last observed provider context estimate so
// resumed sessions can display a realistic context meter before the next turn.
func (s *SQLiteStore) UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error {
	if !s.hasLastTotalTokens || !s.hasLastMessageCount {
		return nil
	}
	if lastTotalTokens < 0 {
		lastTotalTokens = 0
	}
	if lastMessageCount < 0 {
		lastMessageCount = 0
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET
			       last_total_tokens = ?,
			       last_message_count = ?,
			       updated_at = ?
			WHERE id = ?`,
			lastTotalTokens, lastMessageCount, time.Now(), id)
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

// IncrementUserTurns increments the user turn count and updates last_user_message_at.
func (s *SQLiteStore) IncrementUserTurns(ctx context.Context, id string) error {
	return retryOnBusy(ctx, 5, func() error {
		now := time.Now()
		lastUserMsgClause := ""
		if s.hasLastUserMessageAt {
			lastUserMsgClause = ", last_user_message_at = ?"
		}
		query := `UPDATE sessions SET user_turns = user_turns + 1, updated_at = ?` + lastUserMsgClause + ` WHERE id = ?`
		args := []any{now}
		if s.hasLastUserMessageAt {
			args = append(args, now)
		}
		args = append(args, id)
		_, err := s.db.ExecContext(ctx, query, args...)
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
	cacheWriteCol := "0"
	if s.hasCacheWriteTokens {
		cacheWriteCol = "s.cache_write_tokens"
	}
	originCol := "'tui'"
	if s.hasOrigin {
		originCol = "COALESCE(NULLIF(TRIM(s.origin), ''), 'tui')"
	}
	pinnedCol := "FALSE"
	if s.hasPinned {
		pinnedCol = "COALESCE(s.pinned, FALSE)"
	}
	generatedShortCol := "''"
	generatedLongCol := "''"
	titleSourceCol := "''"
	if s.hasGeneratedTitles {
		generatedShortCol = "s.generated_short_title"
		generatedLongCol = "s.generated_long_title"
		titleSourceCol = "s.title_source"
	}
	lastMessageAtCol := "NULL"
	if s.hasLastMessageAt {
		lastMessageAtCol = "s.last_message_at"
	}
	goalCol := "NULL"
	if s.hasGoal {
		goalCol = "s.goal"
	}
	shareCol := "NULL"
	if s.hasShare {
		shareCol = "s.share"
	}
	messageCountCol := "COALESCE(s.message_count, 0)"
	if !s.hasMessageCount {
		messageCountCol = "(SELECT COUNT(*) FROM messages WHERE session_id = s.id AND " + countableConversationMessageSQL("", s.hasMessageCompactionTail) + ")"
	}
	worktreeDirCol := "''"
	if s.hasWorktreeDir {
		worktreeDirCol = "COALESCE(s.worktree_dir, '')"
	}
	fromClause := "FROM sessions s"
	if opts.SortByNumberDesc {
		// Completed-session walks page by descending session number. Force the
		// number index so SQLite can continue scanning from the last seen number
		// instead of picking a filter-only index and re-sorting each page.
		fromClause = "FROM sessions s INDEXED BY idx_sessions_number"
	}
	query := `
		SELECT s.id, s.number, s.name, s.summary, ` + generatedShortCol + `, ` + generatedLongCol + `, ` + titleSourceCol + `,
		       s.provider, COALESCE(s.provider_key, ''), s.model, s.mode, ` + originCol + `, s.archived, ` + pinnedCol + `, s.created_at, s.updated_at, ` + lastMessageAtCol + `,
		       ` + messageCountCol + ` as message_count,
		       s.user_turns, s.llm_turns, s.tool_calls, s.input_tokens, s.cached_input_tokens, ` + cacheWriteCol + `, s.output_tokens, s.status, s.tags, ` + worktreeDirCol + `, ` + goalCol + `, ` + shareCol + `
		` + fromClause + `
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
	if len(opts.Categories) > 0 {
		clauses := make([]string, 0, len(opts.Categories))
		sawSpecificCategory := false
		for _, raw := range opts.Categories {
			category := strings.ToLower(strings.TrimSpace(raw))
			switch category {
			case "", "all":
				clauses = nil
			case "chat":
				sawSpecificCategory = true
				if s.hasOrigin {
					clauses = append(clauses, "(s.mode = 'chat' AND COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') = 'tui')")
				} else {
					clauses = append(clauses, "(s.mode = 'chat')")
				}
			case "web":
				sawSpecificCategory = true
				if s.hasOrigin {
					clauses = append(clauses, "(COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') = 'web')")
				}
			case "ask", "plan", "exec":
				sawSpecificCategory = true
				clauses = append(clauses, "(s.mode = ?)")
				args = append(args, category)
			}
			if clauses == nil {
				break
			}
		}
		if len(clauses) > 0 {
			query += " AND (" + strings.Join(clauses, " OR ") + ")"
		} else if sawSpecificCategory {
			query += " AND 1 = 0"
		}
	}
	if opts.BeforeNumber > 0 {
		query += " AND s.number < ?"
		args = append(args, opts.BeforeNumber)
	}
	if !opts.Archived {
		query += " AND s.archived = FALSE"
	}

	if opts.SortByNumberDesc {
		query += " ORDER BY s.number DESC"
	} else {
		// Sort by last user message time (when the user last interacted), falling back
		// to created_at for sessions with no user messages yet. This prevents background
		// activity (autotitle, mining, status changes) from reordering the sidebar.
		// Web sidebar callers set SortByActivity to use last_message_at instead so
		// assistant-only turns also surface (keeps the top-N window aligned with the
		// client-side "any-message" ordering).
		sortCol := "s.updated_at"
		if opts.SortByActivity && s.hasLastMessageAt {
			sortCol = "COALESCE(last_message_at, last_user_message_at, created_at)"
		} else if s.hasLastUserMessageAt {
			sortCol = "COALESCE(last_user_message_at, created_at)"
		}
		if s.hasPinned {
			query += " ORDER BY COALESCE(pinned, FALSE) DESC, " + sortCol + " DESC"
		} else {
			query += " ORDER BY " + sortCol + " DESC"
		}
	}

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
		var mode, status, tags, generatedShortTitle, generatedLongTitle, titleSource, origin, worktreeDir, goalRaw, shareRaw sql.NullString
		var lastMessageAt sql.NullTime
		err := rows.Scan(&sum.ID, &number, &sum.Name, &sum.Summary, &generatedShortTitle, &generatedLongTitle, &titleSource, &sum.Provider, &sum.ProviderKey, &sum.Model, &mode,
			&origin, &sum.Archived, &sum.Pinned, &sum.CreatedAt, &sum.UpdatedAt, &lastMessageAt, &sum.MessageCount,
			&sum.UserTurns, &sum.LLMTurns, &sum.ToolCalls, &sum.InputTokens, &sum.CachedInputTokens, &sum.CacheWriteTokens, &sum.OutputTokens,
			&status, &tags, &worktreeDir, &goalRaw, &shareRaw)
		if err != nil {
			return nil, fmt.Errorf("scan session summary: %w", err)
		}
		if lastMessageAt.Valid {
			sum.LastMessageAt = lastMessageAt.Time
		}
		if number.Valid {
			sum.Number = number.Int64
		}
		if generatedShortTitle.Valid {
			sum.GeneratedShortTitle = generatedShortTitle.String
		}
		if generatedLongTitle.Valid {
			sum.GeneratedLongTitle = generatedLongTitle.String
		}
		if titleSource.Valid {
			sum.TitleSource = SessionTitleSource(titleSource.String)
		}
		if mode.Valid {
			sum.Mode = SessionMode(mode.String)
		}
		if origin.Valid {
			sum.Origin = SessionOrigin(origin.String)
		} else {
			sum.Origin = OriginTUI
		}
		if status.Valid {
			sum.Status = SessionStatus(status.String)
		}
		if tags.Valid {
			sum.Tags = tags.String
		}
		if worktreeDir.Valid {
			sum.WorktreeDir = worktreeDir.String
		}
		if goal := parseGoalJSONString(goalRaw); goal != nil {
			sum.Goal = goal
		}
		if share := parseShareJSONString(shareRaw); share != nil {
			sum.Share = share
		}
		results = append(results, sum)
	}
	return results, rows.Err()
}

// Search finds sessions containing the query text using FTS5.
func (s *SQLiteStore) Search(ctx context.Context, opts SearchOptions) ([]SearchResult, error) {
	if opts.Limit == 0 {
		opts.Limit = 20
	}

	ftsQuery := sqlitefts.LiteralQuery(opts.Query)
	if ftsQuery == "" {
		return []SearchResult{}, nil
	}

	messageCountCol := "COALESCE(s.message_count, 0)"
	compactionTailClause := ""
	if s.hasMessageCompactionTail {
		compactionTailClause = " AND COALESCE(m.compaction_tail, FALSE) = FALSE"
	}
	if !s.hasMessageCount {
		messageCountCol = "(SELECT COUNT(*) FROM messages WHERE session_id = s.id AND " + countableConversationMessageSQL("", s.hasMessageCompactionTail) + ")"
	}

	originCol := "'tui'"
	if s.hasOrigin {
		originCol = "COALESCE(NULLIF(TRIM(s.origin), ''), 'tui')"
	}
	pinnedCol := "FALSE"
	if s.hasPinned {
		pinnedCol = "COALESCE(s.pinned, FALSE)"
	}
	generatedShortCol := "''"
	generatedLongCol := "''"
	titleSourceCol := "''"
	if s.hasGeneratedTitles {
		generatedShortCol = "s.generated_short_title"
		generatedLongCol = "s.generated_long_title"
		titleSourceCol = "s.title_source"
	}
	lastMessageAtCol := "NULL"
	if s.hasLastMessageAt {
		lastMessageAtCol = "s.last_message_at"
	}

	filterClause := ""
	args := []any{ftsQuery}
	if len(opts.Categories) > 0 {
		clauses := make([]string, 0, len(opts.Categories))
		sawSpecificCategory := false
		for _, raw := range opts.Categories {
			category := strings.ToLower(strings.TrimSpace(raw))
			switch category {
			case "", "all":
				clauses = nil
			case "chat":
				sawSpecificCategory = true
				if s.hasOrigin {
					clauses = append(clauses, "(s.mode = 'chat' AND COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') = 'tui')")
				} else {
					clauses = append(clauses, "(s.mode = 'chat')")
				}
			case "web":
				sawSpecificCategory = true
				if s.hasOrigin {
					clauses = append(clauses, "(COALESCE(NULLIF(TRIM(s.origin), ''), 'tui') = 'web')")
				}
			case "ask", "plan", "exec":
				sawSpecificCategory = true
				clauses = append(clauses, "(s.mode = ?)")
				args = append(args, category)
			}
			if clauses == nil {
				break
			}
		}
		if len(clauses) > 0 {
			filterClause += " AND (" + strings.Join(clauses, " OR ") + ")"
		} else if sawSpecificCategory {
			filterClause += " AND 1 = 0"
		}
	}
	if !opts.Archived {
		filterClause += " AND s.archived = FALSE"
	}
	args = append(args, opts.Limit)

	rows, err := s.db.QueryContext(ctx, `
		WITH raw_matches AS (
			SELECT rowid AS message_id, rank AS match_rank
			FROM messages_fts
			WHERE messages_fts MATCH ?
		), ranked_matches AS (
			SELECT m.id AS message_id, m.session_id, raw_matches.match_rank,
			       ROW_NUMBER() OVER (PARTITION BY m.session_id ORDER BY raw_matches.match_rank, m.id) AS session_row
			FROM raw_matches
			JOIN messages m ON m.id = raw_matches.message_id
			JOIN sessions s ON s.id = m.session_id
			WHERE 1=1`+compactionTailClause+filterClause+`
		), session_matches AS (
			SELECT message_id, session_id, match_rank
			FROM ranked_matches
			WHERE session_row = 1
			ORDER BY match_rank, message_id
			LIMIT ?
		)
		SELECT m.session_id, s.number, m.id, s.name, s.summary, `+generatedShortCol+` AS generated_short_title,
		       `+generatedLongCol+` AS generated_long_title, `+titleSourceCol+` AS title_source,
		       snippet(messages_fts, 0, '**', '**', '...', 32) AS snippet, s.provider, COALESCE(s.provider_key, '') AS provider_key,
		       s.model, s.mode, `+originCol+` AS origin, s.archived, `+pinnedCol+` AS pinned, s.status,
		       `+messageCountCol+` AS message_count, s.created_at, s.updated_at, `+lastMessageAtCol+` AS last_message_at,
		       m.created_at AS message_created_at
		FROM session_matches sm
		JOIN messages m ON m.id = sm.message_id
		JOIN sessions s ON s.id = sm.session_id
		JOIN messages_fts ON messages_fts.rowid = sm.message_id
		ORDER BY sm.match_rank, sm.message_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var number sql.NullInt64
		var generatedShortTitle, generatedLongTitle, titleSource, providerKey, mode, origin, status sql.NullString
		var lastMessageAt sql.NullTime
		err := rows.Scan(&r.SessionID, &number, &r.MessageID, &r.SessionName, &r.Summary,
			&generatedShortTitle, &generatedLongTitle, &titleSource, &r.Snippet, &r.Provider, &providerKey,
			&r.Model, &mode, &origin, &r.Archived, &r.Pinned, &status, &r.MessageCount,
			&r.SessionCreatedAt, &r.UpdatedAt, &lastMessageAt, &r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		if number.Valid {
			r.SessionNumber = number.Int64
		}
		if generatedShortTitle.Valid {
			r.GeneratedShortTitle = generatedShortTitle.String
		}
		if generatedLongTitle.Valid {
			r.GeneratedLongTitle = generatedLongTitle.String
		}
		if titleSource.Valid {
			r.TitleSource = SessionTitleSource(titleSource.String)
		}
		if providerKey.Valid {
			r.ProviderKey = providerKey.String
		}
		if mode.Valid {
			r.Mode = SessionMode(mode.String)
		}
		if origin.Valid {
			r.Origin = SessionOrigin(origin.String)
		} else {
			r.Origin = OriginTUI
		}
		if status.Valid {
			r.Status = SessionStatus(status.String)
		}
		if lastMessageAt.Valid {
			r.LastMessageAt = lastMessageAt.Time
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

	partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
	if err != nil {
		return fmt.Errorf("serialize parts: %w", err)
	}

	// Track whether we need to auto-allocate sequence
	autoSequence := msg.Sequence < 0

	// Retry the entire transaction on SQLITE_BUSY
	return retryOnBusy(ctx, 5, func() error {
		sequence := msg.Sequence
		var id int64
		var err error
		if autoSequence {
			id, sequence, err = s.addMessageAutoSequence(ctx, sessionID, msg, partsJSON)
		} else {
			id, err = s.addMessageExplicitSequence(ctx, sessionID, msg, partsJSON, sequence)
		}
		if err != nil {
			return err
		}
		msg.ID = id
		msg.Sequence = sequence
		return nil
	})
}

func (s *SQLiteStore) addMessageExplicitSequence(ctx context.Context, sessionID string, msg *Message, partsJSON string, sequence int) (int64, error) {
	// Use transaction for atomic insert/session timestamp update.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	id, err := s.insertMessageAndBumpSession(ctx, tx, sessionID, msg, partsJSON, sequence)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return id, nil
}

func (s *SQLiteStore) addMessageAutoSequence(ctx context.Context, sessionID string, msg *Message, partsJSON string) (int64, int, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, 0, fmt.Errorf("begin immediate transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var maxSeq sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(sequence) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq); err != nil {
		return 0, 0, fmt.Errorf("get max sequence: %w", err)
	}
	sequence := 0
	if maxSeq.Valid {
		sequence = int(maxSeq.Int64) + 1
	}

	id, err := s.insertMessageAndBumpSession(ctx, conn, sessionID, msg, partsJSON, sequence)
	if err != nil {
		return 0, 0, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, 0, fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return id, sequence, nil
}

type sqliteExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *SQLiteStore) insertMessageAndBumpSession(ctx context.Context, execer sqliteExecer, sessionID string, msg *Message, partsJSON string, sequence int) (int64, error) {
	result, err := execer.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.TurnIndex, msg.CreatedAt, sequence, msg.CompactionTail)
	if err != nil {
		return 0, fmt.Errorf("insert message: %w", err)
	}
	id, _ := result.LastInsertId()

	// Update session's updated_at. Preserve the existing sidebar activity
	// semantics here: user/assistant role rows bump last_message_at, while
	// tool/developer/system/event rows do not. message_count is maintained by
	// triggers with the stricter chat-bubble predicate above, so tool-call-only
	// assistant rows may affect activity ordering without increasing the count.
	bumpLastMessageAt := s.hasLastMessageAt && !msg.CompactionTail && (msg.Role == "user" || msg.Role == "assistant")
	if bumpLastMessageAt {
		_, err = execer.ExecContext(ctx,
			"UPDATE sessions SET updated_at = ?, last_message_at = ? WHERE id = ?",
			time.Now(), msg.CreatedAt, sessionID)
	} else {
		_, err = execer.ExecContext(ctx, "UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now(), sessionID)
	}
	if err != nil {
		return 0, fmt.Errorf("update session timestamp: %w", err)
	}
	return id, nil
}

// UpdateMessage replaces the content of an existing message (keyed by msg.ID
// within sessionID). Returns ErrNotFound if no row matches. Used by the
// "persist as we go" upsert path: the caller first calls AddMessage to stamp
// an ID, then subsequent snapshots call UpdateMessage with the same ID.
func (s *SQLiteStore) UpdateMessage(ctx context.Context, sessionID string, msg *Message) error {
	return s.updateMessage(ctx, sessionID, msg, true)
}

// UpdateStreamingMessage updates an in-progress assistant message while letting
// the caller defer the FTS-backed text_content rewrite until finalization.
func (s *SQLiteStore) UpdateStreamingMessage(ctx context.Context, sessionID string, msg *Message, finalizeText bool) error {
	return s.updateMessage(ctx, sessionID, msg, finalizeText)
}

func (s *SQLiteStore) updateMessage(ctx context.Context, sessionID string, msg *Message, updateText bool) error {
	if msg == nil {
		return fmt.Errorf("update message: nil msg")
	}
	if msg.ID == 0 {
		return fmt.Errorf("update message: missing id")
	}

	partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
	if err != nil {
		return fmt.Errorf("serialize parts: %w", err)
	}

	query := `
			UPDATE messages
			SET role = ?, parts = ?`
	args := []any{string(msg.Role), partsJSON}
	if updateText {
		query += `, text_content = ?`
		args = append(args, msg.TextContent)
	}
	query += `, duration_ms = ?, turn_index = ?, compaction_tail = ?
			WHERE id = ? AND session_id = ?`
	args = append(args, msg.DurationMs, msg.TurnIndex, msg.CompactionTail, msg.ID, sessionID)

	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("update message: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected: %w", err)
		}
		if rowsAffected == 0 {
			return ErrNotFound
		}

		// Bump session updated_at so sidebar sort reflects the snapshot.
		// Intentionally do NOT touch last_message_at — the message was
		// already counted at AddMessage time; updates shouldn't re-order.
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now(), sessionID); err != nil {
			return fmt.Errorf("update session timestamp: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}
		return nil
	})
}

func (s *SQLiteStore) PersistCompactionTailHints(ctx context.Context, sessionID string, messageIDs []int64) error {
	if !s.hasMessageCompactionTail || len(messageIDs) == 0 {
		return nil
	}

	args := make([]any, 0, len(messageIDs)+1)
	args = append(args, sessionID)
	placeholders := make([]string, 0, len(messageIDs))
	seen := make(map[int64]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	if len(placeholders) == 0 {
		return nil
	}

	query := `
		UPDATE messages
		SET compaction_tail = TRUE
		WHERE session_id = ?
		  AND COALESCE(compaction_tail, FALSE) = FALSE
		  AND id IN (` + strings.Join(placeholders, ",") + `)`

	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("persist compaction tail hints: %w", err)
		}
		return nil
	})
}

func (s *SQLiteStore) ClearCompactionBoundary(ctx context.Context, id string) error {
	if !s.hasCompactionSeq {
		return nil
	}
	query := "UPDATE sessions SET compaction_seq = -1, updated_at = ? WHERE id = ?"
	if s.hasCompactionCount {
		query = "UPDATE sessions SET compaction_seq = -1, compaction_count = 0, updated_at = ? WHERE id = ?"
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, query, time.Now(), id)
		return err
	})
}

// ReplaceMessages reconciles the complete persisted history for a session.
// It preserves any unchanged prefix and rewrites only the changed suffix, avoiding
// a DELETE+INSERT of long histories when serve/web persists an appended snapshot.
// Because the replacement snapshot becomes the complete persisted history, any
// previous compaction boundary is cleared.
func (s *SQLiteStore) ReplaceMessages(ctx context.Context, sessionID string, messages []Message) error {
	partsJSON, err := prepareReplacementMessageParts(messages, s.cfg.StripImageBase64)
	if err != nil {
		return err
	}

	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		commonPrefix, fullDelete, err := replacementCommonPrefix(ctx, tx, sessionID, messages, partsJSON)
		if err != nil {
			return err
		}

		// Drop the first changed row and any stale suffix. Unchanged prefix rows keep
		// their rowids, FTS entries, and created_at values; triggers update FTS and
		// message_count only for rows that actually changed. If legacy/corrupt rows
		// have duplicate or negative sequence numbers, fall back to the old full
		// rewrite so replacement remains a complete-history operation.
		if fullDelete {
			if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
				return fmt.Errorf("delete existing messages: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ? AND sequence >= ?", sessionID, commonPrefix); err != nil {
			return fmt.Errorf("delete changed messages: %w", err)
		}

		if commonPrefix < len(messages) {
			insertStmt, err := tx.PrepareContext(ctx, `
				INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return fmt.Errorf("prepare message insert: %w", err)
			}
			defer insertStmt.Close()

			for i := commonPrefix; i < len(messages); i++ {
				msg := messages[i]
				createdAt := msg.CreatedAt
				if createdAt.IsZero() {
					createdAt = time.Now()
				}
				_, err = insertStmt.ExecContext(ctx,
					sessionID, string(msg.Role), partsJSON[i], msg.TextContent, msg.DurationMs, msg.TurnIndex, createdAt, i, false)
				if err != nil {
					return fmt.Errorf("insert message %d: %w", i, err)
				}
			}
		}

		// Update session activity metadata and clear any stale compaction boundary. A
		// replace is a full-history rewrite; keeping an old compaction_seq could make
		// future active-context loads start past the end of the rewritten rows. Compute
		// activity from the persisted rows after the rewrite so unchanged prefixes keep
		// their original created_at values instead of freshly rebuilt snapshot times.
		now := time.Now()
		if err := s.updateReplaceMessagesSessionMetadata(ctx, tx, sessionID, now, true); err != nil {
			return err
		}

		return tx.Commit()
	})
}

// ReplaceCompactedMessages reconciles the active post-compaction history for a
// session while preserving pre-compaction scrollback and the compaction boundary.
// It must only be used with snapshots that start at the current compaction_seq.
func (s *SQLiteStore) ReplaceCompactedMessages(ctx context.Context, sessionID string, messages []Message) error {
	partsJSON, err := prepareReplacementMessageParts(messages, s.cfg.StripImageBase64)
	if err != nil {
		return err
	}

	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		startSeq, err := s.compactionStartSeq(ctx, tx, sessionID)
		if err != nil {
			return err
		}

		commonPrefix, fullActiveDelete, err := compactedReplacementCommonPrefix(ctx, tx, sessionID, startSeq, messages, partsJSON)
		if err != nil {
			return err
		}

		deleteFrom := startSeq + commonPrefix
		if fullActiveDelete {
			commonPrefix = 0
			deleteFrom = startSeq
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM messages WHERE session_id = ? AND sequence >= ?", sessionID, deleteFrom); err != nil {
			return fmt.Errorf("delete compacted messages: %w", err)
		}

		if commonPrefix < len(messages) {
			insertStmt, err := tx.PrepareContext(ctx, `
				INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				return fmt.Errorf("prepare compacted message insert: %w", err)
			}
			defer insertStmt.Close()

			for i := commonPrefix; i < len(messages); i++ {
				msg := messages[i]
				createdAt := msg.CreatedAt
				if createdAt.IsZero() {
					createdAt = time.Now()
				}
				_, err = insertStmt.ExecContext(ctx,
					sessionID, string(msg.Role), partsJSON[i], msg.TextContent, msg.DurationMs, msg.TurnIndex, createdAt, startSeq+i, msg.CompactionTail)
				if err != nil {
					return fmt.Errorf("insert compacted message %d: %w", i, err)
				}
			}
		}

		now := time.Now()
		if err := s.updateReplaceMessagesSessionMetadata(ctx, tx, sessionID, now, false); err != nil {
			return err
		}

		return tx.Commit()
	})
}

func (s *SQLiteStore) compactionStartSeq(ctx context.Context, tx *sql.Tx, sessionID string) (int, error) {
	if !s.hasCompactionSeq {
		return 0, fmt.Errorf("replace compacted messages: compaction boundary unsupported")
	}

	var startSeq int
	if s.hasCompactionCount {
		var compactionCount int
		err := tx.QueryRowContext(ctx, "SELECT compaction_seq, COALESCE(compaction_count, 0) FROM sessions WHERE id = ?", sessionID).Scan(&startSeq, &compactionCount)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, fmt.Errorf("load compaction boundary: %w", err)
		}
		if startSeq < 0 || (startSeq == 0 && compactionCount <= 0) {
			return 0, fmt.Errorf("replace compacted messages: session %s has no compaction boundary", sessionID)
		}
		return startSeq, nil
	}

	err := tx.QueryRowContext(ctx, "SELECT compaction_seq FROM sessions WHERE id = ?", sessionID).Scan(&startSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("load compaction boundary: %w", err)
	}
	if startSeq < 0 {
		return 0, fmt.Errorf("replace compacted messages: session %s has no compaction boundary", sessionID)
	}
	return startSeq, nil
}

func compactedReplacementCommonPrefix(ctx context.Context, tx *sql.Tx, sessionID string, startSeq int, desired []Message, desiredPartsJSON []string) (prefix int, fullActiveDelete bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, role, parts, text_content, duration_ms, turn_index
		FROM messages
		WHERE session_id = ? AND sequence >= ?
		ORDER BY sequence ASC, id ASC`, sessionID, startSeq)
	if err != nil {
		return 0, false, fmt.Errorf("query compacted messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sequence int
		var role, partsJSON string
		var textContent sql.NullString
		var durationMs sql.NullInt64
		var turnIndex sql.NullInt64
		if err := rows.Scan(&sequence, &role, &partsJSON, &textContent, &durationMs, &turnIndex); err != nil {
			return 0, false, fmt.Errorf("scan compacted message: %w", err)
		}
		if sequence < startSeq {
			return 0, true, nil
		}

		expectedSeq := startSeq + prefix
		if prefix >= len(desired) {
			if sequence < expectedSeq {
				return 0, true, nil
			}
			break
		}
		if sequence != expectedSeq {
			if sequence < expectedSeq {
				return 0, true, nil
			}
			break
		}

		want := desired[prefix]
		if role != string(want.Role) ||
			partsJSON != desiredPartsJSON[prefix] ||
			nullStringValue(textContent) != want.TextContent ||
			nullInt64Value(durationMs) != want.DurationMs ||
			int(nullInt64Value(turnIndex)) != want.TurnIndex {
			break
		}
		prefix++
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return prefix, false, nil
}

func (s *SQLiteStore) updateReplaceMessagesSessionMetadata(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time, clearCompactionBoundary bool) error {
	setClauses := []string{
		"updated_at = ?",
	}
	args := []any{now}
	if clearCompactionBoundary {
		setClauses = append(setClauses, "user_turns = (SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'user')")
		args = append(args, sessionID)
	}

	if s.hasLastUserMessageAt {
		// Preserve existing user-activity semantics: every persisted user row counts,
		// including any internal context-summary user rows that ReplaceMessages is
		// asked to make part of the complete transcript.
		setClauses = append(setClauses, "last_user_message_at = (SELECT MAX(created_at) FROM messages WHERE session_id = ? AND role = 'user')")
		args = append(args, sessionID)
	}
	if s.hasLastMessageAt {
		// last_message_at drives visible conversation activity and matches
		// insertMessageAndBumpSession: user/assistant rows count unless they are
		// retained compaction-tail context.
		visibleTailClause := ""
		if s.hasMessageCompactionTail {
			visibleTailClause = " AND COALESCE(compaction_tail, FALSE) = FALSE"
		}
		setClauses = append(setClauses, "last_message_at = (SELECT MAX(created_at) FROM messages WHERE session_id = ? AND role IN ('user', 'assistant')"+visibleTailClause+")")
		args = append(args, sessionID)
	}
	if clearCompactionBoundary {
		if s.hasCompactionSeq {
			setClauses = append(setClauses, "compaction_seq = -1")
		}
		if s.hasCompactionCount {
			setClauses = append(setClauses, "compaction_count = 0")
		}
	}

	args = append(args, sessionID)
	query := "UPDATE sessions SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update session replacement metadata: %w", err)
	}
	return nil
}

func prepareReplacementMessageParts(messages []Message, stripImageBase64 bool) ([]string, error) {
	partsJSON := make([]string, len(messages))
	for i := range messages {
		parts, err := messages[i].PartsJSONForStorage(stripImageBase64)
		if err != nil {
			return nil, fmt.Errorf("serialize parts for message %d: %w", i, err)
		}
		partsJSON[i] = parts
	}
	return partsJSON, nil
}

func replacementCommonPrefix(ctx context.Context, tx *sql.Tx, sessionID string, desired []Message, desiredPartsJSON []string) (prefix int, fullDelete bool, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sequence, role, parts, text_content, duration_ms, turn_index, COALESCE(compaction_tail, FALSE)
		FROM messages
		WHERE session_id = ?
		ORDER BY sequence ASC, id ASC`, sessionID)
	if err != nil {
		return 0, false, fmt.Errorf("query existing messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sequence int
		var role, partsJSON string
		var textContent sql.NullString
		var durationMs sql.NullInt64
		var turnIndex sql.NullInt64
		var compactionTail bool
		if err := rows.Scan(&sequence, &role, &partsJSON, &textContent, &durationMs, &turnIndex, &compactionTail); err != nil {
			return 0, false, fmt.Errorf("scan existing message: %w", err)
		}
		if sequence < 0 {
			return 0, true, nil
		}

		if prefix >= len(desired) {
			if sequence < prefix {
				return 0, true, nil
			}
			break
		}
		if sequence != prefix {
			// A duplicate/out-of-order sequence below the already accepted prefix
			// cannot be removed by deleting sequence >= prefix, so fall back to a full
			// rewrite. Gaps above the prefix are safely truncated from the gap onward.
			if sequence < prefix {
				return 0, true, nil
			}
			break
		}

		want := desired[prefix]
		if role != string(want.Role) ||
			partsJSON != desiredPartsJSON[prefix] ||
			nullStringValue(textContent) != want.TextContent ||
			nullInt64Value(durationMs) != want.DurationMs ||
			int(nullInt64Value(turnIndex)) != want.TurnIndex ||
			compactionTail {
			break
		}
		prefix++
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return prefix, false, nil
}

func nullStringValue(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func nullInt64Value(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

// CompactMessages appends compacted messages to the session, preserving old
// history, and updates compaction_seq so that resume loads only post-compaction
// messages. Old messages remain in the database for scrollback/history.
func (s *SQLiteStore) CompactMessages(ctx context.Context, sessionID string, messages []Message) error {
	return retryOnBusy(ctx, 5, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin transaction: %w", err)
		}
		defer tx.Rollback()

		// Find the current max sequence number
		var maxSeq int
		err = tx.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(sequence), -1) FROM messages WHERE session_id = ?",
			sessionID).Scan(&maxSeq)
		if err != nil {
			return fmt.Errorf("get max sequence: %w", err)
		}
		startSeq := maxSeq + 1

		insertStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare message insert: %w", err)
		}
		defer insertStmt.Close()

		// Insert new messages starting after the existing ones
		for i, msg := range messages {
			msg.SessionID = sessionID
			msg.Sequence = startSeq + i
			if msg.CreatedAt.IsZero() {
				msg.CreatedAt = time.Now()
			}

			partsJSON, err := msg.PartsJSONForStorage(s.cfg.StripImageBase64)
			if err != nil {
				return fmt.Errorf("serialize parts for message %d: %w", i, err)
			}

			_, err = insertStmt.ExecContext(ctx,
				sessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.TurnIndex, msg.CreatedAt, msg.Sequence, msg.CompactionTail)
			if err != nil {
				return fmt.Errorf("insert message %d: %w", i, err)
			}
		}

		// Update compaction boundary/count and timestamp. Older read-only schemas
		// cannot reach this write path because migrations run before opening.
		now := time.Now()
		if s.hasCompactionCount {
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET compaction_seq = ?, compaction_count = COALESCE(compaction_count, 0) + 1, updated_at = ? WHERE id = ?",
				startSeq, now, sessionID); err != nil {
				return fmt.Errorf("update compaction metrics: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET compaction_seq = ?, updated_at = ? WHERE id = ?",
			startSeq, now, sessionID); err != nil {
			return fmt.Errorf("update compaction_seq: %w", err)
		}

		return tx.Commit()
	})
}

func (s *SQLiteStore) messageSelectCols() string {
	compactionTailCol := "FALSE AS compaction_tail"
	if s.hasMessageCompactionTail {
		compactionTailCol = "COALESCE(compaction_tail, FALSE) AS compaction_tail"
	}
	return `id, session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, ` + compactionTailCol
}

// GetMessagesFrom retrieves messages for a session starting from a given
// sequence number. Used on resume and for keyset-style pagination when walking
// long transcripts.
// When limit <= 0, all rows at/after fromSeq are returned.
func (s *SQLiteStore) GetMessagesFrom(ctx context.Context, sessionID string, fromSeq, limit int) ([]Message, error) {
	query := `
		SELECT ` + s.messageSelectCols() + `
		FROM messages
		WHERE session_id = ? AND sequence >= ?
		ORDER BY sequence ASC`
	args := []any{sessionID, fromSeq}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages from seq %d: %w", fromSeq, err)
	}
	defer rows.Close()

	return scanMessageRows(rows)
}

func scanMessageRows(rows *sql.Rows) ([]Message, error) {
	var messages []Message
	for rows.Next() {
		var msg Message
		var partsJSON string
		var durationMs sql.NullInt64
		err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Role, &partsJSON,
			&msg.TextContent, &durationMs, &msg.TurnIndex, &msg.CreatedAt, &msg.Sequence, &msg.CompactionTail)
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

// GetMessagesPageDescending retrieves messages for a session in reverse
// sequence order. When beforeSeq > 0, only rows with sequence < beforeSeq are
// returned. Used for reverse tail pagination without loading entire sessions.
func (s *SQLiteStore) GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]Message, error) {
	query := `
		SELECT ` + s.messageSelectCols() + `
		FROM messages
		WHERE session_id = ?`
	args := []any{sessionID}
	if beforeSeq > 0 {
		query += " AND sequence < ?"
		args = append(args, beforeSeq)
	}
	query += " ORDER BY sequence DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages descending: %w", err)
	}
	defer rows.Close()

	return scanMessageRows(rows)
}

// GetMessageByID retrieves a single message by its global message id.
func (s *SQLiteStore) GetMessageByID(ctx context.Context, msgID int64) (*Message, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+s.messageSelectCols()+`
		FROM messages
		WHERE id = ?`, msgID)
	var msg Message
	var partsJSON string
	var durationMs sql.NullInt64
	err := row.Scan(&msg.ID, &msg.SessionID, &msg.Role, &partsJSON,
		&msg.TextContent, &durationMs, &msg.TurnIndex, &msg.CreatedAt, &msg.Sequence, &msg.CompactionTail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message: %w", err)
	}
	if durationMs.Valid {
		msg.DurationMs = durationMs.Int64
	}
	if err := msg.SetPartsFromJSON(partsJSON); err != nil {
		return nil, fmt.Errorf("deserialize parts: %w", err)
	}
	return &msg, nil
}

// GetLatestVisibleMessageID retrieves the latest persisted user/assistant message id for a session.
func (s *SQLiteStore) GetLatestVisibleMessageID(ctx context.Context, sessionID string) (int64, error) {
	var msgID int64
	compactionTailClause := ""
	if s.hasMessageCompactionTail {
		compactionTailClause = " AND COALESCE(compaction_tail, FALSE) = FALSE"
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT id
		FROM messages
		WHERE session_id = ? AND role IN (?, ?)`+compactionTailClause+`
		ORDER BY sequence DESC, id DESC
		LIMIT 1`, sessionID, string(llm.RoleUser), string(llm.RoleAssistant)).Scan(&msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query latest visible message id: %w", err)
	}
	return msgID, nil
}

// PreviousUserPrompt returns the newest persisted user prompt older than beforeID.
// When beforeID <= 0, traversal starts at the newest prompt. History is global
// across sessions and optionally filtered by agent.
func (s *SQLiteStore) PreviousUserPrompt(ctx context.Context, agent string, beforeID int64) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		JOIN sessions sess ON sess.id = m.session_id
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND (? = '' OR COALESCE(sess.agent, '') = '' OR COALESCE(sess.agent, '') = ?)
		  AND (? <= 0 OR m.id < ?)
		ORDER BY m.id DESC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, agent, agent, beforeID, beforeID)
}

// NextUserPrompt returns the oldest persisted user prompt newer than afterID.
func (s *SQLiteStore) NextUserPrompt(ctx context.Context, agent string, afterID int64) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		JOIN sessions sess ON sess.id = m.session_id
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND (? = '' OR COALESCE(sess.agent, '') = '' OR COALESCE(sess.agent, '') = ?)
		  AND m.id > ?
		ORDER BY m.id ASC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, agent, agent, afterID)
}

// PreviousUserPromptOutsideSession returns the newest persisted user prompt older
// than the cursor, ordered by message timestamp across all agents, excluding the
// current session.
func (s *SQLiteStore) PreviousUserPromptOutsideSession(ctx context.Context, excludeSessionID string, beforeID int64, beforeCreatedAt time.Time) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND (? = '' OR m.session_id <> ?)`
	args := []any{len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, excludeSessionID, excludeSessionID}
	if beforeID > 0 {
		query += `
		  AND m.id <> ?
		  AND (m.created_at, m.id) < (?, ?)`
		args = append(args, beforeID, beforeCreatedAt, beforeID)
	}
	query += `
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, args...)
}

// NextUserPromptOutsideSession returns the oldest persisted user prompt newer
// than the cursor, ordered by message timestamp across all agents, excluding the
// current session.
func (s *SQLiteStore) NextUserPromptOutsideSession(ctx context.Context, excludeSessionID string, afterID int64, afterCreatedAt time.Time) (*PromptHistoryEntry, error) {
	query := `
		SELECT m.id, m.text_content, m.created_at
		FROM messages m
		WHERE m.role = 'user'
		  AND TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)) <> ''
		  AND substr(TRIM(COALESCE(m.text_content, ''), char(9) || char(10) || char(11) || char(12) || char(13) || char(32)), 1, ?) <> ?
		  AND COALESCE(m.compaction_tail, FALSE) = FALSE
		  AND (? = '' OR m.session_id <> ?)`
	args := []any{len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix, excludeSessionID, excludeSessionID}
	if afterID > 0 {
		query += `
		  AND m.id <> ?
		  AND (m.created_at, m.id) > (?, ?)`
		args = append(args, afterID, afterCreatedAt, afterID)
	}
	query += `
		ORDER BY m.created_at ASC, m.id ASC
		LIMIT 1`
	return s.queryPromptHistoryEntry(ctx, query, args...)
}

func (s *SQLiteStore) queryPromptHistoryEntry(ctx context.Context, query string, args ...any) (*PromptHistoryEntry, error) {
	var entry PromptHistoryEntry
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&entry.ID, &entry.Text, &entry.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query prompt history: %w", err)
	}
	return &entry, nil
}

// GetMessages retrieves messages for a session.
func (s *SQLiteStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]Message, error) {
	query := `
		SELECT ` + s.messageSelectCols() + `
		FROM messages
		WHERE session_id = ?
		ORDER BY sequence ASC`

	args := []any{sessionID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	} else if offset > 0 {
		// SQLite requires LIMIT before OFFSET; use -1 to mean "all rows".
		query += " LIMIT -1"
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
			&msg.TextContent, &durationMs, &msg.TurnIndex, &msg.CreatedAt, &msg.Sequence, &msg.CompactionTail)
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

// SavePushSubscription upserts a Web Push subscription.
func (s *SQLiteStore) SavePushSubscription(ctx context.Context, sub *PushSubscription) error {
	if sub.ID == "" {
		sub.ID = NewID()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO push_subscriptions (id, endpoint, key_p256dh, key_auth)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET
			key_p256dh = excluded.key_p256dh,
			key_auth = excluded.key_auth,
			last_used_at = datetime('now')`,
		sub.ID, sub.Endpoint, sub.KeyP256DH, sub.KeyAuth)
	if err != nil {
		return fmt.Errorf("save push subscription: %w", err)
	}
	return nil
}

// DeletePushSubscription removes a Web Push subscription by endpoint.
func (s *SQLiteStore) DeletePushSubscription(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	if err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	return nil
}

// ListPushSubscriptions returns all stored Web Push subscriptions.
func (s *SQLiteStore) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, endpoint, key_p256dh, key_auth
		FROM push_subscriptions
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []PushSubscription
	for rows.Next() {
		var sub PushSubscription
		if err := rows.Scan(&sub.ID, &sub.Endpoint, &sub.KeyP256DH, &sub.KeyAuth); err != nil {
			return nil, fmt.Errorf("scan push subscription: %w", err)
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// Close closes the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// setCurrentColumns records optional columns that are guaranteed to exist after
// read-write initialization/migration reaches the current schema.
func (s *SQLiteStore) setCurrentColumns() {
	s.hasGeneratedTitles = true
	s.hasCompactionSeq = true
	s.hasCompactionCount = true
	s.hasCacheWriteTokens = true
	s.hasOrigin = true
	s.hasPinned = true
	s.hasTitleSkippedAt = true
	s.hasLastUserMessageAt = true
	s.hasLastMessageAt = true
	s.hasLastTotalTokens = true
	s.hasLastMessageCount = true
	s.hasMessageCount = true
	s.hasReasoningEffort = true
	s.hasReasoningMode = true
	s.hasApprovalMode = true
	s.hasWorktreeDir = true
	s.hasGoal = true
	s.hasShare = true
	s.hasMessageCompactionTail = true
}

// probeSessionColumns checks optional session columns in a single PRAGMA scan.
// Read-only mode skips migrations and may open a database created before one or
// more optional columns existed, so callers use these flags to build compatible
// SELECT/UPDATE statements.
func (s *SQLiteStore) probeSessionColumns() {
	rows, err := s.db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return
		}
		switch name {
		case "generated_short_title":
			s.hasGeneratedTitles = true
		case "compaction_seq":
			s.hasCompactionSeq = true
		case "compaction_count":
			s.hasCompactionCount = true
		case "cache_write_tokens":
			s.hasCacheWriteTokens = true
		case "origin":
			s.hasOrigin = true
		case "pinned":
			s.hasPinned = true
		case "title_skipped_at":
			s.hasTitleSkippedAt = true
		case "last_user_message_at":
			s.hasLastUserMessageAt = true
		case "last_message_at":
			s.hasLastMessageAt = true
		case "last_total_tokens":
			s.hasLastTotalTokens = true
		case "last_message_count":
			s.hasLastMessageCount = true
		case "message_count":
			s.hasMessageCount = true
		case "reasoning_effort":
			s.hasReasoningEffort = true
		case "reasoning_mode":
			s.hasReasoningMode = true
		case "approval_mode":
			s.hasApprovalMode = true
		case "worktree_dir":
			s.hasWorktreeDir = true
		case "goal":
			s.hasGoal = true
		case "share":
			s.hasShare = true
		}
	}
}

func (s *SQLiteStore) probeMessageColumns() {
	rows, err := s.db.Query("PRAGMA table_info(messages)")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			return
		}
		if name == "compaction_tail" {
			s.hasMessageCompactionTail = true
		}
	}
}

// sessionSelectCols returns the SELECT column list for session queries.
// Excludes compaction_seq when the column doesn't exist (old DB in read-only mode).
func (s *SQLiteStore) sessionSelectCols() string {
	base := `id, number, name, summary`
	if s.hasGeneratedTitles {
		base += ", generated_short_title, generated_long_title, title_source, title_generated_at, title_basis_msg_seq"
		if s.hasTitleSkippedAt {
			base += ", title_skipped_at"
		} else {
			base += ", NULL AS title_skipped_at"
		}
	}
	base += `,
	       provider, provider_key, model`
	if s.hasReasoningEffort {
		base += ", reasoning_effort"
	} else {
		base += ", NULL AS reasoning_effort"
	}
	if s.hasReasoningMode {
		base += ", reasoning_mode"
	} else {
		base += ", NULL AS reasoning_mode"
	}
	base += `, mode`
	if s.hasApprovalMode {
		base += ", approval_mode"
	} else {
		base += ", NULL AS approval_mode"
	}
	if s.hasOrigin {
		base += ", origin"
	} else {
		base += ", 'tui' AS origin"
	}
	if s.hasPinned {
		base += ", pinned"
	} else {
		base += ", FALSE AS pinned"
	}
	base += `, agent, cwd`
	if s.hasWorktreeDir {
		base += ", worktree_dir"
	} else {
		base += ", NULL AS worktree_dir"
	}
	base += `, created_at, updated_at, archived, parent_id, search, tools, mcp,
	       user_turns, llm_turns, tool_calls, input_tokens, cached_input_tokens`
	if s.hasCacheWriteTokens {
		base += ", cache_write_tokens"
	}
	base += ", output_tokens"
	if s.hasLastTotalTokens {
		base += ", last_total_tokens"
	}
	if s.hasLastMessageCount {
		base += ", last_message_count"
	}
	base += ", status, tags"
	if s.hasGoal {
		base += ", goal"
	} else {
		base += ", NULL AS goal"
	}
	if s.hasShare {
		base += ", share"
	} else {
		base += ", NULL AS share"
	}
	if s.hasCompactionSeq {
		base += ", compaction_seq"
	}
	if s.hasCompactionCount {
		base += ", compaction_count"
	}
	return base
}

// scanSessionRow scans a session row into a Session struct. The flags
// determine which optional columns are present in the result set.
func scanSessionRow(row *sql.Row, hasGeneratedTitles, hasCacheWriteTokens, hasCompactionSeq, hasCompactionCount, hasTitleSkippedAt, hasLastTotalTokens, hasLastMessageCount bool) (*Session, error) {
	var sess Session
	var number sql.NullInt64
	var name, summary, cwd, worktreeDir sql.NullString
	var generatedShortTitle, generatedLongTitle, titleSource sql.NullString
	var titleGeneratedAt, titleSkippedAt sql.NullTime
	var mode, approvalMode, origin, agent, parentID, tools, mcp, status, tags, providerKey, reasoningEffort, reasoningMode, goalRaw, shareRaw sql.NullString

	var scanArgs []any
	scanArgs = append(scanArgs, &sess.ID, &number, &name, &summary)
	if hasGeneratedTitles {
		scanArgs = append(scanArgs, &generatedShortTitle, &generatedLongTitle, &titleSource, &titleGeneratedAt, &sess.TitleBasisMsgSeq)
		if hasTitleSkippedAt {
			scanArgs = append(scanArgs, &titleSkippedAt)
		}
	}
	scanArgs = append(scanArgs,
		&sess.Provider, &providerKey, &sess.Model, &reasoningEffort, &reasoningMode, &mode, &approvalMode, &origin, &sess.Pinned,
		&agent, &cwd, &worktreeDir, &sess.CreatedAt, &sess.UpdatedAt, &sess.Archived, &parentID,
		&sess.Search, &tools, &mcp,
		&sess.UserTurns, &sess.LLMTurns, &sess.ToolCalls, &sess.InputTokens, &sess.CachedInputTokens,
	)
	if hasCacheWriteTokens {
		scanArgs = append(scanArgs, &sess.CacheWriteTokens)
	}
	scanArgs = append(scanArgs, &sess.OutputTokens)
	if hasLastTotalTokens {
		scanArgs = append(scanArgs, &sess.LastTotalTokens)
	}
	if hasLastMessageCount {
		scanArgs = append(scanArgs, &sess.LastMessageCount)
	}
	scanArgs = append(scanArgs, &status, &tags, &goalRaw, &shareRaw)
	if hasCompactionSeq {
		scanArgs = append(scanArgs, &sess.CompactionSeq)
	}
	if hasCompactionCount {
		scanArgs = append(scanArgs, &sess.CompactionCount)
	}

	err := row.Scan(scanArgs...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	// Default compaction_seq when column is absent
	if !hasCompactionSeq {
		sess.CompactionSeq = -1
	}
	if number.Valid {
		sess.Number = number.Int64
	}
	if name.Valid {
		sess.Name = name.String
	}
	if summary.Valid {
		sess.Summary = summary.String
	}
	if hasGeneratedTitles {
		if generatedShortTitle.Valid {
			sess.GeneratedShortTitle = generatedShortTitle.String
		}
		if generatedLongTitle.Valid {
			sess.GeneratedLongTitle = generatedLongTitle.String
		}
		if titleSource.Valid {
			sess.TitleSource = SessionTitleSource(titleSource.String)
		}
		if titleGeneratedAt.Valid {
			sess.TitleGeneratedAt = titleGeneratedAt.Time
		}
		if hasTitleSkippedAt && titleSkippedAt.Valid {
			sess.TitleSkippedAt = titleSkippedAt.Time
		}
	}
	if cwd.Valid {
		sess.CWD = cwd.String
	}
	if worktreeDir.Valid {
		sess.WorktreeDir = worktreeDir.String
	}
	if mode.Valid {
		sess.Mode = SessionMode(mode.String)
	}
	if approvalMode.Valid {
		sess.ApprovalMode = SessionApprovalMode(approvalMode.String)
	}
	if origin.Valid {
		sess.Origin = SessionOrigin(origin.String)
	} else {
		sess.Origin = OriginTUI
	}
	if providerKey.Valid {
		sess.ProviderKey = providerKey.String
	}
	if reasoningEffort.Valid {
		sess.ReasoningEffort = reasoningEffort.String
	}
	if reasoningMode.Valid {
		sess.ReasoningMode = reasoningMode.String
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
	if goal := parseGoalJSONString(goalRaw); goal != nil {
		sess.Goal = goal
	}
	if share := parseShareJSONString(shareRaw); share != nil {
		sess.Share = share
	}
	return &sess, nil
}

func goalJSONString(goal *Goal) sql.NullString {
	if goal == nil || !goal.Exists() {
		return sql.NullString{}
	}
	clone := goal.Clone()
	clone.Normalize(time.Now())
	data, err := json.Marshal(clone)
	if err != nil || len(data) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(data), Valid: true}
}

func parseGoalJSONString(raw sql.NullString) *Goal {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var goal Goal
	if err := json.Unmarshal([]byte(raw.String), &goal); err != nil {
		return nil
	}
	goal.Normalize(time.Now())
	if !goal.Exists() {
		return nil
	}
	return &goal
}

func shareJSONString(share *ShareState) sql.NullString {
	if share == nil || !share.Exists() {
		return sql.NullString{}
	}
	data, err := json.Marshal(share.Clone())
	if err != nil || len(data) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(data), Valid: true}
}

func parseShareJSONString(raw sql.NullString) *ShareState {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var share ShareState
	if err := json.Unmarshal([]byte(raw.String), &share); err != nil || !share.Exists() {
		return nil
	}
	return &share
}

// nullString converts an empty string to NULL for database storage.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
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
