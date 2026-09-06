package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func jobsRestartFixture(t *testing.T, paths ...string) (*jobsV2Manager, jobsRestartCheckpoint, session.CommandHandoff) {
	t.Helper()
	path := ":memory:"
	if len(paths) > 0 {
		path = paths[0]
	}
	m, err := newJobsV2Manager(path, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if err := initJobsRestartJournal(context.Background(), m.db); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(jobsV2LLMConfig{AgentName: "fixture", Instructions: "original", Cwd: t.TempDir(), SessionID: "source-session"})
	job, err := m.CreateJob(jobsV2Job{Name: "checkpoint", Enabled: true, RunnerType: jobsV2RunnerLLM, RunnerConfig: cfg, TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.TriggerJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`UPDATE job_runs_v2 SET status=?,worker_id=?,session_id=? WHERE id=?`, jobsV2RunRunning, m.workerID, "source-session", run.ID); err != nil {
		t.Fatal(err)
	}
	c := jobsRestartCheckpoint{RemainingTurns: 8, RunID: run.ID, RestartID: "restart", Service: "service", SourceInstance: "old-instance", SessionID: "source-session", Job: job, Deadline: time.Now().Add(time.Minute)}
	grant := session.CommandHandoff{ID: "handoff", Service: c.Service, SourceInstance: c.SourceInstance, SessionID: c.SessionID}
	return m, c, grant
}

func TestJobsRestartJournalAtomicClaim(t *testing.T) {
	m, c, grant := jobsRestartFixture(t)
	ctx := context.Background()
	if err := m.prepareLLMRestart(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); !errors.Is(err, errJobsRestartConflict) {
		t.Fatalf("unsealed intent adopted: %v", err)
	}
	if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{}); err != nil {
		t.Fatal(err)
	}
	wrong := grant
	wrong.SourceInstance = "other"
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, wrong); !errors.Is(err, errJobsRestartConflict) {
		t.Fatalf("wrong grant adopted: %v", err)
	}
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); err != nil {
		t.Fatal(err)
	}
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); !errors.Is(err, errJobsRestartConflict) {
		t.Fatalf("duplicate activation: %v", err)
	}
	// Roll back a claim: the checkpoint must remain available, and no new run
	// or attempt is created by recovery.
	for _, commit := range []bool{false, true} {
		tx, err := m.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`UPDATE job_runs_v2 SET status=? WHERE id=? AND status=?`, jobsV2RunClaimed, c.RunID, jobsV2RunQueued); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		got, err := takeLLMRestart(ctx, tx, c.RunID)
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if got == nil || got.SessionID != c.SessionID || got.Job.ID != c.Job.ID || !got.Deadline.Equal(c.Deadline) {
			tx.Rollback()
			t.Fatalf("checkpoint lost: %+v", got)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if got, err := takeLLMRestart(ctx, tx, c.RunID); err != nil || got != nil {
		t.Fatalf("checkpoint consumed twice: %+v %v", got, err)
	}
}

func TestJobsRestartJournalUserStopWinsEveryBoundary(t *testing.T) {
	for _, stage := range []string{"before-prepare", "before-seal", "before-activate", "before-claim"} {
		t.Run(stage, func(t *testing.T) {
			m, c, grant := jobsRestartFixture(t)
			ctx := context.Background()
			stop := func() {
				t.Helper()
				if _, err := m.CancelRun(c.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "before-prepare" {
				stop()
				if err := m.prepareLLMRestart(ctx, c); !errors.Is(err, errJobsRestartConflict) {
					t.Fatal(err)
				}
				return
			}
			if err := m.prepareLLMRestart(ctx, c); err != nil {
				t.Fatal(err)
			}
			if stage == "before-seal" {
				stop()
				if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{}); !errors.Is(err, errJobsRestartConflict) {
					t.Fatal(err)
				}
				return
			}
			if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{}); err != nil {
				t.Fatal(err)
			}
			if stage == "before-activate" {
				stop()
				if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); !errors.Is(err, errJobsRestartConflict) {
					t.Fatal(err)
				}
				return
			}
			if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); err != nil {
				t.Fatal(err)
			}
			stop()
			tx, err := m.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := takeLLMRestart(ctx, tx, c.RunID); !errors.Is(err, errJobsRestartConflict) {
				t.Fatalf("cancelled run adopted: %v", err)
			}
		})
	}
}

func TestJobsRestartWorkerPreservesCheckpointAcrossUnstartedShutdown(t *testing.T) {
	m, c, grant := jobsRestartFixture(t)
	ctx := context.Background()
	if err := m.prepareLLMRestart(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{}); err != nil {
		t.Fatal(err)
	}
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); err != nil {
		t.Fatal(err)
	}
	run, ok, err := m.claimNextRun()
	if err != nil || !ok || run.restart == nil {
		t.Fatalf("claim lost checkpoint: ok=%v err=%v", ok, err)
	}
	m.requeueClaimedRunAfterShutdown(run.ID)
	run, ok, err = m.claimNextRun()
	if err != nil || !ok || run.restart == nil {
		t.Fatalf("reclaim lost checkpoint: ok=%v err=%v", ok, err)
	}
	calls := 0
	m.runners[jobsV2RunnerLLM] = jobsV2RunnerFunc(func(ctx context.Context, job jobsV2Job, _ progressWriter) (jobsV2RunResult, error) {
		calls++
		checkpoint, _ := ctx.Value(jobsRestartContextKey{}).(*jobsRestartCheckpoint)
		if checkpoint == nil || checkpoint.RunID != c.RunID {
			t.Fatal("worker lost authorized continuation")
		}
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(c.Deadline) {
			t.Fatal("worker reset original deadline")
		}
		var cfg jobsV2LLMConfig
		if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.SessionID != c.SessionID || cfg.MaxTurns != c.RemainingTurns || cfg.Instructions != "original" {
			t.Fatalf("worker changed pinned execution: %+v", cfg)
		}
		return jobsV2RunResult{SessionID: cfg.SessionID, TurnCount: 1}, nil
	})
	// An edit to a recurring job is not permission to replace this run's task.
	if _, err := m.db.Exec(`UPDATE jobs_v2 SET runner_config='{"agent_name":"other","instructions":"wrong task"}' WHERE id=?`, c.Job.ID); err != nil {
		t.Fatal(err)
	}
	m.executeRun(run)
	finished, err := m.GetRun(c.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || finished.Status != jobsV2RunSucceeded || finished.SessionID != c.SessionID || finished.Attempt != run.Attempt {
		t.Fatalf("bad recovered run: calls=%d run=%+v", calls, finished)
	}
	var count int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM job_restart_checkpoints WHERE run_id=?`, c.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("consumed checkpoint left reusable: count=%d err=%v", count, err)
	}
}

func TestJobsRestartWorkerExpiredDeadlineDoesNotRun(t *testing.T) {
	m, c, grant := jobsRestartFixture(t)
	ctx := context.Background()
	c.Deadline = time.Now().Add(-time.Second)
	if err := m.prepareLLMRestart(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{}); err != nil {
		t.Fatal(err)
	}
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); err != nil {
		t.Fatal(err)
	}
	run, ok, err := m.claimNextRun()
	if err != nil || !ok {
		t.Fatal(err)
	}
	m.runners[jobsV2RunnerLLM] = jobsV2RunnerFunc(func(context.Context, jobsV2Job, progressWriter) (jobsV2RunResult, error) {
		t.Error("expired job called runner")
		return jobsV2RunResult{}, nil
	})
	m.executeRun(run)
	finished, err := m.GetRun(c.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != jobsV2RunTimedOut {
		t.Fatalf("expired continuation status=%s", finished.Status)
	}
}

func TestJobsRestartWorkerCrashBeforeAdmissionRetainsAuthorization(t *testing.T) {
	m, c, grant := jobsRestartFixture(t)
	ctx := context.Background()
	if err := m.prepareLLMRestart(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := m.sealLLMRestart(ctx, c.RunID, c.RestartID, grant.ID, jobsV2RunResult{}); err != nil {
		t.Fatal(err)
	}
	if err := m.activateLLMRestart(ctx, c.RunID, c.RestartID, grant); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.claimNextRun(); err != nil || !ok {
		t.Fatal(err)
	}
	if err := m.recoverRuns(); err != nil {
		t.Fatal(err)
	}
	run, ok, err := m.claimNextRun()
	if err != nil || !ok || run.restart == nil {
		t.Fatalf("unstarted recovery lost: ok=%v err=%v", ok, err)
	}
	if run.ID != c.RunID || run.restart.HandoffID != grant.ID {
		t.Fatal("recovery changed run or authorization")
	}
}
