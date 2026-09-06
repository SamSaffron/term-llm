package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestJobsRestartAdoptionPrecedesStartupRecovery(t *testing.T) {
	for _, variant := range []string{"replacement", "reclaim", "advanced-source", "user-stop"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "jobs.db")
			m, c, grant := jobsRestartFixture(t, dbPath)
			store, err := session.NewSQLiteStore(session.Config{Path: filepath.Join(dir, "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Create(ctx, &session.Session{ID: c.SessionID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
				t.Fatal(err)
			}
			grant.Payload = json.RawMessage(`[]`)
			if err := m.prepareLLMRestart(ctx, c); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveCommandHandoff(ctx, grant); err != nil {
				t.Fatal(err)
			}
			if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{TurnCount: 2, Response: "checkpoint output"}); err != nil {
				t.Fatal(err)
			}
			setup := jobsRestartSetup{Store: store, RestartID: c.RestartID, Service: c.Service, Instance: "replacement"}
			if variant == "reclaim" {
				setup.Instance = c.SourceInstance
				if err := m.recoverLLMRestarts(ctx, setup, true); err != nil {
					t.Fatal(err)
				}
				run, ok, err := m.claimNextRun()
				if err != nil || !ok || run.restart == nil {
					t.Fatalf("old instance could not recover: %v", err)
				}
				return
			}
			if variant == "advanced-source" {
				if err := store.AddMessage(ctx, c.SessionID, &session.Message{SessionID: c.SessionID, Role: llm.RoleUser, TextContent: "changed source", Parts: []llm.Part{{Type: llm.PartText, Text: "changed source"}}, Sequence: -1}); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "user-stop" {
				if _, err := m.CancelRun(c.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			replacement, err := newJobsV2ManagerWithNotifier(dbPath, 0, nil, nil, func(next *jobsV2Manager) error { return next.recoverLLMRestarts(ctx, setup, false) })
			if variant == "advanced-source" {
				if err == nil {
					replacement.Close()
					t.Fatal("advanced transcript adopted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			run, err := replacement.GetRun(c.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "user-stop" {
				if run.Status != jobsV2RunCancelled {
					t.Fatalf("Stop resurrected: %s", run.Status)
				}
				return
			}
			if run.Status != jobsV2RunQueued || run.Response != "checkpoint output" {
				t.Fatalf("worker-loss recovery ran before adoption: %+v", run)
			}
			claimed, ok, err := replacement.claimNextRun()
			if err != nil || !ok || claimed.restart == nil || claimed.restart.RemainingTurns != 6 {
				t.Fatalf("replacement claim lost checkpoint: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestJobsRestartCompletionNotificationOccursOnlyOnce(t *testing.T) {
	ctx := context.Background()
	m, c, grant := jobsRestartFixture(t)
	store, err := session.NewSQLiteStore(session.Config{Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Create(ctx, &session.Session{ID: c.SessionID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	notifications := make(chan jobsV2Run, 4)
	m.notifyDone = func(_ context.Context, run jobsV2Run, _ jobsV2Job, _ jobsV2RunStatus, _ jobsV2RunResult, _ string, _ bool, _ string) error {
		notifications <- run
		return nil
	}
	grant.Payload = json.RawMessage(`[]`)
	if err := m.prepareLLMRestart(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCommandHandoff(ctx, grant); err != nil {
		t.Fatal(err)
	}
	if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{TurnCount: 2}); err != nil {
		t.Fatal(err)
	}
	if err := m.recoverLLMRestarts(ctx, jobsRestartSetup{Store: store, RestartID: c.RestartID, Service: c.Service, Instance: "new"}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
		t.Fatal("internal interruption notified completion")
	default:
	}
	run, ok, err := m.claimNextRun()
	if err != nil || !ok {
		t.Fatal(err)
	}
	m.runners[jobsV2RunnerLLM] = jobsV2RunnerFunc(func(context.Context, jobsV2Job, progressWriter) (jobsV2RunResult, error) {
		return jobsV2RunResult{SessionID: c.SessionID, TurnCount: 1, Response: "done"}, nil
	})
	m.executeRun(run)
	select {
	case finished := <-notifications:
		if finished.ID != c.RunID || finished.Status != jobsV2RunSucceeded || finished.TurnCount != 3 {
			t.Fatalf("wrong completion: %+v", finished)
		}
	case <-time.After(time.Second):
		t.Fatal("no final notification")
	}
	// Retrying finalization after a successful commit must not notify again.
	if err := m.finishRun(c.RunID, jobsV2RunSucceeded, jobsV2RunResult{}, nil, run.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
		t.Fatal("duplicate completion notification")
	default:
	}
}
