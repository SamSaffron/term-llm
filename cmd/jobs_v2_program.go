package cmd

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/jobs"
)

type jobsV2ProgramRunner struct{}

var (
	jobsV2ProgramOutputLimit int64         = jobs.ProgramOutputLimit
	jobsV2ProgramWaitDelay   time.Duration = jobs.ProgramWaitDelay
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
