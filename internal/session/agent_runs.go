package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// AgentRunDeliveryStore is implemented by durable stores able to reconcile
// pending parent-loop events across process restarts. Collection suppresses a
// pending wake without discarding the child's transcript or media.
type AgentRunDeliveryStore interface {
	ListPendingAgentRuns(context.Context) ([]AgentRun, error)
	ListPendingAgentRunsForParent(context.Context, string) ([]AgentRun, error)
	MarkAgentRunNotified(context.Context, string, int64, time.Time) (bool, error)
	SuppressPendingAgentWakes(context.Context, string) error
	SuppressAgentWakesForTurn(context.Context, string, string) error
	SuppressAgentEvents(context.Context, []AgentRun) error
	CollectAgentRunGeneration(context.Context, string, int64, time.Time) error
}

func AsAgentRunDeliveryStore(store Store) AgentRunDeliveryStore {
	if logging, ok := store.(*LoggingStore); ok {
		return AsAgentRunDeliveryStore(logging.Store)
	}
	capability, _ := store.(AgentRunDeliveryStore)
	return capability
}

// ListPendingAgentRuns includes unfinished runs so a host can surface only
// those whose process owner is provably dead. No child is auto-resumed.
func (s *SQLiteStore) ListPendingAgentRuns(ctx context.Context) ([]AgentRun, error) {
	return s.listPendingAgentRuns(ctx, "")
}

func (s *SQLiteStore) ListPendingAgentRunsForParent(ctx context.Context, parent string) ([]AgentRun, error) {
	return s.listPendingAgentRuns(ctx, parent)
}

func (s *SQLiteStore) listPendingAgentRuns(ctx context.Context, parent string) ([]AgentRun, error) {
	query := `SELECT ` + agentRunColumns + ` FROM session_agent_runs WHERE notify_origin != '' AND (notify_when_done=1 OR run_status IN ('queued','running','awaiting_approval','interrupted')) AND notified_at IS NULL AND collected_at IS NULL AND wake_suppressed=0 AND stop_reason != 'parent_stopped'`
	var args []any
	if parent != "" {
		query += " AND parent_session_id=?"
		args = append(args, parent)
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY updated_at", args...)
	if err != nil {
		return nil, fmt.Errorf("list pending agent completions: %w", err)
	}
	defer rows.Close()
	var runs []AgentRun
	for rows.Next() {
		run, err := scanAgentRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending agent completion: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// MarkAgentRunNotified acknowledges exactly the observed run generation after
// its parent continuation settles. A later explicit continuation resets this
// marker in PutAgentRun, making its next completion independently deliverable.
func (s *SQLiteStore) MarkAgentRunNotified(ctx context.Context, id string, generation int64, at time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_agent_runs SET notified_at=? WHERE child_session_id=? AND run_generation=? AND notified_at IS NULL AND collected_at IS NULL`, at, id, generation)
	if err != nil {
		return false, fmt.Errorf("acknowledge agent completion: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// SuppressPendingAgentWakes honors an explicit stop of the parent turn. Child
// results remain available to wait_agent; only automatic loop reactivation is
// suppressed, including an earlier completion racing the stop.
func (s *SQLiteStore) SuppressPendingAgentWakes(ctx context.Context, parent string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_agent_runs SET wake_suppressed=1 WHERE parent_session_id=? AND notify_origin != '' AND notified_at IS NULL AND collected_at IS NULL`, parent)
	if err != nil {
		return fmt.Errorf("suppress stopped parent agent wakes: %w", err)
	}
	return nil
}

// AgentRunStore is an optional extension: custom session stores need not implement it.
type AgentRunStore interface {
	PutAgentRun(context.Context, AgentRun) error
	GetAgentRun(context.Context, string) (AgentRun, error)
	ListAgentRuns(context.Context, string) ([]AgentRun, error)
	CollectAgentRun(context.Context, string, time.Time) error
}

// AsAgentRunStore resolves the optional capability through session logging
// wrappers without making unsupported stores appear persistent.
func AsAgentRunStore(store Store) AgentRunStore {
	if logging, ok := store.(*LoggingStore); ok {
		return AsAgentRunStore(logging.Store)
	}
	capability, _ := store.(AgentRunStore)
	return capability
}

type AgentRun struct {
	ID               string              `json:"agent_id"`
	ParentSessionID  string              `json:"parent_session_id"`
	ParentResponseID string              `json:"parent_response_id,omitempty"`
	AgentName        string              `json:"agent_name"`
	Prompt           string              `json:"prompt"`
	Model            string              `json:"model,omitempty"`
	BaseDir          string              `json:"base_dir,omitempty"`
	RunGeneration    int64               `json:"run_generation"`
	NotifyWhenDone   bool                `json:"notify_when_done,omitempty"`
	NotifyOrigin     string              `json:"notify_origin,omitempty"` // Trusted host surface, never a tool argument
	WakeSuppressed   bool                `json:"wake_suppressed,omitempty"`
	NotifiedAt       time.Time           `json:"notified_at,omitempty"`
	Started          bool                `json:"started"`
	Status           string              `json:"status"`
	StopReason       string              `json:"stop_reason,omitempty"`
	CurrentTool      string              `json:"current_tool,omitempty"` // In-process progress; not persisted
	TurnsUsed        int                 `json:"turns_used"`
	TurnsGranted     int                 `json:"turns_granted"`
	Output           string              `json:"output,omitempty"`
	Media            []llm.MediaArtifact `json:"-"`
	Error            string              `json:"error,omitempty"`
	OwnerInstanceID  string              `json:"owner_instance_id"`
	UpdatedAt        time.Time           `json:"last_activity"`
	CollectedAt      time.Time           `json:"collected_at,omitempty"`
}

const agentRunSchemaV62 = `
CREATE TABLE IF NOT EXISTS session_agent_runs (
 child_session_id TEXT PRIMARY KEY,
 parent_session_id TEXT NOT NULL,
 agent_name TEXT NOT NULL,
 prompt TEXT NOT NULL,
 model TEXT NOT NULL DEFAULT '',
 started INTEGER NOT NULL DEFAULT 1,
 run_status TEXT NOT NULL,
 stop_reason TEXT NOT NULL DEFAULT '',
 turns_used INTEGER NOT NULL DEFAULT 0,
 turns_granted INTEGER NOT NULL DEFAULT 500,
 output TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 owner_instance_id TEXT NOT NULL,
 updated_at DATETIME NOT NULL,
 collected_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_session_agent_runs_parent ON session_agent_runs(parent_session_id, updated_at);`

// The released v62 definition is immutable; fresh databases use the v63 shape.
const agentRunSchemaV63 = `
CREATE TABLE IF NOT EXISTS session_agent_runs (
 child_session_id TEXT PRIMARY KEY,
 parent_session_id TEXT NOT NULL,
 agent_name TEXT NOT NULL,
 prompt TEXT NOT NULL,
 model TEXT NOT NULL DEFAULT '',
 base_dir TEXT NOT NULL DEFAULT '',
 parent_response_id TEXT NOT NULL DEFAULT '',
 run_generation INTEGER NOT NULL DEFAULT 0,
 notify_when_done INTEGER NOT NULL DEFAULT 0,
 notify_origin TEXT NOT NULL DEFAULT '',
 media_json TEXT NOT NULL DEFAULT '[]',
 notified_at DATETIME,
 wake_suppressed INTEGER NOT NULL DEFAULT 0,
 started INTEGER NOT NULL DEFAULT 1,
 run_status TEXT NOT NULL,
 stop_reason TEXT NOT NULL DEFAULT '',
 turns_used INTEGER NOT NULL DEFAULT 0,
 turns_granted INTEGER NOT NULL DEFAULT 500,
 output TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 owner_instance_id TEXT NOT NULL,
 updated_at DATETIME NOT NULL,
 collected_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_session_agent_runs_parent ON session_agent_runs(parent_session_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_session_agent_runs_pending ON session_agent_runs(parent_session_id, updated_at) WHERE notify_origin != '' AND notified_at IS NULL AND collected_at IS NULL AND wake_suppressed=0;`

func (s *SQLiteStore) PutAgentRun(ctx context.Context, a AgentRun) error {
	var collected any
	if !a.CollectedAt.IsZero() {
		collected = a.CollectedAt
	}
	var notified any
	if !a.NotifiedAt.IsZero() {
		notified = a.NotifiedAt
	}
	media, err := json.Marshal(a.Media)
	if err != nil {
		return fmt.Errorf("encode agent media: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO session_agent_runs (child_session_id,parent_session_id,agent_name,prompt,model,base_dir,parent_response_id,run_generation,notify_when_done,notify_origin,media_json,notified_at,wake_suppressed,started,run_status,stop_reason,turns_used,turns_granted,output,error,owner_instance_id,updated_at,collected_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id) DO UPDATE SET model=excluded.model,base_dir=excluded.base_dir,parent_response_id=excluded.parent_response_id,run_generation=excluded.run_generation,notify_when_done=excluded.notify_when_done,notify_origin=excluded.notify_origin,media_json=excluded.media_json,notified_at=CASE WHEN excluded.run_generation!=session_agent_runs.run_generation THEN excluded.notified_at ELSE COALESCE(excluded.notified_at,session_agent_runs.notified_at) END,started=excluded.started,run_status=excluded.run_status,stop_reason=excluded.stop_reason,wake_suppressed=CASE WHEN excluded.run_generation!=session_agent_runs.run_generation THEN excluded.wake_suppressed ELSE MAX(excluded.wake_suppressed,session_agent_runs.wake_suppressed) END,turns_used=excluded.turns_used,turns_granted=excluded.turns_granted,output=excluded.output,error=excluded.error,owner_instance_id=excluded.owner_instance_id,updated_at=excluded.updated_at,collected_at=CASE WHEN excluded.run_generation!=session_agent_runs.run_generation THEN excluded.collected_at ELSE COALESCE(excluded.collected_at,session_agent_runs.collected_at) END WHERE excluded.run_generation>=session_agent_runs.run_generation AND (excluded.run_generation!=session_agent_runs.run_generation OR session_agent_runs.run_status NOT IN ('completed','turn_limit','cancelled','interrupted','failed') OR excluded.run_status NOT IN ('queued','running','awaiting_approval'))`,
		a.ID, a.ParentSessionID, a.AgentName, a.Prompt, a.Model, a.BaseDir, a.ParentResponseID, a.RunGeneration, a.NotifyWhenDone, a.NotifyOrigin, string(media), notified, a.WakeSuppressed, a.Started, a.Status, a.StopReason, a.TurnsUsed, a.TurnsGranted, a.Output, a.Error, a.OwnerInstanceID, a.UpdatedAt, collected)
	if err != nil {
		return fmt.Errorf("save agent run: %w", err)
	}
	return nil
}

const agentRunColumns = `child_session_id,parent_session_id,agent_name,prompt,model,base_dir,parent_response_id,run_generation,notify_when_done,notify_origin,media_json,notified_at,wake_suppressed,started,run_status,stop_reason,turns_used,turns_granted,output,error,owner_instance_id,updated_at,collected_at`

func scanAgentRun(row interface{ Scan(...any) error }) (AgentRun, error) {
	var a AgentRun
	var collected, notified sql.NullTime
	var media string
	err := row.Scan(&a.ID, &a.ParentSessionID, &a.AgentName, &a.Prompt, &a.Model, &a.BaseDir, &a.ParentResponseID, &a.RunGeneration, &a.NotifyWhenDone, &a.NotifyOrigin, &media, &notified, &a.WakeSuppressed, &a.Started, &a.Status, &a.StopReason, &a.TurnsUsed, &a.TurnsGranted, &a.Output, &a.Error, &a.OwnerInstanceID, &a.UpdatedAt, &collected)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(media), &a.Media); err != nil {
		return a, fmt.Errorf("decode agent media: %w", err)
	}
	if notified.Valid {
		a.NotifiedAt = notified.Time
	}
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

// CollectAgentRun marks a result as observed without rewriting a concurrently
// saved terminal status.
func (s *SQLiteStore) CollectAgentRun(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_agent_runs SET collected_at=COALESCE(collected_at,?) WHERE child_session_id=?`, at, id)
	if err != nil {
		return fmt.Errorf("collect agent run: %w", err)
	}
	return nil
}

// CollectAgentRunGeneration cannot collect a newer explicit continuation.
func (s *SQLiteStore) CollectAgentRunGeneration(ctx context.Context, id string, generation int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_agent_runs SET collected_at=COALESCE(collected_at,?) WHERE child_session_id=? AND run_generation=?`, at, id, generation)
	return err
}

// SuppressAgentWakesForTurn suppresses children still owned by the stopped
// session, plus completions from that turn racing the stop. Earlier completed
// results keep their lifecycle reason and remain eligible for later delivery.
func (s *SQLiteStore) SuppressAgentWakesForTurn(ctx context.Context, parent, response string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE session_agent_runs SET wake_suppressed=1 WHERE parent_session_id=? AND notify_origin!='' AND notified_at IS NULL AND collected_at IS NULL AND (parent_response_id=? OR run_status IN ('queued','running','awaiting_approval'))`, parent, response)
	return err
}

func validateAgentWakeAdmissionTx(ctx context.Context, tx *sql.Tx, admission ResponseRunAdmission) error {
	if len(admission.AgentEvents) == 0 {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id=? AND origin='web' AND archived=0`, admission.SessionID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return fmt.Errorf("%w: agent wake parent disappeared", ErrSessionTurnOwned)
	}
	for _, event := range admission.AgentEvents {
		var eligible int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_agent_runs WHERE child_session_id=? AND parent_session_id=? AND run_generation=? AND notify_origin='web' AND notified_at IS NULL AND collected_at IS NULL AND wake_suppressed=0 AND stop_reason!='parent_stopped'`, event.ID, admission.SessionID, event.RunGeneration).Scan(&eligible)
		if err != nil {
			return err
		}
		if eligible != 1 {
			return fmt.Errorf("%w: agent event %s was invalidated before admission", ErrSessionTurnOwned, event.ID)
		}
	}
	return nil
}

// SuppressAgentEvents honors stopping an admitted host wake itself, including
// events originating in an older turn but offered in this continuation.
func (s *SQLiteStore) SuppressAgentEvents(ctx context.Context, events []AgentRun) error {
	for _, event := range events {
		if _, err := s.db.ExecContext(ctx, `UPDATE session_agent_runs SET wake_suppressed=1 WHERE child_session_id=? AND parent_session_id=? AND run_generation=?`, event.ID, event.ParentSessionID, event.RunGeneration); err != nil {
			return err
		}
	}
	return nil
}
