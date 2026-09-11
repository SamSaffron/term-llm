package session

import (
	"database/sql"
	"sync"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	inputInstanceID          string
	db                       *sql.DB
	readDB                   *sql.DB
	responseRunReadDB        *sql.DB
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
	hasTranscriptRev         bool // true if sessions table has transcript_rev column
	hasMessagesTable         bool // true if the messages table exists
	hasMessageCompactionTail bool // true if messages table has compaction_tail column
	hasMessageStreamIdentity bool // true if messages table has response-scoped segment identity columns
	hasMessageClientID       bool // true if messages table has client_message_id
	hasSessionBranches       bool // true if session_branches table exists
	hasProjectID             bool // true if sessions table has project_id
	hasProjectsTable         bool // true if projects table exists
	storeInstanceMu          sync.Mutex
	storeInstanceID          string
}

var _ MessageSequenceStore = (*SQLiteStore)(nil)
var _ ConversationBranchStore = (*SQLiteStore)(nil)
var _ ConversationBranchReplayStore = (*SQLiteStore)(nil)
var _ StoreChangeStore = (*SQLiteStore)(nil)

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
    project_id TEXT,
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
    compaction_count INTEGER DEFAULT 0,
    transcript_rev INTEGER NOT NULL DEFAULT 0
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
    compaction_tail BOOLEAN DEFAULT FALSE,
    client_message_id TEXT NOT NULL DEFAULT '',
    response_id TEXT NOT NULL DEFAULT '',
    assistant_segment_ordinal INTEGER NOT NULL DEFAULT -1,
    segment_start_sequence INTEGER NOT NULL DEFAULT 0,
    segment_end_sequence INTEGER NOT NULL DEFAULT 0
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

CREATE TABLE IF NOT EXISTS session_plans (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    snapshot TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS session_workspace_grants (
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
);
CREATE INDEX IF NOT EXISTS idx_session_workspace_grants_session
    ON session_workspace_grants(session_id, created_at, id);

CREATE TABLE IF NOT EXISTS session_pending_steering (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    message TEXT NOT NULL,
    display_text TEXT NOT NULL DEFAULT '',
    attachment_summary TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    acceptance_sequence INTEGER NOT NULL DEFAULT 0,
    origin TEXT NOT NULL DEFAULT 'legacy_unknown',
    owner_kind TEXT NOT NULL DEFAULT '',
    owner_id TEXT NOT NULL DEFAULT '',
    owner_fence INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_pending_steering_order
    ON session_pending_steering(session_id, acceptance_sequence);
CREATE TABLE IF NOT EXISTS session_steering_sequence (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS session_redo (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    stack_pos INTEGER NOT NULL,
    suffix TEXT NOT NULL,
    metadata TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (session_id, stack_pos)
);

CREATE TABLE IF NOT EXISTS session_branches (
    child_session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    parent_session_id TEXT NOT NULL,
    fork_after_message_id INTEGER,
    fork_after_sequence INTEGER NOT NULL,
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_branches_idempotency
    ON session_branches(parent_session_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX IF NOT EXISTS idx_session_branches_parent
    ON session_branches(parent_session_id);

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
    status TEXT NOT NULL DEFAULT 'active',
    vapid_key_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT '',
    last_used_at TEXT,
    last_failure_code TEXT NOT NULL DEFAULT '',
    last_failure TEXT NOT NULL DEFAULT '',
    last_failure_at TEXT
);

CREATE TABLE IF NOT EXISTS completion_push_outbox (
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
    ON completion_push_outbox(status, next_attempt_at, id);

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

// projectsSchemaV47 freezes the SQL published by historical migrations 47/48.
// Existing upgrades execute it only from an owning migration; it is never an
// upgrade prelude.
const projectsSchemaV47 = `
CREATE TABLE IF NOT EXISTS projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    canonical_dir TEXT NOT NULL UNIQUE,
    is_bootstrap INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    last_used_at DATETIME NOT NULL,
    archived_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_projects_recent
    ON projects(archived_at, last_used_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_single_bootstrap
    ON projects(is_bootstrap) WHERE is_bootstrap = 1;
CREATE INDEX IF NOT EXISTS idx_sessions_project_activity
    ON sessions(project_id, pinned DESC, last_message_at DESC, number DESC);
`

const changeLogSchemaV52 = `
CREATE TABLE IF NOT EXISTS session_change_log (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    session_id TEXT NOT NULL DEFAULT '',
    project_id TEXT NOT NULL DEFAULT '',
    transcript_rev INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER IF NOT EXISTS session_change_log_session_insert
AFTER INSERT ON sessions BEGIN
    INSERT INTO session_change_log(kind, session_id, project_id, transcript_rev, status)
    VALUES ('session.created', NEW.id, COALESCE(NEW.project_id, ''), COALESCE(NEW.transcript_rev, 0), COALESCE(NEW.status, ''));
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_session_delete
AFTER DELETE ON sessions BEGIN
    INSERT INTO session_change_log(kind, session_id, project_id, transcript_rev, status)
    VALUES ('session.deleted', OLD.id, COALESCE(OLD.project_id, ''), COALESCE(OLD.transcript_rev, 0), COALESCE(OLD.status, ''));
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_session_metadata
AFTER UPDATE OF name, generated_short_title, generated_long_title, provider_key, model, agent, tools, mcp, cwd, worktree_dir, archived, pinned ON sessions
WHEN OLD.name IS NOT NEW.name
  OR OLD.generated_short_title IS NOT NEW.generated_short_title
  OR OLD.generated_long_title IS NOT NEW.generated_long_title
  OR OLD.provider_key IS NOT NEW.provider_key
  OR OLD.model IS NOT NEW.model
  OR OLD.agent IS NOT NEW.agent
  OR OLD.tools IS NOT NEW.tools
  OR OLD.mcp IS NOT NEW.mcp
  OR OLD.cwd IS NOT NEW.cwd
  OR OLD.worktree_dir IS NOT NEW.worktree_dir
  OR OLD.archived IS NOT NEW.archived
  OR OLD.pinned IS NOT NEW.pinned
BEGIN
    INSERT INTO session_change_log(kind, session_id, project_id, transcript_rev, status)
    VALUES ('session.metadata_changed', NEW.id, COALESCE(NEW.project_id, ''), COALESCE(NEW.transcript_rev, 0), COALESCE(NEW.status, ''));
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_session_project
AFTER UPDATE OF project_id ON sessions
WHEN OLD.project_id IS NOT NEW.project_id
BEGIN
    INSERT INTO session_change_log(kind, session_id, project_id, transcript_rev, status)
    VALUES ('project.membership_changed', NEW.id, COALESCE(NEW.project_id, ''), COALESCE(NEW.transcript_rev, 0), COALESCE(NEW.status, ''));
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_session_transcript
AFTER UPDATE OF transcript_rev ON sessions
WHEN OLD.transcript_rev IS NOT NEW.transcript_rev
BEGIN
    INSERT INTO session_change_log(kind, session_id, project_id, transcript_rev, status)
    VALUES ('session.transcript_changed', NEW.id, COALESCE(NEW.project_id, ''), COALESCE(NEW.transcript_rev, 0), COALESCE(NEW.status, ''));
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_session_status
AFTER UPDATE OF status ON sessions
WHEN OLD.status IS NOT NEW.status
BEGIN
    INSERT INTO session_change_log(kind, session_id, project_id, transcript_rev, status)
    VALUES ('session.status_changed', NEW.id, COALESCE(NEW.project_id, ''), COALESCE(NEW.transcript_rev, 0), COALESCE(NEW.status, ''));
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_project_insert
AFTER INSERT ON projects BEGIN
    INSERT INTO session_change_log(kind, project_id) VALUES ('project.created', NEW.id);
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_project_update
AFTER UPDATE OF name, canonical_dir, archived_at ON projects
WHEN OLD.name IS NOT NEW.name OR OLD.canonical_dir IS NOT NEW.canonical_dir OR OLD.archived_at IS NOT NEW.archived_at
BEGIN
    INSERT INTO session_change_log(kind, project_id) VALUES ('project.updated', NEW.id);
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_project_delete
AFTER DELETE ON projects BEGIN
    INSERT INTO session_change_log(kind, project_id) VALUES ('project.deleted', OLD.id);
END;

CREATE TRIGGER IF NOT EXISTS session_change_log_trim
AFTER INSERT ON session_change_log
WHEN (NEW.sequence % 256) = 0
BEGIN
    DELETE FROM session_change_log WHERE sequence <= NEW.sequence - 8192;
END;
`

const attentionSchemaV54 = `
CREATE TABLE IF NOT EXISTS serve_response_lifecycle (
    response_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_epoch INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running','completed','failed','cancelled','orphaned')),
    owner_instance_id TEXT NOT NULL,
    fencing_token INTEGER NOT NULL,
    lease_expires_at INTEGER NOT NULL,
    started_rev INTEGER NOT NULL DEFAULT 0,
    final_rev INTEGER NOT NULL DEFAULT 0,
    durable_output_count INTEGER NOT NULL DEFAULT 0,
    attention_seq INTEGER NOT NULL DEFAULT 0,
    interaction_required_count INTEGER NOT NULL DEFAULT 0,
    interaction_required_kinds TEXT NOT NULL DEFAULT '',
    interaction_required_since INTEGER,
    interaction_state_rev INTEGER NOT NULL DEFAULT 0,
    started_at INTEGER NOT NULL,
    ended_at INTEGER,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS serve_response_lifecycle_expired
    ON serve_response_lifecycle(state, lease_expires_at) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS serve_response_lifecycle_session
    ON serve_response_lifecycle(session_id, started_at DESC);
CREATE INDEX IF NOT EXISTS serve_response_lifecycle_terminal_retention
    ON serve_response_lifecycle(ended_at) WHERE state <> 'running';

CREATE TABLE IF NOT EXISTS session_attention (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    latest_attention_seq INTEGER NOT NULL DEFAULT 0,
    response_id TEXT NOT NULL DEFAULT '',
    run_epoch INTEGER NOT NULL DEFAULT 0,
    outcome TEXT NOT NULL DEFAULT '',
    started_rev INTEGER NOT NULL DEFAULT 0,
    final_rev INTEGER NOT NULL DEFAULT 0,
    terminal_at INTEGER,
    seen_through_seq INTEGER NOT NULL DEFAULT 0,
    seen_at INTEGER,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS session_attention_unseen
    ON session_attention(latest_attention_seq DESC)
    WHERE latest_attention_seq > seen_through_seq;
`

const canonicalSessionSchema = schema + projectsSchemaV47 + changeLogSchemaV52 + attentionSchemaV54 + rushSchemaV57
