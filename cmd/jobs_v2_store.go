package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
)

func validateJobsV2MisfirePolicy(policy string) error {
	switch policy {
	case jobsV2MisfireSkip, jobsV2MisfireRun:
		return nil
	default:
		return fmt.Errorf("misfire_policy must be one of: skip, run")
	}
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
	req.MisfirePolicy = strings.TrimSpace(req.MisfirePolicy)
	if req.MisfirePolicy == "" {
		req.MisfirePolicy = jobsV2MisfireSkip
	}
	if err := validateJobsV2MisfirePolicy(req.MisfirePolicy); err != nil {
		return jobsV2Job{}, err
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
		servejobs.NormalizeTZ(req.ScheduleTimezone),
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

	query := `SELECT ` + servejobs.RunSummaryColumns + ` FROM (
		SELECT ` + servejobs.RunSummaryColumns + `,
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
		return jobsV2Job{}, err
	}
	if err := validateJobsV2MisfirePolicy(current.MisfirePolicy); err != nil {
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
		servejobs.NormalizeTZ(current.ScheduleTimezone),
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

func (m *jobsV2Manager) GetRunSummary(id string) (jobsV2Run, error) {
	row := m.db.QueryRow(`SELECT `+servejobs.RunSummaryColumns+` FROM job_runs_v2 INDEXED BY `+servejobs.RunByIDSummaryIndexName+` WHERE id = ?`, id)
	return scanRunSummaryV2(row)
}

func (m *jobsV2Manager) ListRuns(jobID string, limit, offset int) ([]jobsV2Run, int, error) {
	return m.listRuns(jobID, limit, offset, true)
}

func (m *jobsV2Manager) ListRunSummaries(jobID string, limit, offset int) ([]jobsV2Run, int, error) {
	return m.listRuns(jobID, limit, offset, false)
}

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
	columns := servejobs.RunSummaryColumns
	if includeOutput {
		columns = servejobs.RunFullColumns
	}
	query := "SELECT " + columns + " FROM job_runs_v2" + where + " ORDER BY created_at DESC, id ASC LIMIT ? OFFSET ?"
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
	now := time.Now().UTC()
	var transitioned jobsV2RunStatus
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
		jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunCancelled,
		jobsV2RunCancelRequested,
		jobsV2RunQueued, jobsV2RunClaimed, now,
		id, jobsV2RunQueued, jobsV2RunClaimed, jobsV2RunRunning,
	).Scan(&transitioned)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent worker completed the run, or it was already terminal.
		// Return that authoritative state without overwriting it.
		return m.GetRun(id)
	}
	if err != nil {
		return jobsV2Run{}, fmt.Errorf("cancel run: %w", err)
	}

	switch transitioned {
	case jobsV2RunCancelled:
		_ = m.addRunEvent(id, "cancelled", "run cancelled before start", nil)
	case jobsV2RunCancelRequested:
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
