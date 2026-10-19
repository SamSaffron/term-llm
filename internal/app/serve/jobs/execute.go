package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	"github.com/samsaffron/term-llm/internal/jobs"
	"github.com/samsaffron/term-llm/internal/restart"
)

const (
	jobsV2WorkerMinDelay       = 100 * time.Millisecond
	jobsV2WorkerIdleDelay      = time.Minute
	jobsV2WorkerErrorDelay     = 200 * time.Millisecond
	jobsV2FinishRunRetryWindow = 30 * time.Second
	jobsV2FinishRunRetryDelay  = 50 * time.Millisecond
	jobsV2FinishRunMaxDelay    = time.Second
)

func classifyRunError(err error, result RunResult) (exitReason string, truncated bool) {
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

func (m *Manager) workerLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.doneChan():
			return
		default:
		}

		ok, err := m.runNextOwned()
		if err != nil {
			if !m.waitForWorkerWake(jobsV2WorkerErrorDelay) {
				return
			}
			continue
		}
		if ok {
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

func (m *Manager) runNextOwned() (bool, error) {
	ctx, release, err := restart.Default.Root(context.Background())
	if err != nil {
		return false, err
	}
	defer release()
	run, ok, err := m.claimNextRun()
	if ok && err == nil {
		ctx, finish := restart.Default.Cancellable(ctx)
		defer finish()
		m.executeRunContext(ctx, run)
	}
	return ok, err
}

func (m *Manager) waitForWorkerWake(delay time.Duration) bool {
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

func (m *Manager) nextWorkerDelay(now time.Time) time.Duration {
	delay, err := m.nextWorkerDelayWithError(now)
	if err != nil {
		return jobsV2WorkerErrorDelay
	}
	return delay
}

func (m *Manager) nextWorkerDelayWithError(now time.Time) (time.Duration, error) {
	idleDelay := m.workerIdleDelay
	if idleDelay <= 0 {
		idleDelay = jobsV2WorkerIdleDelay
	}
	delay := idleDelay

	var nextRun sql.NullTime
	err := m.db.QueryRow(`SELECT scheduled_for FROM job_runs_v2 WHERE status = ? ORDER BY scheduled_for ASC LIMIT 1`, RunQueued).Scan(&nextRun)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil && nextRun.Valid {
		delay = minDuration(delay, nextRun.Time.UTC().Sub(now.UTC()))
	}

	return clampJobsV2Delay(delay, jobsV2WorkerMinDelay, idleDelay), nil
}

func (m *Manager) claimNextRun() (Run, bool, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return Run{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	row := tx.QueryRow(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at FROM job_runs_v2 WHERE status = ? AND scheduled_for <= ? ORDER BY scheduled_for ASC LIMIT 1`, RunQueued, now)
	run, err := scanRunV2(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, false, nil
		}
		return Run{}, false, err
	}

	res, err := tx.Exec(`UPDATE job_runs_v2 SET status = ?, worker_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`, RunClaimed, m.workerID, run.ID, RunQueued)
	if err != nil {
		return Run{}, false, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return Run{}, false, nil
	}

	if err := tx.Commit(); err != nil {
		return Run{}, false, err
	}
	run.Status = RunClaimed
	run.WorkerID = m.workerID
	_ = m.addRunEvent(run.ID, "claimed", "run claimed by worker", map[string]any{"worker_id": m.workerID})
	return run, true, nil
}

func (m *Manager) requeueClaimedRunAfterShutdown(runID string) {
	res, err := m.db.Exec(`UPDATE job_runs_v2 SET status = ?, worker_id = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ? AND worker_id = ?`, RunQueued, runID, RunClaimed, m.workerID)
	if err != nil {
		log.Printf("jobs v2: failed to requeue unstarted run %q during shutdown: %v", runID, err)
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		log.Printf("jobs v2: could not confirm requeue of unstarted run %q during shutdown: %v", runID, err)
		return
	}
	if affected == 0 {
		return
	}
	if err := m.addRunEvent(runID, "requeued", "run returned to queue during worker shutdown", map[string]any{"worker_id": m.workerID}); err != nil {
		log.Printf("jobs v2: failed to record shutdown requeue event for run %q: %v", runID, err)
	}
}

func (m *Manager) executeRun(run Run) { m.executeRunContext(context.Background(), run) }

func (m *Manager) executeRunContext(parent context.Context, run Run) {
	// Avoid turning a claim into a terminal failure when shutdown had already won
	// before this worker began admission. Keep the check below as well: shutdown
	// can still race with loading the job and preparing its context.
	if m.isClosed() {
		m.requeueClaimedRunAfterShutdown(run.ID)
		return
	}

	job, err := m.GetJob(run.JobID)
	if err != nil {
		m.finishRunWithRetry(run.ID, RunFailed, RunResult{}, fmt.Errorf("load job: %w", err), run.Attempt)
		return
	}

	runner, ok := m.runners[job.RunnerType]
	if !ok {
		m.finishRunWithRetry(run.ID, RunFailed, RunResult{}, fmt.Errorf("unknown runner type: %s", job.RunnerType), run.Attempt)
		return
	}

	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		m.requeueClaimedRunAfterShutdown(run.ID)
		return
	}
	m.cancels[run.ID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, run.ID)
		m.mu.Unlock()
	}()

	started := time.Now().UTC()
	res, err := m.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`, RunRunning, started, run.ID, RunClaimed)
	if err != nil {
		m.finishRunWithRetry(run.ID, RunFailed, RunResult{}, fmt.Errorf("mark run running: %w", err), run.Attempt)
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
		m.finishRunWithRetry(run.ID, RunTimedOut, result, context.DeadlineExceeded, run.Attempt)
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		m.finishRunWithRetry(run.ID, RunCancelled, result, context.Canceled, run.Attempt)
		return
	}
	if result.ExitReason == exitReasonTimeout {
		m.finishRunWithRetry(run.ID, RunTimedOut, result, context.DeadlineExceeded, run.Attempt)
		return
	}
	if result.ExitReason == exitReasonCancelled {
		m.finishRunWithRetry(run.ID, RunCancelled, result, context.Canceled, run.Attempt)
		return
	}
	if runErr != nil {
		m.finishRunWithRetry(run.ID, RunFailed, result, runErr, run.Attempt)
		return
	}
	m.finishRunWithRetry(run.ID, RunSucceeded, result, nil, run.Attempt)
}

func (m *Manager) finishRunWithRetry(runID string, status RunStatus, result RunResult, runErr error, policyAttempt int) {
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

func (m *Manager) finishRun(runID string, status RunStatus, result RunResult, runErr error, attempt int) error {
	now := time.Now().UTC()
	exitReason, truncated := classifyRunError(runErr, result)
	var errText string
	if runErr != nil {
		errText = runErr.Error()
	}
	cancelledErrText := context.Canceled.Error()
	res, err := m.db.Exec(`UPDATE job_runs_v2 SET status = CASE WHEN status = ? THEN ? ELSE ? END, finished_at = ?, exit_code = ?, error = CASE WHEN status = ? THEN ? ELSE ? END, stdout = ?, stderr = ?, thinking = ?, response = ?, session_id = ?, exit_reason = CASE WHEN status = ? THEN ? ELSE ? END, truncated = ?, turn_count = ?, input_tokens = ?, output_tokens = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status IN (?, ?, ?, ?)`,
		RunCancelRequested, RunCancelled, status,
		now, result.ExitCode,
		RunCancelRequested, cancelledErrText, errText,
		result.Stdout, result.Stderr, result.Thinking, result.Response, result.SessionID,
		RunCancelRequested, exitReasonCancelled, exitReason,
		boolToInt(truncated), result.TurnCount, result.InputTokens, result.OutputTokens,
		runID, RunQueued, RunClaimed, RunRunning, RunCancelRequested)
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

	if status == RunFailed || status == RunTimedOut {
		job, err := m.GetJob(run.JobID)
		if err != nil {
			return nil
		}
		policy := decodeRetryPolicy(job.RetryPolicy)
		if attempt < policy.MaxAttempts {
			delay := computeRetryDelay(policy, attempt)
			retryID := "run_" + wire.RandomSuffix()
			_, _ = m.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, retryID, job.ID, attempt+1, "retry", now.Add(delay), RunQueued)
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

func decodeRetryPolicy(raw json.RawMessage) RetryPolicy {
	policy := RetryPolicy{MaxAttempts: 1, Backoff: "fixed", InitialDelay: "10s", MaxDelay: "5m"}
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

func computeRetryDelay(policy RetryPolicy, attempt int) time.Duration {
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
