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

	"github.com/samsaffron/term-llm/internal/config"

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

// ChangeRecord describes one before→after file transition to record.
type ChangeRecord struct {
	SessionID  string
	RunID      string
	ToolName   string
	ToolCallID string
	Path       string // absolute path

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
	Seq        int64
	RunID      string
	Path       string
	Kind       string
	ToolName   string
	ToolCallID string
	BeforeHash string // empty when absent/unknown/not retained
	AfterHash  string
	BeforeSize int64
	AfterSize  int64
	Adds       int
	Dels       int
	Truncated  bool
	IsBinary   bool
}

// CumulativeChange summarizes a file's net change relative to the session baseline.
type CumulativeChange struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Adds        int    `json:"adds"`
	Dels        int    `json:"dels"`
	Truncated   bool   `json:"truncated"`
	Seq         int64  `json:"seq"`                    // latest change sequence for this path in the session
	SnapshotSeq int64  `json:"snapshot_seq,omitempty"` // shared identity for a multi-run window
}

// FileDiffContent holds the baseline and current contents for one file.
type FileDiffContent struct {
	Path      string
	Kind      string
	Before    []byte
	After     []byte
	Truncated bool
	IsImage   bool
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

// Options configures a Store.
type Options struct {
	MaxFileBytes    int   // 0 = DefaultMaxFileBytes
	MaxSessionBytes int   // 0 = DefaultMaxSessionBytes
	MaxTotalBytes   int64 // 0 = DefaultMaxTotalBytes; whole-database size cap enforced live and by GC
}

// Store persists file-change history in a dedicated SQLite database.
type Store struct {
	db              *sql.DB
	maxFileBytes    int
	maxSessionBytes int
	maxTotalBytes   int64

	// recordMu serializes change inserts so per-session sequence allocation and
	// retained-byte budget checks remain deterministic under parallel tool calls.
	// It also protects the amortized total-budget counters.
	recordMu                  sync.Mutex
	uncheckedTotalBytes       int64
	uncheckedTotalRecordCount int

	mu           sync.Mutex
	sessionBytes map[string]int64 // retained-bytes budget cache per session
}

const schemaVersion = 2

const (
	// Check after at most 8 MiB of retained input, or 64 metadata-only rows.
	// Smaller configured caps use a proportional interval below.
	maxUncheckedTotalBytes   = int64(8 * 1024 * 1024)
	maxUncheckedTotalRecords = 64
)

const schema = `
CREATE TABLE IF NOT EXISTS schema_version (
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
	created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_file_changes_session_path ON file_changes(session_id, path, seq);
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
	dsn += "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=auto_vacuum(2)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open file history database: %w", err)
	}
	if path == ":memory:" {
		// Keep a single connection so schema and data stay visible everywhere.
		db.SetMaxOpenConns(1)
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

	return &Store{
		db:              db,
		maxFileBytes:    maxFile,
		maxSessionBytes: maxSession,
		maxTotalBytes:   maxTotal,
		sessionBytes:    make(map[string]int64),
	}, nil
}

func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	var version int
	err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err == sql.ErrNoRows {
		tx, txErr := db.Begin()
		if txErr != nil {
			return fmt.Errorf("begin file change schema initialization: %w", txErr)
		}
		defer tx.Rollback()
		if _, txErr = tx.Exec("CREATE INDEX IF NOT EXISTS idx_file_changes_session_run ON file_changes(session_id, run_id, seq)"); txErr != nil {
			return txErr
		}
		if _, txErr = tx.Exec("INSERT INTO schema_version (version) VALUES (?)", schemaVersion); txErr != nil {
			return txErr
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin file change migration: %w", err)
	}
	defer tx.Rollback()
	if version < 2 {
		if _, err := tx.Exec("ALTER TABLE file_changes ADD COLUMN run_id TEXT"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("add file change run identity: %w", err)
		}
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_file_changes_session_run ON file_changes(session_id, run_id, seq)"); err != nil {
		return err
	}
	if version < schemaVersion {
		if _, err := tx.Exec("UPDATE schema_version SET version = ?", schemaVersion); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit file change migration: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
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

// RecordChange records one file transition and returns metadata for event
// emission. Returns (nil, nil) for no-ops (identical content, missing→missing,
// or empty session ID).
func (s *Store) RecordChange(ctx context.Context, rec ChangeRecord) (*Change, error) {
	rec.Path = normalizePath(rec.Path)
	if rec.SessionID == "" || rec.Path == "" {
		return nil, nil
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

	// Serialize RecordChange calls. This avoids races where concurrent tool calls
	// for the same session both choose the same next seq, and keeps the
	// max-session-byte budget from being oversubscribed by parallel inserts.
	s.recordMu.Lock()
	defer s.recordMu.Unlock()

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
	if retain && hasBefore && len(rec.Before) > s.maxFileBytes {
		retain = false
	}
	if retain && hasAfter && len(rec.After) > s.maxFileBytes {
		retain = false
	}
	if retain {
		used, err := s.sessionBytesUsed(ctx, rec.SessionID)
		if err != nil {
			return nil, err
		}
		if used+beforeSize+afterSize > int64(s.maxSessionBytes) {
			retain = false
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
		RunID:      rec.RunID,
		Path:       rec.Path,
		Kind:       kind,
		ToolName:   rec.ToolName,
		ToolCallID: rec.ToolCallID,
		BeforeSize: beforeSize,
		AfterSize:  afterSize,
		Adds:       adds,
		Dels:       dels,
		Truncated:  !retain,
		IsBinary:   isBinary,
	}

	retainedBytes := int64(0)
	if retain {
		retainedBytes = beforeSize + afterSize
	}
	checkTotalBudget := s.totalBudgetCheckDue(retainedBytes)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin file change transaction: %w", err)
	}
	defer tx.Rollback()

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
			 adds, dels, truncated, is_binary)
		VALUES
			(?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM file_changes WHERE session_id = ?),
			 ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING seq`,
		rec.SessionID, nullString(rec.RunID), rec.SessionID,
		rec.Path, kind, rec.ToolName, rec.ToolCallID,
		nullString(change.BeforeHash), nullString(change.AfterHash), beforeSize, afterSize,
		adds, dels, boolInt(change.Truncated), boolInt(isBinary),
	).Scan(&change.Seq)
	if err != nil {
		return nil, fmt.Errorf("insert file change: %w", err)
	}

	var totalBudgetPruned bool
	if checkTotalBudget {
		// This check has consumed the accumulated work even if the transaction
		// later fails. Keeping the counters above threshold would make every
		// subsequent metadata-only record repeat the same expensive sweep.
		s.uncheckedTotalBytes = 0
		s.uncheckedTotalRecordCount = 0

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
		if !checkTotalBudget {
			s.uncheckedTotalBytes += retainedBytes
			s.uncheckedTotalRecordCount++
		}
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

// SessionPaths returns the distinct absolute paths already recorded for a session.
func (s *Store) SessionPaths(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT DISTINCT path FROM file_changes WHERE session_id = ?", sessionID)
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
	path            string
	firstKind       string
	firstBeforeHash string
	firstBinary     bool
	lastKind        string
	lastAfterHash   string
	lastBinary      bool
	lastSeq         int64
}

type recentRunWindow struct {
	runIDs      []string
	snapshotSeq int64
}

func (s *Store) latestRunWindow(ctx context.Context, sessionID string, limit int, snapshotSeq int64) (recentRunWindow, error) {
	if limit < 1 {
		limit = 1
	}
	query := `
		SELECT run_id, MAX(seq) FROM file_changes
		WHERE session_id = ? AND COALESCE(run_id, '') <> ''`
	args := []any{sessionID}
	if snapshotSeq > 0 {
		query += " AND seq <= ?"
		args = append(args, snapshotSeq)
	}
	query += " GROUP BY run_id ORDER BY MAX(seq) DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return recentRunWindow{}, fmt.Errorf("query recent file change runs: %w", err)
	}
	defer rows.Close()

	window := recentRunWindow{}
	for rows.Next() {
		var runID string
		var lastSeq int64
		if err := rows.Scan(&runID, &lastSeq); err != nil {
			return recentRunWindow{}, err
		}
		window.runIDs = append(window.runIDs, runID)
		if lastSeq > window.snapshotSeq {
			window.snapshotSeq = lastSeq
		}
	}
	if err := rows.Err(); err != nil {
		return recentRunWindow{}, err
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
		SELECT seq, path, kind, COALESCE(before_hash, ''), COALESCE(after_hash, ''), is_binary
		FROM file_changes WHERE session_id = ?`
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
		var path, kind, beforeHash, afterHash string
		var isBinary bool
		if err := rows.Scan(&seq, &path, &kind, &beforeHash, &afterHash, &isBinary); err != nil {
			return nil, err
		}
		path = normalizePath(path)
		if path == "" {
			continue
		}
		span, ok := spans[path]
		if !ok {
			span = &pathSpan{path: path, firstKind: kind, firstBeforeHash: beforeHash, firstBinary: isBinary}
			spans[path] = span
			order = append(order, path)
		}
		span.lastKind = kind
		span.lastAfterHash = afterHash
		span.lastBinary = isBinary
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
	where := "session_id = ? AND path = ?"
	args := []any{sessionID, path}
	filter, filterArgs := runWindowFilter(runIDs, snapshotSeq)
	where += filter
	args = append(args, filterArgs...)
	if err := s.db.QueryRowContext(ctx, `
		SELECT kind, COALESCE(before_hash, ''), is_binary
		FROM file_changes
		WHERE `+where+`
		ORDER BY seq ASC LIMIT 1`, args...).
		Scan(&sp.firstKind, &sp.firstBeforeHash, &sp.firstBinary); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("query first path change: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT seq, kind, COALESCE(after_hash, ''), is_binary
		FROM file_changes
		WHERE `+where+`
		ORDER BY seq DESC LIMIT 1`, args...).
		Scan(&sp.lastSeq, &sp.lastKind, &sp.lastAfterHash, &sp.lastBinary); err != nil {
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

// ListRecentRunChanges returns the cumulative changes across the latest runs
// that recorded file changes. Rows without run identities are excluded.
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

		change := CumulativeChange{Path: sp.path, Kind: kind, Seq: sp.lastSeq}
		needBefore, needAfter := blobsNeeded(kind)

		// Retained binaries are browser-renderable images. Their line counts are
		// always zero, so verify blob presence without reading and decompressing
		// potentially multi-megabyte image bodies on every list refresh.
		if sp.retainedImage(kind) {
			if needBefore && !s.blobExists(ctx, sp.firstBeforeHash) {
				change.Truncated = true
			}
			if needAfter && !s.blobExists(ctx, sp.lastAfterHash) {
				change.Truncated = true
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
		} else if _, isImage := imageChangeMediaType(kind, before, after); !isImage {
			change.Adds, change.Dels = CountAddsDels(before, after)
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

	content := &FileDiffContent{Path: sp.path, Kind: kind}
	needBefore, needAfter := blobsNeeded(kind)
	if sp.retainedImage(kind) {
		content.IsImage = true
		if needBefore && !s.blobExists(ctx, sp.firstBeforeHash) {
			content.Truncated = true
		}
		if needAfter && !s.blobExists(ctx, sp.lastAfterHash) {
			content.Truncated = true
		}
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

func (s *Store) fileDiffSideFromSpan(ctx context.Context, sp *pathSpan, side string, err error) (*FileDiffSide, error) {
	if err != nil || sp == nil {
		return nil, err
	}
	kind, ok := sp.resolve()
	if !ok {
		return nil, nil
	}
	needBefore, needAfter := blobsNeeded(kind)
	if (side == "before" && !needBefore) || (side == "after" && !needAfter) {
		return nil, ErrInvalidDiffSide
	}
	if !sp.retainedImage(kind) {
		return nil, nil
	}
	hash := sp.firstBeforeHash
	if side == "after" {
		hash = sp.lastAfterHash
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
		size, err := databaseSize(ctx, db)
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
func databaseSize(ctx context.Context, db totalBudgetDB) (int64, error) {
	var pageCount, pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page count: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page size: %w", err)
	}
	return pageCount * pageSize, nil
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
