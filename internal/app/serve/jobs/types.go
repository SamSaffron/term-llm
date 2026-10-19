package jobs

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/jobs"
	"time"
)

type RunnerType string

type TriggerType string

type RunStatus string

const (
	RunnerLLM     RunnerType = RunnerType(jobs.RunnerLLM)
	RunnerProgram RunnerType = RunnerType(jobs.RunnerProgram)

	TriggerManual TriggerType = TriggerType(jobs.TriggerManual)
	TriggerOnce   TriggerType = TriggerType(jobs.TriggerOnce)
	TriggerCron   TriggerType = TriggerType(jobs.TriggerCron)

	RunQueued          RunStatus = RunStatus(jobs.RunQueued)
	RunClaimed         RunStatus = RunStatus(jobs.RunClaimed)
	RunRunning         RunStatus = RunStatus(jobs.RunRunning)
	RunSucceeded       RunStatus = RunStatus(jobs.RunSucceeded)
	RunFailed          RunStatus = RunStatus(jobs.RunFailed)
	RunCancelled       RunStatus = RunStatus(jobs.RunCancelled)
	RunCancelRequested RunStatus = RunStatus(jobs.RunCancelRequested)
	RunTimedOut        RunStatus = RunStatus(jobs.RunTimedOut)
	RunSkipped         RunStatus = RunStatus(jobs.RunSkipped)

	MisfireSkip = "skip"
	MisfireRun  = "run"
)

type RetryPolicy struct {
	MaxAttempts  int    `json:"max_attempts,omitempty"`
	Backoff      string `json:"backoff,omitempty"`
	InitialDelay string `json:"initial_delay,omitempty"`
	MaxDelay     string `json:"max_delay,omitempty"`
	JitterPct    int    `json:"jitter_pct,omitempty"`
}

type TriggerConfig struct {
	RunAt      string `json:"run_at,omitempty"`
	Expression string `json:"expression,omitempty"`
	Timezone   string `json:"timezone,omitempty"`
}

type Job struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Enabled           bool            `json:"enabled"`
	RunnerType        RunnerType      `json:"runner_type"`
	RunnerConfig      json.RawMessage `json:"runner_config"`
	TriggerType       TriggerType     `json:"trigger_type"`
	TriggerConfig     json.RawMessage `json:"trigger_config"`
	ScheduleTimezone  string          `json:"schedule_timezone,omitempty"`
	ConcurrencyPolicy string          `json:"concurrency_policy,omitempty"`
	MaxConcurrentRuns int             `json:"max_concurrent_runs,omitempty"`
	RetryPolicy       json.RawMessage `json:"retry_policy,omitempty"`
	TimeoutSeconds    int             `json:"timeout_seconds,omitempty"`
	MisfirePolicy     string          `json:"misfire_policy,omitempty"`
	Labels            json.RawMessage `json:"labels,omitempty"`
	NextRunAt         *time.Time      `json:"next_run_at,omitempty"`
	LastRun           *Run            `json:"last_run,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type JobRequest struct {
	Name              string          `json:"name"`
	Enabled           *bool           `json:"enabled,omitempty"`
	RunnerType        RunnerType      `json:"runner_type"`
	RunnerConfig      json.RawMessage `json:"runner_config"`
	TriggerType       TriggerType     `json:"trigger_type"`
	TriggerConfig     json.RawMessage `json:"trigger_config"`
	ScheduleTimezone  string          `json:"schedule_timezone,omitempty"`
	ConcurrencyPolicy string          `json:"concurrency_policy,omitempty"`
	MaxConcurrentRuns int             `json:"max_concurrent_runs,omitempty"`
	RetryPolicy       json.RawMessage `json:"retry_policy,omitempty"`
	TimeoutSeconds    int             `json:"timeout_seconds,omitempty"`
	MisfirePolicy     string          `json:"misfire_policy,omitempty"`
	Labels            json.RawMessage `json:"labels,omitempty"`
}

func (req JobRequest) ToJob(defaultEnabled bool) Job {
	enabled := defaultEnabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return Job{
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

func JobToRequest(req Job) JobRequest {
	enabled := req.Enabled
	return JobRequest{
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

type Run struct {
	ID           string     `json:"id"`
	JobID        string     `json:"job_id"`
	Attempt      int        `json:"attempt"`
	Trigger      string     `json:"trigger"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	Status       RunStatus  `json:"status"`
	WorkerID     string     `json:"worker_id,omitempty"`
	SessionID    string     `json:"session_id,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	Error        string     `json:"error,omitempty"`
	Stdout       string     `json:"stdout,omitempty"`
	Stderr       string     `json:"stderr,omitempty"`
	Thinking     string     `json:"thinking,omitempty"`
	Response     string     `json:"response,omitempty"`
	ExitReason   string     `json:"exit_reason,omitempty"`
	Truncated    bool       `json:"truncated,omitempty"`
	TurnCount    int        `json:"turn_count,omitempty"`
	InputTokens  int        `json:"input_tokens,omitempty"`
	OutputTokens int        `json:"output_tokens,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type RunEvent struct {
	ID        int64           `json:"id"`
	RunID     string          `json:"run_id"`
	EventType string          `json:"event_type"`
	Message   string          `json:"message,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type RunResult struct {
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

// ProgressWriter receives real-time progress updates from a running job.
// eventType is one of: "tool_start", "tool_end", "phase", "turn_complete", "response_flush", "progress_update".
// For "response_flush": message is the current accumulated response text, data is nil.
// For others: message is a human-readable summary, data is structured metadata.
type ProgressWriter func(eventType, message string, data any)

type Runner interface {
	Run(ctx context.Context, job Job, pw ProgressWriter) (RunResult, error)
}

type RunDoneNotifier func(ctx context.Context, run Run, job Job, status RunStatus, result RunResult, exitReason string, truncated bool, errText string) error
