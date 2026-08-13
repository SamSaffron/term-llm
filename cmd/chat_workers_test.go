package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func newChatWorkerRuntimeTest(t *testing.T, mode session.SessionApprovalMode) (*chatWorkerRuntime, *session.SQLiteStore, session.WorkerEdge) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	cfg := &config.Config{Sessions: config.SessionsConfig{Enabled: true, Path: path}}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now()
	parent := &session.Session{
		ID: "main", Provider: "mock", ProviderKey: "mock", Model: "model",
		Mode: session.ModeChat, Origin: session.OriginTUI, ApprovalMode: mode,
		CWD: t.TempDir(), CreatedAt: now, UpdatedAt: now, Status: session.StatusActive,
	}
	if err := store.Create(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	edge, err := store.CreateWorker(context.Background(), parent.ID, "harmless task")
	if err != nil {
		t.Fatal(err)
	}
	return &chatWorkerRuntime{cfg: cfg}, store, edge
}

func workerLLMConfig(edge session.WorkerEdge) jobsV2LLMConfig {
	return jobsV2LLMConfig{
		AgentName: "developer", Instructions: edge.Task, Cwd: "/tmp",
		SessionID: edge.ChildSessionID, ParentSessionID: edge.CoordinatorSessionID, Worker: true,
	}
}

func TestCmdRunnerWorkerRegistersMailboxToolOnlyForWorker(t *testing.T) {
	runtime, workerStore, edge := newChatWorkerRuntimeTest(t, session.ApprovalModePrompt)
	_ = runtime
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers:       map[string]config.ProviderConfig{"mock": {Model: "mock-model"}},
		Agents:          config.AgentsConfig{UseBuiltin: true},
	}
	runner := newCmdRunner(cfg, cmdRunnerOptions{Store: workerStore, ApprovalMode: tools.ModePrompt, ApprovalModeSet: true}).(*cmdRunner)
	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform: runpkg.PlatformJob, AgentName: "developer", Prompt: "test",
		ProviderInstance: llm.NewMockProvider("mock"), SessionID: edge.ChildSessionID,
		ParentSessionID: edge.CoordinatorSessionID, IsWorker: true, Persist: true, Cwd: t.TempDir(),
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	found := false
	for _, spec := range env.llmReq.Tools {
		found = found || spec.Name == tools.WorkerReportToolName
	}
	if !found {
		t.Fatalf("worker request tools = %#v, report missing", env.llmReq.Tools)
	}

	ordinary, err := runner.prepare(context.Background(), runpkg.Request{
		Platform: runpkg.PlatformJob, AgentName: "developer", Prompt: "test",
		ProviderInstance: llm.NewMockProvider("mock"), SessionID: session.NewID(), Persist: true, Cwd: t.TempDir(),
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	for _, spec := range ordinary.llmReq.Tools {
		if spec.Name == tools.WorkerReportToolName {
			t.Fatal("ordinary job received worker report tool")
		}
	}
}

func TestChatWorkerApprovalComesFromDurableCoordinator(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode session.SessionApprovalMode
		want tools.ApprovalMode
	}{
		{name: "prompt", mode: session.ApprovalModePrompt, want: tools.ModePrompt},
		{name: "auto", mode: session.ApprovalModeAuto, want: tools.ModeAuto},
		{name: "yolo", mode: session.ApprovalModeYolo, want: tools.ModeYolo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, store, edge := newChatWorkerRuntimeTest(t, tc.mode)
			resolved, err := runtime.resolveApproval(context.Background(), workerLLMConfig(edge))
			if err != nil || resolved.Mode != tc.want || resolved.Source != approvalModeSourceSession {
				t.Fatalf("resolved approval = %#v, %v", resolved, err)
			}
			loaded, _ := store.GetWorker(context.Background(), edge.ChildSessionID)
			if loaded.Status != session.WorkerRunning {
				t.Fatalf("worker status = %s", loaded.Status)
			}
		})
	}
}

func TestChatWorkerApprovalRejectsForgedParent(t *testing.T) {
	runtime, _, edge := newChatWorkerRuntimeTest(t, session.ApprovalModePrompt)
	cfg := workerLLMConfig(edge)
	cfg.ParentSessionID = "different-main"
	if _, err := runtime.resolveApproval(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("forged parent error = %v", err)
	}
}

func TestChatWorkerTerminalSynthesisAlwaysLeavesResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status jobsV2RunStatus
		exit   string
		err    string
		want   session.WorkerStatus
	}{
		{name: "natural", status: jobsV2RunSucceeded, exit: exitReasonNatural, want: session.WorkerDone},
		{name: "failed", status: jobsV2RunFailed, exit: exitReasonException, err: "provider failed", want: session.WorkerFailed},
		{name: "timeout", status: jobsV2RunTimedOut, exit: exitReasonTimeout, err: "deadline exceeded", want: session.WorkerFailed},
		{name: "cancelled", status: jobsV2RunCancelled, exit: exitReasonCancelled, err: "cancelled", want: session.WorkerCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, store, edge := newChatWorkerRuntimeTest(t, session.ApprovalModePrompt)
			cfg := workerLLMConfig(edge)
			raw, _ := json.Marshal(cfg)
			job := jobsV2Job{ID: "job", RunnerConfig: raw}
			run := jobsV2Run{ID: "run", JobID: job.ID}
			if err := runtime.notifyDone(context.Background(), run, job, tc.status, jobsV2RunResult{SessionID: edge.ChildSessionID}, tc.exit, false, tc.err); err != nil {
				t.Fatal(err)
			}
			reports, err := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
			if err != nil || len(reports) != 1 || reports[0].Kind != session.WorkerReportResult || reports[0].Origin != "terminal_synthesis" {
				t.Fatalf("terminal reports = %#v, %v", reports, err)
			}
			loaded, _ := store.GetWorker(context.Background(), edge.ChildSessionID)
			if loaded.Status != tc.want || !loaded.Status.Terminal() {
				t.Fatalf("terminal status = %s, want %s", loaded.Status, tc.want)
			}
		})
	}
}

func TestChatWorkerTerminalDoesNotDuplicateReportedResult(t *testing.T) {
	runtime, store, edge := newChatWorkerRuntimeTest(t, session.ApprovalModePrompt)
	_, err := store.AddWorkerReport(context.Background(), session.WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
		Title: "Explicit result", Body: "done", Origin: "worker_tool",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(workerLLMConfig(edge))
	if err := runtime.notifyDone(context.Background(), jobsV2Run{ID: "run"}, jobsV2Job{ID: "job", RunnerConfig: raw}, jobsV2RunSucceeded, jobsV2RunResult{}, exitReasonNatural, false, ""); err != nil {
		t.Fatal(err)
	}
	reports, _ := store.ListWorkerReports(context.Background(), edge.ChildSessionID)
	if len(reports) != 1 || reports[0].Origin != "worker_tool" {
		t.Fatalf("explicit result was duplicated: %#v", reports)
	}
}

func TestJobsV2ClaimPartitionKeepsThreadWorkersOnWorkerRunner(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	ordinary, err := mgr.CreateJob(jobsV2Job{
		Name: "ordinary", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo"}`), TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	threadLabels, _ := json.Marshal(map[string]string{threadWorkerLabelKey: threadWorkerLabelValue})
	thread, err := mgr.CreateJob(jobsV2Job{
		Name: "thread", Enabled: true, RunnerType: jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo"}`), TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`), Labels: threadLabels,
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinaryRun, _ := mgr.TriggerJob(ordinary.ID)
	threadRun, _ := mgr.TriggerJob(thread.ID)
	claimed, ok, err := mgr.claimNextRun()
	if err != nil || !ok || claimed.ID != ordinaryRun.ID {
		t.Fatalf("ordinary claim = %#v, %v, %v", claimed, ok, err)
	}
	mgr.claimThreadWorkers = true
	claimed, ok, err = mgr.claimNextRun()
	if err != nil || !ok || claimed.ID != threadRun.ID {
		t.Fatalf("thread claim = %#v, %v, %v", claimed, ok, err)
	}
}
