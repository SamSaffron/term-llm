package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

func newReconciliationRuntime(t *testing.T) (*chatWorkerRuntime, *session.SQLiteStore, *jobsV2Manager) {
	t.Helper()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.db")
	cfg := &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: sessionPath}}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: sessionPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	parent := &session.Session{
		ID: "main", Provider: "mock", ProviderKey: "mock", Model: "model",
		Mode: session.ModeChat, Origin: session.OriginTUI, ApprovalMode: session.ApprovalModePrompt,
		CWD: t.TempDir(), CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	runtime := &chatWorkerRuntime{cfg: cfg}
	manager, err := newJobsV2ManagerConfigured(filepath.Join(dir, "thread.db"), 0, nil, runtime.notifyDone, true, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime.manager = manager
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime, store, manager
}

func createReconciliationExecution(t *testing.T, store *session.SQLiteStore, manager *jobsV2Manager) (session.WorkerEdge, jobsV2Job, jobsV2Run) {
	t.Helper()
	edge, err := store.CreateWorkerOwned(context.Background(), "main", manager.workerID, "reconcile fixture")
	if err != nil {
		t.Fatal(err)
	}
	runnerConfig, _ := json.Marshal(workerLLMConfig(edge))
	labels, _ := json.Marshal(map[string]string{
		threadWorkerLabelKey:            threadWorkerLabelValue,
		threadWorkerOwnerLabelKey:       manager.workerID,
		threadWorkerCoordinatorLabelKey: edge.CoordinatorSessionID,
		threadWorkerChildLabelKey:       edge.ChildSessionID,
	})
	job, err := manager.CreateJob(jobsV2Job{
		Name: "thread-" + session.ShortID(edge.ChildSessionID), Enabled: true,
		RunnerType: jobsV2RunnerLLM, RunnerConfig: runnerConfig,
		TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`),
		ConcurrencyPolicy: "forbid", MaxConcurrentRuns: 1,
		RetryPolicy: json.RawMessage(`{"max_attempts":1}`), TimeoutSeconds: 300,
		MisfirePolicy: jobsV2MisfireSkip, Labels: labels,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := manager.TriggerJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetWorkerExecution(context.Background(), edge.ChildSessionID, job.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	edge.JobID, edge.RunID = job.ID, run.ID
	return edge, job, run
}

func waitForWorkerTerminal(t *testing.T, store *session.SQLiteStore, childID string) session.WorkerEdge {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		edge, err := store.GetWorker(context.Background(), childID)
		if err == nil && edge.Status.Terminal() {
			return edge
		}
		time.Sleep(10 * time.Millisecond)
	}
	edge, err := store.GetWorker(context.Background(), childID)
	t.Fatalf("worker did not become terminal: %#v, %v", edge, err)
	return session.WorkerEdge{}
}

func TestChatWorkerReconcileClearsCrashedUnstartedEdge(t *testing.T) {
	runtime, store, manager := newReconciliationRuntime(t)
	oldGrace := threadWorkerUnstartedGracePeriod
	threadWorkerUnstartedGracePeriod = 0
	t.Cleanup(func() { threadWorkerUnstartedGracePeriod = oldGrace })
	edge, err := store.CreateWorkerOwned(context.Background(), "main", "dead-owner", "never started")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ReconcileWorkers(context.Background(), edge.CoordinatorSessionID); err != nil {
		t.Fatal(err)
	}
	loaded := waitForWorkerTerminal(t, store, edge.ChildSessionID)
	if loaded.Status != session.WorkerFailed || loaded.OwnerID != manager.workerID {
		t.Fatalf("reconciled edge = %#v", loaded)
	}
	reports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if err != nil || len(reports) != 1 || reports[0].Kind != session.WorkerReportResult || reports[0].Origin != "terminal_synthesis" {
		t.Fatalf("terminal reports = %#v, %v", reports, err)
	}
}

func TestChatWorkerReconcileSynthesizesMissedTerminalOnce(t *testing.T) {
	runtime, store, manager := newReconciliationRuntime(t)
	edge, _, run := createReconciliationExecution(t, store, manager)
	if _, err := manager.db.Exec(`UPDATE job_runs_v2 SET status = ?, finished_at = CURRENT_TIMESTAMP, exit_reason = ?, lease_expires_at = NULL WHERE id = ?`, jobsV2RunCancelled, exitReasonCancelled, run.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runtime.ReconcileWorkers(context.Background(), edge.CoordinatorSessionID); err != nil {
			t.Fatal(err)
		}
	}
	loaded := waitForWorkerTerminal(t, store, edge.ChildSessionID)
	if loaded.Status != session.WorkerCancelled {
		t.Fatalf("worker status = %s", loaded.Status)
	}
	reports, _ := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if len(reports) != 1 {
		t.Fatalf("terminal reconciliation produced %d results: %#v", len(reports), reports)
	}
}

func TestJobsV2ExpiredCancelRecoveryNotifiesMailbox(t *testing.T) {
	_, store, manager := newReconciliationRuntime(t)
	edge, _, run := createReconciliationExecution(t, store, manager)
	if _, err := manager.db.Exec(`UPDATE job_runs_v2 SET status = ?, worker_id = 'dead', lease_expires_at = ? WHERE id = ?`, jobsV2RunCancelRequested, time.Now().Add(-time.Second).UnixMilli(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.recoverRuns(); err != nil {
		t.Fatal(err)
	}
	loaded := waitForWorkerTerminal(t, store, edge.ChildSessionID)
	if loaded.Status != session.WorkerCancelled {
		t.Fatalf("recovered cancel status = %s", loaded.Status)
	}
	reports, _ := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if len(reports) != 1 {
		t.Fatalf("cancel recovery reports = %#v", reports)
	}
}

func TestJobsV2WorkerLoopRevisitsInitiallyLiveLeaseWithoutRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	seed, err := newJobsV2Manager(path, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := seed.CreateJob(jobsV2Job{
		Name: "lease-revisit", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo should-not-run"}`),
		TriggerType:  jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := seed.TriggerJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.db.Exec(`UPDATE job_runs_v2 SET status = ?, worker_id = 'crashed', lease_expires_at = ? WHERE id = ?`, jobsV2RunRunning, time.Now().Add(150*time.Millisecond).UnixMilli(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	manager, err := newJobsV2Manager(path, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	manager.runners[jobsV2RunnerProgram] = jobsV2RunnerFunc(func(context.Context, jobsV2Job, progressWriter) (jobsV2RunResult, error) {
		executions.Add(1)
		return jobsV2RunResult{}, nil
	})
	manager.workers = 1
	manager.wg.Add(1)
	go manager.workerLoop()
	defer manager.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := manager.GetRun(run.ID)
		if err == nil && got.Status == jobsV2RunFailed {
			if got.ExitReason != exitReasonWorkerLost || executions.Load() != 0 {
				t.Fatalf("recovered run = %#v, executions=%d", got, executions.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := manager.GetRun(run.ID)
	t.Fatalf("expired lease was not revisited: %#v", got)
}

func TestThreadJobsDatabaseIsIsolatedFromOrdinaryJobsDatabase(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	ordinary, err := newJobsV2Manager("", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	thread, err := newThreadWorkerJobsV2Manager("", 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer thread.Close()
	labels, _ := json.Marshal(map[string]string{
		threadWorkerLabelKey: threadWorkerLabelValue, threadWorkerOwnerLabelKey: thread.workerID,
		threadWorkerCoordinatorLabelKey: "main", threadWorkerChildLabelKey: "child",
	})
	if _, err := thread.CreateJob(jobsV2Job{
		Name: "isolated-thread", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo"}`), TriggerType: jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`), Labels: labels,
	}); err != nil {
		t.Fatal(err)
	}
	if _, total, err := ordinary.ListJobs(10, 0); err != nil || total != 0 {
		t.Fatalf("ordinary jobs can see thread jobs: total=%d err=%v", total, err)
	}
	if _, total, err := thread.ListJobs(10, 0); err != nil || total != 1 {
		t.Fatalf("thread jobs missing: total=%d err=%v", total, err)
	}
	ordinaryPath := filepath.Join(data, "term-llm", "jobs_v2.db")
	threadPath := filepath.Join(data, "term-llm", "thread_jobs_v2.db")
	if ordinaryPath == threadPath {
		t.Fatal("thread and ordinary jobs paths are identical")
	}
	for _, path := range []string{ordinaryPath, threadPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("jobs database %s missing: %v", path, err)
		}
	}
}

func TestTwoThreadManagersEnforceLiveOwnerAffinityAndCrashAdoption(t *testing.T) {
	runtimeA, store, managerA := newReconciliationRuntime(t)
	managerB, err := newJobsV2ManagerConfigured(filepath.Join(filepath.Dir(runtimeA.cfg.Sessions.Path), "thread.db"), 0, nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	runtimeB := &chatWorkerRuntime{cfg: runtimeA.cfg, manager: managerB}
	edge, _, run := createReconciliationExecution(t, store, managerA)

	if claimed, ok, err := managerB.claimNextRun(); err != nil || ok {
		t.Fatalf("second manager claimed live owner's run: %#v ok=%v err=%v", claimed, ok, err)
	}
	err = runtimeB.CancelWorker(context.Background(), chat.WorkerCancelRequest{
		ChildSessionID: edge.ChildSessionID, CoordinatorSessionID: edge.CoordinatorSessionID, RunID: edge.RunID,
	})
	if err == nil || err.Error() != "worker is owned by another live chat" {
		t.Fatalf("foreign live cancellation error = %v", err)
	}
	if got, _ := managerA.GetRun(run.ID); got.Status != jobsV2RunQueued {
		t.Fatalf("foreign manager changed run status: %s", got.Status)
	}
	if err := runtimeB.Close(); err != nil {
		t.Fatal(err)
	}
	if got, _ := managerA.GetRun(run.ID); got.Status != jobsV2RunQueued {
		t.Fatalf("closing unrelated manager changed run status: %s", got.Status)
	}

	managerC, err := newJobsV2ManagerConfigured(filepath.Join(filepath.Dir(runtimeA.cfg.Sessions.Path), "thread.db"), 0, nil, nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	runtimeC := &chatWorkerRuntime{cfg: runtimeA.cfg, manager: managerC}
	defer runtimeC.Close()
	if _, err := managerA.db.Exec(`UPDATE thread_worker_owners_v1 SET lease_expires_at = ? WHERE owner_id = ?`, time.Now().Add(-time.Second).UnixMilli(), managerA.workerID); err != nil {
		t.Fatal(err)
	}
	if err := runtimeC.ReconcileWorkers(context.Background(), edge.CoordinatorSessionID); err != nil {
		t.Fatal(err)
	}
	adopted, err := store.GetWorker(context.Background(), edge.ChildSessionID)
	if err != nil || adopted.OwnerID != managerC.workerID {
		t.Fatalf("adopted edge = %#v, %v", adopted, err)
	}
	claimed, ok, err := managerC.claimNextRun()
	if err != nil || !ok || claimed.ID != run.ID {
		t.Fatalf("crash-adopted claim = %#v ok=%v err=%v", claimed, ok, err)
	}
}
