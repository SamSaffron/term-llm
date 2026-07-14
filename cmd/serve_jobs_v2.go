package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/jobs"
	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	_ "modernc.org/sqlite"
)

type jobsV2RunnerType string

type jobsV2TriggerType string

type jobsV2RunStatus string

type serveJobsExecResult struct {
	Progressive *progressiveRunResult
}

type serveJobsExecutor func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error)

const (
	jobsV2RunnerLLM     jobsV2RunnerType = jobsV2RunnerType(jobs.RunnerLLM)
	jobsV2RunnerProgram jobsV2RunnerType = jobsV2RunnerType(jobs.RunnerProgram)

	jobsV2TriggerManual jobsV2TriggerType = jobsV2TriggerType(jobs.TriggerManual)
	jobsV2TriggerOnce   jobsV2TriggerType = jobsV2TriggerType(jobs.TriggerOnce)
	jobsV2TriggerCron   jobsV2TriggerType = jobsV2TriggerType(jobs.TriggerCron)

	jobsV2RunQueued          jobsV2RunStatus = jobsV2RunStatus(jobs.RunQueued)
	jobsV2RunClaimed         jobsV2RunStatus = jobsV2RunStatus(jobs.RunClaimed)
	jobsV2RunRunning         jobsV2RunStatus = jobsV2RunStatus(jobs.RunRunning)
	jobsV2RunSucceeded       jobsV2RunStatus = jobsV2RunStatus(jobs.RunSucceeded)
	jobsV2RunFailed          jobsV2RunStatus = jobsV2RunStatus(jobs.RunFailed)
	jobsV2RunCancelled       jobsV2RunStatus = jobsV2RunStatus(jobs.RunCancelled)
	jobsV2RunCancelRequested jobsV2RunStatus = jobsV2RunStatus(jobs.RunCancelRequested)
	jobsV2RunTimedOut        jobsV2RunStatus = jobsV2RunStatus(jobs.RunTimedOut)
	jobsV2RunSkipped         jobsV2RunStatus = jobsV2RunStatus(jobs.RunSkipped)
)

const (
	exitReasonNatural    = jobs.ExitReasonNatural
	exitReasonMaxTurns   = jobs.ExitReasonMaxTurns
	exitReasonTimeout    = jobs.ExitReasonTimeout
	exitReasonCancelled  = jobs.ExitReasonCancelled
	exitReasonException  = jobs.ExitReasonException
	exitReasonEmpty      = jobs.ExitReasonEmpty
	exitReasonWorkerLost = jobs.ExitReasonWorkerLost
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
	LastRun           *jobsV2Run        `json:"last_run,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type jobsV2JobRequest struct {
	Name              string            `json:"name"`
	Enabled           *bool             `json:"enabled,omitempty"`
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
}

func (req jobsV2JobRequest) toJob(defaultEnabled bool) jobsV2Job {
	enabled := defaultEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return jobsV2Job{
		Name:              req.Name,
		Enabled:           enabled,
		RunnerType:        req.RunnerType,
		RunnerConfig:      req.RunnerConfig,
		TriggerType:       req.TriggerType,
		TriggerConfig:     req.TriggerConfig,
		ScheduleTimezone:  req.ScheduleTimezone,
		ConcurrencyPolicy: req.ConcurrencyPolicy,
		MaxConcurrentRuns: req.MaxConcurrentRuns,
		RetryPolicy:       req.RetryPolicy,
		TimeoutSeconds:    req.TimeoutSeconds,
		MisfirePolicy:     req.MisfirePolicy,
		Labels:            req.Labels,
	}
}

func jobsV2JobToRequest(req jobsV2Job) jobsV2JobRequest {
	enabled := req.Enabled
	return jobsV2JobRequest{
		Name:              req.Name,
		Enabled:           &enabled,
		RunnerType:        req.RunnerType,
		RunnerConfig:      req.RunnerConfig,
		TriggerType:       req.TriggerType,
		TriggerConfig:     req.TriggerConfig,
		ScheduleTimezone:  req.ScheduleTimezone,
		ConcurrencyPolicy: req.ConcurrencyPolicy,
		MaxConcurrentRuns: req.MaxConcurrentRuns,
		RetryPolicy:       req.RetryPolicy,
		TimeoutSeconds:    req.TimeoutSeconds,
		MisfirePolicy:     req.MisfirePolicy,
		Labels:            req.Labels,
	}
}

type jobsV2Run struct {
	ID           string          `json:"id"`
	JobID        string          `json:"job_id"`
	Attempt      int             `json:"attempt"`
	Trigger      string          `json:"trigger"`
	ScheduledFor time.Time       `json:"scheduled_for"`
	Status       jobsV2RunStatus `json:"status"`
	WorkerID     string          `json:"worker_id,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	ExitCode     *int            `json:"exit_code,omitempty"`
	Error        string          `json:"error,omitempty"`
	Stdout       string          `json:"stdout,omitempty"`
	Stderr       string          `json:"stderr,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Response     string          `json:"response,omitempty"`
	ExitReason   string          `json:"exit_reason,omitempty"`
	Truncated    bool            `json:"truncated,omitempty"`
	TurnCount    int             `json:"turn_count,omitempty"`
	InputTokens  int             `json:"input_tokens,omitempty"`
	OutputTokens int             `json:"output_tokens,omitempty"`
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
	ExitCode     int
	Stdout       string
	Stderr       string
	Thinking     string
	Response     string
	SessionID    string
	TurnCount    int    // number of LLM turns taken
	InputTokens  int    // total input tokens consumed
	OutputTokens int    // total output tokens generated
	ExitReason   string // see exit reason constants
	Truncated    bool   // true when exit_reason is "max_turns_exceeded"
}

// progressWriter receives real-time progress updates from a running job.
// eventType is one of: "tool_start", "tool_end", "phase", "turn_complete", "response_flush", "progress_update".
// For "response_flush": message is the current accumulated response text, data is nil.
// For others: message is a human-readable summary, data is structured metadata.
type progressWriter func(eventType, message string, data any)

type jobsV2Runner interface {
	Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error)
}

type jobsV2RunDoneNotifier func(ctx context.Context, run jobsV2Run, job jobsV2Job, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) error

type jobsV2ProgramRunner struct{}

var (
	jobsV2ProgramOutputLimit int64         = jobs.ProgramOutputLimit
	jobsV2ProgramWaitDelay   time.Duration = jobs.ProgramWaitDelay
)

const (
	jobsV2SchedulerMinDelay    = 100 * time.Millisecond
	jobsV2SchedulerIdleDelay   = time.Minute
	jobsV2SchedulerErrorDelay  = 200 * time.Millisecond
	jobsV2WorkerMinDelay       = 100 * time.Millisecond
	jobsV2WorkerIdleDelay      = time.Minute
	jobsV2WorkerErrorDelay     = 200 * time.Millisecond
	jobsV2FinishRunRetryWindow = 30 * time.Second
	jobsV2FinishRunRetryDelay  = 50 * time.Millisecond
	jobsV2FinishRunMaxDelay    = time.Second
)

type jobsV2ProgramConfig = jobs.ProgramConfig

func (r *jobsV2ProgramRunner) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
	result, err := (&jobs.ProgramRunner{
		OutputLimit: jobsV2ProgramOutputLimit,
		WaitDelay:   jobsV2ProgramWaitDelay,
	}).Run(ctx, jobs.Job{
		ID:                job.ID,
		Name:              job.Name,
		Enabled:           job.Enabled,
		RunnerType:        jobs.RunnerType(job.RunnerType),
		RunnerConfig:      job.RunnerConfig,
		TriggerType:       jobs.TriggerType(job.TriggerType),
		TriggerConfig:     job.TriggerConfig,
		ScheduleTimezone:  job.ScheduleTimezone,
		ConcurrencyPolicy: job.ConcurrencyPolicy,
		MaxConcurrentRuns: job.MaxConcurrentRuns,
		RetryPolicy:       job.RetryPolicy,
		TimeoutSeconds:    job.TimeoutSeconds,
		MisfirePolicy:     job.MisfirePolicy,
		Labels:            job.Labels,
		NextRunAt:         job.NextRunAt,
		CreatedAt:         job.CreatedAt,
		UpdatedAt:         job.UpdatedAt,
	}, jobs.ProgressWriter(pw))
	return jobsV2RunResult{
		ExitCode:     result.ExitCode,
		Stdout:       result.Stdout,
		Stderr:       result.Stderr,
		Thinking:     result.Thinking,
		Response:     result.Response,
		SessionID:    result.SessionID,
		TurnCount:    result.TurnCount,
		InputTokens:  result.InputTokens,
		OutputTokens: result.OutputTokens,
		ExitReason:   result.ExitReason,
		Truncated:    result.Truncated,
	}, err
}

type jobsV2LLMRunner struct {
	exec serveJobsExecutor
}

type jobsV2NotifyOrigin struct {
	Origin         string `json:"origin,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	TelegramChatID int64  `json:"telegram_chat_id,omitempty"`
}

type jobsV2LLMConfig struct {
	AgentName      string              `json:"agent_name"`
	Instructions   string              `json:"instructions"`
	Progressive    bool                `json:"progressive,omitempty"`
	StopWhen       string              `json:"stop_when,omitempty"`
	ContinueWith   string              `json:"continue_with,omitempty"`
	PersistSession *bool               `json:"persist_session,omitempty"`
	SessionID      string              `json:"session_id,omitempty"`
	SessionName    string              `json:"session_name,omitempty"`
	NotifyWhenDone bool                `json:"notify_when_done,omitempty"`
	NotifyOrigin   *jobsV2NotifyOrigin `json:"notify_origin,omitempty"`

	// cwd is REQUIRED: it roots this run's file/shell tools at a directory so a
	// job never silently inherits the jobs server's process working directory.
	// It is NOT an os.Chdir (see resolveJobLLMSettings) — the server runs many
	// runs concurrently in one process.
	Cwd string `json:"cwd"`

	// Ask-compatible execution overrides. All optional; omitting a field preserves
	// the previous behavior (it falls back to the serve-level flag / agent / config).
	Provider        string   `json:"provider,omitempty"`          // e.g. "chatgpt" or "chatgpt:gpt-5.5-xhigh"
	Model           string   `json:"model,omitempty"`             // model override (alt to provider:model)
	ReadDir         []string `json:"read_dir,omitempty"`          // additional read roots
	WriteDir        []string `json:"write_dir,omitempty"`         // additional write roots
	Tools           string   `json:"tools,omitempty"`             // tool set override ("all" or csv)
	MaxTurns        int      `json:"max_turns,omitempty"`         // agentic turn cap
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"` // 0 = provider default
	Search          bool     `json:"search,omitempty"`
	SystemMessage   string   `json:"system_message,omitempty"`
	Skills          string   `json:"skills,omitempty"`
}

func (c jobsV2LLMConfig) sessionPersistenceEnabled() bool {
	if c.PersistSession == nil {
		return true
	}
	return *c.PersistSession
}

func (c jobsV2LLMConfig) effectiveSessionID() string {
	if id := strings.TrimSpace(c.SessionID); id != "" {
		return id
	}
	return session.NewID()
}

func validateJobsV2RunnerConfig(runnerType jobsV2RunnerType, raw json.RawMessage) error {
	switch runnerType {
	case jobsV2RunnerLLM:
		var cfg jobsV2LLMConfig
		if err := json.Unmarshal([]byte(stringOrEmptyRaw(raw, "{}")), &cfg); err != nil {
			return fmt.Errorf("invalid llm runner config: %w", err)
		}
		if strings.TrimSpace(cfg.AgentName) == "" {
			return fmt.Errorf("llm runner_config.agent_name is required")
		}
		if strings.TrimSpace(cfg.Instructions) == "" {
			return fmt.Errorf("llm runner_config.instructions is required")
		}
		if strings.TrimSpace(cfg.Cwd) == "" {
			return fmt.Errorf("llm runner_config.cwd is required")
		}
	case jobsV2RunnerProgram:
		// Program runner config is intentionally still validated at execution time:
		// its optional cwd remains an explicit escape hatch, unlike llm cwd.
	case "":
		return fmt.Errorf("runner_type is required")
	default:
		return fmt.Errorf("runner_type must be one of: llm, program")
	}
	return nil
}

func (r *jobsV2LLMRunner) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
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
	// cwd is mandatory: an llm job must declare where its file/shell tools are
	// rooted rather than silently inheriting the jobs server's working directory.
	if strings.TrimSpace(cfg.Cwd) == "" {
		return jobsV2RunResult{}, fmt.Errorf("llm runner cwd is required")
	}
	cfg.SessionID = cfg.effectiveSessionID()
	if strings.TrimSpace(cfg.SessionName) == "" {
		cfg.SessionName = jobsV2SessionName(job)
	}
	progressiveOpts := askProgressiveOptions{
		Enabled:      cfg.Progressive,
		Timeout:      time.Duration(job.TimeoutSeconds) * time.Second,
		StopWhen:     progressiveStopWhen(strings.TrimSpace(cfg.StopWhen)),
		ContinueWith: cfg.ContinueWith,
	}
	if err := validateAskProgressiveOptions(&progressiveOpts); err != nil {
		return jobsV2RunResult{}, err
	}
	// Write resolved defaults back so the exec closure can use cfg directly.
	cfg.StopWhen = string(progressiveOpts.StopWhen)
	cfg.ContinueWith = progressiveOpts.ContinueWith
	res := jobsV2RunResult{SessionID: cfg.SessionID}
	// Delegated hub jobs carry their delegation id as a hub-written label;
	// surface it on the context so hub_delegate chains depth/loop tracking
	// without trusting the model to pass parent_delegation_id.
	if delegationID := hubDelegationIDFromJobLabels(job.Labels); delegationID != "" {
		ctx = tools.WithHubDelegationID(ctx, delegationID)
	}
	var thinkingBuilder strings.Builder
	var thinkingItemID string
	var responseBuilder strings.Builder
	progressTracker := newProgressTracker()
	execResult, err := r.exec(ctx, cfg, func(ev llm.Event) {
		switch ev.Type {
		case llm.EventReasoningDelta:
			internalreasoning.AppendStreamItemText(&thinkingBuilder, &thinkingItemID, ev.Text, ev.ReasoningItemID)
		case llm.EventTextDelta:
			if !cfg.Progressive {
				responseBuilder.WriteString(ev.Text)
			}
		case llm.EventToolCall:
			if ev.Tool != nil && cfg.Progressive && isProgressToolName(ev.Tool.Name) {
				progressTracker.observeToolCall(strings.TrimSpace(ev.Tool.ID), strings.TrimSpace(ev.Tool.Name), ev.Tool.Arguments)
			}
		case llm.EventUsage:
			if ev.Use != nil {
				res.InputTokens += ev.Use.InputTokens
				res.OutputTokens += ev.Use.OutputTokens
				res.TurnCount++
				// Flush accumulated response to DB after each turn so callers can see partial output.
				if pw != nil {
					if !cfg.Progressive {
						pw("response_flush", responseBuilder.String(), nil)
					}
					pw("turn_complete", fmt.Sprintf("turn %d complete (%d in, %d out tokens)", res.TurnCount, ev.Use.InputTokens, ev.Use.OutputTokens), map[string]any{
						"turn":          res.TurnCount,
						"input_tokens":  ev.Use.InputTokens,
						"output_tokens": ev.Use.OutputTokens,
					})
				}
			}
		case llm.EventToolExecStart:
			if cfg.Progressive && isProgressToolName(ev.ToolName) {
				return
			}
			if pw != nil {
				info := ev.ToolInfo
				if info == "" {
					info = ev.ToolName
				}
				pw("tool_start", fmt.Sprintf("→ %s: %s", ev.ToolName, info), map[string]any{
					"tool": ev.ToolName,
					"info": info,
					"id":   ev.ToolCallID,
				})
			}
		case llm.EventToolExecEnd:
			if cfg.Progressive && isProgressToolName(ev.ToolName) {
				if commit := progressTracker.commitToolCall(strings.TrimSpace(ev.ToolCallID), strings.TrimSpace(ev.ToolName), ev.ToolSuccess); commit != nil && pw != nil {
					message := commit.Message
					if message == "" {
						message = "progress updated"
					}
					envelope := buildProgressiveRunResult(cfg.SessionID, "", commit.Final, commit, responseBuilder.String())
					pw("progress_update", message, envelope)
				}
				return
			}
			if pw != nil {
				status := "ok"
				if !ev.ToolSuccess {
					status = "failed"
				}
				pw("tool_end", fmt.Sprintf("← %s (%s)", ev.ToolName, status), map[string]any{
					"tool":    ev.ToolName,
					"success": ev.ToolSuccess,
					"id":      ev.ToolCallID,
				})
			}
		case llm.EventPhase:
			if pw != nil && ev.Text != "" {
				pw("phase", ev.Text, map[string]any{"text": ev.Text})
			}
		}
	})
	res.Thinking = thinkingBuilder.String()
	if !cfg.Progressive {
		res.Response = responseBuilder.String()
	}
	if execResult.Progressive != nil {
		if strings.TrimSpace(execResult.Progressive.SessionID) == "" {
			execResult.Progressive.SessionID = cfg.SessionID
		}
		res.ExitReason = execResult.Progressive.ExitReason
		res.Response = progressiveOutputText(*execResult.Progressive)
	}
	return res, err
}

func classifyRunError(err error, result jobsV2RunResult) (exitReason string, truncated bool) {
	return jobs.ClassifyRunError(err, jobs.RunResult{
		ExitCode:     result.ExitCode,
		Stdout:       result.Stdout,
		Stderr:       result.Stderr,
		Thinking:     result.Thinking,
		Response:     result.Response,
		SessionID:    result.SessionID,
		TurnCount:    result.TurnCount,
		InputTokens:  result.InputTokens,
		OutputTokens: result.OutputTokens,
		ExitReason:   result.ExitReason,
		Truncated:    result.Truncated,
	})
}

type jobsV2Manager struct {
	db         *sql.DB
	workers    int
	workerID   string
	runners    map[jobsV2RunnerType]jobsV2Runner
	notifyDone jobsV2RunDoneNotifier
	// Idle timers are only fallbacks; job/run mutations wake the loops immediately.
	schedulerIdleDelay time.Duration
	workerIdleDelay    time.Duration
	// Retention settings for protecting disk usage.
	retentionRunDays     int
	retentionEventDays   int
	retentionMaxRunsJob  int
	cleanupInterval      time.Duration
	lastCleanupCompleted time.Time
	cleanupRunning       bool

	enqueueMu     sync.Mutex
	mu            sync.Mutex
	closed        bool
	done          chan struct{}
	schedulerWake chan struct{}
	workerWake    chan struct{}
	wg            sync.WaitGroup
	cancels       map[string]context.CancelFunc
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
	session_id TEXT,
	started_at TIMESTAMP,
	finished_at TIMESTAMP,
	exit_code INTEGER,
	error TEXT,
	stdout TEXT,
	stderr TEXT,
	thinking TEXT,
	response TEXT,
	exit_reason TEXT,
	truncated INTEGER NOT NULL DEFAULT 0,
	turn_count INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_jobs_v2_next_run_at ON jobs_v2(next_run_at);
CREATE INDEX IF NOT EXISTS idx_job_runs_v2_status ON job_runs_v2(status, scheduled_for);

CREATE TABLE IF NOT EXISTS job_run_events_v2 (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL REFERENCES job_runs_v2(id) ON DELETE CASCADE,
	event_type TEXT NOT NULL,
	message TEXT,
	data TEXT,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_job_run_events_v2_run_id_id ON job_run_events_v2(run_id, id);
DROP INDEX IF EXISTS idx_job_run_events_v2_run_id;
`

func newJobsV2Manager(dbPath string, workers int, llmExec serveJobsExecutor) (*jobsV2Manager, error) {
	return newJobsV2ManagerWithNotifier(dbPath, workers, llmExec, nil)
}

func newJobsV2ManagerWithNotifier(dbPath string, workers int, llmExec serveJobsExecutor, notifyDone jobsV2RunDoneNotifier) (*jobsV2Manager, error) {
	if workers < 0 {
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
	// The jobs scheduler, workers, and admin paths all share one SQLite file.
	// Keep the database/sql pool to one connection so concurrent writes are
	// serialized before they reach SQLite's single-writer lock.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := execJobsV2Schema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("init jobs schema: %w", err)
	}

	migrations := []string{
		`ALTER TABLE job_runs_v2 ADD COLUMN exit_reason TEXT`,
		`ALTER TABLE job_runs_v2 ADD COLUMN truncated INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE job_runs_v2 ADD COLUMN turn_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE job_runs_v2 ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE job_runs_v2 ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE job_runs_v2 ADD COLUMN session_id TEXT`,
	}
	for _, migration := range migrations {
		_, _ = db.Exec(migration)
	}
	// Replace the narrow job_id/created_at index with covering indexes for the
	// run-summary API. Large stdout/stderr/thinking/response columns sit before
	// summary metadata in the table record; without covering indexes, SQLite
	// walks overflow payload pages just to list recent runs. The job-scoped index
	// serves /v2/runs?job_id=..., while the global index avoids a temp sort for
	// all-job summaries used by the jobs CLI/status views.
	for _, index := range []struct {
		name string
		sql  string
	}{
		{name: jobsV2RunSummaryIndexName, sql: jobsV2RunSummaryIndexSQL},
		{name: jobsV2RunGlobalSummaryIndexName, sql: jobsV2RunGlobalSummaryIndexSQL},
	} {
		if _, err := db.Exec(index.sql); err != nil {
			db.Close()
			return nil, fmt.Errorf("init jobs run summary index %s: %w", index.name, err)
		}
	}
	_, _ = db.Exec(`DROP INDEX IF EXISTS idx_job_runs_v2_job_id`)

	mgr := &jobsV2Manager{
		db:                 db,
		workers:            workers,
		workerID:           "worker_" + randomSuffix(),
		notifyDone:         notifyDone,
		schedulerIdleDelay: jobsV2SchedulerIdleDelay,
		workerIdleDelay:    jobsV2WorkerIdleDelay,
		// Conservative defaults: enough history for debugging without unbounded growth.
		retentionRunDays:    30,
		retentionEventDays:  30,
		retentionMaxRunsJob: 1000,
		cleanupInterval:     time.Hour,
		runners: map[jobsV2RunnerType]jobsV2Runner{
			jobsV2RunnerProgram: &jobsV2ProgramRunner{},
			jobsV2RunnerLLM:     &jobsV2LLMRunner{exec: llmExec},
		},
		done:          make(chan struct{}),
		schedulerWake: make(chan struct{}, 1),
		workerWake:    make(chan struct{}, max(1, workers)),
		cancels:       make(map[string]context.CancelFunc),
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
	rows, err := m.db.Query(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at FROM job_runs_v2 WHERE status IN (?, ?) ORDER BY created_at ASC`, jobsV2RunClaimed, jobsV2RunRunning)
	if err != nil {
		return fmt.Errorf("load interrupted runs: %w", err)
	}

	var interrupted []jobsV2Run
	for rows.Next() {
		run, err := scanRunV2(rows)
		if err != nil {
			return fmt.Errorf("scan interrupted run: %w", err)
		}
		interrupted = append(interrupted, run)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan interrupted runs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close interrupted runs query: %w", err)
	}

	if _, err := m.db.Exec(`
		UPDATE job_runs_v2
		SET status = ?,
			finished_at = CURRENT_TIMESTAMP,
			exit_reason = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE status = ?`,
		jobsV2RunCancelled,
		exitReasonCancelled,
		jobsV2RunCancelRequested); err != nil {
		return fmt.Errorf("recover cancel requested runs: %w", err)
	}

	for _, run := range interrupted {
		exitCode := 0
		if run.ExitCode != nil {
			exitCode = *run.ExitCode
		}
		result := jobsV2RunResult{
			ExitCode:     exitCode,
			Stdout:       run.Stdout,
			Stderr:       run.Stderr,
			Thinking:     run.Thinking,
			Response:     run.Response,
			SessionID:    run.SessionID,
			ExitReason:   exitReasonWorkerLost,
			Truncated:    run.Truncated,
			TurnCount:    run.TurnCount,
			InputTokens:  run.InputTokens,
			OutputTokens: run.OutputTokens,
		}
		if err := m.finishRun(run.ID, jobsV2RunFailed, result, errors.New("worker lost before run could finish"), run.Attempt); err != nil {
			return fmt.Errorf("recover interrupted run %s: %w", run.ID, err)
		}
	}

	return nil
}

func (m *jobsV2Manager) Close() error {
	return m.CloseContext(context.Background())
}

func (m *jobsV2Manager) CloseContext(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	if m.done != nil {
		close(m.done)
	}
	cancels := make([]context.CancelFunc, 0, len(m.cancels))
	for _, cancel := range m.cancels {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	waitDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		return m.db.Close()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *jobsV2Manager) schedulerLoop() {
	defer m.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-m.doneChan():
			return
		case <-timer.C:
		case <-m.schedulerWake:
			stopTimer(timer)
		}
		if m.isClosed() {
			return
		}

		now := time.Now().UTC()
		if err := m.scheduleDueRuns(now); err != nil {
			resetTimer(timer, jobsV2SchedulerErrorDelay)
			continue
		}
		if err := m.maybeRunCleanup(now); err != nil {
			resetTimer(timer, jobsV2SchedulerErrorDelay)
			continue
		}
		delay, err := m.nextSchedulerDelayWithError(time.Now().UTC())
		if err != nil {
			delay = jobsV2SchedulerErrorDelay
		}
		resetTimer(timer, delay)
	}
}

func (m *jobsV2Manager) doneChan() <-chan struct{} {
	if m.done == nil {
		return nil
	}
	return m.done
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	stopTimer(timer)
	timer.Reset(delay)
}

func clampJobsV2Delay(delay, minDelay, idleDelay time.Duration) time.Duration {
	if idleDelay <= 0 {
		idleDelay = time.Minute
	}
	if delay < 0 {
		delay = 0
	}
	if delay > idleDelay {
		delay = idleDelay
	}
	if minDelay > 0 && delay < minDelay {
		delay = minDelay
	}
	return delay
}

func (m *jobsV2Manager) nextSchedulerDelay(now time.Time) time.Duration {
	delay, err := m.nextSchedulerDelayWithError(now)
	if err != nil {
		return jobsV2SchedulerErrorDelay
	}
	return delay
}

func (m *jobsV2Manager) nextSchedulerDelayWithError(now time.Time) (time.Duration, error) {
	idleDelay := m.schedulerIdleDelay
	if idleDelay <= 0 {
		idleDelay = jobsV2SchedulerIdleDelay
	}
	delay := idleDelay

	var nextRun sql.NullTime
	err := m.db.QueryRow(`SELECT next_run_at FROM jobs_v2 WHERE enabled = 1 AND next_run_at IS NOT NULL ORDER BY next_run_at ASC LIMIT 1`).Scan(&nextRun)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil && nextRun.Valid {
		delay = minDuration(delay, nextRun.Time.UTC().Sub(now.UTC()))
	}

	if m.cleanupInterval > 0 {
		m.mu.Lock()
		lastCleanup := m.lastCleanupCompleted
		m.mu.Unlock()
		cleanupDelay := time.Duration(0)
		if !lastCleanup.IsZero() {
			cleanupDelay = lastCleanup.Add(m.cleanupInterval).Sub(now.UTC())
		}
		delay = minDuration(delay, cleanupDelay)
	}

	return clampJobsV2Delay(delay, jobsV2SchedulerMinDelay, idleDelay), nil
}

func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}

func (m *jobsV2Manager) maybeRunCleanup(now time.Time) error {
	if m.cleanupInterval <= 0 {
		return nil
	}
	cleanupStartedAt := now.UTC()
	m.mu.Lock()
	if !m.lastCleanupCompleted.IsZero() && cleanupStartedAt.Sub(m.lastCleanupCompleted) < m.cleanupInterval {
		m.mu.Unlock()
		return nil
	}
	if m.cleanupRunning {
		m.mu.Unlock()
		return nil
	}
	m.cleanupRunning = true
	m.mu.Unlock()

	err := m.pruneOldData(cleanupStartedAt)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupRunning = false
	if err != nil {
		return err
	}
	m.lastCleanupCompleted = cleanupStartedAt
	return nil
}

func (m *jobsV2Manager) pruneOldData(now time.Time) error {
	terminalStatuses := []any{jobsV2RunSucceeded, jobsV2RunFailed, jobsV2RunCancelled, jobsV2RunTimedOut, jobsV2RunSkipped}

	if m.retentionRunDays > 0 {
		cutoff := now.Add(-time.Duration(m.retentionRunDays) * 24 * time.Hour)
		_, err := m.db.Exec(`
			DELETE FROM jobs_v2
			WHERE (
				labels = ?
				OR CASE
					WHEN json_valid(labels) THEN json_extract(labels, ?) = ?
					ELSE 0
				END
			  )
			  AND NOT EXISTS (
				  SELECT 1
				  FROM job_runs_v2 active_runs
				  WHERE active_runs.job_id = jobs_v2.id
				    AND active_runs.status NOT IN (?, ?, ?, ?, ?)
			  )
			  AND COALESCE(
				  (
					  SELECT MAX(COALESCE(all_runs.finished_at, all_runs.created_at))
					  FROM job_runs_v2 all_runs
					  WHERE all_runs.job_id = jobs_v2.id
				  ),
				  jobs_v2.created_at
			  ) < ?`,
			tools.QueueAgentEphemeralJobLabelsJSON,
			"$."+tools.QueueAgentEphemeralJobLabelKey,
			tools.QueueAgentEphemeralJobLabelValue,
			terminalStatuses[0], terminalStatuses[1], terminalStatuses[2], terminalStatuses[3], terminalStatuses[4], cutoff)
		if err != nil {
			return err
		}
		_, err = m.db.Exec(`
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
		select {
		case <-m.doneChan():
			return
		default:
		}

		run, ok, err := m.claimNextRun()
		if err != nil {
			if !m.waitForWorkerWake(jobsV2WorkerErrorDelay) {
				return
			}
			continue
		}
		if ok {
			m.executeRun(run)
			continue
		}
		delay, err := m.nextWorkerDelayWithError(time.Now().UTC())
		if err != nil {
			delay = jobsV2WorkerErrorDelay
		}
		if !m.waitForWorkerWake(delay) {
			return
		}
	}
}

func (m *jobsV2Manager) waitForWorkerWake(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer stopTimer(timer)
	select {
	case <-m.doneChan():
		return false
	case <-m.workerWake:
		return true
	case <-timer.C:
		return true
	}
}

func (m *jobsV2Manager) nextWorkerDelay(now time.Time) time.Duration {
	delay, err := m.nextWorkerDelayWithError(now)
	if err != nil {
		return jobsV2WorkerErrorDelay
	}
	return delay
}

func (m *jobsV2Manager) nextWorkerDelayWithError(now time.Time) (time.Duration, error) {
	idleDelay := m.workerIdleDelay
	if idleDelay <= 0 {
		idleDelay = jobsV2WorkerIdleDelay
	}
	delay := idleDelay

	var nextRun sql.NullTime
	err := m.db.QueryRow(`SELECT scheduled_for FROM job_runs_v2 WHERE status = ? ORDER BY scheduled_for ASC LIMIT 1`, jobsV2RunQueued).Scan(&nextRun)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil && nextRun.Valid {
		delay = minDuration(delay, nextRun.Time.UTC().Sub(now.UTC()))
	}

	return clampJobsV2Delay(delay, jobsV2WorkerMinDelay, idleDelay), nil
}

func (m *jobsV2Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *jobsV2Manager) notifyScheduler() {
	select {
	case m.schedulerWake <- struct{}{}:
	default:
	}
}

func (m *jobsV2Manager) notifyWorkers(n int) {
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		select {
		case m.workerWake <- struct{}{}:
		default:
			return
		}
	}
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

func jobsV2ConcurrencyLimit(job jobsV2Job) int {
	if job.ConcurrencyPolicy == "forbid" {
		return 1
	}
	if job.MaxConcurrentRuns > 0 {
		return job.MaxConcurrentRuns
	}
	return 1
}

func (m *jobsV2Manager) countActiveRuns(jobID string) (int, error) {
	var active int
	err := m.db.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE job_id = ? AND status IN (?, ?, ?)`, jobID, jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning).Scan(&active)
	if err != nil {
		return 0, err
	}
	return active, nil
}

func (m *jobsV2Manager) enqueueRunWithConcurrencyLimit(job jobsV2Job, trigger string, scheduledFor time.Time, onLimit func() error, afterInsert func(string) error) (string, int, bool, error) {
	m.enqueueMu.Lock()
	defer m.enqueueMu.Unlock()

	active, err := m.countActiveRuns(job.ID)
	if err != nil {
		return "", 0, false, err
	}
	limit := jobsV2ConcurrencyLimit(job)
	if active >= limit {
		if onLimit != nil {
			if err := onLimit(); err != nil {
				return "", 0, false, err
			}
		}
		return "", active, false, nil
	}

	runID := "run_" + randomSuffix()
	_, err = m.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, runID, job.ID, trigger, scheduledFor.UTC(), jobsV2RunQueued)
	if err != nil {
		return "", 0, false, err
	}
	if afterInsert != nil {
		if err := afterInsert(runID); err != nil {
			return "", 0, false, err
		}
	}
	return runID, active + 1, true, nil
}

func (m *jobsV2Manager) scheduleOne(job jobsV2Job, now time.Time) error {
	next, err := computeNextRunAt(job, now)
	if err != nil {
		return err
	}

	runID, _, queued, err := m.enqueueRunWithConcurrencyLimit(job, "schedule", now, func() error {
		_, err := m.db.Exec(`UPDATE jobs_v2 SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, job.ID)
		return err
	}, func(string) error {
		if job.TriggerType == jobsV2TriggerOnce {
			_, err := m.db.Exec(`UPDATE jobs_v2 SET enabled = 0, next_run_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, job.ID)
			return err
		}
		_, err := m.db.Exec(`UPDATE jobs_v2 SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, job.ID)
		return err
	})
	if err != nil {
		return err
	}
	if queued {
		_ = m.addRunEvent(runID, "queued", "scheduled run queued", map[string]any{"trigger": "schedule", "attempt": 1})
		m.notifyWorkers(1)
	}
	return nil
}

func (m *jobsV2Manager) claimNextRun() (jobsV2Run, bool, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return jobsV2Run{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	row := tx.QueryRow(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at FROM job_runs_v2 WHERE status = ? AND scheduled_for <= ? ORDER BY scheduled_for ASC LIMIT 1`, jobsV2RunQueued, now)
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
		m.finishRunWithRetry(run.ID, jobsV2RunFailed, jobsV2RunResult{}, fmt.Errorf("load job: %w", err), run.Attempt)
		return
	}

	runner, ok := m.runners[job.RunnerType]
	if !ok {
		m.finishRunWithRetry(run.ID, jobsV2RunFailed, jobsV2RunResult{}, fmt.Errorf("unknown runner type: %s", job.RunnerType), run.Attempt)
		return
	}

	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	m.mu.Lock()
	closed := m.closed
	m.cancels[run.ID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, run.ID)
		m.mu.Unlock()
	}()
	if closed {
		return
	}

	started := time.Now().UTC()
	res, err := m.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`, jobsV2RunRunning, started, run.ID, jobsV2RunClaimed)
	if err != nil {
		m.finishRunWithRetry(run.ID, jobsV2RunFailed, jobsV2RunResult{}, fmt.Errorf("mark run running: %w", err), run.Attempt)
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return
	}
	_ = m.addRunEvent(run.ID, "running", "run started", map[string]any{"worker_id": m.workerID})

	// Build a progress writer that writes events and flushes partial response to DB.
	pw := func(eventType, message string, data any) {
		switch eventType {
		case "response_flush":
			// Update response column so GET /v2/runs/{id} shows partial output.
			_, _ = m.db.Exec(`UPDATE job_runs_v2 SET response = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, message, run.ID)
		case "progress_update":
			if data != nil {
				if payload, err := json.Marshal(data); err == nil {
					_, _ = m.db.Exec(`UPDATE job_runs_v2 SET response = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, string(payload), run.ID)
				}
			}
			_ = m.addRunEvent(run.ID, eventType, message, data)
		default:
			_ = m.addRunEvent(run.ID, eventType, message, data)
		}
	}
	result, runErr := runner.Run(ctx, job, pw)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		m.finishRunWithRetry(run.ID, jobsV2RunTimedOut, result, context.DeadlineExceeded, run.Attempt)
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		m.finishRunWithRetry(run.ID, jobsV2RunCancelled, result, context.Canceled, run.Attempt)
		return
	}
	if result.ExitReason == exitReasonTimeout {
		m.finishRunWithRetry(run.ID, jobsV2RunTimedOut, result, context.DeadlineExceeded, run.Attempt)
		return
	}
	if result.ExitReason == exitReasonCancelled {
		m.finishRunWithRetry(run.ID, jobsV2RunCancelled, result, context.Canceled, run.Attempt)
		return
	}
	if runErr != nil {
		m.finishRunWithRetry(run.ID, jobsV2RunFailed, result, runErr, run.Attempt)
		return
	}
	m.finishRunWithRetry(run.ID, jobsV2RunSucceeded, result, nil, run.Attempt)
}

func (m *jobsV2Manager) finishRunWithRetry(runID string, status jobsV2RunStatus, result jobsV2RunResult, runErr error, policyAttempt int) {
	deadline := time.Now().Add(jobsV2FinishRunRetryWindow)
	delay := jobsV2FinishRunRetryDelay
	attempts := 0
	var lastErr error
	for {
		attempts++
		if err := m.finishRun(runID, status, result, runErr, policyAttempt); err != nil {
			lastErr = err
		} else {
			return
		}

		if m.isClosed() {
			log.Printf("jobs v2: failed to finish run %q as %s after %d attempts during shutdown; restart recovery may be needed: %v", runID, status, attempts, lastErr)
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-time.After(wait):
		case <-m.done:
			log.Printf("jobs v2: failed to finish run %q as %s after %d attempts during shutdown; restart recovery may be needed: %v", runID, status, attempts, lastErr)
			return
		}
		if delay < jobsV2FinishRunMaxDelay {
			delay *= 2
			if delay > jobsV2FinishRunMaxDelay {
				delay = jobsV2FinishRunMaxDelay
			}
		}
	}
	log.Printf("jobs v2: failed to finish run %q as %s for %s after %d attempts; run may remain active until restart recovery: %v", runID, status, jobsV2FinishRunRetryWindow, attempts, lastErr)
}

func (m *jobsV2Manager) finishRun(runID string, status jobsV2RunStatus, result jobsV2RunResult, runErr error, attempt int) error {
	now := time.Now().UTC()
	exitReason, truncated := classifyRunError(runErr, result)
	var errText string
	if runErr != nil {
		errText = runErr.Error()
	}
	cancelledErrText := context.Canceled.Error()
	res, err := m.db.Exec(`UPDATE job_runs_v2 SET status = CASE WHEN status = ? THEN ? ELSE ? END, finished_at = ?, exit_code = ?, error = CASE WHEN status = ? THEN ? ELSE ? END, stdout = ?, stderr = ?, thinking = ?, response = ?, session_id = ?, exit_reason = CASE WHEN status = ? THEN ? ELSE ? END, truncated = ?, turn_count = ?, input_tokens = ?, output_tokens = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status IN (?, ?, ?, ?)`,
		jobsV2RunCancelRequested, jobsV2RunCancelled, status,
		now, result.ExitCode,
		jobsV2RunCancelRequested, cancelledErrText, errText,
		result.Stdout, result.Stderr, result.Thinking, result.Response, result.SessionID,
		jobsV2RunCancelRequested, exitReasonCancelled, exitReason,
		boolToInt(truncated), result.TurnCount, result.InputTokens, result.OutputTokens,
		runID, jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning, jobsV2RunCancelRequested)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil
	}

	run, err := m.GetRun(runID)
	if err != nil {
		return err
	}
	status = run.Status
	exitReason = run.ExitReason
	truncated = run.Truncated
	errText = run.Error

	_ = m.addRunEvent(runID, string(status), "run finished", map[string]any{
		"status":        status,
		"attempt":       attempt,
		"session_id":    result.SessionID,
		"exit_code":     result.ExitCode,
		"error":         errText,
		"exit_reason":   exitReason,
		"truncated":     truncated,
		"turn_count":    result.TurnCount,
		"input_tokens":  result.InputTokens,
		"output_tokens": result.OutputTokens,
	})
	m.enqueueRunDoneNotification(run, status, result, exitReason, truncated, errText)

	if status == jobsV2RunFailed || status == jobsV2RunTimedOut {
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
			m.notifyWorkers(1)
		}
	}

	return nil
}

func jobsV2NotifyTerminalStatus(status jobsV2RunStatus) bool {
	switch status {
	case jobsV2RunSucceeded, jobsV2RunFailed, jobsV2RunCancelled, jobsV2RunTimedOut:
		return true
	default:
		return false
	}
}

func (m *jobsV2Manager) enqueueRunDoneNotification(run jobsV2Run, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) {
	if m == nil || m.notifyDone == nil || !jobsV2NotifyTerminalStatus(status) {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.notifyRunDone(run, status, result, exitReason, truncated, errText)
	}()
}

func (m *jobsV2Manager) notifyRunDone(run jobsV2Run, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) {
	if m == nil || m.notifyDone == nil || !jobsV2NotifyTerminalStatus(status) {
		return
	}
	job, err := m.GetJob(run.JobID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.notifyDone(ctx, run, job, status, result, exitReason, truncated, errText); err != nil {
		_ = m.addRunEvent(run.ID, "notify_failed", "completion notification failed", map[string]any{"error": err.Error()})
	}
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

func (m *jobsV2Manager) ListRunEvents(runID string, sinceID int64, limit, offset int) ([]jobsV2RunEvent, int, error) {
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

	var where string
	args := make([]any, 0, 4)
	args = append(args, runID)
	if sinceID > 0 {
		where = "WHERE run_id = ? AND id > ?"
		args = append(args, sinceID)
	} else {
		where = "WHERE run_id = ?"
	}

	var total int
	countQuery := fmt.Sprintf(`SELECT COUNT(1) FROM job_run_events_v2 %s`, where)
	if err := m.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rowsArgs := append(append([]any{}, args...), limit, offset)
	// Event IDs are AUTOINCREMENT and match insertion order. Ordering by id lets
	// since_id polling seek into idx_job_run_events_v2_run_id_id instead of
	// walking the full per-run created_at timeline on every incremental poll.
	query := fmt.Sprintf(`SELECT id, run_id, event_type, message, data, created_at FROM job_run_events_v2 %s ORDER BY id ASC LIMIT ? OFFSET ?`, where)
	rows, err := m.db.Query(query, rowsArgs...)
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
	if err := validateJobsV2RunnerConfig(req.RunnerType, req.RunnerConfig); err != nil {
		return jobsV2Job{}, err
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

	cfg, err := parseTriggerConfig(req.TriggerType, req.TriggerConfig, req.ScheduleTimezone)
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
	created, err := m.GetJob(id)
	if err != nil {
		return jobsV2Job{}, err
	}
	m.notifyScheduler()
	return created, nil
}

func (m *jobsV2Manager) GetJob(id string) (jobsV2Job, error) {
	row := m.db.QueryRow(`SELECT id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at FROM jobs_v2 WHERE id = ?`, id)
	job, err := scanJobV2(row)
	if err != nil {
		return jobsV2Job{}, err
	}
	if err := m.attachLastRuns([]*jobsV2Job{&job}); err != nil {
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
	jobs := make([]jobsV2Job, 0)
	for rows.Next() {
		job, err := scanJobV2(rows)
		if err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	jobPtrs := make([]*jobsV2Job, 0, len(jobs))
	for i := range jobs {
		jobPtrs = append(jobPtrs, &jobs[i])
	}
	if err := m.attachLastRuns(jobPtrs); err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

func (m *jobsV2Manager) attachLastRuns(jobs []*jobsV2Job) error {
	ids := make([]string, 0, len(jobs))
	byID := make(map[string]*jobsV2Job, len(jobs))
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.ID) == "" {
			continue
		}
		ids = append(ids, job.ID)
		byID[job.ID] = job
	}
	if len(ids) == 0 {
		return nil
	}

	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `SELECT ` + jobsV2RunSummaryColumns + ` FROM (
		SELECT ` + jobsV2RunSummaryColumns + `,
			ROW_NUMBER() OVER (PARTITION BY job_id ORDER BY created_at DESC, id DESC) AS rn
		FROM job_runs_v2
		WHERE job_id IN (` + strings.Join(placeholders, ",") + `)
	) WHERE rn = 1`
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		run, err := scanRunSummaryV2(rows)
		if err != nil {
			return err
		}
		if job := byID[run.JobID]; job != nil {
			r := run
			job.LastRun = &r
		}
	}
	return rows.Err()
}

func (m *jobsV2Manager) UpdateJob(id string, req jobsV2Job) (jobsV2Job, error) {
	return m.UpdateJobPatch(id, jobsV2JobToRequest(req))
}

func (m *jobsV2Manager) UpdateJobPatch(id string, req jobsV2JobRequest) (jobsV2Job, error) {
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
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}

	cfg, err := parseTriggerConfig(current.TriggerType, current.TriggerConfig, current.ScheduleTimezone)
	if err != nil {
		return jobsV2Job{}, err
	}
	if err := validateJobsV2RunnerConfig(current.RunnerType, current.RunnerConfig); err != nil {
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
	updated, err := m.GetJob(id)
	if err != nil {
		return jobsV2Job{}, err
	}
	m.notifyScheduler()
	return updated, nil
}

func (m *jobsV2Manager) DeleteJob(id string, cancelActive bool) error {
	if cancelActive {
		runs, _, err := m.ListRunSummaries(id, 200, 0)
		if err == nil {
			for _, run := range runs {
				if run.Status == jobsV2RunRunning || run.Status == jobsV2RunClaimed || run.Status == jobsV2RunQueued {
					_, _ = m.CancelRun(run.ID)
				}
			}
		}
	}
	_, err := m.db.Exec(`DELETE FROM jobs_v2 WHERE id = ?`, id)
	if err == nil {
		m.notifyScheduler()
	}
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

	runID, active, queued, err := m.enqueueRunWithConcurrencyLimit(job, "manual", time.Now().UTC(), nil, nil)
	if err != nil {
		return jobsV2Run{}, err
	}
	if !queued {
		limit := jobsV2ConcurrencyLimit(job)
		if limit == 1 {
			return jobsV2Run{}, fmt.Errorf("job already has an active run")
		}
		return jobsV2Run{}, fmt.Errorf("job already has %d active runs (max %d)", active, limit)
	}

	_ = m.addRunEvent(runID, "queued", "manual run queued", map[string]any{"trigger": "manual", "attempt": 1})
	m.notifyWorkers(1)
	return m.GetRun(runID)
}

func (m *jobsV2Manager) GetRun(id string) (jobsV2Run, error) {
	row := m.db.QueryRow(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at FROM job_runs_v2 WHERE id = ?`, id)
	return scanRunV2(row)
}

func (m *jobsV2Manager) ListRuns(jobID string, limit, offset int) ([]jobsV2Run, int, error) {
	return m.listRuns(jobID, limit, offset, true)
}

func (m *jobsV2Manager) ListRunSummaries(jobID string, limit, offset int) ([]jobsV2Run, int, error) {
	return m.listRuns(jobID, limit, offset, false)
}

const jobsV2RunSummaryIndexName = "idx_job_runs_v2_summary_by_job_created"

const jobsV2RunSummaryIndexSQL = "CREATE INDEX IF NOT EXISTS " + jobsV2RunSummaryIndexName + " ON job_runs_v2(job_id, created_at DESC, id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, updated_at)"

const jobsV2RunGlobalSummaryIndexName = "idx_job_runs_v2_summary_created"

const jobsV2RunGlobalSummaryIndexSQL = "CREATE INDEX IF NOT EXISTS " + jobsV2RunGlobalSummaryIndexName + " ON job_runs_v2(created_at DESC, id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, updated_at)"

const jobsV2RunFullColumns = "id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at"

const jobsV2RunSummaryColumns = "id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at"

func (m *jobsV2Manager) listRuns(jobID string, limit, offset int, includeOutput bool) ([]jobsV2Run, int, error) {
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
	columns := jobsV2RunSummaryColumns
	if includeOutput {
		columns = jobsV2RunFullColumns
	}
	query := "SELECT " + columns + " FROM job_runs_v2" + where + " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	runs := make([]jobsV2Run, 0, limit)
	for rows.Next() {
		var run jobsV2Run
		var err error
		if includeOutput {
			run, err = scanRunV2(rows)
		} else {
			run, err = scanRunSummaryV2(rows)
		}
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
	var sessionID sql.NullString
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	var exitCode sql.NullInt64
	var errText sql.NullString
	var stdout sql.NullString
	var stderr sql.NullString
	var thinking sql.NullString
	var response sql.NullString
	var exitReason sql.NullString
	var truncatedInt sql.NullInt64
	var turnCount sql.NullInt64
	var inputTokens sql.NullInt64
	var outputTokens sql.NullInt64
	err := scanner.Scan(
		&run.ID,
		&run.JobID,
		&run.Attempt,
		&run.Trigger,
		&run.ScheduledFor,
		&status,
		&workerID,
		&sessionID,
		&startedAt,
		&finishedAt,
		&exitCode,
		&errText,
		&stdout,
		&stderr,
		&thinking,
		&response,
		&exitReason,
		&truncatedInt,
		&turnCount,
		&inputTokens,
		&outputTokens,
		&run.CreatedAt,
		&run.UpdatedAt,
	)
	if err != nil {
		return jobsV2Run{}, err
	}
	applyRunV2NullableFields(&run, status, workerID, sessionID, startedAt, finishedAt, exitCode, errText, exitReason, truncatedInt, turnCount, inputTokens, outputTokens)
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

func scanRunSummaryV2(scanner interface{ Scan(dest ...any) error }) (jobsV2Run, error) {
	var run jobsV2Run
	var status string
	var workerID sql.NullString
	var sessionID sql.NullString
	var startedAt sql.NullTime
	var finishedAt sql.NullTime
	var exitCode sql.NullInt64
	var errText sql.NullString
	var exitReason sql.NullString
	var truncatedInt sql.NullInt64
	var turnCount sql.NullInt64
	var inputTokens sql.NullInt64
	var outputTokens sql.NullInt64
	err := scanner.Scan(
		&run.ID,
		&run.JobID,
		&run.Attempt,
		&run.Trigger,
		&run.ScheduledFor,
		&status,
		&workerID,
		&sessionID,
		&startedAt,
		&finishedAt,
		&exitCode,
		&errText,
		&exitReason,
		&truncatedInt,
		&turnCount,
		&inputTokens,
		&outputTokens,
		&run.CreatedAt,
		&run.UpdatedAt,
	)
	if err != nil {
		return jobsV2Run{}, err
	}
	applyRunV2NullableFields(&run, status, workerID, sessionID, startedAt, finishedAt, exitCode, errText, exitReason, truncatedInt, turnCount, inputTokens, outputTokens)
	return run, nil
}

func applyRunV2NullableFields(run *jobsV2Run, status string, workerID, sessionID sql.NullString, startedAt, finishedAt sql.NullTime, exitCode sql.NullInt64, errText, exitReason sql.NullString, truncatedInt, turnCount, inputTokens, outputTokens sql.NullInt64) {
	run.Status = jobsV2RunStatus(status)
	if workerID.Valid {
		run.WorkerID = workerID.String
	}
	if sessionID.Valid {
		run.SessionID = sessionID.String
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
	if exitReason.Valid {
		run.ExitReason = exitReason.String
	}
	if truncatedInt.Valid {
		run.Truncated = truncatedInt.Int64 != 0
	}
	if turnCount.Valid {
		run.TurnCount = int(turnCount.Int64)
	}
	if inputTokens.Valid {
		run.InputTokens = int(inputTokens.Int64)
	}
	if outputTokens.Valid {
		run.OutputTokens = int(outputTokens.Int64)
	}
}

func parseTriggerConfig(tt jobsV2TriggerType, raw json.RawMessage, scheduleTZ string) (jobsV2TriggerConfig, error) {
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
		cfg.Timezone = effectiveCronTimezone(cfg.Timezone, scheduleTZ)
		if cfg.Timezone == "" {
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
		next, err := nextCronTime(cfg.Expression, effectiveCronTimezone(cfg.Timezone, scheduleTZ), now)
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
	cfg, err := parseTriggerConfig(job.TriggerType, job.TriggerConfig, job.ScheduleTimezone)
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

func effectiveCronTimezone(cfgTZ, scheduleTZ string) string {
	if tz := normalizeTZ(scheduleTZ); tz != "" {
		return tz
	}
	return normalizeTZ(cfgTZ)
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
		var req jobsV2JobRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		job, err := s.jobsV2.UpdateJobPatch(jobID, req)
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
	var req jobsV2JobRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	jobReq := req.toJob(true)
	if jobReq.RunnerType == jobsV2RunnerLLM {
		jobReq.RunnerConfig = sanitizeJobsV2LLMNotifyOriginForRequest(r, jobReq.RunnerConfig)
	}
	if s.cfgRef != nil && jobReq.RunnerType == jobsV2RunnerLLM {
		var llmCfg jobsV2LLMConfig
		_ = json.Unmarshal(jobReq.RunnerConfig, &llmCfg)
		if strings.TrimSpace(llmCfg.AgentName) != "" {
			if _, err := LoadAgent(llmCfg.AgentName, s.cfgRef); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
		}
	}
	job, err := s.jobsV2.CreateJob(jobReq)
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
	var items []jobsV2Run
	var total int
	if queryBool(r, "summary") {
		items, total, err = s.jobsV2.ListRunSummaries(jobID, limit, offset)
	} else {
		items, total, err = s.jobsV2.ListRuns(jobID, limit, offset)
	}
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
		sinceID, err := parseNonNegativeIntQuery(r, "since_id", 0)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		items, total, err := s.jobsV2.ListRunEvents(runID, int64(sinceID), limit, offset)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object":   "list",
			"data":     items,
			"total":    total,
			"offset":   offset,
			"limit":    limit,
			"since_id": sinceID,
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

func newServeJobsV2Manager(cfg *config.Config, workers int, notifyDone jobsV2RunDoneNotifier) (*jobsV2Manager, error) {
	_ = cfg
	return newJobsV2ManagerWithNotifier("", workers, newServeJobsExecutor(cfg), notifyDone)
}

func queryBool(r *http.Request, key string) bool {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	return strings.EqualFold(value, "1") || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
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
		for name, pc := range src.Providers {
			// Deep copy the slice/pointer fields so per-run provider/model
			// overrides never mutate the shared base config (mirrors the
			// deep-copy discipline in spawn_runner.go). A shallow copy would
			// alias Models, UseNativeSearch and OAuthCreds across runs.
			if pc.Models != nil {
				pc.Models = append([]string(nil), pc.Models...)
			}
			if pc.ModelConfigs != nil {
				pc.ModelConfigs = append([]config.ProviderModelConfig(nil), pc.ModelConfigs...)
				for i := range pc.ModelConfigs {
					pc.ModelConfigs[i].ReasoningEfforts = append([]string(nil), pc.ModelConfigs[i].ReasoningEfforts...)
				}
			}
			if pc.UseNativeSearch != nil {
				v := *pc.UseNativeSearch
				pc.UseNativeSearch = &v
			}
			if pc.OAuthCreds != nil {
				creds := *pc.OAuthCreds
				pc.OAuthCreds = &creds
			}
			clone.Providers[name] = pc
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

// jobLLMServeDefaults captures serve-command-level flag values that act as
// fallbacks for a job's own (more specific) runner config. A job that omits a
// parity field inherits the serve-level default, preserving prior behavior.
type jobLLMServeDefaults struct {
	Provider      string
	Tools         string
	ReadDirs      []string
	WriteDirs     []string
	ShellAllow    []string
	MCP           string
	SystemMessage string
	MaxTurns      int
	Search        bool
	NoSearch      bool
}

// applyJobLLMProviderModel applies the job's provider/model selection onto jobCfg,
// reusing the same override machinery as `ask` and spawn_runner.go.
//
// The job's provider behaves like `ask`'s --provider: it may carry a
// provider:model form and takes precedence over the serve-level --provider
// (serveProviderFlag) when set. The job's model is an additional override applied
// last, so it wins as the most specific selector for the active provider.
func applyJobLLMProviderModel(jobCfg *config.Config, cfg jobsV2LLMConfig, serveProviderFlag, agentProvider, agentModel string) error {
	providerFlag := strings.TrimSpace(cfg.Provider)
	if providerFlag == "" {
		providerFlag = serveProviderFlag
	}
	if err := applyProviderOverridesWithAgent(jobCfg, jobCfg.Ask.Provider, jobCfg.Ask.Model, providerFlag, agentProvider, agentModel); err != nil {
		return err
	}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		if err := applyAgentModelOverride(jobCfg, model); err != nil {
			return fmt.Errorf("apply model override %q: %w", model, err)
		}
	}
	return nil
}

// resolveJobLLMSettings merges a job's runner config with the serve-level defaults
// into the final SessionSettings and user prompt. Every parity field on
// jobsV2LLMConfig is optional; an omitted field falls back to the serve default so
// existing jobs resolve to exactly the same settings as before.
//
// cwd is rooted WITHOUT os.Chdir: the jobs server runs many runs concurrently in
// one process, so a global chdir would race every other run. Instead, cwd is
// appended to the read/write tool roots and set as the shell tool's working
// directory (exec.Cmd.Dir).
func resolveJobLLMSettings(jobCfg *config.Config, agent *agents.Agent, cfg jobsV2LLMConfig, def jobLLMServeDefaults) (SessionSettings, string, error) {
	toolsFlag := def.Tools
	if strings.TrimSpace(cfg.Tools) != "" {
		toolsFlag = cfg.Tools
	}
	systemMessage := def.SystemMessage
	if strings.TrimSpace(cfg.SystemMessage) != "" {
		systemMessage = cfg.SystemMessage
	}
	providerFlag := strings.TrimSpace(cfg.Provider)
	if providerFlag == "" {
		providerFlag = def.Provider
	}
	maxTurns := def.MaxTurns
	maxTurnsSet := false
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
		maxTurnsSet = true
	}
	// CLI flags replace (not extend) agent dirs, so combine the serve-level dirs
	// with the job's extra dirs before handing them to ResolveSettings.
	readDirs := append(append([]string{}, def.ReadDirs...), cfg.ReadDir...)
	writeDirs := append(append([]string{}, def.WriteDirs...), cfg.WriteDir...)

	settings, err := ResolveSettings(jobCfg, agent, CLIFlags{
		Provider:        providerFlag,
		Tools:           toolsFlag,
		ReadDirs:        readDirs,
		WriteDirs:       writeDirs,
		ShellAllow:      def.ShellAllow,
		MCP:             def.MCP,
		SystemMessage:   systemMessage,
		MaxTurns:        maxTurns,
		MaxTurnsSet:     maxTurnsSet,
		MaxOutputTokens: cfg.MaxOutputTokens,
		Search:          def.Search || cfg.Search,
		NoSearch:        def.NoSearch && !cfg.Search,
		Platform:        runpkg.PlatformJob,
	}, jobCfg.Ask.Provider, jobCfg.Ask.Model, jobCfg.Ask.Instructions, jobCfg.Ask.MaxTurns, 50)
	if err != nil {
		return SessionSettings{}, "", err
	}

	// cwd (required, enforced in jobsV2LLMRunner.Run) roots this run's file/shell
	// tools at a directory WITHOUT a process-wide os.Chdir: it authorizes
	// read/write within cwd and sets the shell tool's exec working directory. The
	// guard keeps this resolver defensive if a caller passes an empty cwd.
	if runCwd := strings.TrimSpace(cfg.Cwd); runCwd != "" {
		settings.ReadDirs = append(settings.ReadDirs, runCwd)
		settings.WriteDirs = append(settings.WriteDirs, runCwd)
		settings.ShellWorkingDir = runCwd
	}

	return settings, cfg.Instructions, nil
}

func newServeJobsExecutor(baseCfg *config.Config) serveJobsExecutor {
	return func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error) {
		var search *bool
		if cfg.Search {
			v := true
			search = &v
		}

		var progressive *runpkg.ProgressiveOptions
		if cfg.Progressive {
			progressive = &runpkg.ProgressiveOptions{
				StopWhen:     cfg.StopWhen,
				ContinueWith: cfg.ContinueWith,
			}
		}

		runner := newCmdRunner(baseCfg, cmdRunnerOptions{
			Provider:       serveProvider,
			Tools:          serveTools,
			ReadDirs:       append([]string(nil), serveReadDirs...),
			WriteDirs:      append([]string(nil), serveWriteDirs...),
			ShellAllow:     append([]string(nil), serveShellAllow...),
			MCP:            serveMCP,
			SystemMessage:  serveSystemMessage,
			MaxTurns:       serveMaxTurns,
			Search:         serveSearch,
			NoSearch:       serveNoSearch,
			NativeSearch:   serveNativeSearch,
			NoNativeSearch: serveNoNativeSearch,
			Yolo:           serveYolo,
			Auto:           serveAuto,
			Debug:          serveDebug,
			DebugRaw:       debugRaw,
			ErrWriter:      io.Discard,
			WireSpawn:      WireSpawnAgentRunner,
		})

		result, err := runner.Run(ctx, runpkg.Request{
			Platform:        runpkg.PlatformJob,
			AgentName:       cfg.AgentName,
			Prompt:          cfg.Instructions,
			SessionID:       cfg.SessionID,
			SessionName:     cfg.SessionName,
			Persist:         cfg.sessionPersistenceEnabled(),
			Provider:        cfg.Provider,
			Model:           cfg.Model,
			Cwd:             cfg.Cwd,
			Tools:           cfg.Tools,
			ReadDirs:        append([]string(nil), cfg.ReadDir...),
			WriteDirs:       append([]string(nil), cfg.WriteDir...),
			MaxTurns:        cfg.MaxTurns,
			MaxOutputTokens: cfg.MaxOutputTokens,
			Search:          search,
			SystemMessage:   cfg.SystemMessage,
			Skills:          cfg.Skills,
			Progressive:     progressive,
		}, eventSinkFunc(onEvent))
		return serveJobsExecResult{Progressive: progressiveFromRunResult(result.Progressive)}, err
	}
}
