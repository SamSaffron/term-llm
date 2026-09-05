// Package filetrack records file changes made by agent tools so sessions can
// expose a cumulative diff (baseline = file state at first touch in a session).
// Bulky before/after contents live in a dedicated SQLite database as
// content-addressed compressed blobs, keeping the main sessions DB slim.
package filetrack

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/sqliteutil"

	_ "modernc.org/sqlite"
)

// Default caps, overridable via config.
const (
	DefaultMaxFileBytes    = config.DefaultFileTrackingMaxFileBytes    // per-file content cap
	DefaultMaxSessionBytes = config.DefaultFileTrackingMaxSessionBytes // retained-content budget per session
	DefaultMaxTotalBytes   = config.DefaultFileTrackingMaxTotalBytes   // whole-database size cap (across sessions)
)

// Change kinds.
const (
	KindCreate = "create"
	KindModify = "modify"
	KindDelete = "delete"
)

// Closed attribution and evidence values persisted by the tracker.
const (
	ProvenanceDirect            = "direct"
	ProvenanceDeclaredTransform = "declared_transform"
	ProvenanceDeclaredGenerate  = "declared_generate"
	ProvenanceLegacyUnverified  = "legacy_unverified"

	ClaimTransform   = "transform"
	ClaimGenerate    = "generate"
	ClaimMaterialize = "materialize"

	CoverageComplete    = "complete"
	CoverageTruncated   = "truncated"
	CoverageUnavailable = "unavailable"

	BaselineNormal           = "normal"
	BaselinePreexistingDirty = "preexisting_dirty"
	BaselineUnknown          = "unknown"

	ContentRetained           = "retained"
	ContentRetainedImage      = "retained_image"
	ContentBinaryUnrenderable = "binary_unrenderable"
	ContentOversized          = "oversized"
	ContentSessionBudget      = "session_budget"
	ContentStoreBudget        = "store_budget"
	ContentBeforeUnknown      = "before_unknown"
	ContentAfterUnknown       = "after_unknown"
	ContentBothUnknown        = "both_unknown"
)

// ChangeRecord describes one before→after attributed file transition to record.
type ChangeRecord struct {
	SessionID  string
	RunID      string
	ToolName   string
	ToolCallID string
	Path       string // absolute path

	// Provenance is mandatory for new callers of RecordAttributedChange.
	Provenance    string
	ClaimKind     string
	ClaimPattern  string
	ClaimLiteral  bool
	ClaimCoverage string
	BaselineState string

	Before []byte // content before the change (ignored when BeforeMissing/BeforeUnknown)
	After  []byte // content after the change (ignored when AfterMissing/AfterUnknown)

	BeforeMissing bool // file did not exist before the change
	AfterMissing  bool // file does not exist after the change (deletion)
	BeforeUnknown bool // file existed before but its content was not captured
	AfterUnknown  bool // file exists after but its content was not captured (e.g. oversized)

	// Size hints for unknown-content sides (from stat); ignored when the
	// corresponding content is provided.
	BeforeSizeHint int64
	AfterSizeHint  int64
}

// Change is one recorded change row.
type Change struct {
	Seq              int64
	EventSeq         int64
	RunID            string
	Path             string
	Kind             string
	ToolName         string
	ToolCallID       string
	BeforeHash       string // empty when absent/unknown/not retained
	AfterHash        string
	BeforeSize       int64
	AfterSize        int64
	Adds             int
	Dels             int
	Truncated        bool
	IsBinary         bool
	Provenance       string
	Provenances      []string
	ClaimKind        string
	ClaimPattern     string
	ClaimLiteral     bool
	ClaimCoverage    string
	BaselineState    string
	ContentStatus    string
	ContentAvailable bool
}

// CumulativeChange summarizes a file's net attributed change relative to the selected baseline.
type CumulativeChange struct {
	Path             string   `json:"path"`
	Kind             string   `json:"kind"`
	Adds             int      `json:"adds"`
	Dels             int      `json:"dels"`
	Truncated        bool     `json:"truncated"`
	Seq              int64    `json:"seq"`                    // latest change sequence for this path in the session
	SnapshotSeq      int64    `json:"snapshot_seq,omitempty"` // compatibility identity for a multi-run window
	Provenance       string   `json:"provenance,omitempty"`
	Provenances      []string `json:"provenances,omitempty"`
	BaselineState    string   `json:"baseline_state,omitempty"`
	ContentStatus    string   `json:"content_status,omitempty"`
	ContentAvailable bool     `json:"content_available"`
	ClaimCoverage    string   `json:"claim_coverage,omitempty"`
}

// FileDiffContent holds the baseline and current contents for one file.
type FileDiffContent struct {
	Path             string
	Kind             string
	Before           []byte
	After            []byte
	Truncated        bool
	IsImage          bool
	ContentStatus    string
	ContentAvailable bool
	Provenance       string
	BaselineState    string
	ClaimCoverage    string
}

// ErrInvalidDiffSide means the requested side does not exist for the resolved
// change kind, such as the baseline side of a newly created file.
var ErrInvalidDiffSide = errors.New("invalid file diff side")

// FileDiffSide contains one retained side of a browser-renderable image diff.
type FileDiffSide struct {
	Path      string
	Kind      string
	Side      string
	Data      []byte
	MediaType string
}

// FileDiffTextSide contains one retained textual side of a file diff.
type FileDiffTextSide struct {
	Path string
	Kind string
	Side string
	Data []byte
}

// Options configures a Store.
type Options struct {
	MaxFileBytes               int   // 0 = DefaultMaxFileBytes
	MaxSessionBytes            int   // 0 = DefaultMaxSessionBytes
	MaxTotalBytes              int64 // 0 = DefaultMaxTotalBytes; whole-database size cap enforced live and by GC
	MaxObservationRows         int   // 0 = 10,000; independent sidecar row cap
	MaxObservationSessionRows  int   // 0 = 1,000; independent per-session sidecar row cap
	MaxObservationBytes        int64 // 0 = 16 MiB metadata cap
	MaxObservationSessionBytes int64 // 0 = 2 MiB per-session metadata cap
	MaxObservationAgeDays      int   // 0 = 30
}

type recordSessionLock struct {
	mu   sync.Mutex
	refs int
}

// Store persists file-change history in a dedicated SQLite database.
type Store struct {
	db                         *sql.DB
	observationDB              *sql.DB
	observationPath            string
	maxFileBytes               int
	maxSessionBytes            int
	maxTotalBytes              int64
	maxObservationRows         int
	maxObservationSessionRows  int
	maxObservationBytes        int64
	maxObservationSessionBytes int64
	maxObservationAgeDays      int

	// RecordChange is serialized only within a session. The map lock is held for
	// lock bookkeeping only; file analysis and SQLite work never run under it.
	recordLocksMu sync.Mutex
	recordLocks   map[string]*recordSessionLock

	// Total-budget accounting and rare pruning are independent of per-session
	// recording, so unrelated sessions never wait for another session's normal
	// file-change processing.
	totalMu                   sync.Mutex
	pruneMu                   sync.Mutex
	uncheckedTotalBytes       int64
	uncheckedTotalRecordCount int

	mu           sync.Mutex
	sessionBytes map[string]int64 // retained-bytes budget cache per session
}

const schemaVersion = 4

const (
	// Check after at most 8 MiB of retained input, or 64 metadata-only rows.
	// Smaller configured caps use a proportional interval below.
	maxUncheckedTotalBytes   = int64(8 * 1024 * 1024)
	maxUncheckedTotalRecords = 64
)

const schema = `
CREATE TABLE IF NOT EXISTS schema_version (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS blobs (
	hash        TEXT PRIMARY KEY,
	size        INTEGER NOT NULL,
	compression TEXT NOT NULL DEFAULT 'gzip',
	data        BLOB NOT NULL,
	created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS file_changes (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id   TEXT NOT NULL,
	run_id       TEXT,
	seq          INTEGER NOT NULL,
	path         TEXT NOT NULL,
	kind         TEXT NOT NULL CHECK (kind IN ('create','modify','delete')),
	tool_name    TEXT,
	tool_call_id TEXT,
	before_hash  TEXT,
	after_hash   TEXT,
	before_size  INTEGER NOT NULL DEFAULT 0,
	after_size   INTEGER NOT NULL DEFAULT 0,
	adds         INTEGER NOT NULL DEFAULT 0,
	dels         INTEGER NOT NULL DEFAULT 0,
	truncated    INTEGER NOT NULL DEFAULT 0,
	is_binary    INTEGER NOT NULL DEFAULT 0,
	provenance   TEXT NOT NULL DEFAULT 'legacy_unverified' CHECK (provenance IN ('direct','declared_transform','declared_generate','legacy_unverified')),
	claim_kind   TEXT,
	claim_pattern TEXT,
	claim_literal INTEGER NOT NULL DEFAULT 0,
	claim_coverage TEXT NOT NULL DEFAULT 'complete' CHECK (claim_coverage IN ('complete','truncated','unavailable')),
	baseline_state TEXT NOT NULL DEFAULT 'unknown' CHECK (baseline_state IN ('normal','preexisting_dirty','unknown')),
	content_status TEXT NOT NULL DEFAULT 'retained',
	event_seq INTEGER,
	created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	CHECK ((provenance = 'declared_transform' AND claim_kind = 'transform') OR
	       (provenance = 'declared_generate' AND claim_kind = 'generate') OR
	       (provenance IN ('direct','legacy_unverified') AND claim_kind IS NULL)),
	UNIQUE(session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_file_changes_session_path ON file_changes(session_id, path, seq);
CREATE INDEX IF NOT EXISTS idx_file_changes_session_run ON file_changes(session_id, run_id, seq);

CREATE TABLE IF NOT EXISTS filetrack_event_counters (
	session_id TEXT PRIMARY KEY,
	next_event_seq INTEGER NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS filetrack_runs (
	session_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	ordinal INTEGER NOT NULL,
	started_at TIMESTAMP NOT NULL,
	completed_at TIMESTAMP,
	PRIMARY KEY(session_id, run_id),
	UNIQUE(session_id, ordinal)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_filetrack_runs_recent ON filetrack_runs(session_id, ordinal DESC);
`

func preparePrivateDBFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create file history data directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("create file history database: %w", err)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close file history database: %w", closeErr)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("secure file history database permissions: %w", err)
	}
	return nil
}

func chmodSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("secure file history sqlite file permissions: %w", err)
		}
	}
	return nil
}

// configureFileTrackingDB enables WAL and sets incremental vacuum only when
// needed. Reassigning auto_vacuum even to its current value starts a write
// transaction; doing it in the connection DSN makes every open pay that cost.
func configureFileTrackingDB(db *sql.DB) error {
	var mode int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return err
	}
	if mode != 2 {
		if _, err := db.Exec("PRAGMA auto_vacuum = 2"); err != nil {
			return err
		}
	}
	// On an empty database auto_vacuum must be configured before WAL mode
	// creates its header. Both settings persist for subsequent connections.
	_, err := db.Exec("PRAGMA journal_mode = WAL")
	return err
}

// Open opens (creating if necessary) the file-change history database at path.
func Open(path string, opts Options) (*Store, error) {
	if path != ":memory:" {
		if err := preparePrivateDBFile(path); err != nil {
			return nil, err
		}
	}

	dsn := path
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open file history database: %w", err)
	}
	if path == ":memory:" {
		// Keep a single connection so schema and data stay visible everywhere.
		db.SetMaxOpenConns(1)
	}

	if err := configureFileTrackingDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure file history database: %w", err)
	}

	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize file history schema: %w", err)
	}
	if path != ":memory:" {
		if err := chmodSQLiteFiles(path); err != nil {
			db.Close()
			return nil, err
		}
	}

	maxFile := opts.MaxFileBytes
	if maxFile <= 0 {
		maxFile = DefaultMaxFileBytes
	}
	maxSession := opts.MaxSessionBytes
	if maxSession <= 0 {
		maxSession = DefaultMaxSessionBytes
	}
	maxTotal := opts.MaxTotalBytes
	if maxTotal <= 0 {
		maxTotal = DefaultMaxTotalBytes
	}
	maxObservationRows := opts.MaxObservationRows
	if maxObservationRows <= 0 {
		maxObservationRows = 10000
	}
	maxObservationSessionRows := opts.MaxObservationSessionRows
	if maxObservationSessionRows <= 0 {
		maxObservationSessionRows = 1000
	}
	maxObservationBytes := opts.MaxObservationBytes
	if maxObservationBytes <= 0 {
		maxObservationBytes = 16 * 1024 * 1024
	}
	maxObservationSessionBytes := opts.MaxObservationSessionBytes
	if maxObservationSessionBytes <= 0 {
		maxObservationSessionBytes = 2 * 1024 * 1024
	}
	maxObservationAgeDays := opts.MaxObservationAgeDays
	if maxObservationAgeDays <= 0 {
		maxObservationAgeDays = 30
	}
	observationDB, observationPath, err := openObservationDB(path)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:                         db,
		observationDB:              observationDB,
		observationPath:            observationPath,
		maxFileBytes:               maxFile,
		maxSessionBytes:            maxSession,
		maxTotalBytes:              maxTotal,
		maxObservationRows:         maxObservationRows,
		maxObservationSessionRows:  maxObservationSessionRows,
		maxObservationBytes:        maxObservationBytes,
		maxObservationSessionBytes: maxObservationSessionBytes,
		maxObservationAgeDays:      maxObservationAgeDays,
		sessionBytes:               make(map[string]int64),
	}, nil
}

type filetrackMigration struct {
	version     int
	description string
	up          func(sqliteutil.Executor) error
}

var filetrackMigrations = []filetrackMigration{
	{
		version:     2,
		description: "add file change run identity and index",
		up: func(tx sqliteutil.Executor) error {
			exists, err := sqliteutil.ColumnExists(tx, "file_changes", "run_id")
			if err != nil {
				return fmt.Errorf("inspect file_changes.run_id: %w", err)
			}
			if !exists {
				if _, err := tx.Exec(`ALTER TABLE file_changes ADD COLUMN run_id TEXT`); err != nil {
					return fmt.Errorf("add file change run identity: %w", err)
				}
			}
			if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_file_changes_session_run ON file_changes(session_id, run_id, seq)`); err != nil {
				return fmt.Errorf("create file change run index: %w", err)
			}
			return nil
		},
	},
	{
		version:     3,
		description: "canonicalize file history schema and enforce singleton marker",
		up: func(tx sqliteutil.Executor) error {
			if err := canonicalizeFileChangesTable(tx); err != nil {
				return err
			}
			return normalizeFiletrackMarker(tx, 2)
		},
	},
	{
		version:     4,
		description: "separate attributed provenance and add run/event indexes",
		up: func(tx sqliteutil.Executor) error {
			return canonicalizeAttributedFileChangesTable(tx)
		},
	},
}

func canonicalizeAttributedFileChangesTable(tx sqliteutil.Executor) error {
	statements := []string{
		`DROP INDEX IF EXISTS idx_file_changes_session_path`,
		`DROP INDEX IF EXISTS idx_file_changes_session_run`,
		`ALTER TABLE file_changes RENAME TO file_changes_old`,
		`CREATE TABLE file_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, run_id TEXT, seq INTEGER NOT NULL,
			path TEXT NOT NULL, kind TEXT NOT NULL CHECK (kind IN ('create','modify','delete')), tool_name TEXT,
			tool_call_id TEXT, before_hash TEXT, after_hash TEXT, before_size INTEGER NOT NULL DEFAULT 0,
			after_size INTEGER NOT NULL DEFAULT 0, adds INTEGER NOT NULL DEFAULT 0, dels INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0, is_binary INTEGER NOT NULL DEFAULT 0,
			provenance TEXT NOT NULL DEFAULT 'legacy_unverified' CHECK (provenance IN ('direct','declared_transform','declared_generate','legacy_unverified')),
			claim_kind TEXT, claim_pattern TEXT, claim_literal INTEGER NOT NULL DEFAULT 0,
			claim_coverage TEXT NOT NULL DEFAULT 'complete' CHECK (claim_coverage IN ('complete','truncated','unavailable')),
			baseline_state TEXT NOT NULL DEFAULT 'unknown' CHECK (baseline_state IN ('normal','preexisting_dirty','unknown')),
			content_status TEXT NOT NULL DEFAULT 'retained', event_seq INTEGER,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CHECK ((provenance = 'declared_transform' AND claim_kind = 'transform') OR
			       (provenance = 'declared_generate' AND claim_kind = 'generate') OR
			       (provenance IN ('direct','legacy_unverified') AND claim_kind IS NULL)),
			UNIQUE(session_id, seq))`,
		`INSERT INTO file_changes(id,session_id,run_id,seq,path,kind,tool_name,tool_call_id,before_hash,after_hash,before_size,after_size,adds,dels,truncated,is_binary,provenance,created_at)
		 SELECT id,session_id,run_id,seq,path,kind,tool_name,tool_call_id,before_hash,after_hash,before_size,after_size,adds,dels,truncated,is_binary,
		 CASE WHEN tool_name IN ('write_file','edit_file','unified_diff') THEN 'direct' ELSE 'legacy_unverified' END,created_at FROM file_changes_old`,
		`DROP TABLE file_changes_old`,
		`CREATE INDEX idx_file_changes_session_path ON file_changes(session_id, path, seq)`,
		`CREATE INDEX idx_file_changes_session_run ON file_changes(session_id, run_id, seq)`,
		`CREATE TABLE IF NOT EXISTS filetrack_event_counters (session_id TEXT PRIMARY KEY, next_event_seq INTEGER NOT NULL) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS filetrack_runs (session_id TEXT NOT NULL, run_id TEXT NOT NULL, ordinal INTEGER NOT NULL, started_at TIMESTAMP NOT NULL, completed_at TIMESTAMP, PRIMARY KEY(session_id, run_id), UNIQUE(session_id, ordinal)) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS idx_filetrack_runs_recent ON filetrack_runs(session_id, ordinal DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("canonicalize attributed file history with %q: %w", statement, err)
		}
	}
	return nil
}

func canonicalizeFileChangesTable(tx sqliteutil.Executor) error {
	statements := []string{
		`DROP INDEX IF EXISTS idx_file_changes_session_path`,
		`DROP INDEX IF EXISTS idx_file_changes_session_run`,
		`ALTER TABLE file_changes RENAME TO file_changes_old`,
		`CREATE TABLE file_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			run_id TEXT,
			seq INTEGER NOT NULL,
			path TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('create','modify','delete')),
			tool_name TEXT,
			tool_call_id TEXT,
			before_hash TEXT,
			after_hash TEXT,
			before_size INTEGER NOT NULL DEFAULT 0,
			after_size INTEGER NOT NULL DEFAULT 0,
			adds INTEGER NOT NULL DEFAULT 0,
			dels INTEGER NOT NULL DEFAULT 0,
			truncated INTEGER NOT NULL DEFAULT 0,
			is_binary INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(session_id, seq)
		)`,
		`INSERT INTO file_changes(id,session_id,run_id,seq,path,kind,tool_name,tool_call_id,before_hash,after_hash,before_size,after_size,adds,dels,truncated,is_binary,created_at)
		 SELECT id,session_id,run_id,seq,path,kind,tool_name,tool_call_id,before_hash,after_hash,before_size,after_size,adds,dels,truncated,is_binary,created_at FROM file_changes_old`,
		`DROP TABLE file_changes_old`,
		`CREATE INDEX idx_file_changes_session_path ON file_changes(session_id, path, seq)`,
		`CREATE INDEX idx_file_changes_session_run ON file_changes(session_id, run_id, seq)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("canonicalize file history schema with %q: %w", statement, err)
		}
	}
	return nil
}

type filetrackMarkerState struct {
	rows, distinct int
	min, max       int
}

type filetrackQueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func readFiletrackMarker(db filetrackQueryRower) (filetrackMarkerState, error) {
	var state filetrackMarkerState
	err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT version), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0) FROM schema_version`).Scan(
		&state.rows, &state.distinct, &state.min, &state.max,
	)
	return state, err
}

func normalizeFiletrackMarker(tx sqliteutil.Executor, expectedVersion int) error {
	state, err := readFiletrackMarker(tx)
	if err != nil {
		return err
	}
	if state.distinct > 1 {
		return fmt.Errorf("conflicting file history schema markers range from %d to %d", state.min, state.max)
	}
	if state.rows > 0 && state.max != expectedVersion {
		return fmt.Errorf("file history schema marker changed unexpectedly: observed %d, expected %d", state.max, expectedVersion)
	}
	for _, statement := range []string{
		`DROP TABLE IF EXISTS schema_version_new`,
		`CREATE TABLE schema_version_new (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_version_new(id, version) VALUES(1, ?)`, expectedVersion); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE schema_version`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE schema_version_new RENAME TO schema_version`); err != nil {
		return err
	}
	return nil
}

func initSchema(db *sql.DB) error {
	state, err := readFiletrackMarker(db)
	if err == nil {
		if state.distinct > 1 {
			return fmt.Errorf("conflicting file history schema markers range from %d to %d", state.min, state.max)
		}
		if state.rows > 0 && state.max > schemaVersion {
			return fmt.Errorf("file history database schema version %d is newer than supported version %d", state.max, schemaVersion)
		}
		if state.rows == 1 && state.max == schemaVersion {
			return nil
		}
	}

	shared := make([]sqliteutil.Migration, len(filetrackMigrations))
	for i, migration := range filetrackMigrations {
		shared[i] = sqliteutil.Migration{Version: migration.version, Description: migration.description, Up: migration.up}
	}
	if err := sqliteutil.ValidateMigrations(shared, 2, schemaVersion, true); err != nil {
		return fmt.Errorf("validate file history migrations: %w", err)
	}

	currentVersion := 1
	if err := sqliteutil.WithImmediateMigrationTx(context.Background(), db, func(tx sqliteutil.Executor) error {
		markerExists, err := sqliteutil.TableExists(tx, "schema_version")
		if err != nil {
			return fmt.Errorf("inspect file history marker table: %w", err)
		}
		changesExist, err := sqliteutil.TableExists(tx, "file_changes")
		if err != nil {
			return fmt.Errorf("inspect file history schema: %w", err)
		}
		blobsExist, err := sqliteutil.TableExists(tx, "blobs")
		if err != nil {
			return fmt.Errorf("inspect file history blobs schema: %w", err)
		}

		if !markerExists && !changesExist && !blobsExist {
			userTables, err := sqliteutil.UserTableCount(tx)
			if err != nil {
				return fmt.Errorf("classify fresh file history database: %w", err)
			}
			if userTables != 0 {
				return fmt.Errorf("unknown unversioned file history schema: found %d unrelated tables; restore a backup or move the database aside to recreate it", userTables)
			}
			if _, err := tx.Exec(schema); err != nil {
				return fmt.Errorf("bootstrap file history schema: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO schema_version(id, version) VALUES(1, ?)`, schemaVersion); err != nil {
				return fmt.Errorf("publish file history schema version %d: %w", schemaVersion, err)
			}
			currentVersion = schemaVersion
			return nil
		}
		if markerExists {
			state, err := readFiletrackMarker(tx)
			if err != nil {
				return fmt.Errorf("read locked file history marker: %w", err)
			}
			if state.distinct > 1 {
				return fmt.Errorf("conflicting file history schema markers range from %d to %d", state.min, state.max)
			}
			if state.rows > 0 {
				currentVersion = state.max
			}
			if currentVersion > schemaVersion {
				return fmt.Errorf("file history database schema version %d is newer than supported version %d", currentVersion, schemaVersion)
			}
			if currentVersion == schemaVersion && state.rows != 1 {
				return normalizeFiletrackMarker(tx, schemaVersion)
			}
		}

		if !changesExist {
			return fmt.Errorf("unknown file history schema: file_changes table is missing; restore a backup or move the database aside to recreate it")
		}
		if !blobsExist {
			// The earliest supported test/release shape may omit blobs when no
			// content was retained. Install only this v1 baseline table.
			if _, err := tx.Exec(`CREATE TABLE blobs (hash TEXT PRIMARY KEY, size INTEGER NOT NULL, compression TEXT NOT NULL DEFAULT 'gzip', data BLOB NOT NULL, created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
				return fmt.Errorf("install file history v1 blobs table: %w", err)
			}
		}

		if !markerExists {
			if _, err := tx.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
				return fmt.Errorf("create legacy file history marker: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES(1)`); err != nil {
				return fmt.Errorf("publish legacy file history baseline: %w", err)
			}
		}

		return nil
	}); err != nil {
		return err
	}

	for _, migration := range filetrackMigrations {
		if migration.version <= currentVersion {
			continue
		}
		if err := runFiletrackMigration(db, migration); err != nil {
			return err
		}
		currentVersion = migration.version
	}
	finalState, err := readFiletrackMarker(db)
	if err != nil {
		return fmt.Errorf("read final file history marker: %w", err)
	}
	if finalState.rows != 1 || finalState.distinct != 1 || finalState.max != schemaVersion {
		return fmt.Errorf("file history migrations ended with marker rows=%d distinct=%d version=%d, want one row at %d", finalState.rows, finalState.distinct, finalState.max, schemaVersion)
	}
	return nil
}

func runFiletrackMigration(db *sql.DB, migration filetrackMigration) error {
	return sqliteutil.WithImmediateMigrationTx(context.Background(), db, func(tx sqliteutil.Executor) error {
		state, err := readFiletrackMarker(tx)
		if err != nil {
			return fmt.Errorf("read marker before file history migration %d (%s): %w", migration.version, migration.description, err)
		}
		if state.rows == 0 || state.distinct != 1 {
			return fmt.Errorf("invalid marker before file history migration %d (%s): rows=%d distinct=%d", migration.version, migration.description, state.rows, state.distinct)
		}
		if state.max >= migration.version {
			return nil
		}
		if state.max != migration.version-1 {
			return fmt.Errorf("file history migration %d (%s) expected prior version %d, observed %d", migration.version, migration.description, migration.version-1, state.max)
		}
		if err := migration.up(tx); err != nil {
			return fmt.Errorf("file history migration %d (%s), prior version %d remains safely committed: %w", migration.version, migration.description, state.max, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, migration.version); err != nil {
			return fmt.Errorf("publish file history migration %d (%s): %w", migration.version, migration.description, err)
		}
		return nil
	})
}

// Close closes both attributed and observation databases.
func (s *Store) Close() error {
	var first error
	if s.observationDB != nil {
		first = s.observationDB.Close()
	}
	if err := s.db.Close(); first == nil {
		first = err
	}
	return first
}

// MaxFileBytes returns the per-file content cap.
func (s *Store) MaxFileBytes() int {
	return s.maxFileBytes
}

func normalizePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return filepath.Clean(path)
}

func (s *Store) lockRecordSession(sessionID string) func() {
	s.recordLocksMu.Lock()
	if s.recordLocks == nil {
		s.recordLocks = make(map[string]*recordSessionLock)
	}
	lock := s.recordLocks[sessionID]
	if lock == nil {
		lock = &recordSessionLock{}
		s.recordLocks[sessionID] = lock
	}
	lock.refs++
	s.recordLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.recordLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.recordLocks, sessionID)
		}
		s.recordLocksMu.Unlock()
	}
}

func (s *Store) totalBudgetCheckDue(retainedBytes int64) bool {
	byteInterval := s.maxTotalBytes / 16
	if byteInterval < 1 {
		byteInterval = 1
	}
	if byteInterval > maxUncheckedTotalBytes {
		byteInterval = maxUncheckedTotalBytes
	}
	return s.uncheckedTotalBytes+retainedBytes >= byteInterval ||
		s.uncheckedTotalRecordCount+1 >= maxUncheckedTotalRecords
}

func (s *Store) resolveAttributedBaseline(ctx context.Context, rec ChangeRecord) string {
	var priorAfterHash, priorKind string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(after_hash,''), kind FROM file_changes
		WHERE session_id=? AND path=? AND provenance IN ('direct','declared_transform','declared_generate')
		ORDER BY seq DESC LIMIT 1`, rec.SessionID, rec.Path).Scan(&priorAfterHash, &priorKind)
	if err == sql.ErrNoRows {
		return rec.BaselineState
	}
	if err != nil {
		return BaselineUnknown
	}
	if rec.BeforeMissing {
		if priorAfterHash == "" {
			return BaselineNormal
		}
		return BaselinePreexistingDirty
	}
	if rec.BeforeUnknown {
		return BaselineUnknown
	}
	if priorAfterHash == "" {
		if priorKind == KindDelete {
			return BaselinePreexistingDirty
		}
		return BaselineUnknown
	}
	sum := sha256.Sum256(rec.Before)
	if priorAfterHash == hex.EncodeToString(sum[:]) {
		return BaselineNormal
	}
	return BaselinePreexistingDirty
}

// RecordChange is retained for source compatibility with older direct callers.
// New code must use RecordAttributedChange and provide explicit provenance. The
// compatibility path never upgrades shell detections: shell rows remain legacy
// unverified and are excluded from attributed views.
func (s *Store) RecordChange(ctx context.Context, rec ChangeRecord) (*Change, error) {
	if rec.Provenance == "" {
		rec.Provenance = ProvenanceLegacyUnverified
	}
	return s.recordChange(ctx, rec, false)
}

// RecordAttributedChange records a classified, witnessed/claim-verified file
// transition. Missing or incompatible attribution metadata is rejected.
func (s *Store) RecordAttributedChange(ctx context.Context, rec ChangeRecord) (*Change, error) {
	return s.recordChange(ctx, rec, true)
}

func (s *Store) recordChange(ctx context.Context, rec ChangeRecord, requireAttributed bool) (*Change, error) {
	rec.Path = normalizePath(rec.Path)
	if rec.SessionID == "" || rec.Path == "" {
		return nil, nil
	}
	if rec.ClaimCoverage == "" {
		rec.ClaimCoverage = CoverageComplete
	}
	if rec.BaselineState == "" {
		rec.BaselineState = BaselineUnknown
	}
	if err := validateChangeRecord(rec, requireAttributed); err != nil {
		return nil, err
	}

	var kind string
	switch {
	case rec.BeforeMissing && rec.AfterMissing:
		return nil, nil
	case rec.BeforeMissing:
		kind = KindCreate
	case rec.AfterMissing:
		kind = KindDelete
	default:
		kind = KindModify
		if !rec.BeforeUnknown && !rec.AfterUnknown && bytes.Equal(rec.Before, rec.After) {
			return nil, nil
		}
	}

	// Sequence and per-session budget decisions need ordering only within the
	// owning session; unrelated sessions proceed independently.
	unlockSession := s.lockRecordSession(rec.SessionID)
	defer unlockSession()
	if rec.Provenance != ProvenanceLegacyUnverified {
		rec.BaselineState = s.resolveAttributedBaseline(ctx, rec)
	}

	hasBefore := !rec.BeforeMissing && !rec.BeforeUnknown
	hasAfter := !rec.AfterMissing && !rec.AfterUnknown

	var beforeSize, afterSize int64
	switch {
	case hasBefore:
		beforeSize = int64(len(rec.Before))
	case rec.BeforeUnknown:
		beforeSize = rec.BeforeSizeHint
	}
	switch {
	case hasAfter:
		afterSize = int64(len(rec.After))
	case rec.AfterUnknown:
		afterSize = rec.AfterSizeHint
	}

	_, isImage := imageChangeMediaType(kind, rec.Before, rec.After)
	isBinary := isImage || (hasBefore && isBinaryContent(rec.Before)) || (hasAfter && isBinaryContent(rec.After))

	// A change is either fully retained (all sides the kind needs are stored)
	// or metadata-only. Mixed retention would complicate baseline resolution
	// for marginal benefit. Browser-renderable images are the sole binary
	// exception: retaining them lets the web diff show the actual before/after.
	retain := (!isBinary || isImage) && !rec.BeforeUnknown && !rec.AfterUnknown
	contentStatus := ContentRetained
	if isImage {
		contentStatus = ContentRetainedImage
	} else if isBinary {
		contentStatus = ContentBinaryUnrenderable
	}
	if rec.BeforeUnknown && rec.AfterUnknown {
		contentStatus = ContentBothUnknown
	} else if rec.BeforeUnknown {
		contentStatus = ContentBeforeUnknown
	} else if rec.AfterUnknown {
		contentStatus = ContentAfterUnknown
	}
	if retain && hasBefore && len(rec.Before) > s.maxFileBytes {
		retain = false
		contentStatus = ContentOversized
	}
	if retain && hasAfter && len(rec.After) > s.maxFileBytes {
		retain = false
		contentStatus = ContentOversized
	}
	if retain {
		used, err := s.sessionBytesUsed(ctx, rec.SessionID)
		if err != nil {
			return nil, err
		}
		if used+beforeSize+afterSize > int64(s.maxSessionBytes) {
			retain = false
			contentStatus = ContentSessionBudget
		}
	}

	var adds, dels int
	if retain && !isImage {
		switch kind {
		case KindCreate:
			adds, _ = CountAddsDels(nil, rec.After)
		case KindDelete:
			_, dels = CountAddsDels(rec.Before, nil)
		default:
			adds, dels = CountAddsDels(rec.Before, rec.After)
		}
	}

	change := &Change{
		RunID:            rec.RunID,
		Path:             rec.Path,
		Kind:             kind,
		ToolName:         rec.ToolName,
		ToolCallID:       rec.ToolCallID,
		BeforeSize:       beforeSize,
		AfterSize:        afterSize,
		Adds:             adds,
		Dels:             dels,
		Truncated:        !retain,
		IsBinary:         isBinary,
		Provenance:       rec.Provenance,
		Provenances:      []string{rec.Provenance},
		ClaimKind:        rec.ClaimKind,
		ClaimPattern:     rec.ClaimPattern,
		ClaimLiteral:     rec.ClaimLiteral,
		ClaimCoverage:    rec.ClaimCoverage,
		BaselineState:    rec.BaselineState,
		ContentStatus:    contentStatus,
		ContentAvailable: retain,
	}

	retainedBytes := int64(0)
	if retain {
		retainedBytes = beforeSize + afterSize
	}
	s.totalMu.Lock()
	checkTotalBudget := s.totalBudgetCheckDue(retainedBytes)
	if checkTotalBudget {
		s.uncheckedTotalBytes = 0
		s.uncheckedTotalRecordCount = 0
	} else {
		// Reserve this accounting before I/O. Failed writes may trigger an earlier
		// check, which is conservative and avoids cross-session synchronization.
		s.uncheckedTotalBytes += retainedBytes
		s.uncheckedTotalRecordCount++
	}
	s.totalMu.Unlock()

	if checkTotalBudget {
		s.pruneMu.Lock()
		defer s.pruneMu.Unlock()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin file change transaction: %w", err)
	}
	defer tx.Rollback()
	if err := ensureRunForChangeTx(ctx, tx, rec.SessionID, rec.RunID); err != nil {
		return nil, fmt.Errorf("ensure file tracking run: %w", err)
	}
	change.EventSeq, err = allocateEventSeqTx(ctx, tx, rec.SessionID)
	if err != nil {
		return nil, err
	}

	if retain {
		if hasBefore {
			h, err := insertBlob(ctx, tx, rec.Before)
			if err != nil {
				return nil, err
			}
			change.BeforeHash = h
		}
		if hasAfter {
			h, err := insertBlob(ctx, tx, rec.After)
			if err != nil {
				return nil, err
			}
			change.AfterHash = h
		}
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO file_changes
			(session_id, run_id, seq, path, kind, tool_name, tool_call_id,
			 before_hash, after_hash, before_size, after_size,
			 adds, dels, truncated, is_binary, provenance, claim_kind,
			 claim_pattern, claim_literal, claim_coverage, baseline_state,
			 content_status, event_seq)
		VALUES
			(?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM file_changes WHERE session_id = ?),
			 ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING seq`,
		rec.SessionID, nullString(rec.RunID), rec.SessionID,
		rec.Path, kind, rec.ToolName, rec.ToolCallID,
		nullString(change.BeforeHash), nullString(change.AfterHash), beforeSize, afterSize,
		adds, dels, boolInt(change.Truncated), boolInt(isBinary), rec.Provenance,
		nullString(rec.ClaimKind), nullString(rec.ClaimPattern), boolInt(rec.ClaimLiteral),
		rec.ClaimCoverage, rec.BaselineState, contentStatus, change.EventSeq,
	).Scan(&change.Seq)
	if err != nil {
		return nil, fmt.Errorf("insert file change: %w", err)
	}

	var totalBudgetPruned bool
	if checkTotalBudget {
		totalBudgetPruned, err = s.enforceTotalBudget(ctx, tx)
		if err != nil {
			return nil, fmt.Errorf("enforce total budget: %w", err)
		}
		var exists bool
		if err := tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM file_changes WHERE session_id = ? AND seq = ?)",
			rec.SessionID, change.Seq).Scan(&exists); err != nil {
			return nil, fmt.Errorf("verify recorded file change: %w", err)
		}
		if !exists {
			return nil, fmt.Errorf("file history database is over its total budget; change not retained")
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit file change: %w", err)
	}

	if checkTotalBudget && totalBudgetPruned {
		// Budget enforcement pruned at least one session, invalidating cached
		// retained-byte totals. The next write will reload only what it needs.
		s.mu.Lock()
		s.sessionBytes = make(map[string]int64)
		s.mu.Unlock()
	} else {
		if retain {
			s.mu.Lock()
			if _, ok := s.sessionBytes[rec.SessionID]; ok {
				s.sessionBytes[rec.SessionID] += retainedBytes
			}
			s.mu.Unlock()
		}
	}

	return change, nil
}

// sessionBytesUsed returns the retained-content bytes already recorded for a
// session, warm-loading the cache from the DB on first touch.
func (s *Store) sessionBytesUsed(ctx context.Context, sessionID string) (int64, error) {
	s.mu.Lock()
	used, ok := s.sessionBytes[sessionID]
	s.mu.Unlock()
	if ok {
		return used, nil
	}

	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(before_size + after_size), 0)
		FROM file_changes WHERE session_id = ? AND truncated = 0`, sessionID).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("load session budget: %w", err)
	}

	s.mu.Lock()
	s.sessionBytes[sessionID] = used
	s.mu.Unlock()
	return used, nil
}

func (s *Store) HasAttributedPath(ctx context.Context, sessionID, path string) (bool, error) {
	path = normalizePath(path)
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM file_changes WHERE session_id=? AND path=?
		AND provenance IN ('direct','declared_transform','declared_generate'))`, sessionID, path).Scan(&exists)
	return exists, err
}

// SessionPaths returns the distinct absolute paths already recorded for a session.
func (s *Store) SessionPaths(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT DISTINCT path FROM file_changes WHERE session_id = ? AND provenance IN ('direct','declared_transform','declared_generate')", sessionID)
	if err != nil {
		return nil, fmt.Errorf("query session paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	seen := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if p = normalizePath(p); p != "" {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}
	return paths, rows.Err()
}

// pathSpan is the fold of all change rows for one path: its baseline (first
// row) and latest state (last row).
type pathSpan struct {
	path               string
	firstKind          string
	firstBeforeHash    string
	firstBinary        bool
	firstBaselineState string
	firstContentStatus string
	lastKind           string
	lastAfterHash      string
	lastBinary         bool
	lastContentStatus  string
	lastSeq            int64
	provenance         string
	provenances        []string
	claimCoverage      string
}

type recentRunWindow struct {
	runIDs      []string
	snapshotSeq int64
}

func (s *Store) latestRunWindow(ctx context.Context, sessionID string, limit int, snapshotSeq int64) (recentRunWindow, error) {
	if limit < 1 {
		limit = 1
	}
	var rows *sql.Rows
	var err error
	if snapshotSeq > 0 {
		// Compatibility for the legacy integer snapshot: resolve runs from rows
		// visible at that attributed sequence. New clients pin dual streams with
		// the opaque snapshot token.
		rows, err = s.db.QueryContext(ctx, `SELECT run_id FROM file_changes
			WHERE session_id=? AND seq<=? AND COALESCE(run_id,'')<>''
			GROUP BY run_id ORDER BY MAX(seq) DESC LIMIT ?`, sessionID, snapshotSeq, limit)
	} else {
		// The active run is the current turn. Include it so clients can refresh
		// file changes while tools are still running rather than one turn later.
		rows, err = s.db.QueryContext(ctx, `
			SELECT run_id FROM filetrack_runs
			WHERE session_id = ?
			ORDER BY ordinal DESC LIMIT ?`, sessionID, limit)
	}
	if err != nil {
		return recentRunWindow{}, fmt.Errorf("query recent file tracking runs: %w", err)
	}
	defer rows.Close()

	window := recentRunWindow{snapshotSeq: snapshotSeq}
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return recentRunWindow{}, err
		}
		window.runIDs = append(window.runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return recentRunWindow{}, err
	}
	if window.snapshotSeq == 0 {
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM file_changes WHERE session_id = ?`, sessionID).Scan(&window.snapshotSeq)
	}
	return window, nil
}

func runWindowFilter(runIDs []string, snapshotSeq int64) (string, []any) {
	filter := ""
	var args []any
	if runIDs != nil {
		if len(runIDs) == 0 {
			return " AND 1 = 0", nil
		}
		args = make([]any, len(runIDs))
		for i, runID := range runIDs {
			args[i] = runID
		}
		filter = " AND run_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",") + ")"
	}
	if snapshotSeq > 0 {
		filter += " AND seq <= ?"
		args = append(args, snapshotSeq)
	}
	return filter, args
}

func (s *Store) sessionSpans(ctx context.Context, sessionID string) ([]*pathSpan, error) {
	return s.sessionRunSpans(ctx, sessionID, nil, 0)
}

func (s *Store) sessionRunSpans(ctx context.Context, sessionID string, runIDs []string, snapshotSeq int64) ([]*pathSpan, error) {
	query := `
		SELECT seq, path, kind, COALESCE(before_hash, ''), COALESCE(after_hash, ''), is_binary,
		       provenance, baseline_state, content_status, claim_coverage
		FROM file_changes WHERE session_id = ?
		AND provenance IN ('direct','declared_transform','declared_generate')`
	args := []any{sessionID}
	filter, filterArgs := runWindowFilter(runIDs, snapshotSeq)
	query += filter
	args = append(args, filterArgs...)
	query += " ORDER BY seq"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query session changes: %w", err)
	}
	defer rows.Close()

	spans := make(map[string]*pathSpan)
	var order []string
	for rows.Next() {
		var seq int64
		var path, kind, beforeHash, afterHash, provenance, baselineState, contentStatus, coverage string
		var isBinary bool
		if err := rows.Scan(&seq, &path, &kind, &beforeHash, &afterHash, &isBinary, &provenance, &baselineState, &contentStatus, &coverage); err != nil {
			return nil, err
		}
		path = normalizePath(path)
		if path == "" {
			continue
		}
		span, ok := spans[path]
		if !ok {
			span = &pathSpan{path: path, firstKind: kind, firstBeforeHash: beforeHash, firstBinary: isBinary,
				firstBaselineState: baselineState, firstContentStatus: contentStatus, provenance: provenance,
				provenances: []string{provenance}, claimCoverage: coverage}
			spans[path] = span
			order = append(order, path)
		} else if !containsString(span.provenances, provenance) {
			span.provenances = append(span.provenances, provenance)
			span.provenance = "mixed"
		}
		span.claimCoverage = worstCoverage(span.claimCoverage, coverage)
		span.lastKind = kind
		span.lastAfterHash = afterHash
		span.lastBinary = isBinary
		span.lastContentStatus = contentStatus
		span.lastSeq = seq
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]*pathSpan, 0, len(order))
	for _, p := range order {
		result = append(result, spans[p])
	}
	return result, nil
}

func (s *Store) sessionPathSpan(ctx context.Context, sessionID, path string) (*pathSpan, error) {
	return s.sessionRunPathSpan(ctx, sessionID, nil, 0, path)
}

func (s *Store) sessionRunPathSpan(ctx context.Context, sessionID string, runIDs []string, snapshotSeq int64, path string) (*pathSpan, error) {
	path = normalizePath(path)
	if path == "" {
		return nil, nil
	}
	sp := &pathSpan{path: path}
	where := "session_id = ? AND path = ? AND provenance IN ('direct','declared_transform','declared_generate')"
	args := []any{sessionID, path}
	filter, filterArgs := runWindowFilter(runIDs, snapshotSeq)
	where += filter
	args = append(args, filterArgs...)
	if err := s.db.QueryRowContext(ctx, `
		SELECT kind, COALESCE(before_hash, ''), is_binary, baseline_state, content_status,
		       provenance, claim_coverage
		FROM file_changes
		WHERE `+where+`
		ORDER BY seq ASC LIMIT 1`, args...).
		Scan(&sp.firstKind, &sp.firstBeforeHash, &sp.firstBinary, &sp.firstBaselineState,
			&sp.firstContentStatus, &sp.provenance, &sp.claimCoverage); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("query first path change: %w", err)
	}
	sp.provenances = []string{sp.provenance}
	rows, err := s.db.QueryContext(ctx, `SELECT provenance, claim_coverage FROM file_changes WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("query path provenance: %w", err)
	}
	for rows.Next() {
		var provenance, coverage string
		if err := rows.Scan(&provenance, &coverage); err != nil {
			rows.Close()
			return nil, err
		}
		if !containsString(sp.provenances, provenance) {
			sp.provenances = append(sp.provenances, provenance)
			sp.provenance = "mixed"
		}
		sp.claimCoverage = worstCoverage(sp.claimCoverage, coverage)
	}
	rows.Close()
	if err := s.db.QueryRowContext(ctx, `
		SELECT seq, kind, COALESCE(after_hash, ''), is_binary, content_status
		FROM file_changes
		WHERE `+where+`
		ORDER BY seq DESC LIMIT 1`, args...).
		Scan(&sp.lastSeq, &sp.lastKind, &sp.lastAfterHash, &sp.lastBinary, &sp.lastContentStatus); err != nil {
		return nil, fmt.Errorf("query last path change: %w", err)
	}
	return sp, nil
}

// resolve computes the cumulative kind for a span. ok=false means the file is
// a net no-op for the session (e.g. created then deleted).
func (sp *pathSpan) resolve() (kind string, ok bool) {
	existedAtBaseline := sp.firstKind != KindCreate
	existsNow := sp.lastKind != KindDelete

	switch {
	case !existedAtBaseline && !existsNow:
		return "", false
	case !existedAtBaseline:
		return KindCreate, true
	case !existsNow:
		return KindDelete, true
	default:
		if sp.firstBeforeHash != "" && sp.firstBeforeHash == sp.lastAfterHash {
			return "", false // content returned to baseline
		}
		return KindModify, true
	}
}

// retainedImage relies on images being the only binary content RecordChange
// retains. Hash presence distinguishes retained images from metadata-only
// unsupported binaries.
func (sp *pathSpan) retainedImage(kind string) bool {
	needBefore, needAfter := blobsNeeded(kind)
	if needBefore && (!sp.firstBinary || sp.firstBeforeHash == "") {
		return false
	}
	if needAfter && (!sp.lastBinary || sp.lastAfterHash == "") {
		return false
	}
	return needBefore || needAfter
}

func (sp *pathSpan) hasBinarySide(kind string) bool {
	needBefore, needAfter := blobsNeeded(kind)
	return (needBefore && sp.firstBinary) || (needAfter && sp.lastBinary)
}

// blobsNeeded reports which sides a cumulative diff requires.
func blobsNeeded(kind string) (needBefore, needAfter bool) {
	switch kind {
	case KindCreate:
		return false, true
	case KindDelete:
		return true, false
	default:
		return true, true
	}
}

// ListSessionChanges returns the cumulative per-file changes for a session,
// sorted by path. Net no-ops are omitted.
func (s *Store) ListSessionChanges(ctx context.Context, sessionID string) ([]CumulativeChange, error) {
	spans, err := s.sessionSpans(ctx, sessionID)
	return s.listChangesFromSpans(ctx, spans, err)
}

// ListRecentRunChanges returns the cumulative changes across the latest file
// tracking runs, including an in-progress run. Rows without run identities are excluded.
func (s *Store) ListRecentRunChanges(ctx context.Context, sessionID string, runs int) ([]CumulativeChange, error) {
	window, err := s.latestRunWindow(ctx, sessionID, runs, 0)
	if err != nil || len(window.runIDs) == 0 {
		return nil, err
	}
	spans, err := s.sessionRunSpans(ctx, sessionID, window.runIDs, window.snapshotSeq)
	changes, err := s.listChangesFromSpans(ctx, spans, err)
	if err != nil {
		return nil, err
	}
	if runs > 1 {
		for i := range changes {
			changes[i].SnapshotSeq = window.snapshotSeq
		}
	}
	return changes, nil
}

func (s *Store) listChangesFromSpans(ctx context.Context, spans []*pathSpan, err error) ([]CumulativeChange, error) {
	if err != nil {
		return nil, err
	}

	changes := make([]CumulativeChange, 0, len(spans))
	for _, sp := range spans {
		kind, ok := sp.resolve()
		if !ok {
			continue
		}

		change := CumulativeChange{
			Path: sp.path, Kind: kind, Seq: sp.lastSeq, Provenance: sp.provenance,
			Provenances: append([]string(nil), sp.provenances...), BaselineState: sp.firstBaselineState,
			ClaimCoverage: sp.claimCoverage, ContentStatus: cumulativeContentStatus(sp, kind),
		}

		needBefore, needAfter := blobsNeeded(kind)

		// Retained binaries are browser-renderable images. Their line counts are
		// always zero, so verify blob presence without reading and decompressing
		// potentially multi-megabyte image bodies on every list refresh.
		if sp.retainedImage(kind) {
			change.ContentAvailable = true
			if needBefore && !s.blobExists(ctx, sp.firstBeforeHash) {
				change.Truncated = true
				change.ContentAvailable = false
			}
			if needAfter && !s.blobExists(ctx, sp.lastAfterHash) {
				change.Truncated = true
				change.ContentAvailable = false
			}
			changes = append(changes, change)
			continue
		}
		// A binary side paired with a non-binary side is not a renderable image
		// comparison and must never be passed through line-based diffing.
		if sp.hasBinarySide(kind) {
			change.Truncated = true
			changes = append(changes, change)
			continue
		}

		var before, after []byte
		truncated := false
		if needBefore {
			if sp.firstBeforeHash == "" {
				truncated = true
			} else if before, err = s.getBlob(ctx, sp.firstBeforeHash); err != nil {
				truncated = true
			}
		}
		if needAfter {
			if sp.lastAfterHash == "" {
				truncated = true
			} else if after, err = s.getBlob(ctx, sp.lastAfterHash); err != nil {
				truncated = true
			}
		}

		if truncated {
			change.Truncated = true
			change.ContentAvailable = false
		} else {
			change.ContentAvailable = true
			if _, isImage := imageChangeMediaType(kind, before, after); !isImage {
				change.Adds, change.Dels = CountAddsDels(before, after)
			}
		}
		changes = append(changes, change)
	}

	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// GetFileDiffContent returns the baseline and current contents for one path
// in a session, or nil when the path has no net change recorded.
func (s *Store) GetFileDiffContent(ctx context.Context, sessionID, path string) (*FileDiffContent, error) {
	sp, err := s.sessionPathSpan(ctx, sessionID, path)
	return s.fileDiffContentFromSpan(ctx, sp, err)
}

// GetRecentRunFileDiffContent returns one file diff across the latest runs that
// recorded file changes. A positive snapshotSeq pins the rolling window.
func (s *Store) GetRecentRunFileDiffContent(ctx context.Context, sessionID, path string, runs int, snapshotSeq int64) (*FileDiffContent, error) {
	window, err := s.latestRunWindow(ctx, sessionID, runs, snapshotSeq)
	if err != nil || len(window.runIDs) == 0 {
		return nil, err
	}
	sp, err := s.sessionRunPathSpan(ctx, sessionID, window.runIDs, window.snapshotSeq, path)
	return s.fileDiffContentFromSpan(ctx, sp, err)
}

func (s *Store) fileDiffContentFromSpan(ctx context.Context, sp *pathSpan, err error) (*FileDiffContent, error) {
	if err != nil || sp == nil {
		return nil, err
	}
	kind, ok := sp.resolve()
	if !ok {
		return nil, nil
	}

	content := &FileDiffContent{Path: sp.path, Kind: kind, ContentStatus: cumulativeContentStatus(sp, kind), Provenance: sp.provenance, BaselineState: sp.firstBaselineState, ClaimCoverage: sp.claimCoverage}
	needBefore, needAfter := blobsNeeded(kind)
	if sp.retainedImage(kind) {
		content.IsImage = true
		content.ContentAvailable = true
		if needBefore && !s.blobExists(ctx, sp.firstBeforeHash) {
			content.Truncated = true
		}
		if needAfter && !s.blobExists(ctx, sp.lastAfterHash) {
			content.Truncated = true
		}
		content.ContentAvailable = !content.Truncated
		return content, nil
	}
	if sp.hasBinarySide(kind) {
		content.Truncated = true
		return content, nil
	}
	if needBefore {
		if sp.firstBeforeHash == "" {
			content.Truncated = true
		} else if content.Before, err = s.getBlob(ctx, sp.firstBeforeHash); err != nil {
			content.Truncated = true
		}
	}
	if needAfter {
		if sp.lastAfterHash == "" {
			content.Truncated = true
		} else if content.After, err = s.getBlob(ctx, sp.lastAfterHash); err != nil {
			content.Truncated = true
		}
	}
	if !content.Truncated && (imageMediaType(content.Before) != "" || imageMediaType(content.After) != "" || isBinaryContent(content.Before) || isBinaryContent(content.After)) {
		content.Before = nil
		content.After = nil
		content.Truncated = true
	}
	content.ContentAvailable = !content.Truncated
	return content, nil
}

// GetFileDiffSide returns one retained side of an image diff without loading
// the other side. It returns nil for unknown paths, truncated content, and
// non-image diffs.
func (s *Store) GetFileDiffSide(ctx context.Context, sessionID, path, side string) (*FileDiffSide, error) {
	if side != "before" && side != "after" {
		return nil, ErrInvalidDiffSide
	}
	sp, err := s.sessionPathSpan(ctx, sessionID, path)
	return s.fileDiffSideFromSpan(ctx, sp, side, err)
}

// GetRecentRunFileDiffSide returns one retained image side across the latest
// runs that recorded file changes. A positive snapshotSeq pins the window.
func (s *Store) GetRecentRunFileDiffSide(ctx context.Context, sessionID, path, side string, runs int, snapshotSeq int64) (*FileDiffSide, error) {
	if side != "before" && side != "after" {
		return nil, ErrInvalidDiffSide
	}
	window, err := s.latestRunWindow(ctx, sessionID, runs, snapshotSeq)
	if err != nil || len(window.runIDs) == 0 {
		return nil, err
	}
	sp, err := s.sessionRunPathSpan(ctx, sessionID, window.runIDs, window.snapshotSeq, path)
	return s.fileDiffSideFromSpan(ctx, sp, side, err)
}

// GetFileDiffTextSide returns one retained UTF-8 text side without loading the
// other side. It returns nil for unknown, truncated, binary, and image content.
func (s *Store) GetFileDiffTextSide(ctx context.Context, sessionID, path, side string) (*FileDiffTextSide, error) {
	if side != "before" && side != "after" {
		return nil, ErrInvalidDiffSide
	}
	sp, err := s.sessionPathSpan(ctx, sessionID, path)
	return s.fileDiffTextSideFromSpan(ctx, sp, side, err)
}

// GetRecentRunFileDiffTextSide returns one retained UTF-8 text side across the
// latest runs that recorded file changes. A positive snapshotSeq pins the window.
func (s *Store) GetRecentRunFileDiffTextSide(ctx context.Context, sessionID, path, side string, runs int, snapshotSeq int64) (*FileDiffTextSide, error) {
	if side != "before" && side != "after" {
		return nil, ErrInvalidDiffSide
	}
	window, err := s.latestRunWindow(ctx, sessionID, runs, snapshotSeq)
	if err != nil || len(window.runIDs) == 0 {
		return nil, err
	}
	sp, err := s.sessionRunPathSpan(ctx, sessionID, window.runIDs, window.snapshotSeq, path)
	return s.fileDiffTextSideFromSpan(ctx, sp, side, err)
}

func (sp *pathSpan) diffSide(side string) (kind, hash string, binary bool, ok bool, err error) {
	kind, ok = sp.resolve()
	if !ok {
		return "", "", false, false, nil
	}
	needBefore, needAfter := blobsNeeded(kind)
	if (side == "before" && !needBefore) || (side == "after" && !needAfter) {
		return "", "", false, false, ErrInvalidDiffSide
	}
	if side == "before" {
		return kind, sp.firstBeforeHash, sp.firstBinary, true, nil
	}
	return kind, sp.lastAfterHash, sp.lastBinary, true, nil
}

func (s *Store) fileDiffSideFromSpan(ctx context.Context, sp *pathSpan, side string, spanErr error) (*FileDiffSide, error) {
	if spanErr != nil || sp == nil {
		return nil, spanErr
	}
	kind, hash, _, ok, err := sp.diffSide(side)
	if err != nil || !ok {
		return nil, err
	}
	if !sp.retainedImage(kind) || hash == "" {
		return nil, nil
	}
	data, err := s.getBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	mediaType := imageMediaType(data)
	if mediaType == "" {
		return nil, nil
	}
	return &FileDiffSide{Path: sp.path, Kind: kind, Side: side, Data: data, MediaType: mediaType}, nil
}

func (s *Store) fileDiffTextSideFromSpan(ctx context.Context, sp *pathSpan, side string, spanErr error) (*FileDiffTextSide, error) {
	if spanErr != nil || sp == nil {
		return nil, spanErr
	}
	kind, hash, binary, ok, err := sp.diffSide(side)
	if err != nil || !ok {
		return nil, err
	}
	if binary || hash == "" {
		return nil, nil
	}
	data, err := s.getBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !IsRenderableText(data) {
		return nil, nil
	}
	return &FileDiffTextSide{Path: sp.path, Kind: kind, Side: side, Data: data}, nil
}

// GC removes change rows for sessions that no longer exist in the sessions DB
// (and rows older than maxAgeDays when > 0), then sweeps unreferenced blobs.
func (s *Store) GC(ctx context.Context, sessionsDBPath string, maxAgeDays int) error {
	// ATTACH is per-connection, so pin one connection for the whole sweep.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire gc connection: %w", err)
	}
	defer conn.Close()

	if sessionsDBPath != "" && sessionsDBPath != ":memory:" {
		if _, statErr := os.Stat(sessionsDBPath); statErr == nil {
			uri := "file:" + filepath.ToSlash(sessionsDBPath) + "?mode=ro"
			if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS sess", uri); err != nil {
				return fmt.Errorf("attach sessions db: %w", err)
			}
			_, delErr := conn.ExecContext(ctx,
				"DELETE FROM file_changes WHERE session_id NOT IN (SELECT id FROM sess.sessions)")
			if _, err := conn.ExecContext(ctx, "DETACH DATABASE sess"); err != nil && delErr == nil {
				delErr = err
			}
			if delErr != nil {
				return fmt.Errorf("gc stale sessions: %w", delErr)
			}
		}
	}

	if maxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
		if _, err := conn.ExecContext(ctx,
			"DELETE FROM file_changes WHERE created_at < ?", cutoff); err != nil {
			return fmt.Errorf("gc old changes: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, blobSweepSQL); err != nil {
		return fmt.Errorf("gc unreferenced blobs: %w", err)
	}

	// Reclaim space freed by the sweep; cheap when nothing was deleted.
	if _, err := conn.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}

	if _, err := s.enforceTotalBudget(ctx, conn); err != nil {
		return fmt.Errorf("enforce total budget: %w", err)
	}

	s.mu.Lock()
	s.sessionBytes = make(map[string]int64)
	s.mu.Unlock()
	if err := s.gcObservations(ctx, sessionsDBPath, maxAgeDays); err != nil {
		return err
	}
	return nil
}

const blobSweepSQL = `
	DELETE FROM blobs WHERE hash NOT IN (
		SELECT before_hash FROM file_changes WHERE before_hash IS NOT NULL
		UNION
		SELECT after_hash FROM file_changes WHERE after_hash IS NOT NULL
	)`

type totalBudgetDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// enforceTotalBudget prunes the least recently changed sessions' history until
// the database fits maxTotalBytes. This is the cross-session backstop: the
// per-session budget bounds one session, but many sessions could otherwise
// grow the database without limit.
func (s *Store) enforceTotalBudget(ctx context.Context, db totalBudgetDB) (bool, error) {
	pruned := false
	var previousSize int64 = -1
	for {
		var size int64
		var err error
		if s.maxTotalBytes <= filetrackStructuralReserveBytes {
			size, err = rawDatabaseSize(ctx, db)
		} else {
			size, err = databaseSize(ctx, db)
		}
		if err != nil {
			return pruned, err
		}
		if size <= s.maxTotalBytes {
			return pruned, nil
		}
		if previousSize >= 0 && size >= previousSize {
			// Page reclamation may not become visible until a live transaction
			// commits. Stop after making bounded progress rather than deleting
			// every session while chasing an unchanged page count.
			return pruned, nil
		}
		previousSize = size

		var oldest string
		err = db.QueryRowContext(ctx, `
			SELECT session_id FROM file_changes
			GROUP BY session_id
			ORDER BY MAX(created_at) ASC, MAX(id) ASC, session_id ASC
			LIMIT 1`).Scan(&oldest)
		if err == sql.ErrNoRows {
			return pruned, nil // nothing left to prune; remaining size is structural overhead
		}
		if err != nil {
			return pruned, err
		}

		if _, err := db.ExecContext(ctx, "DELETE FROM file_changes WHERE session_id = ?", oldest); err != nil {
			return pruned, err
		}
		pruned = true
		if _, err := db.ExecContext(ctx, blobSweepSQL); err != nil {
			return pruned, err
		}
		if _, err := db.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
			return pruned, err
		}
	}
}

// databaseSize returns the database file size in bytes (page count × page size).
// SQLite's fixed schema/index pages do not grow with retained content and would
// otherwise make very small configured budgets evict all useful history after
// adding the mandatory run/event indexes. Charge the growing database footprint
// while allowing a bounded structural reserve.
const filetrackStructuralReserveBytes int64 = 64 * 1024

func rawDatabaseSize(ctx context.Context, db totalBudgetDB) (int64, error) {
	var pageCount, pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page count: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page size: %w", err)
	}
	return pageCount * pageSize, nil
}

func databaseSize(ctx context.Context, db totalBudgetDB) (int64, error) {
	size, err := rawDatabaseSize(ctx, db)
	if err != nil {
		return 0, err
	}
	if size > filetrackStructuralReserveBytes {
		size -= filetrackStructuralReserveBytes
	} else {
		size = 0
	}
	return size, nil
}

func insertBlob(ctx context.Context, tx *sql.Tx, content []byte) (string, error) {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	data, compression := compress(content)
	_, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO blobs (hash, size, compression, data) VALUES (?, ?, ?, ?)",
		hash, len(content), compression, data)
	if err != nil {
		return "", fmt.Errorf("insert blob: %w", err)
	}
	return hash, nil
}

func (s *Store) getBlob(ctx context.Context, hash string) ([]byte, error) {
	var data []byte
	var compression string
	err := s.db.QueryRowContext(ctx,
		"SELECT data, compression FROM blobs WHERE hash = ?", hash).Scan(&data, &compression)
	if err != nil {
		return nil, fmt.Errorf("load blob %s: %w", hash, err)
	}
	return decompress(data, compression)
}

func (s *Store) blobExists(ctx context.Context, hash string) bool {
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)", hash).Scan(&exists); err != nil {
		return false
	}
	return exists
}

func compress(content []byte) (data []byte, compression string) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(content); err != nil {
		return content, "none"
	}
	if err := w.Close(); err != nil {
		return content, "none"
	}
	if buf.Len() >= len(content) {
		return content, "none"
	}
	return buf.Bytes(), "gzip"
}

func decompress(data []byte, compression string) ([]byte, error) {
	switch compression {
	case "none":
		return data, nil
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open gzip blob: %w", err)
		}
		defer r.Close()
		return io.ReadAll(r)
	default:
		return nil, fmt.Errorf("unknown blob compression %q", compression)
	}
}

var browserImageMediaTypes = map[string]struct{}{
	"image/bmp":    {},
	"image/gif":    {},
	"image/jpeg":   {},
	"image/png":    {},
	"image/webp":   {},
	"image/x-icon": {},
}

// imageMediaType returns a conservative browser-renderable image type. SVG is
// intentionally excluded because it is text and remains more useful as a
// source diff; unknown binary formats continue to use metadata-only tracking.
func imageMediaType(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	contentType := strings.TrimSpace(strings.SplitN(http.DetectContentType(sample), ";", 2)[0])
	if _, ok := browserImageMediaTypes[contentType]; ok {
		return contentType
	}
	return ""
}

// imageChangeMediaType reports whether every side needed by this change is a
// renderable image. The selected type prefers the current side so callers can
// use it as the diff's display hint even if an image changed formats.
func imageChangeMediaType(kind string, before, after []byte) (string, bool) {
	needBefore, needAfter := blobsNeeded(kind)
	beforeType, afterType := "", ""
	if needBefore {
		beforeType = imageMediaType(before)
		if beforeType == "" {
			return "", false
		}
	}
	if needAfter {
		afterType = imageMediaType(after)
		if afterType == "" {
			return "", false
		}
	}
	if afterType != "" {
		return afterType, true
	}
	return beforeType, beforeType != ""
}

// IsRenderableText reports whether data is valid UTF-8 text and not a
// browser-renderable image or other binary content.
func IsRenderableText(data []byte) bool {
	return utf8.Valid(data) && imageMediaType(data) == "" && !isBinaryContent(data)
}

// isBinaryContent detects binary content via http.DetectContentType plus a NUL
// sniff (mirrors internal/tools; duplicated to keep this package a leaf).
func isBinaryContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	contentType := http.DetectContentType(sample)
	if strings.HasPrefix(contentType, "text/") {
		return false
	}
	if strings.Contains(contentType, "json") || strings.Contains(contentType, "xml") {
		return false
	}
	for _, b := range sample {
		if b == 0 {
			return true
		}
	}
	return false
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
