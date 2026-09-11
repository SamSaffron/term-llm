package session

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

func sqliteFileURI(path string) string {
	slashPath := filepath.ToSlash(path)
	windowsDrivePath := len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
	windowsUNCPath := strings.HasPrefix(path, `\\`)
	if windowsDrivePath || windowsUNCPath {
		// Keep URI generation independently testable on non-Windows builders.
		slashPath = strings.ReplaceAll(path, `\`, "/")
	}

	u := url.URL{Scheme: "file", Path: slashPath}
	// Drive-letter paths need a leading slash. UNC paths retain their leading
	// double slash as an empty-authority file URI (file:////server/share), since
	// stock SQLite rejects non-empty authorities other than localhost.
	if windowsDrivePath && !strings.HasPrefix(slashPath, "/") {
		u.Path = "/" + slashPath
	}
	return u.String()
}

// NewSQLiteStore creates a new SQLite-based session store.
func NewSQLiteStore(cfg Config) (*SQLiteStore, error) {
	dbPath, err := ResolveDBPath(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("get db path: %w", err)
	}

	// Retain the resolved identity even if a caller later changes process CWD.
	cfg.Path = dbPath

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
	if dbPath != ":memory:" {
		dsn = sqliteFileURI(dbPath)
	}
	if cfg.ReadOnly && dbPath != ":memory:" {
		dsn += "?mode=ro&_pragma=query_only(1)"
	} else {
		dsn += "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	dsn += "&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(134217728)&_pragma=cache_size(-64000)"
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
	}

	store := &SQLiteStore{db: db, cfg: cfg}

	// Read-write stores have just created or migrated the schema above, so the
	// current optional columns are known to be present. Read-only stores skip
	// migrations and may be pointed at an older DB, so probe the tables once and
	// derive compatibility flags from those scans.
	if cfg.ReadOnly {
		store.probeSessionColumns()
		store.probeMessageColumns()
		store.probeBranchTable()
		store.probeProjectsTable()
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

	// File-backed WAL databases can serve reads while the single writer is
	// active. Keep transcript scans on one dedicated read-only connection so
	// decoding long tool-heavy histories cannot occupy the writer connection.
	// If an unusual filesystem could not enter WAL mode, retain the serialized
	// single-connection behavior rather than introducing rollback-journal locks.
	if dbPath != ":memory:" && !cfg.ReadOnly {
		var journalMode string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
			db.Close()
			return nil, fmt.Errorf("read database journal mode: %w", err)
		}
		if strings.EqualFold(journalMode, "wal") {
			readDSN := sqliteFileURI(dbPath) + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)&_pragma=mmap_size(134217728)&_pragma=cache_size(-64000)"
			openReader := func(purpose string) (*sql.DB, error) {
				reader, err := sql.Open("sqlite", readDSN)
				if err != nil {
					return nil, fmt.Errorf("open %s database: %w", purpose, err)
				}
				reader.SetMaxOpenConns(1)
				reader.SetMaxIdleConns(1)
				if err := reader.Ping(); err != nil {
					reader.Close()
					return nil, fmt.Errorf("connect %s database: %w", purpose, err)
				}
				return reader, nil
			}
			readDB, err := openReader("session read")
			if err != nil {
				db.Close()
				return nil, err
			}
			// WAL readers do not exclude one another. Do not impose an application-
			// level one-reader queue that lets a long transcript scan in one session
			// gate hydration or metadata reads in another.
			readDB.SetMaxOpenConns(0)
			readDB.SetMaxIdleConns(4)
			responseRunReadDB, err := openReader("response-run read")
			if err != nil {
				readDB.Close()
				db.Close()
				return nil, err
			}
			store.readDB = readDB
			store.responseRunReadDB = responseRunReadDB
		}
	}

	return store, nil
}

// schemaVersion is the current schema version.
// - Fresh databases get the full schema from `schema` const and start at this version
// - Existing databases run migrations to reach this version
// Increment when adding new migrations.
const (
	projectSchemaVersion = 47
	schemaVersion        = 57
)

// migration represents a schema migration.
type migration struct {
	version     int
	description string
	up          func(db schemaExecutor) error
}

type schemaExecutor = sqliteutil.Executor

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
			return rebuildMessagesTableForRolesV17(db)
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
			return rebuildMessagesTableForRolesV22(db)
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
			if err := createMessageCountTriggersV26(db); err != nil {
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
			if err := createMessageCountTriggersV27(db); err != nil {
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
	{
		// Version 38 was briefly shipped by the persistent side-session rework.
		// Reserve it so databases on the overlay implementation never reuse that
		// version for a different schema.
		version:     38,
		description: "reserve retired persistent side-session schema version",
		up:          func(schemaExecutor) error { return nil },
	},
	{
		version:     39,
		description: "remove retired persistent side sessions",
		up: func(db schemaExecutor) error {
			rows, err := db.Query("PRAGMA table_info(sessions)")
			if err != nil {
				return err
			}
			hasKind := false
			hasRootID := false
			for rows.Next() {
				var cid int
				var name, columnType string
				var notNull, primaryKey int
				var defaultValue any
				if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
					rows.Close()
					return err
				}
				if name == "kind" {
					hasKind = true
				}
				if name == "root_id" {
					hasRootID = true
				}
			}
			if err := rows.Close(); err != nil {
				return err
			}
			if hasKind {
				// Retired side sessions were never allowed to own durable branches, but
				// clear legacy references defensively so an old/corrupt child cannot
				// prevent the cleanup migration from completing.
				if _, err := db.Exec("UPDATE sessions SET parent_id = NULL WHERE parent_id IN (SELECT id FROM sessions WHERE kind = 'side')"); err != nil {
					return err
				}
				if hasRootID {
					if _, err := db.Exec("UPDATE sessions SET root_id = NULL WHERE root_id IN (SELECT id FROM sessions WHERE kind = 'side') AND kind <> 'side'"); err != nil {
						return err
					}
				}
				if _, err := db.Exec("DELETE FROM sessions WHERE kind = 'side'"); err != nil {
					return err
				}
			}
			for _, stmt := range []string{
				"DROP TABLE IF EXISTS side_context_messages",
				"DROP INDEX IF EXISTS idx_sessions_one_open_side",
				"DROP INDEX IF EXISTS idx_sessions_root_kind",
			} {
				if _, err := db.Exec(stmt); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		version:     40,
		description: "create session plan snapshots table",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_plans (
				session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
				snapshot TEXT NOT NULL,
				version INTEGER NOT NULL DEFAULT 1,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			)`)
			return err
		},
	},
	{
		version:     41,
		description: "add durable transcript revision",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN transcript_rev INTEGER NOT NULL DEFAULT 0"); err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err := db.Exec("UPDATE sessions SET transcript_rev = COALESCE(message_count, 0)")
			return err
		},
	},
	{
		version:     42,
		description: "add response scoped transcript segment identity",
		up: func(db schemaExecutor) error {
			statements := []string{
				"ALTER TABLE messages ADD COLUMN response_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE messages ADD COLUMN assistant_segment_ordinal INTEGER NOT NULL DEFAULT -1",
				"ALTER TABLE messages ADD COLUMN segment_start_sequence INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE messages ADD COLUMN segment_end_sequence INTEGER NOT NULL DEFAULT 0",
			}
			for _, statement := range statements {
				if _, err := db.Exec(statement); err != nil && !isDuplicateColumnError(err) {
					return err
				}
			}
			return nil
		},
	},
	{
		version:     43,
		description: "add stable client message identity",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec("ALTER TABLE messages ADD COLUMN client_message_id TEXT NOT NULL DEFAULT ''"); err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_client_message_id ON messages(session_id, client_message_id) WHERE client_message_id <> ''")
			return err
		},
	},
	{
		version:     44,
		description: "create durable transcript redo stack",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_redo (
				session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				stack_pos INTEGER NOT NULL,
				suffix TEXT NOT NULL,
				metadata TEXT NOT NULL,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (session_id, stack_pos)
			)`)
			return err
		},
	},
	{
		version:     45,
		description: "create conversation branch edges",
		up: func(db schemaExecutor) error {
			for _, statement := range []string{
				`CREATE TABLE IF NOT EXISTS session_branches (
					child_session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
					parent_session_id TEXT NOT NULL,
					fork_after_message_id INTEGER,
					fork_after_sequence INTEGER NOT NULL,
					idempotency_key TEXT NOT NULL DEFAULT '',
					created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
				)`,
				`CREATE UNIQUE INDEX IF NOT EXISTS idx_session_branches_idempotency
					ON session_branches(parent_session_id, idempotency_key)
					WHERE idempotency_key <> ''`,
				`CREATE INDEX IF NOT EXISTS idx_session_branches_parent
					ON session_branches(parent_session_id)`,
			} {
				if _, err := db.Exec(statement); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		version:     46,
		description: "create session workspace grants",
		up: func(db schemaExecutor) error {
			for _, statement := range []string{
				`CREATE TABLE IF NOT EXISTS session_workspace_grants (
					session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
					id TEXT NOT NULL,
					path TEXT NOT NULL,
					access TEXT NOT NULL CHECK (access IN ('read', 'write')),
					provenance TEXT NOT NULL,
					rationale TEXT NOT NULL,
					created_at TIMESTAMP NOT NULL,
					updated_at TIMESTAMP NOT NULL,
					PRIMARY KEY (session_id, id),
					UNIQUE (session_id, path)
				)`,
				`CREATE INDEX IF NOT EXISTS idx_session_workspace_grants_session
					ON session_workspace_grants(session_id, created_at, id)`,
			} {
				if _, err := db.Exec(statement); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		version:     projectSchemaVersion,
		description: "create projects and associate sessions",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN project_id TEXT"); err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err := db.Exec(projectsSchemaV47)
			return err
		},
	},
	{
		// Schema version alone is not sufficient evidence that every version 47
		// database has the project objects. Reconcile them idempotently.
		version:     48,
		description: "repair missing project schema",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN project_id TEXT"); err != nil && !isDuplicateColumnError(err) {
				return err
			}
			_, err := db.Exec(projectsSchemaV47)
			return err
		},
	},
	{
		version:     49,
		description: "publish complete session schema and singleton marker",
		up: func(db schemaExecutor) error {
			// This forward reconciliation owns objects that older releases created
			// outside their migration loop. It runs only after historical migrations,
			// never as an upgrade prelude.
			if _, err := db.Exec(canonicalSessionSchema); err != nil {
				return fmt.Errorf("reconcile canonical session schema: %w", err)
			}
			if err := ensureCurrentSessionIndexes(db); err != nil {
				return err
			}
			if err := createMessageCountTriggersV27(db); err != nil {
				return fmt.Errorf("install current message count triggers: %w", err)
			}
			if _, err := db.Exec(fmt.Sprintf(`UPDATE sessions SET message_count = COALESCE((SELECT COUNT(*) FROM messages m WHERE m.session_id = sessions.id AND %s), 0)`, countableConversationMessageSQL("m", true))); err != nil {
				return fmt.Errorf("backfill current message counts: %w", err)
			}
			return normalizeSessionMarker(db, 48)
		},
	},
	{
		version:     50,
		description: "persist pending session interjections",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_pending_interjections (
				session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				id TEXT NOT NULL,
				message TEXT NOT NULL,
				display_text TEXT NOT NULL DEFAULT '',
				attachment_summary TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (session_id, id)
			);
			CREATE INDEX IF NOT EXISTS idx_session_pending_interjections_order
				ON session_pending_interjections(session_id, created_at, id);`)
			return err
		},
	},
	{
		version:     51,
		description: "add push subscription lifecycle and completion outbox",
		up: func(db schemaExecutor) error {
			for _, statement := range []string{
				"ALTER TABLE push_subscriptions ADD COLUMN status TEXT NOT NULL DEFAULT 'active'",
				"ALTER TABLE push_subscriptions ADD COLUMN vapid_key_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE push_subscriptions ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE push_subscriptions ADD COLUMN last_failure_code TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE push_subscriptions ADD COLUMN last_failure TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE push_subscriptions ADD COLUMN last_failure_at TEXT",
			} {
				if _, err := db.Exec(statement); err != nil && !isDuplicateColumnError(err) {
					return err
				}
			}
			if _, err := db.Exec("UPDATE push_subscriptions SET updated_at = COALESCE(NULLIF(updated_at, ''), created_at, datetime('now'))"); err != nil {
				return err
			}
			_, err := db.Exec(`CREATE TABLE IF NOT EXISTS completion_push_outbox (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				event_id TEXT NOT NULL UNIQUE,
				response_id TEXT NOT NULL,
				subscription_id TEXT NOT NULL,
				payload BLOB NOT NULL,
				status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'dead')),
				attempt_count INTEGER NOT NULL DEFAULT 0,
				next_attempt_at TEXT NOT NULL DEFAULT (datetime('now')),
				last_error TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				updated_at TEXT NOT NULL DEFAULT (datetime('now')),
				UNIQUE(response_id, subscription_id)
			);
			CREATE INDEX IF NOT EXISTS idx_completion_push_outbox_due
				ON completion_push_outbox(status, next_attempt_at, id);`)
			return err
		},
	},
	{
		version:     52,
		description: "add indexed session change log",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(changeLogSchemaV52)
			return err
		},
	},
	{
		version:     53,
		description: "audit runtime identity metadata changes",
		up: func(db schemaExecutor) error {
			if _, err := db.Exec(`DROP TRIGGER IF EXISTS session_change_log_session_metadata`); err != nil {
				return err
			}
			_, err := db.Exec(changeLogSchemaV52)
			return err
		},
	},
	{
		version:     54,
		description: "add durable serve response lifecycle and attention watermarks",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(attentionSchemaV54)
			return err
		},
	},
	{
		version:     55,
		description: "add response interaction-required projection",
		up: func(db schemaExecutor) error {
			for _, statement := range []string{
				"ALTER TABLE serve_response_lifecycle ADD COLUMN interaction_required_count INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE serve_response_lifecycle ADD COLUMN interaction_required_kinds TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE serve_response_lifecycle ADD COLUMN interaction_required_since INTEGER",
				"ALTER TABLE serve_response_lifecycle ADD COLUMN interaction_state_rev INTEGER NOT NULL DEFAULT 0",
			} {
				if _, err := db.Exec(statement); err != nil && !isDuplicateColumnError(err) {
					return err
				}
			}
			return nil
		},
	},
	{
		version:     56,
		description: "rename pending steering and persist FIFO provenance and ownership",
		up: func(db schemaExecutor) error {
			var oldExists int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_pending_interjections'`).Scan(&oldExists); err != nil {
				return err
			}
			if oldExists == 0 {
				return nil
			} // canonical-schema fixture/bootstrap
			var newExists int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_pending_steering'`).Scan(&newExists); err != nil {
				return err
			}
			if newExists != 0 {
				var count int
				if err := db.QueryRow(`SELECT count(*) FROM session_pending_steering`).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					return fmt.Errorf("refusing to replace a populated steering table")
				}
			}
			_, err := db.Exec(steeringMigrationV56)
			return err
		},
	},
	{
		version:     57,
		description: "durable steering rush operations",
		up: func(db schemaExecutor) error {
			_, err := db.Exec(rushSchemaV57)
			return err
		},
	},
}

// Keep in sync with llm.IsInternalCompactionSummaryText. SQLite migrations and
// triggers need a SQL form of the same prefix check.
const internalCompactionSummarySQLPrefix = "[Context Compaction]"

func trimmedMessageTextSQL(alias string) string {
	// SQLite's one-argument TRIM only removes spaces. Include common ASCII
	// whitespace; persisted model/tool text that is only whitespace is expected to
	// use these characters.
	textCol := qualifiedMessageColumn(alias, "text_content")
	trimChars := "char(9) || char(10) || char(11) || char(12) || char(13) || char(32)"
	return "TRIM(COALESCE(" + textCol + ", ''), " + trimChars + ")"
}

func realUserMessageSQL(alias string, excludeCompactionTail bool) string {
	roleCol := qualifiedMessageColumn(alias, "role")
	textExpr := trimmedMessageTextSQL(alias)
	predicate := fmt.Sprintf("(%s = 'user' AND substr(%s, 1, %d) <> '%s')",
		roleCol, textExpr, len(internalCompactionSummarySQLPrefix), internalCompactionSummarySQLPrefix)
	if excludeCompactionTail {
		predicate = "(COALESCE(" + qualifiedMessageColumn(alias, "compaction_tail") + ", FALSE) = FALSE AND " + predicate + ")"
	}
	return predicate
}

func countableConversationMessageSQL(alias string, includeCompactionTail bool) string {
	roleCol := qualifiedMessageColumn(alias, "role")
	textExpr := trimmedMessageTextSQL(alias)

	userPredicate := realUserMessageSQL(alias, false)
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

func (s *SQLiteStore) conversationMessageCountSelectSQL(sessionAlias string) string {
	if s.hasMessageCount {
		return "COALESCE(" + qualifiedMessageColumn(sessionAlias, "message_count") + ", 0)"
	}
	if !s.hasMessagesTable {
		return "0"
	}
	return "(SELECT COUNT(*) FROM messages WHERE session_id = " + qualifiedMessageColumn(sessionAlias, "id") +
		" AND " + countableConversationMessageSQL("", s.hasMessageCompactionTail) + ")"
}

func createMessageCountTriggersV26(db schemaExecutor) error {
	oldCountable := "(COALESCE(old.compaction_tail, FALSE) = FALSE AND old.role IN ('user', 'assistant'))"
	newCountable := "(COALESCE(new.compaction_tail, FALSE) = FALSE AND new.role IN ('user', 'assistant'))"
	return installMessageCountTriggers(db, oldCountable, newCountable)
}

func createMessageCountTriggersV27(db schemaExecutor) error {
	oldCountable := countableConversationMessageSQL("old", true)
	newCountable := countableConversationMessageSQL("new", true)
	return installMessageCountTriggers(db, oldCountable, newCountable)
}

func installMessageCountTriggers(db schemaExecutor, oldCountable, newCountable string) error {
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

func rebuildMessagesTableForRolesV17(db schemaExecutor) error {
	return rebuildMessagesTableHistorical(db, "'user', 'assistant', 'system', 'tool', 'developer'")
}

func rebuildMessagesTableForRolesV22(db schemaExecutor) error {
	return rebuildMessagesTableHistorical(db, "'user', 'assistant', 'system', 'tool', 'developer', 'event'")
}

// rebuildMessagesTableHistorical intentionally freezes the v17/v22 table shape.
// Later canonical columns are added only by their owning migrations.
func rebuildMessagesTableHistorical(db schemaExecutor, roles string) error {
	statements := []string{
		`DROP TRIGGER IF EXISTS messages_ai`,
		`DROP TRIGGER IF EXISTS messages_ad`,
		`DROP TRIGGER IF EXISTS messages_au`,
		`DROP TRIGGER IF EXISTS messages_count_ai`,
		`DROP TRIGGER IF EXISTS messages_count_ad`,
		`DROP TRIGGER IF EXISTS messages_count_au`,
		`DROP INDEX IF EXISTS idx_messages_session_id`,
		`DROP INDEX IF EXISTS idx_messages_session_sequence`,
		`DROP INDEX IF EXISTS idx_messages_session_role`,
		`DROP INDEX IF EXISTS idx_messages_role_id`,
		`DROP INDEX IF EXISTS idx_messages_role_created_id`,
		`DROP INDEX IF EXISTS idx_messages_client_message_id`,
		`DROP TABLE IF EXISTS messages_fts`,
		`ALTER TABLE messages RENAME TO messages_old`,
		fmt.Sprintf(`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			role TEXT NOT NULL CHECK (role IN (%s)),
			parts TEXT NOT NULL,
			text_content TEXT,
			duration_ms INTEGER,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			sequence INTEGER NOT NULL
		)`, roles),
		`INSERT INTO messages (id, session_id, role, parts, text_content, duration_ms, created_at, sequence)
		 SELECT id, session_id, role, parts, text_content, duration_ms, created_at, sequence FROM messages_old`,
		`DROP TABLE messages_old`,
		`CREATE INDEX idx_messages_session_id ON messages(session_id, sequence)`,
		`CREATE UNIQUE INDEX idx_messages_session_sequence ON messages(session_id, sequence)`,
		`CREATE INDEX idx_messages_session_role ON messages(session_id, role)`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(text_content, content='messages', content_rowid='id')`,
		`INSERT INTO messages_fts(rowid, text_content) SELECT id, text_content FROM messages`,
		`CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
		END`,
		`CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
		END`,
		`CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
			INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
		END`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

type sessionMarkerState struct {
	rows, distinct int
	min, max       int
}

type sessionMarkerReader interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readSessionMarker(db sessionMarkerReader) (sessionMarkerState, error) {
	var state sessionMarkerState
	err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0) FROM schema_version`).Scan(
		&state.rows, &state.distinct, &state.min, &state.max,
	)
	return state, err
}

func normalizeSessionMarker(db schemaExecutor, version int) error {
	state, err := func() (sessionMarkerState, error) {
		var state sessionMarkerState
		err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0) FROM schema_version`).Scan(
			&state.rows, &state.distinct, &state.min, &state.max,
		)
		return state, err
	}()
	if err != nil {
		return fmt.Errorf("read legacy session schema marker: %w", err)
	}
	if state.distinct > 1 {
		return fmt.Errorf("conflicting session schema markers range from %d to %d", state.min, state.max)
	}
	if state.rows > 0 && state.max != version {
		return fmt.Errorf("session schema marker changed unexpectedly: observed %d, expected %d", state.max, version)
	}
	for _, statement := range []string{
		`DROP TABLE IF EXISTS schema_version_new`,
		`CREATE TABLE schema_version_new (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`INSERT INTO schema_version_new(id, version) VALUES(1, ?)`, version); err != nil {
		return err
	}
	if _, err := db.Exec(`DROP TABLE schema_version`); err != nil {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE schema_version_new RENAME TO schema_version`); err != nil {
		return err
	}
	return nil
}

func ensureCurrentSessionIndexes(db schemaExecutor) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_sequence ON messages(session_id, sequence)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_client_message_id ON messages(session_id, client_message_id) WHERE client_message_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_number ON sessions(number)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_origin ON sessions(origin)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_pinned ON sessions(pinned)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_title_skipped ON sessions(archived, title_skipped_at, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_last_user_msg ON sessions(last_user_message_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_last_message ON sessions(last_message_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_sidebar_activity ON sessions(archived, COALESCE(pinned, FALSE) DESC, COALESCE(last_message_at, last_user_message_at, created_at) DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_sidebar_last_user_activity ON sessions(archived, COALESCE(pinned, FALSE) DESC, COALESCE(last_user_message_at, created_at) DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("install current session index with %q: %w", statement, err)
		}
	}
	return nil
}

// initSchema uses one aggregate marker query on the current fast path. Any slow
// path acquires BEGIN IMMEDIATE and re-reads all state before writing.
func initSchema(db *sql.DB) error {
	state, err := readSessionMarker(db)
	if err == nil {
		if state.distinct > 1 {
			return fmt.Errorf("conflicting session schema markers range from %d to %d", state.min, state.max)
		}
		if state.rows > 0 && state.max > schemaVersion {
			return fmt.Errorf("session database schema version %d is newer than supported version %d", state.max, schemaVersion)
		}
		if state.rows == 1 && state.max == schemaVersion {
			return nil
		}
	}

	shared := make([]sqliteutil.Migration, len(migrations))
	for i, migration := range migrations {
		shared[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(shared, 1, schemaVersion, true); err != nil {
		return fmt.Errorf("validate session migrations: %w", err)
	}

	currentVersion := 0
	if err := sqliteutil.WithImmediateMigrationTx(context.Background(), db, func(tx sqliteutil.Executor) error {
		markerExists, err := sqliteutil.TableExists(tx, "schema_version")
		if err != nil {
			return fmt.Errorf("inspect session schema marker: %w", err)
		}
		sessionsExist, err := sqliteutil.TableExists(tx, "sessions")
		if err != nil {
			return fmt.Errorf("inspect sessions table: %w", err)
		}
		messagesExist, err := sqliteutil.TableExists(tx, "messages")
		if err != nil {
			return fmt.Errorf("inspect messages table: %w", err)
		}

		if !markerExists && !sessionsExist && !messagesExist {
			userTables, err := sqliteutil.UserTableCount(tx)
			if err != nil {
				return fmt.Errorf("classify fresh session database: %w", err)
			}
			if userTables != 0 {
				return fmt.Errorf("unknown unversioned session schema: found %d unrelated tables; restore a backup or move the database aside to recreate it", userTables)
			}
			if _, err := tx.Exec(canonicalSessionSchema); err != nil {
				return fmt.Errorf("bootstrap canonical session schema: %w", err)
			}
			if err := ensureCurrentSessionIndexes(tx); err != nil {
				return err
			}
			if err := createMessageCountTriggersV27(tx); err != nil {
				return fmt.Errorf("bootstrap message count triggers: %w", err)
			}
			if _, err := tx.Exec(`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL)`); err != nil {
				return fmt.Errorf("create singleton session schema marker: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO schema_version(id, version) VALUES(1, ?)`, schemaVersion); err != nil {
				return fmt.Errorf("publish session schema version %d: %w", schemaVersion, err)
			}
			currentVersion = schemaVersion
			return nil
		}
		if sessionsExist != messagesExist || !sessionsExist {
			return fmt.Errorf("unknown session schema: expected both sessions and messages baseline tables; restore a backup or move the database aside to recreate it")
		}

		if markerExists {
			var locked sessionMarkerState
			if err := tx.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0) FROM schema_version`).Scan(
				&locked.rows, &locked.distinct, &locked.min, &locked.max,
			); err != nil {
				return fmt.Errorf("read locked session schema marker: %w", err)
			}
			if locked.distinct > 1 {
				return fmt.Errorf("conflicting session schema markers range from %d to %d", locked.min, locked.max)
			}
			if locked.rows == 0 {
				if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES(0)`); err != nil {
					return fmt.Errorf("publish empty session legacy baseline: %w", err)
				}
			} else {
				currentVersion = locked.max
			}
			if currentVersion > schemaVersion {
				return fmt.Errorf("session database schema version %d is newer than supported version %d", currentVersion, schemaVersion)
			}
			if currentVersion == schemaVersion && locked.rows != 1 {
				return normalizeSessionMarker(tx, schemaVersion)
			}
			return nil
		}

		if _, err := tx.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
			return fmt.Errorf("create legacy session schema marker: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES(0)`); err != nil {
			return fmt.Errorf("publish session legacy baseline: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	for _, migration := range migrations {
		if migration.version <= currentVersion {
			continue
		}
		if err := runSessionMigration(db, migration); err != nil {
			return err
		}
		currentVersion = migration.version
	}
	finalState, err := readSessionMarker(db)
	if err != nil {
		return fmt.Errorf("read final session schema marker: %w", err)
	}
	if finalState.rows != 1 || finalState.distinct != 1 || finalState.max != schemaVersion {
		return fmt.Errorf("session migrations ended with marker rows=%d distinct=%d version=%d, want one row at %d", finalState.rows, finalState.distinct, finalState.max, schemaVersion)
	}
	return nil
}

func runSessionMigration(db *sql.DB, m migration) error {
	return sqliteutil.WithImmediateMigrationTx(context.Background(), db, func(tx sqliteutil.Executor) error {
		var state sessionMarkerState
		if err := tx.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0) FROM schema_version`).Scan(
			&state.rows, &state.distinct, &state.min, &state.max,
		); err != nil {
			return fmt.Errorf("read marker before session migration %d (%s): %w", m.version, m.description, err)
		}
		if state.distinct != 1 || state.rows == 0 {
			return fmt.Errorf("invalid marker before session migration %d (%s): rows=%d distinct=%d", m.version, m.description, state.rows, state.distinct)
		}
		if state.max >= m.version {
			return nil
		}
		if state.max != m.version-1 {
			return fmt.Errorf("session migration %d (%s) expected prior version %d, observed %d", m.version, m.description, m.version-1, state.max)
		}
		if err := m.up(tx); err != nil {
			return fmt.Errorf("session migration %d (%s), prior version %d remains safely committed: %w", m.version, m.description, state.max, err)
		}
		if _, err := tx.Exec("UPDATE schema_version SET version = ?", m.version); err != nil {
			return fmt.Errorf("publish session migration %d (%s): %w", m.version, m.description, err)
		}
		return nil
	})
}

// isDuplicateColumnError checks if an error is due to a column already existing.
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "duplicate column")
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
