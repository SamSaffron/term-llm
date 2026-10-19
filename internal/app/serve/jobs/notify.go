package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/samsaffron/term-llm/internal/providerhttp"
	"github.com/samsaffron/term-llm/internal/restart"
)

func jobsV2NotifyTerminalStatus(status RunStatus) bool {
	switch status {
	case RunSucceeded, RunFailed, RunCancelled, RunTimedOut:
		return true
	default:
		return false
	}
}

func (m *Manager) enqueueRunDoneNotification(run Run, status RunStatus, result RunResult, exitReason string, truncated bool, errText string) {
	if m == nil || m.notifyDone == nil || !jobsV2NotifyTerminalStatus(status) {
		return
	}
	m.wg.Add(1)
	if err := restart.Default.Go(context.Background(), func(context.Context) {
		defer m.wg.Done()
		m.notifyRunDone(run, status, result, exitReason, truncated, errText)
	}); err != nil {
		m.wg.Done()
	}
}

const (
	jobsV2NotifyMaxAttempts       = 3
	jobsV2NotifyAttemptTimeout    = 10 * time.Second
	jobsV2NotifyTotalTimeout      = 30 * time.Second
	jobsV2NotifyInitialRetryDelay = time.Second
)

func (m *Manager) notifyRunDone(run Run, status RunStatus, result RunResult, exitReason string, truncated bool, errText string) {
	if m == nil || m.notifyDone == nil || !jobsV2NotifyTerminalStatus(status) {
		return
	}
	baseCtx := m.notifyCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	notifyCtx, cancelNotify := context.WithTimeout(baseCtx, jobsV2NotifyTotalTimeout)
	defer cancelNotify()
	if err := notifyCtx.Err(); err != nil {
		return
	}

	job, err := m.GetJob(run.JobID)
	if err != nil {
		if notifyCtx.Err() == nil {
			_ = m.addRunEvent(run.ID, "notify_failed", "completion notification failed", map[string]any{"error": fmt.Sprintf("load job: %v", err)})
		}
		return
	}

	var notifyErr error
	for attempt := 1; attempt <= jobsV2NotifyMaxAttempts; attempt++ {
		attemptCtx, cancelAttempt := context.WithTimeout(notifyCtx, jobsV2NotifyAttemptTimeout)
		notifyErr = m.notifyDone(attemptCtx, run, job, status, result, exitReason, truncated, errText)
		cancelAttempt()
		if notifyErr == nil {
			return
		}
		if notifyCtx.Err() != nil {
			return
		}
		if attempt == jobsV2NotifyMaxAttempts || !NotifyErrorRetryable(notifyErr) {
			break
		}

		timer := time.NewTimer(NotifyRetryDelay(notifyErr, attempt))
		select {
		case <-timer.C:
		case <-notifyCtx.Done():
			stopTimer(timer)
			return
		}
	}
	_ = m.addRunEvent(run.ID, "notify_failed", "completion notification failed", map[string]any{"error": notifyErr.Error()})
}

// NotifyErrorRetryable reports whether a completion notification error should be retried.
func NotifyErrorRetryable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) {
		return providerhttp.RetryableStatus(statusErr.HTTPStatusCode())
	}
	// Persistence and network errors do not generally expose a status code.
	// Treat them as transient; retries remain bounded by attempts and total time.
	return true
}

// NotifyRetryDelay returns the delay before the next notification attempt.
func NotifyRetryDelay(err error, failedAttempt int) time.Duration {
	var retryAfterErr interface {
		RetryAfterDelay() (time.Duration, bool)
	}
	if errors.As(err, &retryAfterErr) {
		if delay, ok := retryAfterErr.RetryAfterDelay(); ok && delay >= 0 {
			return delay
		}
	}
	if failedAttempt < 1 {
		failedAttempt = 1
	}
	return jobsV2NotifyInitialRetryDelay * time.Duration(1<<(failedAttempt-1))
}
