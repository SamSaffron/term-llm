package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestJobsV2CancelRunAfterClaimedToRunningTransition(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "cancel-claimed-race",
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
	if _, err := mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`, jobsV2RunClaimed, run.ID, jobsV2RunQueued); err != nil {
		t.Fatalf("mark run claimed: %v", err)
	}

	runnerStarted := make(chan struct{})
	cancelObserved := make(chan struct{})
	releaseRunner := make(chan struct{})
	runnerDone := make(chan struct{})
	defer func() {
		select {
		case <-releaseRunner:
		default:
			close(releaseRunner)
		}
	}()
	mgr.runners[jobsV2RunnerProgram] = jobsV2RunnerFunc(func(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
		close(runnerStarted)
		<-ctx.Done()
		close(cancelObserved)
		<-releaseRunner
		return jobsV2RunResult{}, ctx.Err()
	})

	go func() {
		mgr.executeRun(run)
		close(runnerDone)
	}()
	select {
	case <-runnerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}

	cancelled, err := mgr.CancelRun(run.ID)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}
	if cancelled.Status != jobsV2RunCancelRequested {
		t.Fatalf("cancelled status = %s, want %s", cancelled.Status, jobsV2RunCancelRequested)
	}
	select {
	case <-cancelObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not observe cancellation")
	}

	close(releaseRunner)
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not finish")
	}
	finished, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if finished.Status != jobsV2RunCancelled {
		t.Fatalf("finished status = %s, want %s", finished.Status, jobsV2RunCancelled)
	}
}

func TestJobsV2CancelRunDoesNotOverrideCompletedRun(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "cancel-completion-race",
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
	if _, err := mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND status = ?`, jobsV2RunRunning, time.Now().UTC(), run.ID, jobsV2RunQueued); err != nil {
		t.Fatalf("mark run running: %v", err)
	}

	cancelCalled := false
	mgr.mu.Lock()
	mgr.cancels[run.ID] = func() { cancelCalled = true }
	mgr.mu.Unlock()

	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, nil, run.Attempt); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	completed, err := mgr.CancelRun(run.ID)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}
	if completed.Status != jobsV2RunSucceeded {
		t.Fatalf("completed status = %s, want %s", completed.Status, jobsV2RunSucceeded)
	}
	if cancelCalled {
		t.Fatal("cancel function called after run completed")
	}
}

func TestJobsV2FinishRunDoesNotOverrideCancelRequested(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "cancel-authoritative",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
		RetryPolicy:   json.RawMessage(`{"max_attempts":2}`),
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected created job to have an id")
	}

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}

	started := time.Now().UTC()
	if _, err := mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunRunning, started, run.ID); err != nil {
		t.Fatalf("mark run running: %v", err)
	}

	cancelled, err := mgr.CancelRun(run.ID)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}
	if cancelled.Status != jobsV2RunCancelRequested {
		t.Fatalf("cancelled status = %s, want %s", cancelled.Status, jobsV2RunCancelRequested)
	}

	result := jobsV2RunResult{Response: "partial output", ExitCode: 17}
	if err := mgr.finishRun(run.ID, jobsV2RunFailed, result, errors.New("runner failed after cancel request"), run.Attempt); err != nil {
		t.Fatalf("finishRun failed: %v", err)
	}

	current, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if current.Status != jobsV2RunCancelled {
		t.Fatalf("run status = %s, want %s", current.Status, jobsV2RunCancelled)
	}
	if current.ExitReason != exitReasonCancelled {
		t.Fatalf("exit reason = %q, want %q", current.ExitReason, exitReasonCancelled)
	}
	if current.Error != context.Canceled.Error() {
		t.Fatalf("error = %q, want %q", current.Error, context.Canceled.Error())
	}
	if current.Response != result.Response {
		t.Fatalf("response = %q, want %q", current.Response, result.Response)
	}
	if current.FinishedAt == nil {
		t.Fatal("expected cancelled run to have finished_at set")
	}

	var retryRuns int
	if err := mgr.db.QueryRow(`SELECT COUNT(*) FROM job_runs_v2 WHERE job_id = ? AND id != ?`, job.ID, run.ID).Scan(&retryRuns); err != nil {
		t.Fatalf("count retry runs: %v", err)
	}
	if retryRuns != 0 {
		t.Fatalf("retry runs = %d, want 0", retryRuns)
	}
}

func TestJobsV2ClaimNextRunSkipsFutureScheduledQueuedRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:           "claim-next-run",
		Enabled:        true,
		RunnerType:     jobsV2RunnerProgram,
		RunnerConfig:   json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:    jobsV2TriggerManual,
		TriggerConfig:  json.RawMessage(`{}`),
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	now := time.Now().UTC()
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'retry', ?, ?, ?, ?)`,
		"run_future", job.ID, now.Add(5*time.Minute), jobsV2RunQueued, now, now)
	if err != nil {
		t.Fatalf("insert future queued run: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?)`,
		"run_ready", job.ID, now.Add(-time.Second), jobsV2RunQueued, now, now)
	if err != nil {
		t.Fatalf("insert ready queued run: %v", err)
	}

	run, ok, err := mgr.claimNextRun()
	if err != nil {
		t.Fatalf("claimNextRun failed: %v", err)
	}
	if !ok {
		t.Fatal("claimNextRun returned no run")
	}
	if run.ID != "run_ready" {
		t.Fatalf("claimed run = %q, want run_ready", run.ID)
	}

	futureRun, err := mgr.GetRun("run_future")
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if futureRun.Status != jobsV2RunQueued {
		t.Fatalf("future run status = %s, want %s", futureRun.Status, jobsV2RunQueued)
	}

	_, ok, err = mgr.claimNextRun()
	if err != nil {
		t.Fatalf("second claimNextRun failed: %v", err)
	}
	if ok {
		t.Fatal("claimNextRun should not claim future-scheduled queued runs")
	}
}

func TestJobsV2ExecuteRunRequeuesClaimWhenManagerClosed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs_v2.db")
	mgr, err := newJobsV2Manager(dbPath, 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() {
		if !mgr.isClosed() {
			_ = mgr.Close()
		}
	}()

	runner := &testJobsV2Runner{}
	mgr.runners[jobsV2RunnerProgram] = runner
	job, err := mgr.CreateJob(jobsV2Job{
		Name:           "closed-execute-run",
		Enabled:        true,
		RunnerType:     jobsV2RunnerProgram,
		RunnerConfig:   json.RawMessage(`{}`),
		TriggerType:    jobsV2TriggerManual,
		TriggerConfig:  json.RawMessage(`{}`),
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	queued, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}
	claimed, ok, err := mgr.claimNextRun()
	if err != nil {
		t.Fatalf("claimNextRun failed: %v", err)
	}
	if !ok || claimed.ID != queued.ID {
		t.Fatalf("claimNextRun = (%q, %v), want %q true", claimed.ID, ok, queued.ID)
	}

	// Hold CloseContext at its worker wait so executeRun sees shutdown while the
	// database is still open, matching a worker between claim and admission.
	mgr.wg.Add(1)
	workerDone := false
	defer func() {
		if !workerDone {
			mgr.wg.Done()
		}
	}()
	closeErr := make(chan error, 1)
	go func() {
		closeErr <- mgr.CloseContext(context.Background())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !mgr.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("manager did not start closing")
		}
		time.Sleep(time.Millisecond)
	}

	mgr.executeRun(claimed)
	mgr.wg.Done()
	workerDone = true
	if err := <-closeErr; err != nil {
		t.Fatalf("CloseContext failed: %v", err)
	}
	if runner.called {
		t.Fatal("runner was invoked after manager shutdown")
	}

	reopened, err := newJobsV2Manager(dbPath, 0, nil)
	if err != nil {
		t.Fatalf("reopen jobs manager failed: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	recovered, err := reopened.GetRun(claimed.ID)
	if err != nil {
		t.Fatalf("GetRun after reopen failed: %v", err)
	}
	if recovered.Status != jobsV2RunQueued {
		t.Fatalf("status after reopen = %s, want %s", recovered.Status, jobsV2RunQueued)
	}
	if recovered.WorkerID != "" {
		t.Fatalf("worker_id after reopen = %q, want empty", recovered.WorkerID)
	}
	if recovered.StartedAt != nil {
		t.Fatal("started_at was set even though closed manager should not start the run")
	}
	events, _, err := reopened.ListRunEvents(claimed.ID, 0, 100, 0)
	if err != nil {
		t.Fatalf("ListRunEvents after reopen failed: %v", err)
	}
	foundRequeue := false
	for _, event := range events {
		if event.EventType == "requeued" {
			foundRequeue = true
			break
		}
	}
	if !foundRequeue {
		t.Fatalf("run events = %#v, want a requeued event", events)
	}

	reopenedRunner := &testJobsV2Runner{}
	reopened.runners[jobsV2RunnerProgram] = reopenedRunner
	reclaimed, ok, err := reopened.claimNextRun()
	if err != nil {
		t.Fatalf("claimNextRun after reopen failed: %v", err)
	}
	if !ok || reclaimed.ID != claimed.ID {
		t.Fatalf("claimNextRun after reopen = (%q, %v), want %q true", reclaimed.ID, ok, claimed.ID)
	}
	reopened.executeRun(reclaimed)
	if !reopenedRunner.called {
		t.Fatal("requeued run was not executable after reopen")
	}
	finished, err := reopened.GetRun(claimed.ID)
	if err != nil {
		t.Fatalf("GetRun after execution failed: %v", err)
	}
	if finished.Status != jobsV2RunSucceeded {
		t.Fatalf("finished status = %s, want %s", finished.Status, jobsV2RunSucceeded)
	}
}

func TestJobsV2ExecuteRunRetriesFinishRunAfterTransientDBLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs_v2.db")
	dsn := dbPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := execJobsV2Schema(db); err != nil {
		t.Fatalf("execJobsV2Schema failed: %v", err)
	}

	lockReleased := make(chan struct{})
	var runID string
	mgr := &jobsV2Manager{
		db:       db,
		workerID: "worker_test",
		runners: map[jobsV2RunnerType]jobsV2Runner{
			jobsV2RunnerProgram: jobsV2RunnerFunc(func(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
				lockDB, err := sql.Open("sqlite", dsn)
				if err != nil {
					return jobsV2RunResult{}, err
				}
				lockDB.SetMaxOpenConns(1)
				lockDB.SetMaxIdleConns(1)
				tx, err := lockDB.Begin()
				if err != nil {
					_ = lockDB.Close()
					return jobsV2RunResult{}, err
				}
				if _, err := tx.Exec(`INSERT INTO job_run_events_v2 (run_id, event_type, message, created_at) VALUES (?, 'lock', 'transient lock', CURRENT_TIMESTAMP)`, runID); err != nil {
					_ = tx.Rollback()
					_ = lockDB.Close()
					return jobsV2RunResult{}, err
				}
				go func() {
					time.Sleep(70 * time.Millisecond)
					_ = tx.Rollback()
					_ = lockDB.Close()
					close(lockReleased)
				}()
				return jobsV2RunResult{Stdout: "ok"}, nil
			}),
		},
		cancels: make(map[string]context.CancelFunc),
	}

	job, err := mgr.CreateJob(jobsV2Job{
		Name:           "retry-finish-run",
		Enabled:        true,
		RunnerType:     jobsV2RunnerProgram,
		RunnerConfig:   json.RawMessage(`{}`),
		TriggerType:    jobsV2TriggerManual,
		TriggerConfig:  json.RawMessage(`{}`),
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	queued, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}
	claimed, ok, err := mgr.claimNextRun()
	if err != nil {
		t.Fatalf("claimNextRun failed: %v", err)
	}
	if !ok || claimed.ID != queued.ID {
		t.Fatalf("claimNextRun = (%q, %v), want %q true", claimed.ID, ok, queued.ID)
	}
	runID = claimed.ID

	mgr.executeRun(claimed)

	select {
	case <-lockReleased:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transient lock release")
	}
	updated, err := mgr.GetRun(claimed.ID)
	if err != nil {
		t.Fatalf("GetRun after executeRun failed: %v", err)
	}
	if updated.Status != jobsV2RunSucceeded {
		t.Fatalf("status = %s, want %s", updated.Status, jobsV2RunSucceeded)
	}
	active, err := mgr.countActiveRuns(job.ID)
	if err != nil {
		t.Fatalf("countActiveRuns failed: %v", err)
	}
	if active != 0 {
		t.Fatalf("active runs = %d, want 0", active)
	}
}
