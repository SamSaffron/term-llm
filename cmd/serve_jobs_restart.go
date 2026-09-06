package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// jobsRestartCheckpoint is mode-owned execution state, not a new job or retry.
// Transcript/steering data belongs to the existing revision-checked session
// CommandHandoff; the jobs journal binds its one-shot ID to the original run.
type jobsRestartCheckpoint struct {
	PassHadWork        bool                 `json:"pass_had_work,omitempty"`
	PassHadCommit      bool                 `json:"pass_had_commit,omitempty"`
	FinalizationReason string               `json:"finalization_reason,omitempty"`
	PassTurnLimit      int                  `json:"pass_turn_limit,omitempty"`
	Pending            []llm.QueuedSteering `json:"pending,omitempty"`
	RemainingTurns     int                  `json:"remaining_turns"`
	RestartID          string               `json:"restart_id"`
	RunID              string               `json:"run_id"`
	Service            string               `json:"service"`
	SourceInstance     string               `json:"source_instance"`
	SessionID          string               `json:"session_id"`
	HandoffID          string               `json:"handoff_id,omitempty"`
	Job                jobsV2Job            `json:"job"`
	Deadline           time.Time            `json:"deadline"`
}

var errJobsRestartConflict = errors.New("job restart checkpoint conflicts with current run")

func initJobsRestartJournal(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS job_restart_checkpoints (
 run_id TEXT PRIMARY KEY REFERENCES job_runs_v2(id) ON DELETE CASCADE,
 restart_id TEXT NOT NULL,
 phase TEXT NOT NULL CHECK(phase IN ('prepared','sealed','ready','claimed')),
 payload TEXT NOT NULL
 )`)
	return err
}

// prepareLLMRestart records intent before internal cancellation. User CancelRun
// changes the authoritative run status and therefore wins every later CAS.
func (m *jobsV2Manager) prepareLLMRestart(ctx context.Context, c jobsRestartCheckpoint) error {
	if c.RunID == "" || c.RestartID == "" || c.Service == "" || c.SourceInstance == "" || c.SessionID == "" || c.HandoffID != "" || c.Deadline.IsZero() || c.Job.RunnerType != jobsV2RunnerLLM {
		return errJobsRestartConflict
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	result, err := m.db.ExecContext(ctx, `INSERT INTO job_restart_checkpoints(run_id,restart_id,phase,payload)
 SELECT id,?,'prepared',? FROM job_runs_v2 WHERE id=? AND job_id=? AND session_id=? AND status=? AND worker_id=?`, c.RestartID, string(raw), c.RunID, c.Job.ID, c.SessionID, jobsV2RunRunning, m.workerID)
	if err != nil {
		return fmt.Errorf("prepare job restart: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errJobsRestartConflict
	}
	return nil
}

// sealLLMRestart is called only after the owned runner has returned, including
// actual tool settlement and persistence cleanup, and saved a CommandHandoff.
func (m *jobsV2Manager) sealLLMRestart(ctx context.Context, runID, restartID, handoffID string, result jobsV2RunResult) error {
	if handoffID == "" {
		return errJobsRestartConflict
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := readJobsRestart(ctx, tx, runID, restartID, "prepared")
	if err != nil {
		return err
	}
	c.HandoffID = handoffID
	if result.restartPass != nil {
		c.PassHadWork = result.restartPass.hadNonProgressTool
		c.PassHadCommit = result.restartPass.newCommitCount > 0
	}
	usedTurns := result.TurnCount
	if result.restartBudgetTurns != nil {
		usedTurns = *result.restartBudgetTurns
	}
	c.RemainingTurns = max(0, c.RemainingTurns-usedTurns)
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE job_restart_checkpoints SET phase='sealed',payload=? WHERE run_id=? AND restart_id=? AND phase='prepared' AND EXISTS(SELECT 1 FROM job_runs_v2 WHERE id=? AND status=? AND session_id=?)`, string(raw), runID, restartID, runID, jobsV2RunRunning, c.SessionID)
	if err != nil {
		return err
	}
	n, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errJobsRestartConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE job_runs_v2 SET stdout=?,stderr=?,thinking=?,response=?,turn_count=turn_count+?,input_tokens=input_tokens+?,output_tokens=output_tokens+?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, result.Stdout, result.Stderr, result.Thinking, result.Response, result.TurnCount, result.InputTokens, result.OutputTokens, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// activateLLMRestart requires a successfully consumed, revision-checked session
// handoff. A crash between consuming that grant and this transaction may lose
// recovery, but cannot execute it twice. Never infer authorization from a job's
// cancelled status, a session ID, or a leftover journal row alone.
func (m *jobsV2Manager) activateLLMRestart(ctx context.Context, runID, restartID string, grant session.CommandHandoff) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := readJobsRestart(ctx, tx, runID, restartID, "sealed")
	if err != nil {
		return err
	}
	if grant.ID == "" || grant.ID != c.HandoffID || grant.Service != c.Service || grant.SourceInstance != c.SourceInstance || grant.SessionID != c.SessionID {
		return errJobsRestartConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE job_runs_v2 SET status=?,worker_id=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=? AND session_id=?`, jobsV2RunQueued, runID, jobsV2RunRunning, c.SessionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errJobsRestartConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE job_restart_checkpoints SET phase='ready' WHERE run_id=? AND restart_id=? AND phase='sealed'`, runID, restartID); err != nil {
		return err
	}
	return tx.Commit()
}

// takeLLMRestart participates in the worker's existing claim transaction, so
// checkpoint consumption cannot be separated from claiming the original run.
func takeLLMRestart(ctx context.Context, tx *sql.Tx, runID string) (*jobsRestartCheckpoint, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT payload FROM job_restart_checkpoints WHERE run_id=? AND phase='ready'`, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c jobsRestartCheckpoint
	if err = json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, err
	}
	if c.RunID != runID || c.HandoffID == "" {
		return nil, errJobsRestartConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE job_restart_checkpoints SET phase='claimed' WHERE run_id=? AND phase='ready' AND EXISTS(SELECT 1 FROM job_runs_v2 WHERE id=? AND status=? AND session_id=?)`, runID, runID, jobsV2RunClaimed, c.SessionID)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, errJobsRestartConflict
	}
	return &c, nil
}

func readJobsRestart(ctx context.Context, tx *sql.Tx, runID, restartID, phase string) (jobsRestartCheckpoint, error) {
	var c jobsRestartCheckpoint
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT payload FROM job_restart_checkpoints WHERE run_id=? AND restart_id=? AND phase=?`, runID, restartID, phase).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, errJobsRestartConflict
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal([]byte(raw), &c)
	return c, err
}

type jobsRestartContextKey struct{}

// startJobRun atomically retires the claimed checkpoint at actual admission.
// Until this point a shutdown can return both the run and its checkpoint to the
// queue. After this point an unexpected crash follows normal worker-loss rules.
func (m *jobsV2Manager) startJobRun(run jobsV2Run, started time.Time) (sql.Result, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE job_runs_v2 SET status=?,started_at=COALESCE(started_at,?),session_id=NULLIF(?,''),updated_at=CURRENT_TIMESTAMP WHERE id=? AND status=?`, jobsV2RunRunning, started, run.SessionID, run.ID, jobsV2RunClaimed)
	if err != nil {
		return nil, err
	}
	if run.restart != nil {
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			deleted, err := tx.Exec(`DELETE FROM job_restart_checkpoints WHERE run_id=? AND restart_id=? AND phase='claimed'`, run.ID, run.restart.RestartID)
			if err != nil {
				return nil, err
			}
			n, err = deleted.RowsAffected()
			if err != nil {
				return nil, err
			}
			if n != 1 {
				return nil, errJobsRestartConflict
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// recoverUnstartedLLMClaims handles a crash after checkpoint claim but before
// actual execution admission. startJobRun atomically deletes the claimed marker,
// so its presence proves the continuation has not executed and may be requeued.
func (m *jobsV2Manager) recoverUnstartedLLMClaims(ctx context.Context) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE job_runs_v2 SET status=?,worker_id=NULL,updated_at=CURRENT_TIMESTAMP WHERE status=? AND EXISTS(SELECT 1 FROM job_restart_checkpoints c WHERE c.run_id=job_runs_v2.id AND c.phase='claimed')`, jobsV2RunQueued, jobsV2RunClaimed); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE job_restart_checkpoints SET phase='ready' WHERE phase='claimed' AND EXISTS(SELECT 1 FROM job_runs_v2 r WHERE r.id=job_restart_checkpoints.run_id AND r.status=?)`, jobsV2RunQueued); err != nil {
		return err
	}
	return tx.Commit()
}
