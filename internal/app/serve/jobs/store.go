package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	corejobs "github.com/samsaffron/term-llm/internal/jobs"
)

func validateJobsV2RunnerConfig(runnerType RunnerType, raw json.RawMessage) error {
	return corejobs.ValidateRunnerConfig(corejobs.RunnerType(runnerType), raw)
}

func validateJobsV2MisfirePolicy(policy string) error {
	switch policy {
	case MisfireSkip, MisfireRun:
		return nil
	default:
		return fmt.Errorf("misfire_policy must be one of: skip, run")
	}
}

func (m *Manager) CreateJob(req Job) (Job, error) {
	if strings.TrimSpace(req.Name) == "" {
		return Job{}, fmt.Errorf("name is required")
	}
	if req.RunnerType != RunnerLLM && req.RunnerType != RunnerProgram {
		return Job{}, fmt.Errorf("runner_type must be one of: llm, program")
	}
	if err := validateJobsV2RunnerConfig(req.RunnerType, req.RunnerConfig); err != nil {
		return Job{}, err
	}
	if req.TriggerType != TriggerManual && req.TriggerType != TriggerOnce && req.TriggerType != TriggerCron {
		return Job{}, fmt.Errorf("trigger_type must be one of: manual, once, cron")
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
	req.MisfirePolicy = strings.TrimSpace(req.MisfirePolicy)
	if req.MisfirePolicy == "" {
		req.MisfirePolicy = MisfireSkip
	}
	if err := validateJobsV2MisfirePolicy(req.MisfirePolicy); err != nil {
		return Job{}, err
	}

	cfg, err := parseTriggerConfig(req.TriggerType, req.TriggerConfig, req.ScheduleTimezone)
	if err != nil {
		return Job{}, err
	}
	next := initialNextRun(req.TriggerType, cfg, req.ScheduleTimezone)

	now := time.Now().UTC()
	id := "job_" + wire.RandomSuffix()
	_, err = m.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		req.Name,
		boolToInt(req.Enabled),
		req.RunnerType,
		stringOrEmptyRaw(req.RunnerConfig, "{}"),
		req.TriggerType,
		stringOrEmptyRaw(req.TriggerConfig, "{}"),
		NormalizeTZ(req.ScheduleTimezone),
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
		return Job{}, err
	}
	created, err := m.GetJob(id)
	if err != nil {
		return Job{}, err
	}
	m.notifyScheduler()
	return created, nil
}

func (m *Manager) GetJob(id string) (Job, error) {
	row := m.db.QueryRow(`SELECT id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, retry_policy, timeout_seconds, misfire_policy, labels, next_run_at, created_at, updated_at FROM jobs_v2 WHERE id = ?`, id)
	job, err := scanJobV2(row)
	if err != nil {
		return Job{}, err
	}
	if err := m.attachLastRuns([]*Job{&job}); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (m *Manager) ListJobs(limit, offset int) ([]Job, int, error) {
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
	jobs := make([]Job, 0)
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
	jobPtrs := make([]*Job, 0, len(jobs))
	for i := range jobs {
		jobPtrs = append(jobPtrs, &jobs[i])
	}
	if err := m.attachLastRuns(jobPtrs); err != nil {
		return nil, 0, err
	}
	return jobs, total, nil
}

func (m *Manager) attachLastRuns(jobs []*Job) error {
	ids := make([]string, 0, len(jobs))
	byID := make(map[string]*Job, len(jobs))
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

	query := `SELECT ` + RunSummaryColumns + ` FROM (
		SELECT ` + RunSummaryColumns + `,
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

func (m *Manager) UpdateJob(id string, req Job) (Job, error) {
	return m.UpdateJobPatch(id, JobToRequest(req))
}

func (m *Manager) UpdateJobPatch(id string, req JobRequest) (Job, error) {
	current, err := m.GetJob(id)
	if err != nil {
		return Job{}, err
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
		current.MisfirePolicy = strings.TrimSpace(req.MisfirePolicy)
	}
	if len(req.Labels) > 0 {
		current.Labels = req.Labels
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}

	cfg, err := parseTriggerConfig(current.TriggerType, current.TriggerConfig, current.ScheduleTimezone)
	if err != nil {
		return Job{}, err
	}
	if err := validateJobsV2MisfirePolicy(current.MisfirePolicy); err != nil {
		return Job{}, err
	}
	if err := validateJobsV2RunnerConfig(current.RunnerType, current.RunnerConfig); err != nil {
		return Job{}, err
	}
	next := initialNextRun(current.TriggerType, cfg, current.ScheduleTimezone)

	_, err = m.db.Exec(`UPDATE jobs_v2 SET name = ?, enabled = ?, runner_type = ?, runner_config = ?, trigger_type = ?, trigger_config = ?, schedule_timezone = ?, concurrency_policy = ?, max_concurrent_runs = ?, retry_policy = ?, timeout_seconds = ?, misfire_policy = ?, labels = ?, next_run_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		current.Name,
		boolToInt(current.Enabled),
		current.RunnerType,
		stringOrEmptyRaw(current.RunnerConfig, "{}"),
		current.TriggerType,
		stringOrEmptyRaw(current.TriggerConfig, "{}"),
		NormalizeTZ(current.ScheduleTimezone),
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
		return Job{}, err
	}
	updated, err := m.GetJob(id)
	if err != nil {
		return Job{}, err
	}
	m.notifyScheduler()
	return updated, nil
}

func (m *Manager) DeleteJob(id string, cancelActive bool) error {
	if cancelActive {
		runs, _, err := m.ListRunSummaries(id, 200, 0)
		if err == nil {
			for _, run := range runs {
				if run.Status == RunRunning || run.Status == RunClaimed || run.Status == RunQueued {
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

func (m *Manager) TriggerJob(id string) (Run, error) {
	job, err := m.GetJob(id)
	if err != nil {
		return Run{}, err
	}
	if !job.Enabled {
		return Run{}, fmt.Errorf("job is disabled")
	}

	runID, active, queued, err := m.enqueueRunWithConcurrencyLimit(job, "manual", time.Now().UTC(), nil, nil)
	if err != nil {
		return Run{}, err
	}
	if !queued {
		limit := jobsV2ConcurrencyLimit(job)
		if limit == 1 {
			return Run{}, fmt.Errorf("job already has an active run")
		}
		return Run{}, fmt.Errorf("job already has %d active runs (max %d)", active, limit)
	}

	_ = m.addRunEvent(runID, "queued", "manual run queued", map[string]any{"trigger": "manual", "attempt": 1})
	m.notifyWorkers(1)
	return m.GetRun(runID)
}

func (m *Manager) GetRun(id string) (Run, error) {
	row := m.db.QueryRow(`SELECT id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at FROM job_runs_v2 WHERE id = ?`, id)
	return scanRunV2(row)
}

func (m *Manager) GetRunSummary(id string) (Run, error) {
	row := m.db.QueryRow(`SELECT `+RunSummaryColumns+` FROM job_runs_v2 INDEXED BY `+RunByIDSummaryIndexName+` WHERE id = ?`, id)
	return scanRunSummaryV2(row)
}

func (m *Manager) ListRuns(jobID string, limit, offset int) ([]Run, int, error) {
	return m.listRuns(jobID, limit, offset, true)
}

func (m *Manager) ListRunSummaries(jobID string, limit, offset int) ([]Run, int, error) {
	return m.listRuns(jobID, limit, offset, false)
}

func (m *Manager) listRuns(jobID string, limit, offset int, includeOutput bool) ([]Run, int, error) {
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
	columns := RunSummaryColumns
	if includeOutput {
		columns = RunFullColumns
	}
	query := "SELECT " + columns + " FROM job_runs_v2" + where + " ORDER BY created_at DESC, id ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	runs := make([]Run, 0, limit)
	for rows.Next() {
		var run Run
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

func (m *Manager) CancelRun(id string) (Run, error) {
	now := time.Now().UTC()
	var transitioned RunStatus
	err := m.db.QueryRow(`
		UPDATE job_runs_v2
		SET status = CASE
				WHEN status IN (?, ?) THEN ?
				ELSE ?
			END,
			finished_at = CASE
				WHEN status IN (?, ?) THEN ?
				ELSE finished_at
			END,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?, ?)
		RETURNING status`,
		RunQueued, RunClaimed, RunCancelled,
		RunCancelRequested,
		RunQueued, RunClaimed, now,
		id, RunQueued, RunClaimed, RunRunning,
	).Scan(&transitioned)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent worker completed the run, or it was already terminal.
		// Return that authoritative state without overwriting it.
		return m.GetRun(id)
	}
	if err != nil {
		return Run{}, fmt.Errorf("cancel run: %w", err)
	}

	switch transitioned {
	case RunCancelled:
		_ = m.addRunEvent(id, "cancelled", "run cancelled before start", nil)
	case RunCancelRequested:
		_ = m.addRunEvent(id, "cancel_requested", "cancellation requested", nil)
		m.mu.Lock()
		cancel := m.cancels[id]
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return m.GetRun(id)
}

func scanJobV2(scanner interface{ Scan(dest ...any) error }) (Job, error) {
	var job Job
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
		return Job{}, err
	}
	job.Enabled = enabled == 1
	job.RunnerType = RunnerType(runnerType)
	job.TriggerType = TriggerType(triggerType)
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

func scanRunV2(scanner interface{ Scan(dest ...any) error }) (Run, error) {
	var run Run
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
		return Run{}, err
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

func scanRunSummaryV2(scanner interface{ Scan(dest ...any) error }) (Run, error) {
	var run Run
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
		return Run{}, err
	}
	applyRunV2NullableFields(&run, status, workerID, sessionID, startedAt, finishedAt, exitCode, errText, exitReason, truncatedInt, turnCount, inputTokens, outputTokens)
	return run, nil
}

func applyRunV2NullableFields(run *Run, status string, workerID, sessionID sql.NullString, startedAt, finishedAt sql.NullTime, exitCode sql.NullInt64, errText, exitReason sql.NullString, truncatedInt, turnCount, inputTokens, outputTokens sql.NullInt64) {
	run.Status = RunStatus(status)
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
