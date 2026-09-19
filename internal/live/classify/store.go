package liveclassify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
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

// DecisionStore persists live routing diagnostics independently of transcripts.
type DecisionStore struct {
	db *sql.DB
}

// OpenDecisionStore opens and initializes the live diagnostics database.
func OpenDecisionStore(path string) (*DecisionStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("live decision database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open live decision database: %w", err)
	}
	if _, err := db.Exec(decisionSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize live decision database: %w", err)
	}
	return &DecisionStore{db: db}, nil
}

func (s *DecisionStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Insert records one decision. An empty StateJSON is stored as NULL.
func (s *DecisionStore) Insert(ctx context.Context, record DecisionRecord) error {
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
