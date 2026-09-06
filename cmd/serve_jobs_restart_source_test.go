package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestJobsRestartSourceCapturesThenSealsAfterCleanup(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "recovery", true: "user-stop"}[stop], func(t *testing.T) {
			m, c, _ := jobsRestartFixture(t)
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewSQLiteStore(session.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := store.Create(ctx, &session.Session{ID: c.SessionID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
				t.Fatal(err)
			}
			engine := llm.NewEngine(llm.NewMockProvider("mock"), llm.NewToolRegistry())
			x := &jobsLLMExecution{manager: m, ctx: ctx, cancel: cancel, checkpoint: c}
			x.bind(&cmdRunEnvironment{engine: engine, store: store, llmReq: llm.Request{MaxTurns: 5}})
			x.markReady()
			if err := x.prepare(context.Background(), c.RestartID, c.Service); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil || !engine.SteeringTransitioning() {
				t.Fatal("internal intent did not freeze then cancel")
			}
			// A cancellation acknowledgement alone is not a sealed checkpoint.
			if parked, err := x.seal(&jobsV2RunResult{}); parked || err == nil {
				t.Fatal("source sealed before capture/cleanup")
			}
			x.capture()
			if x.saveErr != nil || x.savedID == "" {
				t.Fatalf("capture failed: %v", x.saveErr)
			}
			if engine.SteeringTransitioning() {
				t.Fatal("finished source retained execution fence")
			}
			if stop {
				if _, err := m.CancelRun(c.RunID); err != nil {
					t.Fatal(err)
				}
			}
			// The source store can close during runtime cleanup before worker sealing.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			parked, err := x.seal(&jobsV2RunResult{SessionID: c.SessionID, TurnCount: 2, InputTokens: 10, Response: "partial progress"})
			if stop {
				if parked || !errors.Is(err, errJobsRestartConflict) {
					t.Fatalf("user Stop sealed: parked=%v err=%v", parked, err)
				}
				return
			}
			if err != nil || !parked {
				t.Fatalf("source did not park: %v", err)
			}
			reopened, err := session.NewSQLiteStore(session.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			grant, err := reopened.ConsumeCommandHandoff(context.Background(), x.savedID, c.Service, "replacement-instance")
			if err != nil {
				t.Fatal(err)
			}
			if err := m.activateLLMRestart(context.Background(), c.RunID, c.RestartID, grant); err != nil {
				t.Fatal(err)
			}
			claimed, ok, err := m.claimNextRun()
			if err != nil || !ok || claimed.restart == nil {
				t.Fatalf("claim missing: ok=%v err=%v", ok, err)
			}
			if claimed.restart.RemainingTurns != 3 || claimed.TurnCount != 2 || claimed.InputTokens != 10 || claimed.Response != "partial progress" {
				t.Fatalf("source accounting lost: %+v", claimed)
			}
		})
	}
}

func TestJobsProgressiveCheckpointUsesActivePassNotTotalTurns(t *testing.T) {
	m, c, _ := jobsRestartFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := session.NewSQLiteStore(session.Config{Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(ctx, &session.Session{ID: c.SessionID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock")
	for i := 0; i < 5; i++ {
		provider.AddTextResponse("work")
	}
	engine := llm.NewEngine(provider, llm.NewToolRegistry())
	x := &jobsLLMExecution{manager: m, ctx: ctx, cancel: cancel, checkpoint: c}
	x.bind(&cmdRunEnvironment{engine: engine, store: store, llmReq: llm.Request{MaxTurns: 5}, req: runpkg.Request{Progressive: &runpkg.ProgressiveOptions{}}})
	x.markReady()
	req := llm.Request{Messages: []llm.Message{llm.UserText("work")}, MaxTurns: 5}
	first, end, err := x.beginProgressivePass(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := runProgressivePass(first, engine, req, progressiveRunOptions{}, newProgressTracker()); err != nil {
			t.Fatal(err)
		}
	}
	end()
	// Natural-finalization contexts may be detached from the job context. The
	// source owner must nevertheless cancel its admitted active pass.
	second, end, err := x.beginProgressivePass(context.WithoutCancel(ctx), req)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	if _, err := runProgressivePass(second, engine, req, progressiveRunOptions{}, newProgressTracker()); err != nil {
		t.Fatal(err)
	}
	if err := x.prepare(context.Background(), c.RestartID, c.Service); err != nil {
		t.Fatal(err)
	}
	if second.Err() == nil {
		t.Fatal("detached pass escaped restart cancellation")
	}
	x.capture()
	result := jobsV2RunResult{}
	if parked, err := x.seal(&result); !parked || err != nil {
		t.Fatal(err)
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	checkpoint, err := readJobsRestart(context.Background(), tx, c.RunID, c.RestartID, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.RemainingTurns != 4 || checkpoint.PassTurnLimit != 5 || result.TurnCount != 5 {
		t.Fatalf("wrong budgets: remaining=%d per-pass=%d total=%d", checkpoint.RemainingTurns, checkpoint.PassTurnLimit, result.TurnCount)
	}
}

func TestJobsProgressiveDetachedPassHonorsUserStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &jobsLLMExecution{ctx: ctx, engine: llm.NewEngine(llm.NewMockProvider("mock"), nil)}
	pass, release, err := x.beginProgressivePass(context.WithoutCancel(ctx), llm.Request{MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cancel()
	select {
	case <-pass.Done():
	case <-time.After(time.Second):
		t.Fatal("user Stop left detached finalization running")
	}
}

func TestJobsRestartSourceRejectsIncompletePersistence(t *testing.T) {
	failure := errors.New("transcript write failed")
	x := &jobsLLMExecution{prepared: true, engine: llm.NewEngine(llm.NewMockProvider("mock"), nil)}
	x.failCheckpoint(failure)
	// No handoff store is installed: attempting to mint a grant would panic.
	x.capture()
	if x.savedID != "" || !errors.Is(x.saveErr, failure) {
		t.Fatal("incomplete transcript became a valid handoff")
	}
	if parked, err := x.seal(&jobsV2RunResult{}); parked || !errors.Is(err, failure) {
		t.Fatalf("incomplete source parked: %v", err)
	}
}
