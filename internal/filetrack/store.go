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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Default caps, overridable via config.
const (
	DefaultMaxFileBytes    = 2 * 1024 * 1024           // per-file content cap
	DefaultMaxSessionBytes = 100 * 1024 * 1024         // retained-content budget per session
	DefaultMaxTotalBytes   = int64(1024 * 1024 * 1024) // whole-database size cap (across sessions)
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
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Adds      int    `json:"adds"`
	Dels      int    `json:"dels"`
	Truncated bool   `json:"truncated"`
	Seq       int64  `json:"seq"` // latest change sequence for this path in the session
}

// FileDiffContent holds the baseline and current contents for one file.
type FileDiffContent struct {
	Path      string
	Kind      string
	Before    []byte
	After     []byte
	Truncated bool
}

// Options configures a Store.
type Options struct {
	MaxFileBytes    int   // 0 = DefaultMaxFileBytes
	MaxSessionBytes int   // 0 = DefaultMaxSessionBytes
	MaxTotalBytes   int64 // 0 = DefaultMaxTotalBytes; whole-database size cap enforced by GC
}

// Store persists file-change history in a dedicated SQLite database.
type Store struct {
	db              *sql.DB
	maxFileBytes    int
	maxSessionBytes int
	maxTotalBytes   int64

	// recordMu serializes change inserts so per-session sequence allocation and
	// retained-byte budget checks remain deterministic under parallel tool calls.
	recordMu sync.Mutex

	mu           sync.Mutex
	sessionBytes map[string]int64 // retained-bytes budget cache per session
}

const schemaVersion = 1

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
		_, err = db.Exec("INSERT INTO schema_version (version) VALUES (?)", schemaVersion)
		return err
	}
	if err != nil {
		return err
	}

	// Future migrations run here, mirroring internal/session/sqlite.go.
	if version < schemaVersion {
		if _, err := db.Exec("UPDATE schema_version SET version = ?", schemaVersion); err != nil {
			return err
		}
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

	isBinary := (hasBefore && isBinaryContent(rec.Before)) || (hasAfter && isBinaryContent(rec.After))

	// A change is either fully retained (all sides the kind needs are stored)
	// or metadata-only. Mixed retention would complicate baseline resolution
	// for marginal benefit.
	retain := !isBinary && !rec.BeforeUnknown && !rec.AfterUnknown
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
	if retain {
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
			(session_id, seq, path, kind, tool_name, tool_call_id,
			 before_hash, after_hash, before_size, after_size,
			 adds, dels, truncated, is_binary)
		VALUES
			(?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM file_changes WHERE session_id = ?),
			 ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING seq`,
		rec.SessionID, rec.SessionID,
		rec.Path, kind, rec.ToolName, rec.ToolCallID,
		nullString(change.BeforeHash), nullString(change.AfterHash), beforeSize, afterSize,
		adds, dels, boolInt(change.Truncated), boolInt(isBinary),
	).Scan(&change.Seq)
	if err != nil {
		return nil, fmt.Errorf("insert file change: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit file change: %w", err)
	}

	if retain {
		s.mu.Lock()
		if _, ok := s.sessionBytes[rec.SessionID]; ok {
			s.sessionBytes[rec.SessionID] += beforeSize + afterSize
		}
		s.mu.Unlock()
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
	lastKind        string
	lastAfterHash   string
	lastSeq         int64
}

func (s *Store) sessionSpans(ctx context.Context, sessionID string) ([]*pathSpan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, path, kind, COALESCE(before_hash, ''), COALESCE(after_hash, '')
		FROM file_changes WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query session changes: %w", err)
	}
	defer rows.Close()

	spans := make(map[string]*pathSpan)
	var order []string
	for rows.Next() {
		var seq int64
		var path, kind, beforeHash, afterHash string
		if err := rows.Scan(&seq, &path, &kind, &beforeHash, &afterHash); err != nil {
			return nil, err
		}
		path = normalizePath(path)
		if path == "" {
			continue
		}
		span, ok := spans[path]
		if !ok {
			span = &pathSpan{path: path, firstKind: kind, firstBeforeHash: beforeHash}
			spans[path] = span
			order = append(order, path)
		}
		span.lastKind = kind
		span.lastAfterHash = afterHash
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
		} else {
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
	path = normalizePath(path)
	if path == "" {
		return nil, nil
	}
	spans, err := s.sessionSpans(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	for _, sp := range spans {
		if sp.path != path {
			continue
		}
		kind, ok := sp.resolve()
		if !ok {
			return nil, nil
		}

		content := &FileDiffContent{Path: path, Kind: kind}
		needBefore, needAfter := blobsNeeded(kind)
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
		return content, nil
	}
	return nil, nil
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

	if err := s.enforceTotalBudget(ctx, conn); err != nil {
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

// enforceTotalBudget prunes the least recently changed sessions' history until
// the database fits maxTotalBytes. This is the cross-session backstop: the
// per-session budget bounds one session, but many sessions could otherwise
// grow the database without limit.
func (s *Store) enforceTotalBudget(ctx context.Context, conn *sql.Conn) error {
	for {
		size, err := databaseSize(ctx, conn)
		if err != nil {
			return err
		}
		if size <= s.maxTotalBytes {
			return nil
		}

		var oldest string
		err = conn.QueryRowContext(ctx, `
			SELECT session_id FROM file_changes
			GROUP BY session_id
			ORDER BY MAX(created_at) ASC, session_id ASC
			LIMIT 1`).Scan(&oldest)
		if err == sql.ErrNoRows {
			return nil // nothing left to prune; remaining size is structural overhead
		}
		if err != nil {
			return err
		}

		if _, err := conn.ExecContext(ctx, "DELETE FROM file_changes WHERE session_id = ?", oldest); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, blobSweepSQL); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
			return err
		}
	}
}

// databaseSize returns the database file size in bytes (page count × page size).
func databaseSize(ctx context.Context, conn *sql.Conn) (int64, error) {
	var pageCount, pageSize int64
	if err := conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page count: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
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
