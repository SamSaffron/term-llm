package cmd

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
)

func TestJobsV2RejectsInvalidMisfirePolicy(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)
	base := jobsV2Job{
		Name: "invalid-misfire", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"true"}`), TriggerType: jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`), MisfirePolicy: "later",
	}
	if _, err := mgr.CreateJob(base); err == nil || !strings.Contains(err.Error(), "misfire_policy") {
		t.Fatalf("CreateJob invalid policy error = %v", err)
	}

	base.Name = "valid-misfire"
	base.MisfirePolicy = "skip"
	created, err := mgr.CreateJob(base)
	if err != nil {
		t.Fatalf("CreateJob valid policy: %v", err)
	}
	if _, err := mgr.UpdateJobPatch(created.ID, jobsV2JobRequest{MisfirePolicy: "later"}); err == nil || !strings.Contains(err.Error(), "misfire_policy") {
		t.Fatalf("UpdateJobPatch invalid policy error = %v", err)
	}
}

func TestJobsV2RunSummaryUsesCoveringIndex(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	plan := jobsV2ExplainPlan(t, mgr.db, "EXPLAIN QUERY PLAN SELECT "+servejobs.RunSummaryColumns+" FROM job_runs_v2 WHERE job_id = ? ORDER BY created_at DESC, id ASC LIMIT ? OFFSET ?", "job_test", 50, 0)
	if !strings.Contains(plan, "USING COVERING INDEX "+servejobs.RunSummaryIndexName) {
		t.Fatalf("summary query plan = %q, want covering summary index", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("summary query plan = %q, want no temp sort", plan)
	}
}

func TestJobsV2GlobalRunSummaryUsesCoveringIndex(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	plan := jobsV2ExplainPlan(t, mgr.db, "EXPLAIN QUERY PLAN SELECT "+servejobs.RunSummaryColumns+" FROM job_runs_v2 ORDER BY created_at DESC, id ASC LIMIT ? OFFSET ?", 50, 0)
	if !strings.Contains(plan, "USING COVERING INDEX "+servejobs.RunGlobalSummaryIndexName) {
		t.Fatalf("global summary query plan = %q, want covering global summary index", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("global summary query plan = %q, want no temp sort", plan)
	}
}

func TestJobsV2RunSummaryByIDUsesCoveringIndex(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	plan := jobsV2ExplainPlan(t, mgr.db, "EXPLAIN QUERY PLAN SELECT "+servejobs.RunSummaryColumns+" FROM job_runs_v2 INDEXED BY "+servejobs.RunByIDSummaryIndexName+" WHERE id = ?", "run_test")
	if !strings.Contains(plan, "USING COVERING INDEX "+servejobs.RunByIDSummaryIndexName) {
		t.Fatalf("by-id summary query plan = %q, want covering summary index", plan)
	}
}

func jobsV2ExplainPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return strings.Join(details, "\n")
}

func TestJobsV2LLMRunnerConfigValidationRequiresCwdOnCreateAndUpdate(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)

	_, err := mgr.CreateJob(jobsV2Job{
		Name:          "missing-cwd",
		Enabled:       true,
		RunnerType:    jobsV2RunnerLLM,
		RunnerConfig:  json.RawMessage(`{"agent_name":"planner","instructions":"plan"}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "runner_config.cwd is required") {
		t.Fatalf("CreateJob err = %v, want runner_config.cwd is required", err)
	}

	created, err := mgr.CreateJob(jobsV2Job{
		Name:          "has-cwd",
		Enabled:       true,
		RunnerType:    jobsV2RunnerLLM,
		RunnerConfig:  json.RawMessage(`{"agent_name":"planner","instructions":"plan","cwd":"."}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob with cwd failed: %v", err)
	}

	_, err = mgr.UpdateJobPatch(created.ID, jobsV2JobRequest{
		RunnerConfig: json.RawMessage(`{"agent_name":"planner","instructions":"plan again"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "runner_config.cwd is required") {
		t.Fatalf("UpdateJobPatch err = %v, want runner_config.cwd is required", err)
	}
}

func TestJobsV2ValidationDoesNotRequireInstalledRunner(t *testing.T) {
	mgr, err := newJobsV2ManagerWithRunners(":memory:", 0, map[jobsV2RunnerType]jobsV2Runner{}, nil)
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithRunners: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	if len(mgr.runners) != 0 {
		t.Fatalf("installed runners = %d, want 0", len(mgr.runners))
	}

	created, err := mgr.CreateJob(jobsV2Job{
		Name:          "validated-without-runner",
		Enabled:       true,
		RunnerType:    jobsV2RunnerLLM,
		RunnerConfig:  json.RawMessage(`{"agent_name":"planner","instructions":"plan","cwd":"."}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob without installed runner: %v", err)
	}
	if _, err := mgr.UpdateJobPatch(created.ID, jobsV2JobRequest{
		RunnerConfig: json.RawMessage(`{"agent_name":"planner","instructions":"plan again","cwd":"/tmp/work"}`),
	}); err != nil {
		t.Fatalf("UpdateJobPatch without installed runner: %v", err)
	}
	if _, err := mgr.UpdateJobPatch(created.ID, jobsV2JobRequest{
		RunnerConfig: json.RawMessage(`{"agent_name":"planner","instructions":"missing cwd"}`),
	}); err == nil || !strings.Contains(err.Error(), "runner_config.cwd is required") {
		t.Fatalf("invalid UpdateJobPatch err = %v, want runner_config.cwd is required", err)
	}
}

func TestJobsV2ListJobsIncludesLastRunSummary(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC().Truncate(time.Second)
	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_last_run", "last-run", jobsV2RunnerProgram, `{"command":"true"}`, jobsV2TriggerManual, `{}`, now.Add(-time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, started_at, finished_at, exit_code, error, created_at, updated_at) VALUES (?, ?, 1, 'schedule', ?, ?, ?, ?, ?, ?, ?, ?)`,
		"run_old", "job_last_run", now.Add(-2*time.Hour), jobsV2RunSucceeded, now.Add(-2*time.Hour), now.Add(-2*time.Hour).Add(time.Second), 0, "", now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("insert old run: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, started_at, finished_at, exit_code, error, created_at, updated_at) VALUES (?, ?, 1, 'schedule', ?, ?, ?, ?, ?, ?, ?, ?)`,
		"run_new", "job_last_run", now, jobsV2RunFailed, now, now.Add(time.Second), 0, "boom", now, now)
	if err != nil {
		t.Fatalf("insert new run: %v", err)
	}

	jobs, _, err := mgr.ListJobs(10, 0)
	if err != nil {
		t.Fatalf("ListJobs failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	if jobs[0].LastRun == nil {
		t.Fatalf("LastRun = nil, want latest run summary")
	}
	if jobs[0].LastRun.ID != "run_new" || jobs[0].LastRun.Status != jobsV2RunFailed || jobs[0].LastRun.Error != "boom" {
		t.Fatalf("LastRun = %+v, want failed run_new with error", jobs[0].LastRun)
	}
}
