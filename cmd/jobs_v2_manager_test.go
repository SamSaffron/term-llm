package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
)

func TestNewJobsV2ManagerLimitsFileBackedDBConnections(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs_v2.db")
	mgr, err := newJobsV2Manager(dbPath, 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	if got := mgr.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 for file-backed DB", got)
	}
}

func TestNewJobsV2ManagerReconcilesSkippedMisfiresBeforeSchedulerStarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs_v2.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := execJobsV2Schema(db); err != nil {
		_ = db.Close()
		t.Fatalf("execJobsV2Schema: %v", err)
	}
	past := time.Now().UTC().Add(-10 * time.Minute)
	_, err = db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, next_run_at, created_at, updated_at) VALUES ('startup_skip', 'startup_skip', 1, ?, '{"command":"true"}', ?, '{"expression":"*/5 * * * *","timezone":"UTC"}', 'UTC', 'forbid', 1, 30, ?, ?, ?, ?)`, jobsV2RunnerProgram, jobsV2TriggerCron, jobsV2MisfireSkip, past, past, past)
	if err != nil {
		_ = db.Close()
		t.Fatalf("insert startup job: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	startedAt := time.Now().UTC()
	mgr, err := newJobsV2Manager(dbPath, 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	// Give the zero-delay scheduler pass enough time to expose an ordering bug:
	// without synchronous reconciliation it would enqueue the stale run.
	time.Sleep(100 * time.Millisecond)

	job, err := mgr.GetJob("startup_skip")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.NextRunAt == nil || !job.NextRunAt.After(startedAt) {
		t.Fatalf("next_run_at = %v, want after startup %v", job.NextRunAt, startedAt)
	}
	var runs int
	if err := mgr.db.QueryRow(`SELECT COUNT(*) FROM job_runs_v2 WHERE job_id = 'startup_skip'`).Scan(&runs); err != nil {
		t.Fatalf("count startup runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("startup created %d stale runs, want 0", runs)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestJobsV2RecoverRunsCancelsCancelRequestedRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "recover-cancel-requested",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}

	startedAt := time.Now().UTC().Add(-time.Second)
	_, err = mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunCancelRequested, startedAt, run.ID)
	if err != nil {
		t.Fatalf("mark cancel_requested failed: %v", err)
	}

	if err := mgr.recoverRuns(); err != nil {
		t.Fatalf("recoverRuns failed: %v", err)
	}

	recovered, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if recovered.Status != jobsV2RunCancelled {
		t.Fatalf("status = %s, want %s", recovered.Status, jobsV2RunCancelled)
	}
	if recovered.FinishedAt == nil {
		t.Fatalf("finished_at was not set for recovered cancelled run")
	}
}

func TestJobsV2RecoverRunsMarksInterruptedRunsFailed(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "recover-interrupted",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	claimed, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob claimed failed: %v", err)
	}
	runningID := "run_" + randomSuffix()
	queuedID := "run_" + randomSuffix()
	if _, err := mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, runningID, job.ID, jobsV2RunQueued); err != nil {
		t.Fatalf("insert running run failed: %v", err)
	}
	if _, err := mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, queuedID, job.ID, jobsV2RunQueued); err != nil {
		t.Fatalf("insert queued run failed: %v", err)
	}

	if _, err := mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, response = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunClaimed, "partial claimed", claimed.ID); err != nil {
		t.Fatalf("mark claimed failed: %v", err)
	}
	if _, err := mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, response = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunRunning, "partial running", runningID); err != nil {
		t.Fatalf("mark running failed: %v", err)
	}

	if err := mgr.recoverRuns(); err != nil {
		t.Fatalf("recoverRuns failed: %v", err)
	}

	for _, tc := range []struct {
		id       string
		response string
	}{
		{claimed.ID, "partial claimed"},
		{runningID, "partial running"},
	} {
		recovered, err := mgr.GetRun(tc.id)
		if err != nil {
			t.Fatalf("GetRun(%s) failed: %v", tc.id, err)
		}
		if recovered.Status != jobsV2RunFailed {
			t.Fatalf("run %s status = %s, want %s", tc.id, recovered.Status, jobsV2RunFailed)
		}
		if recovered.Response != tc.response {
			t.Fatalf("run %s response = %q, want %q", tc.id, recovered.Response, tc.response)
		}
		if recovered.Error != "worker lost before run could finish" {
			t.Fatalf("run %s error = %q", tc.id, recovered.Error)
		}
		if recovered.ExitReason != exitReasonWorkerLost {
			t.Fatalf("run %s exit_reason = %q, want %q", tc.id, recovered.ExitReason, exitReasonWorkerLost)
		}
	}

	stillQueued, err := mgr.GetRun(queuedID)
	if err != nil {
		t.Fatalf("GetRun queued failed: %v", err)
	}
	if stillQueued.Status != jobsV2RunQueued {
		t.Fatalf("queued status = %s, want %s", stillQueued.Status, jobsV2RunQueued)
	}
}

func TestJobsV2RecoverRunsSchedulesRetryForInterruptedRun(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "recover-retry",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
		RetryPolicy:   json.RawMessage(`{"max_attempts":2,"initial_delay":"1ms"}`),
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}
	if _, err := mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunRunning, run.ID); err != nil {
		t.Fatalf("mark running failed: %v", err)
	}

	if err := mgr.recoverRuns(); err != nil {
		t.Fatalf("recoverRuns failed: %v", err)
	}

	recovered, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if recovered.Status != jobsV2RunFailed {
		t.Fatalf("status = %s, want %s", recovered.Status, jobsV2RunFailed)
	}

	rows, err := mgr.db.Query(`SELECT attempt, trigger, status FROM job_runs_v2 WHERE job_id = ? AND id <> ?`, job.ID, run.ID)
	if err != nil {
		t.Fatalf("query retry runs: %v", err)
	}
	defer rows.Close()
	var retryCount int
	for rows.Next() {
		var attempt int
		var trigger string
		var status string
		if err := rows.Scan(&attempt, &trigger, &status); err != nil {
			t.Fatalf("scan retry run: %v", err)
		}
		retryCount++
		if attempt != 2 || trigger != "retry" || jobsV2RunStatus(status) != jobsV2RunQueued {
			t.Fatalf("retry run = attempt %d trigger %q status %s", attempt, trigger, status)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("retry rows err: %v", err)
	}
	if retryCount != 1 {
		t.Fatalf("retry count = %d, want 1", retryCount)
	}
}

func newJobsV2ManagerWithoutLoops(t *testing.T) *jobsV2Manager {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := execJobsV2Schema(db); err != nil {
		_ = db.Close()
		t.Fatalf("execJobsV2Schema failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &jobsV2Manager{
		db:                 db,
		workerID:           "worker_test",
		schedulerIdleDelay: jobsV2SchedulerIdleDelay,
		workerIdleDelay:    jobsV2WorkerIdleDelay,
		cleanupInterval:    0,
		cancels:            make(map[string]context.CancelFunc),
	}
}

type jobsV2RunnerFunc func(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error)

func (fn jobsV2RunnerFunc) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
	return fn(ctx, job, pw)
}

type testJobsV2Runner struct {
	called bool
}

func (r *testJobsV2Runner) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
	r.called = true
	return jobsV2RunResult{}, nil
}

func TestJobsV2InjectedRunnerAndNotifierDriveExecution(t *testing.T) {
	runnerJobs := make(chan jobsV2Job, 1)
	notifications := make(chan struct {
		run    jobsV2Run
		job    jobsV2Job
		status jobsV2RunStatus
		result jobsV2RunResult
	}, 1)
	wantResult := jobsV2RunResult{
		ExitCode:     7,
		Stdout:       "runner stdout",
		Stderr:       "runner stderr",
		Thinking:     "runner thinking",
		Response:     "runner response",
		SessionID:    "runner-session",
		TurnCount:    2,
		InputTokens:  30,
		OutputTokens: 12,
		ExitReason:   exitReasonNatural,
	}
	runner := jobsV2RunnerFunc(func(_ context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
		runnerJobs <- job
		pw("phase", "injected runner", map[string]any{"source": "test"})
		return wantResult, nil
	})
	notify := func(_ context.Context, run jobsV2Run, job jobsV2Job, status jobsV2RunStatus, result jobsV2RunResult, _ string, _ bool, _ string) error {
		notifications <- struct {
			run    jobsV2Run
			job    jobsV2Job
			status jobsV2RunStatus
			result jobsV2RunResult
		}{run: run, job: job, status: status, result: result}
		return nil
	}
	mgr, err := newJobsV2ManagerWithRunners(":memory:", 1, map[jobsV2RunnerType]jobsV2Runner{
		jobsV2RunnerProgram: runner,
	}, notify)
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithRunners: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "injected-execution",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"ignored"}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}

	select {
	case calledJob := <-runnerJobs:
		if calledJob.ID != job.ID {
			t.Fatalf("runner job ID = %q, want %q", calledJob.ID, job.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("injected runner was not called")
	}

	select {
	case got := <-notifications:
		if got.run.ID != run.ID || got.job.ID != job.ID {
			t.Fatalf("notification run/job = %q/%q, want %q/%q", got.run.ID, got.job.ID, run.ID, job.ID)
		}
		if got.status != jobsV2RunSucceeded {
			t.Fatalf("notification status = %q, want %q", got.status, jobsV2RunSucceeded)
		}
		if got.result != wantResult {
			t.Fatalf("notification result = %+v, want %+v", got.result, wantResult)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("injected notifier was not called")
	}

	stored, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Status != jobsV2RunSucceeded || stored.Stdout != wantResult.Stdout || stored.Stderr != wantResult.Stderr || stored.Thinking != wantResult.Thinking || stored.Response != wantResult.Response || stored.SessionID != wantResult.SessionID || stored.TurnCount != wantResult.TurnCount || stored.InputTokens != wantResult.InputTokens || stored.OutputTokens != wantResult.OutputTokens || stored.ExitReason != wantResult.ExitReason {
		t.Fatalf("stored run did not preserve injected result: %+v", stored)
	}
	events, _, err := mgr.ListRunEvents(run.ID, 0, 100, 0)
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	foundPhase := false
	for _, event := range events {
		if event.EventType == "phase" && event.Message == "injected runner" {
			foundPhase = true
		}
	}
	if !foundPhase {
		t.Fatalf("run events missing injected progress event: %#v", events)
	}
}

func TestJobsV2DatabaseHasOneLiveRecoveryOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	first, err := newJobsV2Manager(path, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := newJobsV2Manager(path, 0, nil); err == nil {
		second.Close()
		t.Fatal("second manager could recover a live owner's database")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := newJobsV2Manager(path, 0, nil)
	if err != nil {
		t.Fatal("closed owner retained lock:", err)
	}
	defer next.Close()
}

// execJobsV2Schema creates a complete current jobs database for tests.
func execJobsV2Schema(db *sql.DB) error {
	return servejobs.InitSchema(context.Background(), db)
}
