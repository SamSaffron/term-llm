// Package proxy implements the persistence and authorization core for the
// `term-llm serve proxy` platform: a standalone capability proxy that exports a
// curated set of provider/model aliases to authenticated API clients.
//
// PROTOTYPE SCOPE — this package is an intentionally small first cut. It stores
// clients, hashed+expiring bearer tokens, provider/model grants, access
// requests, and an audit trail in a single local SQLite database. It is meant
// to gate access to the reused OpenAI Responses/Chat and Anthropic Messages
// handlers; it is NOT a full multi-tenant billing/quota system. Known
// limitations are documented inline and in the command help.
package proxy

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("proxy: record not found")

// ErrTooManyPendingRequests is returned by RequestAccess when a client already
// has maxPendingRequestsPerClient distinct pending access requests. It bounds
// database growth from a client probing many (provider, model) combinations.
var ErrTooManyPendingRequests = errors.New("proxy: too many pending access requests")

// maxPendingRequestsPerClient caps the distinct pending access requests a single
// client may accumulate. Repeat requests for the same (provider, model) dedupe
// onto one row (bumping its counter), so this only limits distinct models.
const maxPendingRequestsPerClient = 100

// Access-request statuses.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
)

// WildcardModel grants (or matches) every model for a provider.
const WildcardModel = "*"

// Client is an API consumer of the proxy. Each client owns zero or more bearer
// tokens and zero or more provider/model grants.
type Client struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Token is a hashed, optionally-expiring bearer credential for a client. The
// plaintext secret is only ever returned once, at creation time; the store only
// persists its SHA-256 hash plus a short display prefix.
type Token struct {
	ID         string     `json:"id"`
	ClientID   string     `json:"client_id"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"-"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}

// Grant authorizes a client to call a specific provider/model. Model may be
// WildcardModel ("*") to allow every model exposed by that provider.
type Grant struct {
	ID        string    `json:"id"`
	ClientID  string    `json:"client_id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AccessRequest records a client's (implicit or explicit) request for access to
// a provider/model that it is not yet granted. Pending requests are deduplicated
// per (client, provider, model).
type AccessRequest struct {
	ID        string     `json:"id"`
	ClientID  string     `json:"client_id"`
	Provider  string     `json:"provider"`
	Model     string     `json:"model"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	Note      string     `json:"note,omitempty"`
	Count     int        `json:"count"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`
}

// AuditEntry is an append-only record of an authorization decision or admin
// action. client_id may be empty for anonymous/unauthenticated events.
type AuditEntry struct {
	ID        string    `json:"id"`
	ClientID  string    `json:"client_id,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	Action    string    `json:"action"`
	Decision  string    `json:"decision,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is the SQLite-backed persistence layer for the proxy platform.
type Store struct {
	db    *sql.DB
	mu    sync.Mutex
	clock func() time.Time
}

const proxySchemaVersion = 1

const proxySchema = `
CREATE TABLE IF NOT EXISTS proxy_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_clients (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    disabled    BOOLEAN NOT NULL DEFAULT 0,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_tokens (
    id           TEXT PRIMARY KEY,
    client_id    TEXT NOT NULL REFERENCES proxy_clients(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL,
    expires_at   DATETIME,
    last_used_at DATETIME,
    revoked      BOOLEAN NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_proxy_tokens_client ON proxy_tokens(client_id);

CREATE TABLE IF NOT EXISTS proxy_grants (
    id         TEXT PRIMARY KEY,
    client_id  TEXT NOT NULL REFERENCES proxy_clients(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    UNIQUE(client_id, provider, model)
);
CREATE INDEX IF NOT EXISTS idx_proxy_grants_client ON proxy_grants(client_id);

CREATE TABLE IF NOT EXISTS proxy_access_requests (
    id         TEXT PRIMARY KEY,
    client_id  TEXT NOT NULL REFERENCES proxy_clients(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    reason     TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    count      INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    decided_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_proxy_requests_client ON proxy_access_requests(client_id);
CREATE INDEX IF NOT EXISTS idx_proxy_requests_status ON proxy_access_requests(status);

CREATE TABLE IF NOT EXISTS proxy_audit (
    id         TEXT PRIMARY KEY,
    client_id  TEXT NOT NULL DEFAULT '',
    provider   TEXT NOT NULL DEFAULT '',
    model      TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    decision   TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_audit_client ON proxy_audit(client_id);
CREATE INDEX IF NOT EXISTS idx_proxy_audit_created ON proxy_audit(created_at);
`

// Open opens (creating if needed) the proxy database at path. Use ":memory:"
// for an ephemeral in-process database (primarily for tests).
func Open(path string) (*Store, error) {
	resolved, err := resolveDBPath(path)
	if err != nil {
		return nil, err
	}
	if resolved != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			return nil, fmt.Errorf("create proxy data directory: %w", err)
		}
	}

	dsn := resolved
	if resolved == ":memory:" {
		dsn = fmt.Sprintf("file:term-llm-proxy-%d?mode=memory&cache=shared", time.Now().UnixNano())
	}
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open proxy db: %w", err)
	}
	// modernc sqlite in shared in-memory mode requires a single connection so
	// the schema survives across statements.
	if resolved == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if err := initProxySchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, clock: time.Now}, nil
}

func initProxySchema(db *sql.DB) error {
	if _, err := db.Exec(proxySchema); err != nil {
		return fmt.Errorf("initialize proxy schema: %w", err)
	}
	// Record the schema version for forward-compatible migrations. This
	// prototype ships version 1 with no migrations yet.
	if _, err := db.Exec(
		`INSERT INTO proxy_meta(key, value) VALUES('schema_version', ?)
         ON CONFLICT(key) DO NOTHING`,
		fmt.Sprintf("%d", proxySchemaVersion),
	); err != nil {
		return fmt.Errorf("record proxy schema version: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SetClock overrides the time source (tests only).
func (s *Store) SetClock(fn func() time.Time) {
	if fn != nil {
		s.clock = fn
	}
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

func resolveDBPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == ":memory:" {
		return path, nil
	}
	if path == "" {
		dir, err := defaultDataDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "proxy.db"), nil
	}
	path = os.ExpandEnv(path)
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve proxy db path %q: %w", path, err)
	}
	return abs, nil
}

// defaultDataDir mirrors the XDG data dir used elsewhere in term-llm.
func defaultDataDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "term-llm"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "term-llm"), nil
}

func newID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().UTC().Format("20060102150405"), hex.EncodeToString(buf))
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
