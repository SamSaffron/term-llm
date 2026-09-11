package memory

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/sqliteutil"
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
CREATE INDEX IF NOT EXISTS idx_memory_embeddings_vector_search_cover ON memory_embeddings(
    provider, model, dimensions, fragment_id, search_vector_norm, search_tail_norm,
    search_prefix_0, search_prefix_1, search_prefix_2, search_prefix_3,
    search_prefix_4, search_prefix_5, search_prefix_6, search_prefix_7
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

const memorySchemaVersion = 12

const memorySchemaVersionKey = "schema_version"

type schemaExecutor = sqliteutil.Executor

type memoryMigration struct {
	version     int
	description string
	up          func(db schemaExecutor) error
}

func addMemoryColumn(db schemaExecutor, table, column, definition string) error {
	exists, err := sqliteutil.ColumnExists(db, table, column)
	if err != nil {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE "` + table + `" ADD COLUMN "` + column + `" ` + definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

var memoryMigrations = []memoryMigration{
	{
		version:     1,
		description: "add compact_content column to memory_insights",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_insights", "compact_content", `TEXT NOT NULL DEFAULT ''`)
		},
	},
	{
		version:     2,
		description: "add vector search prefix 0 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_0", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     3,
		description: "add vector search prefix 1 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_1", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     4,
		description: "add vector search prefix 2 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_2", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     5,
		description: "add vector search prefix 3 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_3", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     6,
		description: "add vector search prefix 4 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_4", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     7,
		description: "add vector search prefix 5 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_5", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     8,
		description: "add vector search prefix 6 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_6", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     9,
		description: "add vector search prefix 7 column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_prefix_7", `REAL NOT NULL DEFAULT 0`)
		},
	},
	{
		version:     10,
		description: "add vector search tail norm column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_tail_norm", `REAL NOT NULL DEFAULT -1`)
		},
	},
	{
		version:     11,
		description: "add vector search norm column",
		up: func(db schemaExecutor) error {
			return addMemoryColumn(db, "memory_embeddings", "search_vector_norm", `REAL NOT NULL DEFAULT -1`)
		},
	},
	{
		version:     12,
		description: "canonicalize vector search schema and add covering index",
		up: func(db schemaExecutor) error {
			if err := canonicalizeMemoryMigrationTables(db); err != nil {
				return err
			}
			_, err := db.Exec(memoryVectorSearchIndexSQL)
			return err
		},
	},
}

func canonicalizeMemoryMigrationTables(db schemaExecutor) error {
	statements := []string{
		`DROP INDEX IF EXISTS idx_memory_embeddings_provider_model_dimensions`,
		`DROP INDEX IF EXISTS idx_memory_embeddings_vector_search_cover`,
		`ALTER TABLE memory_embeddings RENAME TO memory_embeddings_old`,
		`CREATE TABLE memory_embeddings (
			fragment_id TEXT NOT NULL REFERENCES memory_fragments(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			dimensions INTEGER NOT NULL,
			vector BLOB NOT NULL,
			search_prefix_0 REAL NOT NULL DEFAULT 0,
			search_prefix_1 REAL NOT NULL DEFAULT 0,
			search_prefix_2 REAL NOT NULL DEFAULT 0,
			search_prefix_3 REAL NOT NULL DEFAULT 0,
			search_prefix_4 REAL NOT NULL DEFAULT 0,
			search_prefix_5 REAL NOT NULL DEFAULT 0,
			search_prefix_6 REAL NOT NULL DEFAULT 0,
			search_prefix_7 REAL NOT NULL DEFAULT 0,
			search_tail_norm REAL NOT NULL DEFAULT -1,
			search_vector_norm REAL NOT NULL DEFAULT -1,
			embedded_at DATETIME NOT NULL,
			PRIMARY KEY (fragment_id, provider, model)
		)`,
		`INSERT INTO memory_embeddings(fragment_id,provider,model,dimensions,vector,search_prefix_0,search_prefix_1,search_prefix_2,search_prefix_3,search_prefix_4,search_prefix_5,search_prefix_6,search_prefix_7,search_tail_norm,search_vector_norm,embedded_at)
		 SELECT fragment_id,provider,model,dimensions,vector,search_prefix_0,search_prefix_1,search_prefix_2,search_prefix_3,search_prefix_4,search_prefix_5,search_prefix_6,search_prefix_7,search_tail_norm,search_vector_norm,embedded_at FROM memory_embeddings_old`,
		`DROP TABLE memory_embeddings_old`,
		`CREATE INDEX idx_memory_embeddings_provider_model_dimensions ON memory_embeddings(provider, model, dimensions)`,
		`DROP INDEX IF EXISTS idx_insights_agent`,
		`ALTER TABLE memory_insights RENAME TO memory_insights_old`,
		`CREATE TABLE memory_insights (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			agent TEXT NOT NULL,
			content TEXT NOT NULL,
			compact_content TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL DEFAULT '',
			trigger_desc TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0.5,
			reinforcement_count INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			last_reinforced DATETIME NOT NULL DEFAULT (datetime('now'))
		)`,
		`INSERT INTO memory_insights(id,agent,content,compact_content,category,trigger_desc,confidence,reinforcement_count,created_at,last_reinforced)
		 SELECT id,agent,content,compact_content,category,trigger_desc,confidence,reinforcement_count,created_at,last_reinforced FROM memory_insights_old`,
		`DROP TABLE memory_insights_old`,
		`CREATE INDEX idx_insights_agent ON memory_insights(agent)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("canonicalize memory migration schema with %q: %w", statement, err)
		}
	}
	return nil
}

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

const memoryVectorSearchIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_memory_embeddings_vector_search_cover ON memory_embeddings(
	provider, model, dimensions, fragment_id, search_vector_norm, search_tail_norm,
	search_prefix_0, search_prefix_1, search_prefix_2, search_prefix_3,
	search_prefix_4, search_prefix_5, search_prefix_6, search_prefix_7
)`

func initSchema(db *sql.DB) error {
	currentVersion, markerPresent, err := getMemorySchemaVersion(db)
	if err == nil && markerPresent {
		switch {
		case currentVersion == memorySchemaVersion:
			return ensureFTSInitialized(db)
		case currentVersion > memorySchemaVersion:
			return fmt.Errorf("memory database schema version %d is newer than supported version %d", currentVersion, memorySchemaVersion)
		}
	}

	shared := make([]sqliteutil.Migration, len(memoryMigrations))
	for i, migration := range memoryMigrations {
		shared[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(shared, 1, memorySchemaVersion, true); err != nil {
		return fmt.Errorf("validate memory migrations: %w", err)
	}

	currentVersion = 0
	err = sqliteutil.WithImmediateMigrationTx(context.Background(), db, func(tx sqliteutil.Executor) error {
		lockedVersion, lockedMarkerPresent, markerErr := getMemorySchemaVersion(tx)
		if markerErr != nil {
			metaExists, inspectErr := sqliteutil.TableExists(tx, "memory_meta")
			if inspectErr != nil {
				return fmt.Errorf("inspect memory metadata table: %w", inspectErr)
			}
			if metaExists {
				return fmt.Errorf("read memory schema marker: %w", markerErr)
			}
			lockedVersion, lockedMarkerPresent = 0, false
		}
		if lockedMarkerPresent {
			if lockedVersion > memorySchemaVersion {
				return fmt.Errorf("memory database schema version %d is newer than supported version %d", lockedVersion, memorySchemaVersion)
			}
			currentVersion = lockedVersion
			return nil
		}

		fragmentsExist, err := sqliteutil.TableExists(tx, "memory_fragments")
		if err != nil {
			return fmt.Errorf("inspect memory schema: %w", err)
		}
		embeddingsExist, err := sqliteutil.TableExists(tx, "memory_embeddings")
		if err != nil {
			return fmt.Errorf("inspect memory embeddings schema: %w", err)
		}
		insightsExist, err := sqliteutil.TableExists(tx, "memory_insights")
		if err != nil {
			return fmt.Errorf("inspect memory insights schema: %w", err)
		}

		if !fragmentsExist && !embeddingsExist && !insightsExist {
			userTables, err := sqliteutil.UserTableCount(tx)
			if err != nil {
				return fmt.Errorf("classify fresh memory database: %w", err)
			}
			if userTables != 0 {
				return fmt.Errorf("unknown unversioned memory schema: found %d unrelated tables; restore a backup or move the database aside to recreate it", userTables)
			}
			if _, err := tx.Exec(schema); err != nil {
				return fmt.Errorf("bootstrap memory schema: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO memory_meta(key, value) VALUES(?, ?)`, memorySchemaVersionKey, strconv.Itoa(memorySchemaVersion)); err != nil {
				return fmt.Errorf("publish memory schema version %d: %w", memorySchemaVersion, err)
			}
			currentVersion = memorySchemaVersion
			return nil
		}

		// Released pre-marker memory databases contain the embedding table.
		// Install only baseline objects; migrated columns remain version-owned.
		if !embeddingsExist {
			return fmt.Errorf("unknown unversioned memory schema: expected memory_embeddings table; restore a backup or move the database aside to recreate it")
		}
		legacySchema, err := memoryLegacyBaselineSchema()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(legacySchema); err != nil {
			return fmt.Errorf("install memory legacy baseline objects: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO memory_meta(key, value) VALUES(?, '0')`, memorySchemaVersionKey); err != nil {
			return fmt.Errorf("publish memory legacy baseline: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, migration := range memoryMigrations {
		if migration.version <= currentVersion {
			continue
		}
		if err := runMemoryMigration(db, migration); err != nil {
			return err
		}
		currentVersion = migration.version
	}
	finalVersion, present, err := getMemorySchemaVersion(db)
	if err != nil {
		return fmt.Errorf("read final memory schema marker: %w", err)
	}
	if !present || finalVersion != memorySchemaVersion {
		return fmt.Errorf("memory migrations ended at version %d (present=%t), want %d", finalVersion, present, memorySchemaVersion)
	}
	return ensureFTSInitialized(db)
}

func memoryLegacyBaselineSchema() (string, error) {
	legacy := schema
	for _, line := range []string{
		"    compact_content     TEXT NOT NULL DEFAULT '',\n",
		"    search_prefix_0     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_1     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_2     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_3     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_4     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_5     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_6     REAL NOT NULL DEFAULT 0,\n",
		"    search_prefix_7     REAL NOT NULL DEFAULT 0,\n",
		"    search_tail_norm    REAL NOT NULL DEFAULT -1,\n",
		"    search_vector_norm  REAL NOT NULL DEFAULT -1,\n",
	} {
		if !strings.Contains(legacy, line) {
			return "", fmt.Errorf("memory legacy baseline no longer matches canonical column %q", strings.TrimSpace(line))
		}
		legacy = strings.Replace(legacy, line, "", 1)
	}
	indexSQL := `CREATE INDEX IF NOT EXISTS idx_memory_embeddings_vector_search_cover ON memory_embeddings(
    provider, model, dimensions, fragment_id, search_vector_norm, search_tail_norm,
    search_prefix_0, search_prefix_1, search_prefix_2, search_prefix_3,
    search_prefix_4, search_prefix_5, search_prefix_6, search_prefix_7
);
`
	if !strings.Contains(legacy, indexSQL) {
		return "", fmt.Errorf("memory legacy baseline no longer matches canonical vector index")
	}
	legacy = strings.Replace(legacy, indexSQL, "", 1)
	return legacy, nil
}

type memoryVersionReader interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getMemorySchemaVersion(db memoryVersionReader) (version int, present bool, err error) {
	var value string
	err = db.QueryRow(`SELECT value FROM memory_meta WHERE key = ?`, memorySchemaVersionKey).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	version, err = strconv.Atoi(value)
	if err != nil {
		return 0, false, fmt.Errorf("parse memory schema version %q: %w", value, err)
	}
	if version < 0 {
		return 0, false, fmt.Errorf("invalid negative memory schema version %d", version)
	}
	return version, true, nil
}

func runMemoryMigration(db *sql.DB, m memoryMigration) error {
	return sqliteutil.WithImmediateMigrationTx(context.Background(), db, func(tx sqliteutil.Executor) error {
		version, present, err := getMemorySchemaVersion(tx)
		if err != nil {
			return fmt.Errorf("read marker before memory migration %d (%s): %w", m.version, m.description, err)
		}
		if !present {
			return fmt.Errorf("memory migration %d (%s) has no prior marker", m.version, m.description)
		}
		if version >= m.version {
			return nil
		}
		if version != m.version-1 {
			return fmt.Errorf("memory migration %d (%s) expected prior version %d, observed %d", m.version, m.description, m.version-1, version)
		}
		if err := m.up(tx); err != nil {
			return fmt.Errorf("memory migration %d (%s), prior version %d remains safely committed: %w", m.version, m.description, version, err)
		}
		result, err := tx.Exec(`UPDATE memory_meta SET value = ? WHERE key = ?`, strconv.Itoa(m.version), memorySchemaVersionKey)
		if err != nil {
			return fmt.Errorf("publish memory migration %d (%s): %w", m.version, m.description, err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return fmt.Errorf("publish memory migration %d (%s): marker rows affected=%d err=%v", m.version, m.description, rows, err)
		}
		return nil
	})
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sqliteutil.WithImmediateMigrationTx(ctx, db, func(tx sqliteutil.Executor) error {
		// Re-read after taking the write lock; another opener may have completed
		// the rebuild while this one waited.
		err := tx.QueryRow(`SELECT value FROM memory_meta WHERE key = 'fts_initialized'`).Scan(&marker)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO memory_fts(memory_fts) VALUES('rebuild')`); err != nil {
			return fmt.Errorf("rebuild memory FTS: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO memory_meta(key, value) VALUES('fts_initialized', '1')`); err != nil {
			return fmt.Errorf("publish memory FTS initialization: %w", err)
		}
		return nil
	})
}

// GetDataDir returns the XDG data directory for term-llm.
func GetDataDir() (string, error) { return appdata.GetDataDir() }

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
	return sqliteutil.ResolveDBPathOverride(pathOverride, GetDBPath)
}

// CreateFragment inserts a new fragment and syncs FTS explicitly.
