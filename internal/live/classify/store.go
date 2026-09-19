package liveclassify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
	_ "modernc.org/sqlite"
)

const (
	decisionQueueCapacity = 256
	decisionWriteTimeout  = 2 * time.Second
	decisionCloseTimeout  = 3 * time.Second
	dropLogInterval       = time.Minute
)

const decisionSchema = `CREATE TABLE IF NOT EXISTS route_decisions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at TIMESTAMP NOT NULL,
	live_id TEXT NOT NULL,
	bound_session TEXT NOT NULL,
	state_json TEXT,
	probabilities_json TEXT NOT NULL,
	gated_label TEXT NOT NULL,
	acted_label TEXT NOT NULL,
	resolver_outcome TEXT,
	latency_ms INTEGER NOT NULL,
	error TEXT
);
CREATE INDEX IF NOT EXISTS idx_route_decisions_created_at ON route_decisions(created_at DESC);`

// DecisionRecord is one privacy-sensitive live classification audit row.
type DecisionRecord struct {
	ID              int64              `json:"id"`
	CreatedAt       time.Time          `json:"created_at"`
	LiveID          string             `json:"live_id"`
	BoundSession    string             `json:"bound_session"`
	StateJSON       json.RawMessage    `json:"state,omitempty"`
	Probabilities   map[string]float64 `json:"probabilities"`
	GatedLabel      string             `json:"gated_label"`
	ActedLabel      string             `json:"acted_label"`
	ResolverOutcome string             `json:"resolver_outcome,omitempty"`
	Latency         time.Duration      `json:"latency"`
	Error           string             `json:"error,omitempty"`
}

type decisionWriter func(context.Context, DecisionRecord) error

// DecisionStore persists live routing diagnostics independently of transcripts.
// Writes are accepted through a bounded queue so diagnostics can never hold the
// serial live routing worker open.
type DecisionStore struct {
	db *sql.DB

	mu           sync.Mutex
	queue        chan DecisionRecord
	done         chan struct{}
	closed       bool
	write        decisionWriter
	writeTimeout time.Duration
	closeTimeout time.Duration
	writerCtx    context.Context
	writerCancel context.CancelFunc

	dropped     atomic.Uint64
	lastDropLog atomic.Int64
}

// OpenDecisionStore opens and initializes the live diagnostics database.
func OpenDecisionStore(path string) (*DecisionStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("live decision database path is required")
	}
	if err := ensurePrivateDecisionFile(path); err != nil {
		return nil, err
	}
	dsn := sqliteutil.FileURI(path) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open live decision database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(decisionSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize live decision database: %w", err)
	}
	if err := secureDecisionFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := newDecisionStore(db, nil)
	store.write = store.insert
	go store.runWriter()
	return store, nil
}

// OpenDecisionStoreReadOnly opens an existing diagnostics database without
// creating the file, its directory, or its schema.
func OpenDecisionStoreReadOnly(path string) (*DecisionStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("live decision database path is required")
	}
	dsn := sqliteutil.FileURI(path) + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open live decision database read-only: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open live decision database read-only: %w", err)
	}
	return &DecisionStore{db: db}, nil
}

func ensurePrivateDecisionFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("create live decision database: %w", err)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close live decision database file: %w", closeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure live decision database: %w", err)
	}
	return nil
}

func secureDecisionFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0o600); err != nil {
			if os.IsNotExist(err) && candidate != path {
				continue
			}
			return fmt.Errorf("secure live decision database file %q: %w", candidate, err)
		}
	}
	return nil
}

func newDecisionStore(db *sql.DB, write decisionWriter) *DecisionStore {
	writerCtx, writerCancel := context.WithCancel(context.Background())
	return &DecisionStore{
		db:           db,
		queue:        make(chan DecisionRecord, decisionQueueCapacity),
		done:         make(chan struct{}),
		write:        write,
		writeTimeout: decisionWriteTimeout,
		closeTimeout: decisionCloseTimeout,
		writerCtx:    writerCtx,
		writerCancel: writerCancel,
	}
}

func (s *DecisionStore) runWriter() {
	defer close(s.done)
	for {
		select {
		case <-s.writerCtx.Done():
			return
		case record, ok := <-s.queue:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(s.writerCtx, s.writeTimeout)
			err := s.write(ctx, record)
			cancel()
			if err != nil && s.writerCtx.Err() == nil {
				log.Printf("[live] log route decision: %v", err)
			}
		}
	}
}

// Enqueue hands a decision to the diagnostics writer without waiting for disk.
// It returns false when the bounded queue is full or the store is closing.
func (s *DecisionStore) Enqueue(record DecisionRecord) bool {
	if s == nil || s.queue == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.queue <- record:
		return true
	default:
		s.noteDrop()
		return false
	}
}

// Dropped reports how many records could not enter the bounded writer queue.
func (s *DecisionStore) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

func (s *DecisionStore) noteDrop() {
	count := s.dropped.Add(1)
	now := time.Now().UnixNano()
	last := s.lastDropLog.Load()
	if last != 0 && time.Duration(now-last) < dropLogInterval {
		return
	}
	if s.lastDropLog.CompareAndSwap(last, now) {
		go log.Printf("[live] route decision log queue full; dropped %d record(s)", count)
	}
}

// Close drains queued writes for a bounded period and then closes the database.
func (s *DecisionStore) Close() error {
	if s == nil {
		return nil
	}
	if s.queue != nil {
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			close(s.queue)
		}
		s.mu.Unlock()

		timeout := s.closeTimeout
		if timeout <= 0 {
			timeout = decisionCloseTimeout
		}
		timer := time.NewTimer(timeout)
		select {
		case <-s.done:
			if s.writerCancel != nil {
				s.writerCancel()
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			if s.writerCancel != nil {
				s.writerCancel()
			}
			if s.db != nil {
				// database/sql Close may wait for an in-flight driver call. The caller's
				// shutdown bound is authoritative even if a driver ignores cancellation.
				go func() { _ = s.db.Close() }()
			}
			return fmt.Errorf("close live decision store: %w", context.DeadlineExceeded)
		}
	}
	if s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close live decision database: %w", err)
	}
	return nil
}

// insert records one decision. An empty StateJSON is stored as NULL.
func (s *DecisionStore) insert(ctx context.Context, record DecisionRecord) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("live decision store is unavailable")
	}
	probabilities, err := json.Marshal(record.Probabilities)
	if err != nil {
		return fmt.Errorf("encode live decision probabilities: %w", err)
	}
	createdAt := record.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	var state any
	if len(record.StateJSON) > 0 {
		state = string(record.StateJSON)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO route_decisions
		(created_at, live_id, bound_session, state_json, probabilities_json, gated_label, acted_label, resolver_outcome, latency_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, createdAt, record.LiveID, record.BoundSession, state,
		string(probabilities), record.GatedLabel, record.ActedLabel, nullable(record.ResolverOutcome),
		record.Latency.Milliseconds(), nullable(record.Error))
	if err != nil {
		return fmt.Errorf("insert live route decision: %w", err)
	}
	return nil
}

// List returns newest decisions at or after since. A zero time lists all rows.
func (s *DecisionStore) List(ctx context.Context, since time.Time, limit int) ([]DecisionRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("live decision store is unavailable")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT id, created_at, live_id, bound_session, state_json, probabilities_json,
		gated_label, acted_label, resolver_outcome, latency_ms, error FROM route_decisions`
	args := []any{}
	if !since.IsZero() {
		query += ` WHERE created_at >= ?`
		args = append(args, since.UTC())
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list live route decisions: %w", err)
	}
	defer rows.Close()
	var records []DecisionRecord
	for rows.Next() {
		var record DecisionRecord
		var state, probabilities, resolver, errorText sql.NullString
		var latencyMS int64
		if err := rows.Scan(&record.ID, &record.CreatedAt, &record.LiveID, &record.BoundSession, &state, &probabilities,
			&record.GatedLabel, &record.ActedLabel, &resolver, &latencyMS, &errorText); err != nil {
			return nil, fmt.Errorf("scan live route decision: %w", err)
		}
		if state.Valid {
			record.StateJSON = json.RawMessage(state.String)
		}
		if err := json.Unmarshal([]byte(probabilities.String), &record.Probabilities); err != nil {
			return nil, fmt.Errorf("decode live decision probabilities: %w", err)
		}
		record.ResolverOutcome = resolver.String
		record.Latency = time.Duration(latencyMS) * time.Millisecond
		record.Error = errorText.String
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list live route decisions: %w", err)
	}
	return records, nil
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
