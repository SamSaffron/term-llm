package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	jobsV2SchedulerMinDelay   = 100 * time.Millisecond
	jobsV2SchedulerIdleDelay  = time.Minute
	jobsV2SchedulerErrorDelay = 200 * time.Millisecond
)

type jobsV2MisfiredCron struct {
	id               string
	triggerConfig    string
	scheduleTimezone string
	observedNextRun  time.Time
	nextRun          time.Time
}

// reconcileMisfiredSchedules applies startup-only misfire semantics before the
// scheduler can observe overdue rows. Policies set to run remain overdue so the
// normal scheduler queues one catch-up run.
func (m *Manager) reconcileMisfiredSchedules(cutoff time.Time) error {
	cutoff = cutoff.UTC()
	rows, err := m.db.Query(`
		SELECT id, trigger_config, schedule_timezone, next_run_at
		FROM jobs_v2
		WHERE enabled = 1
			AND misfire_policy = ?
			AND trigger_type = ?
			AND next_run_at IS NOT NULL
			AND next_run_at <= ?
		ORDER BY next_run_at, id`, MisfireSkip, TriggerCron, cutoff)
	if err != nil {
		return fmt.Errorf("load skipped cron misfires: %w", err)
	}

	var crons []jobsV2MisfiredCron
	for rows.Next() {
		var cron jobsV2MisfiredCron
		var timezone sql.NullString
		if err := rows.Scan(&cron.id, &cron.triggerConfig, &timezone, &cron.observedNextRun); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan skipped cron misfire: %w", err)
		}
		cron.scheduleTimezone = timezone.String
		cfg, err := parseTriggerConfig(TriggerCron, json.RawMessage(cron.triggerConfig), cron.scheduleTimezone)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse skipped cron misfire %s: %w", cron.id, err)
		}
		cron.nextRun, err = NextCronTime(cfg.Expression, EffectiveCronTimezone(cfg.Timezone, cron.scheduleTimezone), cutoff)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("advance skipped cron misfire %s: %w", cron.id, err)
		}
		cron.nextRun = cron.nextRun.UTC()
		crons = append(crons, cron)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan skipped cron misfires: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close skipped cron misfires: %w", err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("begin misfire reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, cron := range crons {
		// Compare every schedule input observed above. If another process edits the
		// row between selection and this update, leave its newer state untouched.
		if _, err := tx.Exec(`
			UPDATE jobs_v2
			SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
				AND enabled = 1
				AND misfire_policy = ?
				AND trigger_type = ?
				AND trigger_config = ?
				AND COALESCE(schedule_timezone, '') = ?
				AND next_run_at = ?
				AND next_run_at <= ?`,
			cron.nextRun, cron.id, MisfireSkip, TriggerCron,
			cron.triggerConfig, cron.scheduleTimezone, cron.observedNextRun.UTC(), cutoff); err != nil {
			return fmt.Errorf("advance skipped cron misfire %s: %w", cron.id, err)
		}
	}
	if _, err := tx.Exec(`
		UPDATE jobs_v2
		SET enabled = 0, next_run_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE enabled = 1
			AND misfire_policy = ?
			AND trigger_type = ?
			AND next_run_at IS NOT NULL
			AND next_run_at <= ?`, MisfireSkip, TriggerOnce, cutoff); err != nil {
		return fmt.Errorf("disable skipped once misfires: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit misfire reconciliation: %w", err)
	}
	return nil
}

func (m *Manager) schedulerLoop() {
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

func (m *Manager) nextSchedulerDelay(now time.Time) time.Duration {
	delay, err := m.nextSchedulerDelayWithError(now)
	if err != nil {
		return jobsV2SchedulerErrorDelay
	}
	return delay
}

func (m *Manager) nextSchedulerDelayWithError(now time.Time) (time.Duration, error) {
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

func (m *Manager) maybeRunCleanup(now time.Time) error {
	_, release, reloadErr := restart.Default.Root(context.Background())
	if reloadErr != nil {
		return reloadErr
	}
	defer release()
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

func (m *Manager) pruneOldData(now time.Time) error {
	terminalStatuses := []any{RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunSkipped}

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

func (m *Manager) notifyScheduler() {
	select {
	case m.schedulerWake <- struct{}{}:
	default:
	}
}

func (m *Manager) notifyWorkers(n int) {
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

func (m *Manager) scheduleDueRuns(now time.Time) error {
	_, release, reloadErr := restart.Default.Root(context.Background())
	if reloadErr != nil {
		return reloadErr
	}
	defer release()
	rows, err := m.db.Query(`SELECT id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at FROM jobs_v2 WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ? ORDER BY next_run_at ASC LIMIT 200`, now.UTC())
	if err != nil {
		return err
	}
	defer rows.Close()

	var jobs []Job
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

func jobsV2ConcurrencyLimit(job Job) int {
	if job.ConcurrencyPolicy == "forbid" {
		return 1
	}
	if job.MaxConcurrentRuns > 0 {
		return job.MaxConcurrentRuns
	}
	return 1
}

func (m *Manager) countActiveRuns(jobID string) (int, error) {
	var active int
	err := m.db.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE job_id = ? AND status IN (?, ?, ?)`, jobID, RunQueued, RunClaimed, RunRunning).Scan(&active)
	if err != nil {
		return 0, err
	}
	return active, nil
}

func (m *Manager) enqueueRunWithConcurrencyLimit(job Job, trigger string, scheduledFor time.Time, onLimit func(*sql.Tx) error, afterInsert func(*sql.Tx, string) error) (string, int, bool, error) {
	m.enqueueMu.Lock()
	defer m.enqueueMu.Unlock()

	tx, err := m.db.Begin()
	if err != nil {
		return "", 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var active int
	err = tx.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE job_id = ? AND status IN (?, ?, ?)`, job.ID, RunQueued, RunClaimed, RunRunning).Scan(&active)
	if err != nil {
		return "", 0, false, err
	}
	limit := jobsV2ConcurrencyLimit(job)
	if active >= limit {
		if onLimit != nil {
			if err := onLimit(tx); err != nil {
				return "", 0, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return "", 0, false, err
		}
		return "", active, false, nil
	}

	runID := "run_" + wire.RandomSuffix()
	_, err = tx.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, runID, job.ID, trigger, scheduledFor.UTC(), RunQueued)
	if err != nil {
		return "", 0, false, err
	}
	if afterInsert != nil {
		if err := afterInsert(tx, runID); err != nil {
			return "", 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", 0, false, err
	}
	return runID, active + 1, true, nil
}

func (m *Manager) scheduleOne(job Job, now time.Time) error {
	next, err := computeNextRunAt(job, now)
	if err != nil {
		return err
	}

	runID, _, queued, err := m.enqueueRunWithConcurrencyLimit(job, "schedule", now, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE jobs_v2 SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, job.ID)
		return err
	}, func(tx *sql.Tx, _ string) error {
		if job.TriggerType == TriggerOnce {
			_, err := tx.Exec(`UPDATE jobs_v2 SET enabled = 0, next_run_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, job.ID)
			return err
		}
		_, err := tx.Exec(`UPDATE jobs_v2 SET next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, next, job.ID)
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

func parseTriggerConfig(tt TriggerType, raw json.RawMessage, scheduleTZ string) (TriggerConfig, error) {
	cfg := TriggerConfig{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("invalid trigger_config: %w", err)
		}
	}
	switch tt {
	case TriggerManual:
		return cfg, nil
	case TriggerOnce:
		if strings.TrimSpace(cfg.RunAt) == "" {
			return cfg, fmt.Errorf("trigger_config.run_at is required for once trigger")
		}
		if _, err := time.Parse(time.RFC3339, cfg.RunAt); err != nil {
			return cfg, fmt.Errorf("trigger_config.run_at must be RFC3339")
		}
		return cfg, nil
	case TriggerCron:
		if strings.TrimSpace(cfg.Expression) == "" {
			return cfg, fmt.Errorf("trigger_config.expression is required for cron trigger")
		}
		cfg.Timezone = EffectiveCronTimezone(cfg.Timezone, scheduleTZ)
		if cfg.Timezone == "" {
			return cfg, fmt.Errorf("trigger_config.timezone is required for cron trigger")
		}
		if _, err := time.LoadLocation(cfg.Timezone); err != nil {
			return cfg, fmt.Errorf("invalid cron timezone: %w", err)
		}
		if err := ValidateCronExpression(cfg.Expression); err != nil {
			return cfg, err
		}
		return cfg, nil
	default:
		return cfg, fmt.Errorf("unsupported trigger type: %s", tt)
	}
}

func initialNextRun(tt TriggerType, cfg TriggerConfig, scheduleTZ string) *time.Time {
	now := time.Now().UTC()
	switch tt {
	case TriggerManual:
		return nil
	case TriggerOnce:
		t, err := time.Parse(time.RFC3339, cfg.RunAt)
		if err != nil {
			return nil
		}
		u := t.UTC()
		return &u
	case TriggerCron:
		next, err := NextCronTime(cfg.Expression, EffectiveCronTimezone(cfg.Timezone, scheduleTZ), now)
		if err != nil {
			return nil
		}
		u := next.UTC()
		return &u
	default:
		return nil
	}
}

func computeNextRunAt(job Job, now time.Time) (*time.Time, error) {
	cfg, err := parseTriggerConfig(job.TriggerType, job.TriggerConfig, job.ScheduleTimezone)
	if err != nil {
		return nil, err
	}
	return initialNextRun(job.TriggerType, cfg, job.ScheduleTimezone), nil
}
