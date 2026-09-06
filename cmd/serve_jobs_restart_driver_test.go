package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestJobsRestartDriverLateBindAndLateRollbackSettlement(t *testing.T) {
	m, c, _ := jobsRestartFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	own, err := session.NewSQLiteStore(session.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	shared, err := session.NewSQLiteStore(session.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	if err := own.Create(ctx, &session.Session{ID: c.SessionID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	x := &jobsLLMExecution{manager: m, ctx: ctx, cancel: cancel, checkpoint: c}
	m.llmExecutions.Store(c.RunID, x)
	defer m.llmExecutions.Delete(c.RunID)
	setup := jobsRestartSetup{Store: shared, RestartID: c.RestartID, Service: c.Service, Instance: c.SourceInstance}
	// Grace can expire while this already-admitted runner is still preparing.
	if err := m.beginLLMRestart(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("cancelled before runtime could checkpoint")
	}
	x.bind(&cmdRunEnvironment{engine: llm.NewEngine(llm.NewMockProvider("mock"), llm.NewToolRegistry()), store: own, llmReq: llm.Request{MaxTurns: 7}})
	if ctx.Err() != nil {
		t.Fatal("engine binding was mistaken for durable input")
	}
	x.markReady()
	if ctx.Err() == nil {
		t.Fatal("late binding escaped restart interruption")
	}
	if err := m.interruptLLMJobs(context.Background(), setup.RestartID, setup.Service); err != nil {
		t.Fatal("same generation was not idempotent", err)
	}
	// A timed-out drain rolls back while this source is still cleaning up.
	if err := m.rollbackLLMRestart(); err != nil {
		t.Fatal(err)
	}
	run, err := m.GetRun(c.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != jobsV2RunRunning {
		t.Fatal("unsettled source requeued")
	}
	x.capture()
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
	if parked, err := x.seal(&jobsV2RunResult{TurnCount: 2}); !parked || err != nil {
		t.Fatalf("seal failed: %v", err)
	}
	if err := m.recoverRolledBackLLMJobs(); err != nil {
		t.Fatal(err)
	}
	run, ok, err := m.claimNextRun()
	if err != nil || !ok || run.restart == nil {
		t.Fatalf("late settled source stranded: %v", err)
	}
	if run.restart.RemainingTurns != 5 {
		t.Fatalf("late source budget reset: %d", run.restart.RemainingTurns)
	}
	select {
	case <-m.workerWake:
	default:
		t.Fatal("rollback adoption did not wake worker")
	}
}
