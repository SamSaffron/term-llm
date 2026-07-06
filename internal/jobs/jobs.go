package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/procutil"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// RunnerType identifies the implementation used to execute a job.
type RunnerType string

// TriggerType identifies how a job is scheduled.
type TriggerType string

// RunStatus identifies the current state of a run.
type RunStatus string

const (
	RunnerLLM     RunnerType = "llm"
	RunnerProgram RunnerType = "program"

	TriggerManual TriggerType = "manual"
	TriggerOnce   TriggerType = "once"
	TriggerCron   TriggerType = "cron"

	RunQueued          RunStatus = "queued"
	RunClaimed         RunStatus = "claimed"
	RunRunning         RunStatus = "running"
	RunSucceeded       RunStatus = "succeeded"
	RunFailed          RunStatus = "failed"
	RunCancelled       RunStatus = "cancelled"
	RunCancelRequested RunStatus = "cancel_requested"
	RunTimedOut        RunStatus = "timed_out"
	RunSkipped         RunStatus = "skipped"
)

const (
	ExitReasonNatural    = "natural_completion"
	ExitReasonMaxTurns   = "max_turns_exceeded"
	ExitReasonTimeout    = "timeout"
	ExitReasonCancelled  = "cancelled"
	ExitReasonException  = "exception"
	ExitReasonEmpty      = "empty_response"
	ExitReasonWorkerLost = "worker_lost"
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
	TurnCount    int
	InputTokens  int
	OutputTokens int
	ExitReason   string
	Truncated    bool
}

type ProgressWriter func(eventType, message string, data any)

type Runner interface {
	Run(ctx context.Context, job Job, pw ProgressWriter) (RunResult, error)
}

type RunDoneNotifier func(ctx context.Context, run Run, job Job, status RunStatus, result RunResult, exitReason string, truncated bool, errText string) error

type ExecResult struct {
	Progressive any
}

type Executor func(ctx context.Context, cfg LLMConfig, onEvent func(llm.Event)) (ExecResult, error)

type NotifyOrigin struct {
	Origin         string `json:"origin,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	TelegramChatID int64  `json:"telegram_chat_id,omitempty"`
}

type LLMConfig struct {
	AgentName      string        `json:"agent_name"`
	Instructions   string        `json:"instructions"`
	Progressive    bool          `json:"progressive,omitempty"`
	StopWhen       string        `json:"stop_when,omitempty"`
	ContinueWith   string        `json:"continue_with,omitempty"`
	PersistSession *bool         `json:"persist_session,omitempty"`
	SessionID      string        `json:"session_id,omitempty"`
	SessionName    string        `json:"session_name,omitempty"`
	NotifyWhenDone bool          `json:"notify_when_done,omitempty"`
	NotifyOrigin   *NotifyOrigin `json:"notify_origin,omitempty"`
	Cwd            string        `json:"cwd"`

	Provider        string   `json:"provider,omitempty"`
	Model           string   `json:"model,omitempty"`
	ReadDir         []string `json:"read_dir,omitempty"`
	WriteDir        []string `json:"write_dir,omitempty"`
	Tools           string   `json:"tools,omitempty"`
	MaxTurns        int      `json:"max_turns,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Search          bool     `json:"search,omitempty"`
	SystemMessage   string   `json:"system_message,omitempty"`
	Skills          string   `json:"skills,omitempty"`
}

func (c LLMConfig) SessionPersistenceEnabled() bool {
	if c.PersistSession == nil {
		return true
	}
	return *c.PersistSession
}

func (c LLMConfig) EffectiveSessionID() string {
	if id := strings.TrimSpace(c.SessionID); id != "" {
		return id
	}
	return session.NewID()
}

func ValidateRunnerConfig(runnerType RunnerType, raw json.RawMessage) error {
	switch runnerType {
	case RunnerLLM:
		var cfg LLMConfig
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
	case RunnerProgram:
	case "":
		return fmt.Errorf("runner_type is required")
	default:
		return fmt.Errorf("runner_type must be one of: llm, program")
	}
	return nil
}

func ClassifyRunError(err error, result RunResult) (exitReason string, truncated bool) {
	truncated = result.Truncated
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ExitReasonTimeout, truncated
		}
		if errors.Is(err, context.Canceled) {
			return ExitReasonCancelled, truncated
		}
		if llm.IsMaxTurnsExceeded(err) || strings.Contains(err.Error(), "max turns") {
			return ExitReasonMaxTurns, true
		}
		if result.ExitReason == ExitReasonWorkerLost {
			return result.ExitReason, truncated
		}
		return ExitReasonException, truncated
	}
	if strings.TrimSpace(result.ExitReason) != "" {
		return result.ExitReason, truncated || result.ExitReason == ExitReasonMaxTurns
	}
	if strings.TrimSpace(result.Response) == "" &&
		strings.TrimSpace(result.Stdout) == "" &&
		strings.TrimSpace(result.Stderr) == "" &&
		strings.TrimSpace(result.Thinking) == "" {
		return ExitReasonEmpty, truncated
	}
	return ExitReasonNatural, truncated
}

type ProgramConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Env     []string `json:"env,omitempty"`
	Shell   bool     `json:"shell,omitempty"`
}

type ProgramRunner struct {
	OutputLimit int64
	WaitDelay   time.Duration
}

const (
	DefaultProgramOutputLimit int64         = 64 << 10
	DefaultProgramWaitDelay   time.Duration = time.Second
)

// ProgramOutputLimit and ProgramWaitDelay are retained for compatibility with
// existing tests/callers. Prefer configuring ProgramRunner fields directly.
var (
	ProgramOutputLimit int64         = DefaultProgramOutputLimit
	ProgramWaitDelay   time.Duration = DefaultProgramWaitDelay
)

func (r *ProgramRunner) Run(ctx context.Context, job Job, pw ProgressWriter) (RunResult, error) {
	_ = pw
	var cfg ProgramConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
		return RunResult{}, fmt.Errorf("invalid program runner config: %w", err)
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return RunResult{}, fmt.Errorf("program command is required")
	}

	var cmd *exec.Cmd
	if cfg.Shell {
		args := append([]string{"-c", cfg.Command, "--"}, cfg.Args...)
		cmd = exec.CommandContext(ctx, detectShell(), args...)
	} else {
		cmd = exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	}
	if strings.TrimSpace(cfg.Cwd) != "" {
		cmd.Dir = cfg.Cwd
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	cleanup, prepErr := tools.PrepareCommand(cmd)
	if prepErr != nil {
		return RunResult{}, fmt.Errorf("program setup failed: %w", prepErr)
	}
	waitDelay := r.WaitDelay
	if waitDelay <= 0 {
		waitDelay = ProgramWaitDelay
	}
	cmd.WaitDelay = waitDelay
	defer cleanup()

	outputLimit := r.OutputLimit
	if outputLimit <= 0 {
		outputLimit = ProgramOutputLimit
	}
	stdout := procutil.NewLimitedBuffer(outputLimit)
	stderr := procutil.NewLimitedBuffer(outputLimit)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	exitCode := 0
	result := RunResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.Truncated() || stderr.Truncated()}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return result, context.DeadlineExceeded
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return result, context.Canceled
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			result.ExitCode = exitCode
		} else {
			return result, fmt.Errorf("program run failed: %w", err)
		}
	}
	result.ExitCode = exitCode
	if exitCode != 0 {
		return result, fmt.Errorf("program exited with code %d", exitCode)
	}
	return result, nil
}

func stringOrEmptyRaw(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || string(raw) == "null" {
		return fallback
	}
	return string(raw)
}

func detectShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}
