package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AgentRunStore is an optional extension: custom session stores need not implement it.
type AgentRunStore interface {
	PutAgentRun(context.Context, AgentRun) error
	GetAgentRun(context.Context, string) (AgentRun, error)
	ListAgentRuns(context.Context, string) ([]AgentRun, error)
}

type AgentRun struct {
	ID              string    `json:"agent_id"`
	ParentSessionID string    `json:"parent_session_id"`
	AgentName       string    `json:"agent_name"`
	Prompt          string    `json:"prompt"`
	Status          string    `json:"status"`
	StopReason      string    `json:"stop_reason,omitempty"`
	TurnsUsed       int       `json:"turns_used"`
	TurnsGranted    int       `json:"turns_granted"`
	Output          string    `json:"output,omitempty"`
	Error           string    `json:"error,omitempty"`
	OwnerInstanceID string    `json:"owner_instance_id"`
	UpdatedAt       time.Time `json:"last_activity"`
	CollectedAt     time.Time `json:"collected_at,omitempty"`
}

const agentRunSchemaV60 = `
CREATE TABLE IF NOT EXISTS session_agent_runs (
 child_session_id TEXT PRIMARY KEY,
 parent_session_id TEXT NOT NULL,
 agent_name TEXT NOT NULL,
 prompt TEXT NOT NULL,
 run_status TEXT NOT NULL,
 stop_reason TEXT NOT NULL DEFAULT '',
 turns_used INTEGER NOT NULL DEFAULT 0,
 turns_granted INTEGER NOT NULL DEFAULT 20,
 output TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 owner_instance_id TEXT NOT NULL,
 updated_at DATETIME NOT NULL,
 collected_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_session_agent_runs_parent ON session_agent_runs(parent_session_id, updated_at);`

func (s *SQLiteStore) PutAgentRun(ctx context.Context, a AgentRun) error {
	var collected any
	if !a.CollectedAt.IsZero() {
		collected = a.CollectedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_agent_runs (child_session_id,parent_session_id,agent_name,prompt,run_status,stop_reason,turns_used,turns_granted,output,error,owner_instance_id,updated_at,collected_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id) DO UPDATE SET run_status=excluded.run_status,stop_reason=excluded.stop_reason,turns_used=excluded.turns_used,turns_granted=excluded.turns_granted,output=excluded.output,error=excluded.error,owner_instance_id=excluded.owner_instance_id,updated_at=excluded.updated_at,collected_at=excluded.collected_at`,
		a.ID, a.ParentSessionID, a.AgentName, a.Prompt, a.Status, a.StopReason, a.TurnsUsed, a.TurnsGranted, a.Output, a.Error, a.OwnerInstanceID, a.UpdatedAt, collected)
	if err != nil {
		return fmt.Errorf("save agent run: %w", err)
	}
	return nil
}

const agentRunColumns = `child_session_id,parent_session_id,agent_name,prompt,run_status,stop_reason,turns_used,turns_granted,output,error,owner_instance_id,updated_at,collected_at`

func scanAgentRun(row interface{ Scan(...any) error }) (AgentRun, error) {
	var a AgentRun
	var collected sql.NullTime
	err := row.Scan(&a.ID, &a.ParentSessionID, &a.AgentName, &a.Prompt, &a.Status, &a.StopReason, &a.TurnsUsed, &a.TurnsGranted, &a.Output, &a.Error, &a.OwnerInstanceID, &a.UpdatedAt, &collected)
	if collected.Valid {
		a.CollectedAt = collected.Time
	}
	return a, err
}

func (s *SQLiteStore) GetAgentRun(ctx context.Context, id string) (AgentRun, error) {
	a, err := scanAgentRun(s.db.QueryRowContext(ctx, `SELECT `+agentRunColumns+` FROM session_agent_runs WHERE child_session_id=?`, id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("get agent run: %w", err)
	}
	return a, err
}

func (s *SQLiteStore) ListAgentRuns(ctx context.Context, parent string) ([]AgentRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentRunColumns+` FROM session_agent_runs WHERE parent_session_id=? ORDER BY updated_at DESC`, parent)
	if err != nil {
		return nil, fmt.Errorf("list agent runs: %w", err)
	}
	defer rows.Close()
	var agents []AgentRun
	for rows.Next() {
		a, err := scanAgentRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan agent run: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}
