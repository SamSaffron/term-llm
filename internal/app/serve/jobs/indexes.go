package jobs

// Run column lists and covering-index definitions shared by schema creation and queries.

const RunSummaryIndexName = "idx_job_runs_v2_summary_by_job_created"

const jobsV2RunSummaryIndexSQL = "CREATE INDEX IF NOT EXISTS " + RunSummaryIndexName + " ON job_runs_v2(job_id, created_at DESC, id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, updated_at)"

const RunGlobalSummaryIndexName = "idx_job_runs_v2_summary_created"

const jobsV2RunGlobalSummaryIndexSQL = "CREATE INDEX IF NOT EXISTS " + RunGlobalSummaryIndexName + " ON job_runs_v2(created_at DESC, id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, updated_at)"

const RunByIDSummaryIndexName = "idx_job_runs_v2_summary_by_id"

const jobsV2RunByIDSummaryIndexSQL = "CREATE UNIQUE INDEX IF NOT EXISTS " + RunByIDSummaryIndexName + " ON job_runs_v2(id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at)"

const RunFullColumns = "id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at"

const RunSummaryColumns = "id, job_id, attempt, trigger, scheduled_for, status, worker_id, session_id, started_at, finished_at, exit_code, error, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at"
