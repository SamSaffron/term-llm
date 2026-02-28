package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	_ "modernc.org/sqlite"
)

type jobsV2RunnerType string

type jobsV2TriggerType string

type jobsV2RunStatus string

type serveJobsExecutor func(ctx context.Context, agentName, instructions string, onEvent func(llm.Event)) error

const (
	jobsV2RunnerLLM     jobsV2RunnerType = "llm"
	jobsV2RunnerProgram jobsV2RunnerType = "program"

	jobsV2TriggerManual jobsV2TriggerType = "manual"
	jobsV2TriggerOnce   jobsV2TriggerType = "once"
	jobsV2TriggerCron   jobsV2TriggerType = "cron"

	jobsV2RunQueued          jobsV2RunStatus = "queued"
	jobsV2RunClaimed         jobsV2RunStatus = "claimed"
	jobsV2RunRunning         jobsV2RunStatus = "running"
	jobsV2RunSucceeded       jobsV2RunStatus = "succeeded"
	jobsV2RunFailed          jobsV2RunStatus = "failed"
	jobsV2RunCancelled       jobsV2RunStatus = "cancelled"
	jobsV2RunCancelRequested jobsV2RunStatus = "cancel_requested"
	jobsV2RunTimedOut        jobsV2RunStatus = "timed_out"
	jobsV2RunSkipped         jobsV2RunStatus = "skipped"
)

type jobsV2RetryPolicy struct {
	MaxAttempts  int    `json:"max_attempts,omitempty"`
	Backoff      string `json:"backoff,omitempty"`
	InitialDelay string `json:"initial_delay,omitempty"`
	MaxDelay     string `json:"max_delay,omitempty"`
	JitterPct    int    `json:"jitter_pct,omitempty"`
}

type jobsV2TriggerConfig struct {
	RunAt      string `json:"run_at,omitempty"`
	Expression string `json:"expression,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
}

type jobsV2Job struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Enabled           bool              `json:"enabled"`
	RunnerType        jobsV2RunnerType  `json:"runner_type"`
	RunnerConfig      json.RawMessage   `json:"runner_config"`
	TriggerType       jobsV2TriggerType `json:"trigger_type"`
	TriggerConfig     json.RawMessage   `json:"trigger_config"`
	ScheduleTimezone  string            `json:"schedule_timezone,omitempty"`
	ConcurrencyPolicy string            `json:"concurrency_policy,omitempty"`
	MaxConcurrentRuns int               `json:"max_concurrent_runs,omitempty"`
	RetryPolicy       json.RawMessage   `json:"retry_policy,omitempty"`
	TimeoutSeconds    int               `json:"timeout_seconds,omitempty"`
	MisfirePolicy     string            `json:"misfire_policy,omitempty"`
	Labels            json.RawMessage   `json:"labels,omitempty"`
	NextRunAt         *time.Time        `json:"next_run_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type jobsV2Run struct {
	ID           string          `json:"id"`
	JobID        string          `json:"job_id"`
	Attempt      int             `json:"attempt"`
	Trigger      string          `json:"trigger"`
	ScheduledFor time.Time       `json:"scheduled_for"`
	Status       jobsV2RunStatus `json:"status"`
	WorkerID     string          `json:"worker_id,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	ExitCode     *int            `json:"exit_code,omitempty"`
	Error        string          `json:"error,omitempty"`
	Stdout       string          `json:"stdout,omitempty"`
	Stderr       string          `json:"stderr,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Response     string          `json:"response,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type jobsV2RunEvent struct {
	ID        int64           `json:"id"`
	RunID     string          `json:"run_id"`
	EventType string          `json:"event_type"`
	Message   string          `json:"message,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type jobsV2RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Thinking string
	Response string
}

type jobsV2Runner interface {
	Run(ctx context.Context, job jobsV2Job) (jobsV2RunResult, error)
}

type jobsV2ProgramRunner struct{}

type jobsV2ProgramConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Env     []string `json:"env,omitempty"`
	Shell   bool     `json:"shell,omitempty"`
}

func (r *jobsV2ProgramRunner) Run(ctx context.Context, job jobsV2Job) (jobsV2RunResult, error) {
	var cfg jobsV2ProgramConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
		return jobsV2RunResult{}, fmt.Errorf("invalid program runner config: %w", err)
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return jobsV2RunResult{}, fmt.Errorf("program command is required")
	}

	var cmd *exec.Cmd
	if cfg.Shell {
		args := append([]string{"-c", cfg.Command}, cfg.Args...)
		cmd = exec.CommandContext(ctx, detectShell(), args...)
	} else {
		cmd = exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	}
	// When the context is cancelled, Go sends SIGKILL. Without WaitDelay,
	// cmd.Output() can still block indefinitely waiting for the process to exit
	// and for all pipe I/O to drain. Set a short WaitDelay so the pipes are
	// force-closed quickly after the kill signal is sent.
	cmd.WaitDelay = 5 * time.Second
	if strings.TrimSpace(cfg.Cwd) != "" {
		cmd.Dir = cfg.Cwd
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	stdout, err := cmd.Output()
	stderr := ""
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			stderr = string(exitErr.Stderr)
		} else {
			return jobsV2RunResult{}, fmt.Errorf("program run failed: %w", err)
		}
	}

	result := jobsV2RunResult{ExitCode: exitCode, Stdout: string(stdout), Stderr: stderr}
	if exitCode != 0 {
		return result, fmt.Errorf("program exited with code %d", exitCode)
	}
	return result, nil
}

type jobsV2LLMRunner struct {
	exec serveJobsExecutor
}

type jobsV2LLMConfig struct {
	AgentName    string `json:"agent_name"`
	Instructions string `json:"instructions"`
}

func (r *jobsV2LLMRunner) Run(ctx context.Context, job jobsV2Job) (jobsV2RunResult, error) {
	if r.exec == nil {
		return jobsV2RunResult{}, fmt.Errorf("llm runner is not configured")
	}
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
		return jobsV2RunResult{}, fmt.Errorf("invalid llm runner config: %w", err)
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return jobsV2RunResult{}, fmt.Errorf("llm runner agent_name is required")
	}
	if strings.TrimSpace(cfg.Instructions) == "" {
		return jobsV2RunResult{}, fmt.Errorf("llm runner instructions is required")
	}
	res := jobsV2RunResult{}
	err := r.exec(ctx, cfg.AgentName, cfg.Instructions, func(ev llm.Event) {
		switch ev.Type {
		case llm.EventReasoningDelta:
			res.Thinking += ev.Text
		case llm.EventTextDelta:
			res.Response += ev.Text
		}
	})
	return res, err
}

type jobsV2Manager struct {
	db       *sql.DB
	workers  int
	workerID string
	runners  map[jobsV2RunnerType]jobsV2Runner
	tick     time.Duration
	// Retention settings for protecting disk usage.
	retentionRunDays     int
	retentionEventDays   int
	retentionMaxRunsJob  int
	cleanupInterval      time.Duration
	lastCleanupCompleted time.Time

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	cancels map[string]context.CancelFunc
}

const jobsV2Schema = `
CREATE TABLE IF NOT EXISTS jobs_v2 (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	enabled INTEGER NOT NULL DEFAULT 1,
	runner_type TEXT NOT NULL,
	runner_config TEXT NOT NULL,
	trigger_type TEXT NOT NULL,
	trigger_config TEXT NOT NULL,
	schedule_timezone TEXT,
	concurrency_policy TEXT NOT NULL DEFAULT 'forbid',
	max_concurrent_runs INTEGER NOT NULL DEFAULT 1,
	retry_policy TEXT,
	timeout_seconds INTEGER NOT NULL DEFAULT 300,
	misfire_policy TEXT NOT NULL DEFAULT 'skip',
	labels TEXT,
	next_run_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS job_runs_v2 (
	id TEXT PRIMARY KEY,
	job_id TEXT NOT NULL REFERENCES jobs_v2(id) ON DELETE CASCADE,
	attempt INTEGER NOT NULL,
	trigger TEXT NOT NULL,
	scheduled_for TIMESTAMP NOT NULL,
	status TEXT NOT NULL,
	worker_id TEXT,
	started_at TIMESTAMP,
	finished_at TIMESTAMP,
	exit_code INTEGER,
	error TEXT,
	stdout TEXT,
	stderr TEXT,
	thinking TEXT,
	response TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_jobs_v2_next_run_at ON jobs_v2(next_run_at);
CREATE INDEX IF NOT EXISTS idx_job_runs_v2_job_id ON job_runs_v2(job_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_job_runs_v2_status ON job_runs_v2(status, scheduled_for);

CREATE TABLE IF NOT EXISTS job_run_events_v2 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL REFERENCES job_runs_v2(id) ON DELETE CASCADE,
	event_type TEXT NOT NULL,
	message TEXT,
	data TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_job_run_events_v2_run_id ON job_run_events_v2(run_id, created_at, id);
`

func newJobsV2Manager(dbPath string, workers int, llmExec serveJobsExecutor) (*jobsV2Manager, error) {
	if workers <= 0 {
		workers = 1
	}
	if strings.TrimSpace(dbPath) == "" {
		dataDir, err := session.GetDataDir()
		if err != nil {
			return nil, fmt.Errorf("resolve jobs data dir: %w", err)
		}
		dbPath = filepath.Join(dataDir, "jobs_v2.db")
	}
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
			return nil, fmt.Errorf("create jobs db dir: %w", err)
		}
	}

	dsn := dbPath
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open jobs db: %w", err)
	}
	// Required for :memory: databases: keep a single connection so schema/data persist.
	db.SetMaxOpenConns(1)
	if err := execJobsV2Schema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init jobs schema: %w", err)
	}

	mgr := &jobsV2Manager{
		db:       db,
		workers:  workers,
		workerID: "worker_" + randomSuffix(),
		tick:     time.Second,
		// Conservative defaults: enough history for debugging without unbounded growth.
		retentionRunDays:    30,
		retentionEventDays:  30,
		retentionMaxRunsJob: 1000,
		cleanupInterval:     time.Hour,
		runners: map[jobsV2RunnerType]jobsV2Runner{
			jobsV2RunnerProgram: &jobsV2ProgramRunner{},
			jobsV2RunnerLLM:     &jobsV2LLMRunner{exec: llmExec},
		},
		cancels: make(map[string]context.CancelFunc),
	}

	if err := mgr.recoverRuns(); err != nil {
		_ = db.Close()
		return nil, err
	}

	mgr.wg.Add(1)
	go mgr.schedulerLoop()
	for i := 0; i < mgr.workers; i++ {
		mgr.wg.Add(1)
		go mgr.workerLoop()
	}

	return mgr, nil
}

func execJobsV2Schema(db *sql.DB) error {
	stmts := strings.Split(jobsV2Schema, ";")
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (m *jobsV2Manager) recoverRuns() error {
	_, err := m.db.Exec(`UPDATE job_runs_v2 SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE status IN (?, ?, ?)`, jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning, jobsV2RunCancelRequested)
	if err != nil {
		return fmt.Errorf("recover runs: %w", err)
	}
	return nil
}

func (m *jobsV2Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cancels := make([]context.CancelFunc, 0, len(m.cancels))
	for _, cancel := range m.cancels {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	m.wg.Wait()
	return m.db.Close()
}

func (m *jobsV2Manager) schedulerLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.tick)
	defer ticker.Stop()

	for {
		if m.isClosed() {
			return
		}
		now := time.Now()
		_ = m.scheduleDueRuns(now)
		_ = m.maybeRunCleanup(now)
		select {
		case <-ticker.C:
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (m *jobsV2Manager) maybeRunCleanup(now time.Time) error {
	if m.cleanupInterval <= 0 {
		return nil
	}
	m.mu.Lock()
	if !m.lastCleanupCompleted.IsZero() && now.Sub(m.lastCleanupCompleted) < m.cleanupInterval {
		m.mu.Unlock()
		return nil
	}
	// Optimistically set before running to avoid stampeding.
	m.lastCleanupCompleted = now
	m.mu.Unlock()

	return m.pruneOldData(now.UTC())
}

func (m *jobsV2Manager) pruneOldData(now time.Time) error {
	terminalStatuses := []any{jobsV2RunSucceeded, jobsV2RunFailed, jobsV2RunCancelled, jobsV2RunTimedOut, jobsV2RunSkipped}

	if m.retentionRunDays > 0 {
		cutoff := now.Add(-time.Duration(m.retentionRunDays) * 24 * time.Hour)
		_, err := m.db.Exec(`
			DELETE FROM job_runs_v2
			WHERE status IN (?, ?, ?, ?, ?)
			  AND COALESCE(finished_at, created_at) < ?`,
			terminalStatuses[0], terminalStatuses[1], terminalStatuses[2], terminalStatuses[3], terminalStatuses[4], cutoff)
		if err != nil {
			return err
		}
	}

	if m.retentionMaxRunsJob > 0 {
		_, err := m.db.Exec(`
			DELETE FROM job_runs_v2
			WHERE id IN (
				SELECT id FROM (
					SELECT id,
						   ROW_NUMBER() OVER (
							   PARTITION BY job_id
							   ORDER BY COALESCE(finished_at, created_at) DESC, created_at DESC
						   ) AS rn
					FROM job_runs_v2
					WHERE status IN (?, ?, ?, ?, ?)
				)
				WHERE rn > ?
			)`,
			terminalStatuses[0], terminalStatuses[1], terminalStatuses[2], terminalStatuses[3], terminalStatuses[4], m.retentionMaxRunsJob)
		if err != nil {
			return err
		}
	}

	if m.retentionEventDays > 0 {
		cutoff := now.Add(-time.Duration(m.retentionEventDays) * 24 * time.Hour)
		_, err := m.db.Exec(`DELETE FROM job_run_events_v2 WHERE created_at < ?`, cutoff)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *jobsV2Manager) workerLoop() {
	defer m.wg.Done()
	for {
		if m.isClosed() {
			return
		}
		run, ok, err := m.claimNextRun()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if !ok {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		m.executeRun(run)
	}
}

func (m *jobsV2Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *jobsV2Manager) scheduleDueRuns(now time.Time) error {
	rows, err := m.db.Query(`SELECT id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at FROM jobs_v2 WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ? ORDER BY next_run_at ASC LIMIT 200`, now.UTC())
	if err != nil {
		return err
	}
	defer rows.Close()

	var jobs []jobsV2Job
	for rows.Next() {
		job, err := scanJobV2(rows)
		if err != nil {
			return err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, job := range jobs {
		if err := m.scheduleOne(job, now); err != nil {
			continue
		}
	}
	return nil
}

func (m *jobsV2Manager) scheduleOne(job jobsV2Job, now time.Time) error {
	next, err := computeNextRunAt(job, now)
	if err != nil {
		return err
	}

	if job.ConcurrencyPolicy == "forbid" {
		var active int
		err := m.db.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE job_id = ? AND status IN (?, ?, ?)`, job.ID, jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning).Scan(&active)
		if err != nil {
			return err
		}
		if active > 0 {
			_, err = m.db.Exec(`UPDATE jobs_v2 SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, job.ID)
			return err
		}
	}

	runID := "run_" + randomSuffix()
	_, err = m.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, runID, job.ID, "schedule", now.UTC(), jobsV2RunQueued)
	if err != nil {
		return err
	}
	_ = m.addRunEvent(runID, "queued", "scheduled run queued", map[string]any{"trigger": "schedule", "attempt": 1})

	if job.TriggerType == jobsV2TriggerOnce {
		_, err = m.db.Exec(`UPDATE jobs_v2 SET enabled = 0, next_run_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, job.ID)
		return err
	}

	_, err = m.db.Exec(`UPDATE jobs_v2 SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, job.ID)
	return err
}

func (m *jobsV2Manager) claimNextRun() (jobsV2Run, bool, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return jobsV2Run{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRow(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, created_at, updated_at FROM job_runs_v2 WHERE status = ? ORDER BY scheduled_for ASC LIMIT 1`, jobsV2RunQueued)
	run, err := scanRunV2(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return jobsV2Run{}, false, nil
		}
		return jobsV2Run{}, false, err
	}

	res, err := tx.Exec(`UPDATE job_runs_v2 SET status = ?, worker_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`, jobsV2RunClaimed, m.workerID, run.ID, jobsV2RunQueued)
	if err != nil {
		return jobsV2Run{}, false, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return jobsV2Run{}, false, nil
	}

	if err := tx.Commit(); err != nil {
		return jobsV2Run{}, false, err
	}
	run.Status = jobsV2RunClaimed
	run.WorkerID = m.workerID
	_ = m.addRunEvent(run.ID, "claimed", "run claimed by worker", map[string]any{"worker_id": m.workerID})
	return run, true, nil
}

func (m *jobsV2Manager) executeRun(run jobsV2Run) {
	job, err := m.GetJob(run.JobID)
	if err != nil {
		_ = m.finishRun(run.ID, jobsV2RunFailed, jobsV2RunResult{}, fmt.Errorf("load job: %w", err), run.Attempt)
		return
	}

	runner, ok := m.runners[job.RunnerType]
	if !ok {
		_ = m.finishRun(run.ID, jobsV2RunFailed, jobsV2RunResult{}, fmt.Errorf("unknown runner type: %s", job.RunnerType), run.Attempt)
		return
	}

	started := time.Now().UTC()
	_, _ = m.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunRunning, started, run.ID)
	_ = m.addRunEvent(run.ID, "running", "run started", map[string]any{"worker_id": m.workerID})

	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	m.mu.Lock()
	if !m.closed {
		m.cancels[run.ID] = cancel
	}
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, run.ID)
		m.mu.Unlock()
	}()

	result, runErr := runner.Run(ctx, job)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		_ = m.finishRun(run.ID, jobsV2RunTimedOut, result, fmt.Errorf("timed out"), run.Attempt)
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		_ = m.finishRun(run.ID, jobsV2RunCancelled, result, context.Canceled, run.Attempt)
		return
	}
	if runErr != nil {
		_ = m.finishRun(run.ID, jobsV2RunFailed, result, runErr, run.Attempt)
		return
	}
	_ = m.finishRun(run.ID, jobsV2RunSucceeded, result, nil, run.Attempt)
}

func (m *jobsV2Manager) finishRun(runID string, status jobsV2RunStatus, result jobsV2RunResult, runErr error, attempt int) error {
	now := time.Now().UTC()
	var errText string
	if runErr != nil {
		errText = runErr.Error()
	}
	_, err := m.db.Exec(`UPDATE job_runs_v2 SET status = ?, finished_at = ?, exit_code = ?, error = ?, stdout = ?, stderr = ?, thinking = ?, response = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, status, now, result.ExitCode, errText, result.Stdout, result.Stderr, result.Thinking, result.Response, runID)
	if err != nil {
		return err
	}
	_ = m.addRunEvent(runID, string(status), "run finished", map[string]any{
		"status":    status,
		"attempt":   attempt,
		"exit_code": result.ExitCode,
		"error":     errText,
	})

	if status == jobsV2RunFailed || status == jobsV2RunTimedOut {
		run, err := m.GetRun(runID)
		if err != nil {
			return nil
		}
		job, err := m.GetJob(run.JobID)
		if err != nil {
			return nil
		}
		policy := decodeRetryPolicy(job.RetryPolicy)
		if attempt < policy.MaxAttempts {
			delay := computeRetryDelay(policy, attempt)
			retryID := "run_" + randomSuffix()
			_, _ = m.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, retryID, job.ID, attempt+1, "retry", now.Add(delay), jobsV2RunQueued)
			_ = m.addRunEvent(runID, "retry_scheduled", "retry run scheduled", map[string]any{
				"retry_run_id": retryID,
				"next_attempt": attempt + 1,
				"delay":        delay.String(),
			})
			_ = m.addRunEvent(retryID, "queued", "retry run queued", map[string]any{
				"trigger": "retry",
				"attempt": attempt + 1,
			})
		}
	}

	return nil
}

func (m *jobsV2Manager) addRunEvent(runID, eventType, message string, payload any) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(eventType) == "" {
		return nil
	}
	var data string
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		data = string(encoded)
	}
	_, err := m.db.Exec(`INSERT INTO job_run_events_v2 (run_id, event_type, message, data, created_at) VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		runID, eventType, strings.TrimSpace(message), nullableString(data))
	return err
}

func (m *jobsV2Manager) ListRunEvents(runID string, limit, offset int) ([]jobsV2RunEvent, int, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, 0, fmt.Errorf("run_id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := m.db.QueryRow(`SELECT COUNT(1) FROM job_run_events_v2 WHERE run_id = ?`, runID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := m.db.Query(`SELECT id, run_id, event_type, message, data, created_at FROM job_run_events_v2 WHERE run_id = ? ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`, runID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := make([]jobsV2RunEvent, 0)
	for rows.Next() {
		var ev jobsV2RunEvent
		var message sql.NullString
		var data sql.NullString
		if err := rows.Scan(&ev.ID, &ev.RunID, &ev.EventType, &message, &data, &ev.CreatedAt); err != nil {
			return nil, 0, err
		}
		if message.Valid {
			ev.Message = message.String
		}
		if data.Valid {
			ev.Data = json.RawMessage(data.String)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

func decodeRetryPolicy(raw json.RawMessage) jobsV2RetryPolicy {
	policy := jobsV2RetryPolicy{MaxAttempts: 1, Backoff: "fixed", InitialDelay: "10s", MaxDelay: "5m"}
	if len(raw) == 0 {
		return policy
	}
	_ = json.Unmarshal(raw, &policy)
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.Backoff == "" {
		policy.Backoff = "fixed"
	}
	if policy.InitialDelay == "" {
		policy.InitialDelay = "10s"
	}
	if policy.MaxDelay == "" {
		policy.MaxDelay = "5m"
	}
	return policy
}

func computeRetryDelay(policy jobsV2RetryPolicy, attempt int) time.Duration {
	base, err := time.ParseDuration(policy.InitialDelay)
	if err != nil || base <= 0 {
		base = 10 * time.Second
	}
	maxDelay, err := time.ParseDuration(policy.MaxDelay)
	if err != nil || maxDelay <= 0 {
		maxDelay = 5 * time.Minute
	}
	delay := base
	switch policy.Backoff {
	case "linear":
		delay = time.Duration(attempt) * base
	case "exponential":
		mul := 1
		for i := 1; i < attempt; i++ {
			mul *= 2
		}
		delay = time.Duration(mul) * base
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

func (m *jobsV2Manager) CreateJob(req jobsV2Job) (jobsV2Job, error) {
	if strings.TrimSpace(req.Name) == "" {
		return jobsV2Job{}, fmt.Errorf("name is required")
	}
	if req.RunnerType != jobsV2RunnerLLM && req.RunnerType != jobsV2RunnerProgram {
		return jobsV2Job{}, fmt.Errorf("runner_type must be one of: llm, program")
	}
	if req.TriggerType != jobsV2TriggerManual && req.TriggerType != jobsV2TriggerOnce && req.TriggerType != jobsV2TriggerCron {
		return jobsV2Job{}, fmt.Errorf("trigger_type must be one of: manual, once, cron")
	}
	if req.MaxConcurrentRuns <= 0 {
		req.MaxConcurrentRuns = 1
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 300
	}
	if req.ConcurrencyPolicy == "" {
		req.ConcurrencyPolicy = "forbid"
	}
	if req.MisfirePolicy == "" {
		req.MisfirePolicy = "skip"
	}

	cfg, err := parseTriggerConfig(req.TriggerType, req.TriggerConfig)
	if err != nil {
		return jobsV2Job{}, err
	}
	next := initialNextRun(req.TriggerType, cfg, req.ScheduleTimezone)

	now := time.Now().UTC()
	id := "job_" + randomSuffix()
	_, err = m.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		req.Name,
		boolToInt(req.Enabled),
		req.RunnerType,
		stringOrEmptyRaw(req.RunnerConfig, "{}"),
		req.TriggerType,
		stringOrEmptyRaw(req.TriggerConfig, "{}"),
		normalizeTZ(req.ScheduleTimezone),
		req.ConcurrencyPolicy,
		req.MaxConcurrentRuns,
		nullableRaw(req.RetryPolicy),
		req.TimeoutSeconds,
		req.MisfirePolicy,
		nullableRaw(req.Labels),
		next,
		now,
		now,
	)
	if err != nil {
		return jobsV2Job{}, err
	}
	return m.GetJob(id)
}

func (m *jobsV2Manager) GetJob(id string) (jobsV2Job, error) {
	row := m.db.QueryRow(`SELECT id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at FROM jobs_v2 WHERE id = ?`, id)
	job, err := scanJobV2(row)
	if err != nil {
		return jobsV2Job{}, err
	}
	return job, nil
}

func (m *jobsV2Manager) ListJobs(limit, offset int) ([]jobsV2Job, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := m.db.QueryRow(`SELECT COUNT(1) FROM jobs_v2`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := m.db.Query(`SELECT id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at FROM jobs_v2 ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	jobs := make([]jobsV2Job, 0)
	for rows.Next() {
		job, err := scanJobV2(rows)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

func (m *jobsV2Manager) UpdateJob(id string, req jobsV2Job) (jobsV2Job, error) {
	current, err := m.GetJob(id)
	if err != nil {
		return jobsV2Job{}, err
	}
	if strings.TrimSpace(req.Name) != "" {
		current.Name = req.Name
	}
	if req.RunnerType != "" {
		current.RunnerType = req.RunnerType
	}
	if len(req.RunnerConfig) > 0 {
		current.RunnerConfig = req.RunnerConfig
	}
	if req.TriggerType != "" {
		current.TriggerType = req.TriggerType
	}
	if len(req.TriggerConfig) > 0 {
		current.TriggerConfig = req.TriggerConfig
	}
	if req.ScheduleTimezone != "" {
		current.ScheduleTimezone = req.ScheduleTimezone
	}
	if req.ConcurrencyPolicy != "" {
		current.ConcurrencyPolicy = req.ConcurrencyPolicy
	}
	if req.MaxConcurrentRuns > 0 {
		current.MaxConcurrentRuns = req.MaxConcurrentRuns
	}
	if len(req.RetryPolicy) > 0 {
		current.RetryPolicy = req.RetryPolicy
	}
	if req.TimeoutSeconds > 0 {
		current.TimeoutSeconds = req.TimeoutSeconds
	}
	if req.MisfirePolicy != "" {
		current.MisfirePolicy = req.MisfirePolicy
	}
	if len(req.Labels) > 0 {
		current.Labels = req.Labels
	}
	current.Enabled = req.Enabled

	cfg, err := parseTriggerConfig(current.TriggerType, current.TriggerConfig)
	if err != nil {
		return jobsV2Job{}, err
	}
	next := initialNextRun(current.TriggerType, cfg, current.ScheduleTimezone)

	_, err = m.db.Exec(`UPDATE jobs_v2 SET name = ?, enabled = ?, runner_type = ?, runner_config = ?, trigger_type = ?, trigger_config = ?, schedule_timezone = ?, concurrency_policy = ?, max_concurrent_runs = ?, retry_policy = ?, timeout_seconds = ?, misfire_policy = ?, labels = ?, next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		current.Name,
		boolToInt(current.Enabled),
		current.RunnerType,
		stringOrEmptyRaw(current.RunnerConfig, "{}"),
		current.TriggerType,
		stringOrEmptyRaw(current.TriggerConfig, "{}"),
		normalizeTZ(current.ScheduleTimezone),
		current.ConcurrencyPolicy,
		current.MaxConcurrentRuns,
		nullableRaw(current.RetryPolicy),
		current.TimeoutSeconds,
		current.MisfirePolicy,
		nullableRaw(current.Labels),
		next,
		id,
	)
	if err != nil {
		return jobsV2Job{}, err
	}
	return m.GetJob(id)
}

func (m *jobsV2Manager) DeleteJob(id string, cancelActive bool) error {
	if cancelActive {
		runs, _, err := m.ListRuns(id, 200, 0)
		if err == nil {
			for _, run := range runs {
				if run.Status == jobsV2RunRunning || run.Status == jobsV2RunClaimed || run.Status == jobsV2RunQueued {
					_, _ = m.CancelRun(run.ID)
				}
			}
		}
	}
	_, err := m.db.Exec(`DELETE FROM jobs_v2 WHERE id = ?`, id)
	return err
}

func (m *jobsV2Manager) TriggerJob(id string) (jobsV2Run, error) {
	job, err := m.GetJob(id)
	if err != nil {
		return jobsV2Run{}, err
	}
	if !job.Enabled {
		return jobsV2Run{}, fmt.Errorf("job is disabled")
	}
	if job.ConcurrencyPolicy == "forbid" {
		var active int
		err := m.db.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE job_id = ? AND status IN (?, ?, ?)`, id, jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning).Scan(&active)
		if err != nil {
			return jobsV2Run{}, err
		}
		if active > 0 {
			return jobsV2Run{}, fmt.Errorf("job already has an active run")
		}
	}
	runID := "run_" + randomSuffix()
	now := time.Now().UTC()
	_, err = m.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, runID, id, "manual", now, jobsV2RunQueued)
	if err != nil {
		return jobsV2Run{}, err
	}
	_ = m.addRunEvent(runID, "queued", "manual run queued", map[string]any{"trigger": "manual", "attempt": 1})
	return m.GetRun(runID)
}

func (m *jobsV2Manager) GetRun(id string) (jobsV2Run, error) {
	row := m.db.QueryRow(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, created_at, updated_at FROM job_runs_v2 WHERE id = ?`, id)
	return scanRunV2(row)
}

func (m *jobsV2Manager) ListRuns(jobID string, limit, offset int) ([]jobsV2Run, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	where := ""
	args := []any{}
	if strings.TrimSpace(jobID) != "" {
		where = " WHERE job_id = ?"
		args = append(args, jobID)
	}
	countQuery := "SELECT COUNT(1) FROM job_runs_v2" + where
	var total int
	if err := m.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := "SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, created_at, updated_at FROM job_runs_v2" + where + " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	runs := make([]jobsV2Run, 0)
	for rows.Next() {
		run, err := scanRunV2(rows)
		if err != nil {
			return nil, 0, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

func (m *jobsV2Manager) CancelRun(id string) (jobsV2Run, error) {
	run, err := m.GetRun(id)
	if err != nil {
		return jobsV2Run{}, err
	}
	now := time.Now().UTC()
	switch run.Status {
	case jobsV2RunQueued, jobsV2RunClaimed:
		_, err = m.db.Exec(`UPDATE job_runs_v2 SET status = ?, finished_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunCancelled, now, id)
		_ = m.addRunEvent(id, "cancelled", "run cancelled before start", nil)
	case jobsV2RunRunning:
		_, err = m.db.Exec(`UPDATE job_runs_v2 SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunCancelRequested, id)
		_ = m.addRunEvent(id, "cancel_requested", "cancellation requested", nil)
		m.mu.Lock()
		cancel := m.cancels[id]
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	default:
		return run, nil
	}
	if err != nil {
		return jobsV2Run{}, err
	}
	return m.GetRun(id)
}

func scanJobV2(scanner interface{ Scan(dest ...any) error }) (jobsV2Job, error) {
	var job jobsV2Job
	var enabled int
	var runnerType, triggerType string
	var runnerConfig string
	var triggerConfig string
	var scheduleTZ sql.NullString
	var retryPolicy, labels sql.NullString
	var nextRun sql.NullTime
	err := scanner.Scan(
		&job.ID,
		&job.Name,
		&enabled,
		&runnerType,
		&runnerConfig,
		&triggerType,
		&triggerConfig,
		&scheduleTZ,
		&job.ConcurrencyPolicy,
		&job.MaxConcurrentRuns,
		&retryPolicy,
		&job.TimeoutSeconds,
		&job.MisfirePolicy,
		&labels,
		&nextRun,
		&job.CreatedAt,
		&job.UpdatedAt,
	)
	if err != nil {
		return jobsV2Job{}, err
	}
	job.Enabled = enabled == 1
	job.RunnerType = jobsV2RunnerType(runnerType)
	job.TriggerType = jobsV2TriggerType(triggerType)
	job.RunnerConfig = json.RawMessage(runnerConfig)
	job.TriggerConfig = json.RawMessage(triggerConfig)
	if scheduleTZ.Valid {
		job.ScheduleTimezone = scheduleTZ.String
	}
	if retryPolicy.Valid {
		job.RetryPolicy = json.RawMessage(retryPolicy.String)
	}
	if labels.Valid {
		job.Labels = json.RawMessage(labels.String)
	}
	if nextRun.Valid {
		t := nextRun.Time.UTC()
		job.NextRunAt = &t
	}
	return job, nil
}

func scanRunV2(scanner interface{ Scan(dest ...any) error }) (jobsV2Run, error) {
	var run jobsV2Run
	var status string
	var workerID sql.NullString
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	var exitCode sql.NullInt64
	var errText sql.NullString
	var stdout sql.NullString
	var stderr sql.NullString
	var thinking sql.NullString
	var response sql.NullString
	err := scanner.Scan(
		&run.ID,
		&run.JobID,
		&run.Attempt,
		&run.Trigger,
		&run.ScheduledFor,
		&status,
		&workerID,
		&startedAt,
		&finishedAt,
		&exitCode,
		&errText,
		&stdout,
		&stderr,
		&thinking,
		&response,
		&run.CreatedAt,
		&run.UpdatedAt,
	)
	if err != nil {
		return jobsV2Run{}, err
	}
	run.Status = jobsV2RunStatus(status)
	if workerID.Valid {
		run.WorkerID = workerID.String
	}
	if startedAt.Valid {
		t := startedAt.Time.UTC()
		run.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time.UTC()
		run.FinishedAt = &t
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		run.ExitCode = &v
	}
	if errText.Valid {
		run.Error = errText.String
	}
	if stdout.Valid {
		run.Stdout = stdout.String
	}
	if stderr.Valid {
		run.Stderr = stderr.String
	}
	if thinking.Valid {
		run.Thinking = thinking.String
	}
	if response.Valid {
		run.Response = response.String
	}
	return run, nil
}

func parseTriggerConfig(tt jobsV2TriggerType, raw json.RawMessage) (jobsV2TriggerConfig, error) {
	cfg := jobsV2TriggerConfig{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("invalid trigger_config: %w", err)
		}
	}
	switch tt {
	case jobsV2TriggerManual:
		return cfg, nil
	case jobsV2TriggerOnce:
		if strings.TrimSpace(cfg.RunAt) == "" {
			return cfg, fmt.Errorf("trigger_config.run_at is required for once trigger")
		}
		if _, err := time.Parse(time.RFC3339, cfg.RunAt); err != nil {
			return cfg, fmt.Errorf("trigger_config.run_at must be RFC3339")
		}
		return cfg, nil
	case jobsV2TriggerCron:
		if strings.TrimSpace(cfg.Expression) == "" {
			return cfg, fmt.Errorf("trigger_config.expression is required for cron trigger")
		}
		if strings.TrimSpace(cfg.Timezone) == "" {
			return cfg, fmt.Errorf("trigger_config.timezone is required for cron trigger")
		}
		if _, err := time.LoadLocation(cfg.Timezone); err != nil {
			return cfg, fmt.Errorf("invalid cron timezone: %w", err)
		}
		if _, err := parseCronExpression(cfg.Expression); err != nil {
			return cfg, err
		}
		return cfg, nil
	default:
		return cfg, fmt.Errorf("unsupported trigger type: %s", tt)
	}
}

func initialNextRun(tt jobsV2TriggerType, cfg jobsV2TriggerConfig, scheduleTZ string) *time.Time {
	now := time.Now().UTC()
	switch tt {
	case jobsV2TriggerManual:
		return nil
	case jobsV2TriggerOnce:
		t, err := time.Parse(time.RFC3339, cfg.RunAt)
		if err != nil {
			return nil
		}
		u := t.UTC()
		return &u
	case jobsV2TriggerCron:
		next, err := nextCronTime(cfg.Expression, cfg.Timezone, now)
		if err != nil {
			return nil
		}
		u := next.UTC()
		return &u
	default:
		return nil
	}
}

func computeNextRunAt(job jobsV2Job, now time.Time) (*time.Time, error) {
	cfg, err := parseTriggerConfig(job.TriggerType, job.TriggerConfig)
	if err != nil {
		return nil, err
	}
	return initialNextRun(job.TriggerType, cfg, job.ScheduleTimezone), nil
}

type cronField struct {
	any    bool
	values map[int]bool
}

type cronSpec struct {
	minute cronField
	hour   cronField
	dom    cronField
	month  cronField
	dow    cronField
}

func parseCronExpression(expr string) (cronSpec, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 5 {
		return cronSpec{}, fmt.Errorf("cron expression must have 5 fields")
	}
	minute, err := parseCronField(parts[0], 0, 59)
	if err != nil {
		return cronSpec{}, fmt.Errorf("minute: %w", err)
	}
	hour, err := parseCronField(parts[1], 0, 23)
	if err != nil {
		return cronSpec{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseCronField(parts[2], 1, 31)
	if err != nil {
		return cronSpec{}, fmt.Errorf("day-of-month: %w", err)
	}
	month, err := parseCronField(parts[3], 1, 12)
	if err != nil {
		return cronSpec{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseCronField(parts[4], 0, 6)
	if err != nil {
		return cronSpec{}, fmt.Errorf("day-of-week: %w", err)
	}
	return cronSpec{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

func parseCronField(raw string, min, max int) (cronField, error) {
	raw = strings.TrimSpace(raw)
	if raw == "*" {
		return cronField{any: true}, nil
	}
	values := make(map[int]bool)
	segments := strings.Split(raw, ",")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return cronField{}, fmt.Errorf("empty segment")
		}
		if strings.HasPrefix(seg, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(seg, "*/"))
			if err != nil || step <= 0 {
				return cronField{}, fmt.Errorf("invalid step %q", seg)
			}
			for i := min; i <= max; i += step {
				values[i] = true
			}
			continue
		}
		if strings.Contains(seg, "-") {
			r := strings.SplitN(seg, "-", 2)
			if len(r) != 2 {
				return cronField{}, fmt.Errorf("invalid range %q", seg)
			}
			start, err1 := strconv.Atoi(strings.TrimSpace(r[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(r[1]))
			if err1 != nil || err2 != nil || start > end {
				return cronField{}, fmt.Errorf("invalid range %q", seg)
			}
			if start < min || end > max {
				return cronField{}, fmt.Errorf("range %q out of bounds", seg)
			}
			for i := start; i <= end; i++ {
				values[i] = true
			}
			continue
		}
		v, err := strconv.Atoi(seg)
		if err != nil {
			return cronField{}, fmt.Errorf("invalid value %q", seg)
		}
		if v < min || v > max {
			return cronField{}, fmt.Errorf("value %d out of bounds", v)
		}
		values[v] = true
	}
	return cronField{values: values}, nil
}

func (f cronField) match(v int) bool {
	if f.any {
		return true
	}
	return f.values[v]
}

func nextCronTime(expr, tz string, after time.Time) (time.Time, error) {
	spec, err := parseCronExpression(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	max := t.Add(366 * 24 * time.Hour)
	for !t.After(max) {
		dow := int(t.Weekday())
		if spec.minute.match(t.Minute()) && spec.hour.match(t.Hour()) && spec.month.match(int(t.Month())) && spec.dom.match(t.Day()) && spec.dow.match(dow) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("no cron fire time found within one year")
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullableRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func stringOrEmptyRaw(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

func normalizeTZ(v string) string {
	return strings.TrimSpace(v)
}

func (s *serveServer) handleJobsV2(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleCreateJobV2(w, r)
	case http.MethodGet:
		s.handleListJobsV2(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *serveServer) handleJobV2ByID(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	prefix := "/v2/jobs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	jobID := parts[0]

	if len(parts) == 2 && parts[1] == "trigger" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		run, err := s.jobsV2.TriggerJob(jobID)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, run)
		return
	}
	if len(parts) == 2 && parts[1] == "pause" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		updated, err := s.jobsV2.UpdateJob(jobID, jobsV2Job{Enabled: false})
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}
	if len(parts) == 2 && parts[1] == "resume" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		updated, err := s.jobsV2.UpdateJob(jobID, jobsV2Job{Enabled: true})
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated)
		return
	}

	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		job, err := s.jobsV2.GetJob(jobID)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "job not found")
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodPatch:
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req jobsV2Job
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		job, err := s.jobsV2.UpdateJob(jobID, req)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodDelete:
		cancelActive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("cancel_active")), "true")
		if err := s.jobsV2.DeleteJob(jobID, cancelActive); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": jobID, "deleted": true})
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *serveServer) handleCreateJobV2(w http.ResponseWriter, r *http.Request) {
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	var req jobsV2Job
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if s.cfgRef != nil && req.RunnerType == jobsV2RunnerLLM {
		var llmCfg jobsV2LLMConfig
		_ = json.Unmarshal(req.RunnerConfig, &llmCfg)
		if strings.TrimSpace(llmCfg.AgentName) != "" {
			if _, err := LoadAgent(llmCfg.AgentName, s.cfgRef); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
		}
	}
	job, err := s.jobsV2.CreateJob(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *serveServer) handleListJobsV2(w http.ResponseWriter, r *http.Request) {
	offset, err := parseNonNegativeIntQuery(r, "offset", 0)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	limit, err := parseNonNegativeIntQuery(r, "limit", 50)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	items, total, err := s.jobsV2.ListJobs(limit, offset)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

func (s *serveServer) handleRunsV2(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	offset, err := parseNonNegativeIntQuery(r, "offset", 0)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	limit, err := parseNonNegativeIntQuery(r, "limit", 50)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("job_id"))
	items, total, err := s.jobsV2.ListRuns(jobID, limit, offset)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

func (s *serveServer) handleRunV2ByID(w http.ResponseWriter, r *http.Request) {
	if s.jobsV2 == nil {
		http.NotFound(w, r)
		return
	}
	prefix := "/v2/runs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	runID := parts[0]
	if len(parts) == 2 && parts[1] == "events" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		offset, err := parseNonNegativeIntQuery(r, "offset", 0)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		limit, err := parseNonNegativeIntQuery(r, "limit", 200)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		items, total, err := s.jobsV2.ListRunEvents(runID, limit, offset)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   items,
			"total":  total,
			"offset": offset,
			"limit":  limit,
		})
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		run, err := s.jobsV2.CancelRun(runID)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	run, err := s.jobsV2.GetRun(runID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func newServeJobsV2Manager(cfg *config.Config, workers int) (*jobsV2Manager, error) {
	_ = cfg
	return newJobsV2Manager("", workers, newServeJobsExecutor(cfg))
}

func parseNonNegativeIntQuery(r *http.Request, key string, defaultValue int) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return parsed, nil
}

func cloneConfigForServeJob(src *config.Config) *config.Config {
	if src == nil {
		return &config.Config{}
	}
	clone := *src
	if src.Providers != nil {
		clone.Providers = make(map[string]config.ProviderConfig, len(src.Providers))
		for name, cfg := range src.Providers {
			clone.Providers[name] = cfg
		}
	}
	if src.Agents.Preferences != nil {
		clone.Agents.Preferences = make(map[string]config.AgentPreference, len(src.Agents.Preferences))
		for name, pref := range src.Agents.Preferences {
			clone.Agents.Preferences[name] = pref
		}
	}
	return &clone
}

func newServeJobsExecutor(baseCfg *config.Config) serveJobsExecutor {
	return func(ctx context.Context, agentName, instructions string, onEvent func(llm.Event)) error {
		jobCfg := cloneConfigForServeJob(baseCfg)

		agent, err := LoadAgent(agentName, jobCfg)
		if err != nil {
			return err
		}
		if agent == nil {
			return fmt.Errorf("agent %q not found", agentName)
		}

		if err := applyProviderOverridesWithAgent(jobCfg, jobCfg.Ask.Provider, jobCfg.Ask.Model, serveProvider, agent.Provider, agent.Model); err != nil {
			return err
		}

		settings, err := ResolveSettingsWithPlatform(jobCfg, agent, CLIFlags{
			Provider:      serveProvider,
			Tools:         serveTools,
			ReadDirs:      serveReadDirs,
			WriteDirs:     serveWriteDirs,
			ShellAllow:    serveShellAllow,
			MCP:           serveMCP,
			SystemMessage: serveSystemMessage,
			MaxTurns:      serveMaxTurns,
			Search:        serveSearch,
		}, jobCfg.Ask.Provider, jobCfg.Ask.Model, jobCfg.Ask.Instructions, jobCfg.Ask.MaxTurns, 20, agents.TemplatePlatformJobs)
		if err != nil {
			return err
		}

		modelName := activeModel(jobCfg)
		provider, err := llm.NewProvider(jobCfg)
		if err != nil {
			return err
		}

		engine, toolMgr, err := newServeEngineWithTools(jobCfg, settings, provider, serveYolo, func(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
			return WireSpawnAgentRunnerForPlatform(cfg, toolMgr, yoloMode, agents.TemplatePlatformJobs)
		})
		if err != nil {
			return err
		}

		forceExternalSearch := resolveForceExternalSearch(jobCfg, serveNativeSearch, serveNoNativeSearch)
		runtime := &serveRuntime{
			provider:            provider,
			engine:              engine,
			toolMgr:             toolMgr,
			systemPrompt:        settings.SystemPrompt,
			search:              settings.Search,
			forceExternalSearch: forceExternalSearch,
			maxTurns:            settings.MaxTurns,
			debug:               serveDebug,
			debugRaw:            debugRaw,
			defaultModel:        modelName,
			toolsSetting:        settings.Tools,
			mcpSetting:          settings.MCP,
			agentName:           agent.Name,
		}
		defer runtime.Close()

		if settings.MCP != "" {
			mcpOpts := &MCPOptions{Provider: provider, Model: modelName, YoloMode: serveYolo}
			mgr, err := enableMCPServersWithFeedback(ctx, settings.MCP, engine, io.Discard, mcpOpts)
			if err != nil {
				return err
			}
			runtime.mcpManager = mgr
		}

		llmReq := llm.Request{
			Tools:               runtime.selectTools(nil),
			ToolChoice:          llm.ToolChoice{Mode: llm.ToolChoiceAuto},
			ParallelToolCalls:   true,
			Search:              settings.Search,
			ForceExternalSearch: forceExternalSearch,
			MaxTurns:            settings.MaxTurns,
			Debug:               serveDebug,
			DebugRaw:            debugRaw,
		}

		_, err = runtime.RunWithEvents(ctx, false, false, []llm.Message{llm.UserText(instructions)}, llmReq, func(ev llm.Event) error {
			if onEvent != nil {
				onEvent(ev)
			}
			return nil
		})
		return err
	}
}
