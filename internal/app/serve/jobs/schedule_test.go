package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestJobsV2ReconcileMisfiredSchedulesHonorsPolicyAndTrigger(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)
	cutoff := time.Date(2026, time.July, 22, 12, 7, 0, 0, time.UTC)
	past := cutoff.Add(-10 * time.Minute)
	future := cutoff.Add(10 * time.Minute)

	tests := []struct {
		id            string
		enabled       bool
		triggerType   TriggerType
		triggerConfig string
		policy        string
		nextRun       time.Time
		wantEnabled   bool
		wantNext      *time.Time
	}{
		{id: "skip_cron", enabled: true, triggerType: TriggerCron, triggerConfig: `{"expression":"*/5 * * * *","timezone":"UTC"}`, policy: "skip", nextRun: past, wantEnabled: true, wantNext: timePointer(time.Date(2026, time.July, 22, 12, 10, 0, 0, time.UTC))},
		{id: "skip_once", enabled: true, triggerType: TriggerOnce, triggerConfig: `{"run_at":"2026-07-22T11:57:00Z"}`, policy: "skip", nextRun: past, wantEnabled: false},
		{id: "run_cron", enabled: true, triggerType: TriggerCron, triggerConfig: `{"expression":"*/5 * * * *","timezone":"UTC"}`, policy: "run", nextRun: past, wantEnabled: true, wantNext: timePointer(past)},
		{id: "run_once", enabled: true, triggerType: TriggerOnce, triggerConfig: `{"run_at":"2026-07-22T11:57:00Z"}`, policy: "run", nextRun: past, wantEnabled: true, wantNext: timePointer(past)},
		{id: "future_skip", enabled: true, triggerType: TriggerCron, triggerConfig: `{"expression":"*/5 * * * *","timezone":"UTC"}`, policy: "skip", nextRun: future, wantEnabled: true, wantNext: timePointer(future)},
		{id: "disabled_skip", enabled: false, triggerType: TriggerCron, triggerConfig: `{"expression":"*/5 * * * *","timezone":"UTC"}`, policy: "skip", nextRun: past, wantEnabled: false, wantNext: timePointer(past)},
		{id: "manual_skip", enabled: true, triggerType: TriggerManual, triggerConfig: `{}`, policy: "skip", nextRun: past, wantEnabled: true, wantNext: timePointer(past)},
	}
	for _, tc := range tests {
		_, err := mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, schedule_timezone, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'UTC', 'forbid', 1, 30, ?, ?, ?, ?)`,
			tc.id, tc.id, boolToInt(tc.enabled), RunnerProgram, `{"command":"true"}`, tc.triggerType, tc.triggerConfig, tc.policy, tc.nextRun, cutoff, cutoff)
		if err != nil {
			t.Fatalf("insert %s: %v", tc.id, err)
		}
	}

	if err := mgr.reconcileMisfiredSchedules(cutoff); err != nil {
		t.Fatalf("reconcileMisfiredSchedules: %v", err)
	}
	if err := mgr.reconcileMisfiredSchedules(cutoff); err != nil {
		t.Fatalf("second reconcileMisfiredSchedules: %v", err)
	}

	for _, tc := range tests {
		var enabled bool
		var next *time.Time
		if err := mgr.db.QueryRow(`SELECT enabled, next_run_at FROM jobs_v2 WHERE id = ?`, tc.id).Scan(&enabled, &next); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		}
		if enabled != tc.wantEnabled {
			t.Errorf("%s enabled = %v, want %v", tc.id, enabled, tc.wantEnabled)
		}
		if tc.wantNext == nil {
			if next != nil {
				t.Errorf("%s next_run_at = %v, want nil", tc.id, next)
			}
		} else if next == nil || !next.Equal(*tc.wantNext) {
			t.Errorf("%s next_run_at = %v, want %v", tc.id, next, tc.wantNext)
		}
	}
	var runs int
	if err := mgr.db.QueryRow(`SELECT COUNT(*) FROM job_runs_v2`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("reconciliation created %d runs, want 0", runs)
	}
}

func TestJobsV2CronValidation(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	_, err = mgr.CreateJob(Job{
		Name:         "cron-job",
		Enabled:      true,
		RunnerType:   RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:  TriggerCron,
		TriggerConfig: json.RawMessage(`{
			"expression":"0 0 * * *",
			"timezone":"America/Los_Angeles"
		}`),
	})
	if err != nil {
		t.Fatalf("CreateJob cron failed: %v", err)
	}

	_, err = mgr.CreateJob(Job{
		Name:         "bad-cron-job",
		Enabled:      true,
		RunnerType:   RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:  TriggerCron,
		TriggerConfig: json.RawMessage(`{
			"expression":"bad expr",
			"timezone":"America/Los_Angeles"
		}`),
	})
	if err == nil {
		t.Fatalf("expected invalid cron expression error")
	}
}

func TestParseTriggerConfigCronUsesScheduleTimezone(t *testing.T) {
	cfg, err := parseTriggerConfig(TriggerCron, json.RawMessage(`{
		"expression":"0 0 * * *",
		"timezone":"UTC"
	}`), "America/Los_Angeles")
	if err != nil {
		t.Fatalf("parseTriggerConfig failed: %v", err)
	}
	if cfg.Timezone != "America/Los_Angeles" {
		t.Fatalf("timezone = %q, want %q", cfg.Timezone, "America/Los_Angeles")
	}
}

func TestJobsV2CronCreateUsesScheduleTimezoneField(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(Job{
		Name:             "cron-with-schedule-timezone",
		Enabled:          true,
		RunnerType:       RunnerProgram,
		RunnerConfig:     json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:      TriggerCron,
		TriggerConfig:    json.RawMessage(`{"expression":"0 0 * * *"}`),
		ScheduleTimezone: "America/Los_Angeles",
	})
	if err != nil {
		t.Fatalf("CreateJob cron with schedule_timezone failed: %v", err)
	}
	if job.ScheduleTimezone != "America/Los_Angeles" {
		t.Fatalf("schedule_timezone = %q, want %q", job.ScheduleTimezone, "America/Los_Angeles")
	}
	if job.NextRunAt == nil {
		t.Fatalf("next_run_at = nil, want scheduled time")
	}
}

func TestJobsV2RetentionPrunesOldData(t *testing.T) {
	// This exercises pruning directly; live scheduler/worker loops would race
	// with the test's retention settings and fixture insertion.
	mgr := newJobsV2ManagerWithoutLoops(t)

	// Tight retention for test.
	mgr.retentionRunDays = 1
	mgr.retentionEventDays = 1
	mgr.retentionMaxRunsJob = 1

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	_, err := mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_retention_1", "retention-job", RunnerProgram, `{"command":"echo","args":["x"]}`, TriggerManual, `{}`, now, now)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_old", "job_retention_1", old, RunSucceeded, old, old, old)
	if err != nil {
		t.Fatalf("insert old run: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_recent", "job_retention_1", recent, RunSucceeded, recent, recent, recent)
	if err != nil {
		t.Fatalf("insert recent run: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_run_events_v2 (run_id, event_type, message, created_at) VALUES (?, 'queued', 'old event', ?)`,
		"run_recent", old)
	if err != nil {
		t.Fatalf("insert old event: %v", err)
	}

	if err := mgr.pruneOldData(now); err != nil {
		t.Fatalf("pruneOldData failed: %v", err)
	}

	var runsCount int
	if err := mgr.db.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE job_id = ?`, "job_retention_1").Scan(&runsCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runsCount != 1 {
		t.Fatalf("runsCount = %d, want 1", runsCount)
	}
	var remainingRunID string
	if err := mgr.db.QueryRow(`SELECT id FROM job_runs_v2 WHERE job_id = ?`, "job_retention_1").Scan(&remainingRunID); err != nil {
		t.Fatalf("get remaining run: %v", err)
	}
	if remainingRunID != "run_recent" {
		t.Fatalf("remaining run = %q, want run_recent", remainingRunID)
	}

	var eventsCount int
	if err := mgr.db.QueryRow(`SELECT COUNT(1) FROM job_run_events_v2 WHERE run_id = ?`, "run_recent").Scan(&eventsCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventsCount != 0 {
		t.Fatalf("eventsCount = %d, want 0 after event retention pruning", eventsCount)
	}
}

func TestJobsV2RetentionPrunesOldQueueAgentJobs(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)

	mgr.retentionRunDays = 1
	mgr.retentionEventDays = 1
	mgr.retentionMaxRunsJob = 10

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)
	labels := tools.QueueAgentEphemeralJobLabelsJSON
	var err error

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, labels, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'allow', 1, 60, 'run', ?, ?, ?)`,
		"job_queue_agent_old", "queue-agent-old", RunnerLLM, `{"agent_name":"developer","instructions":"old","cwd":"/tmp"}`, TriggerManual, `{}`, labels, old, old)
	if err != nil {
		t.Fatalf("insert old queue_agent job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_queue_agent_old", "job_queue_agent_old", old, RunSucceeded, old, old, old)
	if err != nil {
		t.Fatalf("insert old queue_agent run: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, labels, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'allow', 1, 60, 'run', ?, ?, ?)`,
		"job_queue_agent_recent", "queue-agent-recent", RunnerLLM, `{"agent_name":"developer","instructions":"recent","cwd":"/tmp"}`, TriggerManual, `{}`, labels, recent, recent)
	if err != nil {
		t.Fatalf("insert recent queue_agent job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_queue_agent_recent", "job_queue_agent_recent", recent, RunSucceeded, recent, recent, recent)
	if err != nil {
		t.Fatalf("insert recent queue_agent run: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, labels, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'allow', 1, 60, 'run', ?, ?, ?)`,
		"job_queue_agent_running", "queue-agent-running", RunnerLLM, `{"agent_name":"developer","instructions":"running","cwd":"/tmp"}`, TriggerManual, `{}`, labels, old, old)
	if err != nil {
		t.Fatalf("insert running queue_agent job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?)`,
		"run_queue_agent_running", "job_queue_agent_running", old, RunRunning, old, old)
	if err != nil {
		t.Fatalf("insert running queue_agent run: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, labels, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'allow', 1, 60, 'run', ?, ?, ?)`,
		"job_queue_agent_untriggered", "queue-agent-untriggered", RunnerLLM, `{"agent_name":"developer","instructions":"untriggered","cwd":"/tmp"}`, TriggerManual, `{}`, labels, old, old)
	if err != nil {
		t.Fatalf("insert untriggered queue_agent job: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, labels, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'allow', 1, 60, 'run', ?, ?, ?)`,
		"job_queue_agent_extra_labels", "queue-agent-extra-labels", RunnerLLM, `{"agent_name":"developer","instructions":"old","cwd":"/tmp"}`, TriggerManual, `{}`, `{"other":true,"term_llm_queue_agent":"ephemeral"}`, old, old)
	if err != nil {
		t.Fatalf("insert extra-label queue_agent job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_queue_agent_extra_labels", "job_queue_agent_extra_labels", old, RunSucceeded, old, old, old)
	if err != nil {
		t.Fatalf("insert extra-label queue_agent run: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_manual_old", "manual-old", RunnerProgram, `{"command":"echo","args":["x"]}`, TriggerManual, `{}`, old, old)
	if err != nil {
		t.Fatalf("insert manual control job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_manual_old", "job_manual_old", old, RunSucceeded, old, old, old)
	if err != nil {
		t.Fatalf("insert manual control run: %v", err)
	}

	if err := mgr.pruneOldData(now); err != nil {
		t.Fatalf("pruneOldData failed: %v", err)
	}

	assertJobExists := func(jobID string, want bool) {
		t.Helper()
		var count int
		if err := mgr.db.QueryRow(`SELECT COUNT(1) FROM jobs_v2 WHERE id = ?`, jobID).Scan(&count); err != nil {
			t.Fatalf("count job %s: %v", jobID, err)
		}
		if got := count == 1; got != want {
			t.Fatalf("job %s exists = %v, want %v", jobID, got, want)
		}
	}
	assertRunExists := func(runID string, want bool) {
		t.Helper()
		var count int
		if err := mgr.db.QueryRow(`SELECT COUNT(1) FROM job_runs_v2 WHERE id = ?`, runID).Scan(&count); err != nil {
			t.Fatalf("count run %s: %v", runID, err)
		}
		if got := count == 1; got != want {
			t.Fatalf("run %s exists = %v, want %v", runID, got, want)
		}
	}

	assertJobExists("job_queue_agent_old", false)
	assertRunExists("run_queue_agent_old", false)
	assertJobExists("job_queue_agent_recent", true)
	assertRunExists("run_queue_agent_recent", true)
	assertJobExists("job_queue_agent_running", true)
	assertRunExists("run_queue_agent_running", true)
	assertJobExists("job_queue_agent_untriggered", false)
	assertJobExists("job_queue_agent_extra_labels", false)
	assertRunExists("run_queue_agent_extra_labels", false)
	assertJobExists("job_manual_old", true)
	assertRunExists("run_manual_old", false)
}

func TestJobsV2TriggerJobRespectsMaxConcurrentRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(Job{
		Name:              "manual-bounded-concurrency",
		Enabled:           true,
		RunnerType:        RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:       TriggerManual,
		TriggerConfig:     json.RawMessage(`{}`),
		ConcurrencyPolicy: "allow",
		MaxConcurrentRuns: 2,
		TimeoutSeconds:    30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	now := time.Now().UTC()
	for _, runID := range []string{"run_existing_1", "run_existing_2"} {
		_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?)`,
			runID, job.ID, now, RunRunning, now, now)
		if err != nil {
			t.Fatalf("insert active run %s: %v", runID, err)
		}
	}

	if _, err := mgr.TriggerJob(job.ID); err == nil {
		t.Fatal("TriggerJob should reject runs above max_concurrent_runs")
	}

	active, err := mgr.countActiveRuns(job.ID)
	if err != nil {
		t.Fatalf("countActiveRuns failed: %v", err)
	}
	if active != 2 {
		t.Fatalf("active runs = %d, want 2", active)
	}
}

func TestJobsV2EnqueueRunWithConcurrencyLimitSerializesConcurrentEnqueues(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(Job{
		Name:              "serialized-concurrency-gate",
		Enabled:           true,
		RunnerType:        RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:       TriggerManual,
		TriggerConfig:     json.RawMessage(`{}`),
		ConcurrencyPolicy: "forbid",
		MaxConcurrentRuns: 1,
		TimeoutSeconds:    30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})

	var firstRunID string
	var firstQueued bool
	var firstErr error
	go func() {
		defer close(firstDone)
		firstRunID, _, firstQueued, firstErr = mgr.enqueueRunWithConcurrencyLimit(job, "manual", time.Now().UTC(), nil, func(*sql.Tx, string) error {
			close(started)
			<-release
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first enqueue to hold the concurrency gate")
	}

	type enqueueResult struct {
		runID  string
		active int
		queued bool
		err    error
	}
	secondCh := make(chan enqueueResult, 1)
	go func() {
		runID, active, queued, err := mgr.enqueueRunWithConcurrencyLimit(job, "manual", time.Now().UTC(), nil, nil)
		secondCh <- enqueueResult{runID: runID, active: active, queued: queued, err: err}
	}()

	select {
	case result := <-secondCh:
		t.Fatalf("second enqueue returned before first released the concurrency gate: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first enqueue to finish")
	}
	if firstErr != nil {
		t.Fatalf("first enqueue failed: %v", firstErr)
	}
	if !firstQueued {
		t.Fatal("first enqueue should queue a run")
	}
	if firstRunID == "" {
		t.Fatal("first enqueue returned empty run ID")
	}

	var second enqueueResult
	select {
	case second = <-secondCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second enqueue result")
	}
	if second.err != nil {
		t.Fatalf("second enqueue failed: %v", second.err)
	}
	if second.queued {
		t.Fatal("second enqueue should observe the queued run and refuse to queue another")
	}
	if second.active != 1 {
		t.Fatalf("second enqueue saw %d active runs, want 1", second.active)
	}
	if second.runID != "" {
		t.Fatalf("second enqueue returned run ID %q, want empty", second.runID)
	}

	active, err := mgr.countActiveRuns(job.ID)
	if err != nil {
		t.Fatalf("countActiveRuns failed: %v", err)
	}
	if active != 1 {
		t.Fatalf("active runs = %d, want 1", active)
	}
}

func TestJobsV2ScheduledEnqueueRollsBackWhenScheduleAdvancementFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs_v2.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := InitSchema(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("execJobsV2Schema failed: %v", err)
	}

	mgr := &Manager{db: db}
	runAt := time.Now().UTC().Add(-time.Minute)
	job, err := mgr.CreateJob(Job{
		Name:              "atomic-scheduled-enqueue",
		Enabled:           true,
		RunnerType:        RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"true"}`),
		TriggerType:       TriggerOnce,
		TriggerConfig:     json.RawMessage(`{"run_at":"` + runAt.Format(time.RFC3339Nano) + `"}`),
		ConcurrencyPolicy: "forbid",
		MaxConcurrentRuns: 1,
		MisfirePolicy:     MisfireRun,
		TimeoutSeconds:    30,
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("CreateJob failed: %v", err)
	}

	advanceErr := errors.New("injected schedule advancement failure")
	_, _, _, err = mgr.enqueueRunWithConcurrencyLimit(job, "schedule", runAt, nil, func(*sql.Tx, string) error {
		return advanceErr
	})
	if !errors.Is(err, advanceErr) {
		_ = db.Close()
		t.Fatalf("enqueue error = %v, want %v", err, advanceErr)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database after failed enqueue: %v", err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	mgr = &Manager{db: db}

	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_runs_v2 WHERE job_id = ?`, job.ID).Scan(&runs); err != nil {
		t.Fatalf("count runs after reopen: %v", err)
	}
	if runs != 0 {
		t.Fatalf("runs after rolled-back enqueue = %d, want 0", runs)
	}
	job, err = mgr.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob after reopen: %v", err)
	}
	if !job.Enabled || job.NextRunAt == nil {
		t.Fatalf("job after rolled-back enqueue = enabled:%t next_run_at:%v, want still due", job.Enabled, job.NextRunAt)
	}

	if err := mgr.scheduleOne(job, time.Now().UTC()); err != nil {
		t.Fatalf("scheduleOne after rollback: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_runs_v2 WHERE job_id = ?`, job.ID).Scan(&runs); err != nil {
		t.Fatalf("count runs after scheduleOne: %v", err)
	}
	if runs != 1 {
		t.Fatalf("runs after scheduleOne = %d, want 1", runs)
	}
	job, err = mgr.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob after scheduleOne: %v", err)
	}
	if job.Enabled || job.NextRunAt != nil {
		t.Fatalf("job after scheduleOne = enabled:%t next_run_at:%v, want disabled", job.Enabled, job.NextRunAt)
	}
}

func TestJobsV2ScheduleOneRespectsMaxConcurrentRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(Job{
		Name:              "cron-bounded-concurrency",
		Enabled:           true,
		RunnerType:        RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:       TriggerCron,
		TriggerConfig:     json.RawMessage(`{"expression":"* * * * *","timezone":"UTC"}`),
		ConcurrencyPolicy: "allow",
		MaxConcurrentRuns: 2,
		TimeoutSeconds:    30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	now := time.Now().UTC()
	for _, runID := range []string{"run_existing_1", "run_existing_2"} {
		_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'schedule', ?, ?, ?, ?)`,
			runID, job.ID, now, RunRunning, now, now)
		if err != nil {
			t.Fatalf("insert active run %s: %v", runID, err)
		}
	}

	if err := mgr.scheduleOne(job, now); err != nil {
		t.Fatalf("scheduleOne failed: %v", err)
	}

	active, err := mgr.countActiveRuns(job.ID)
	if err != nil {
		t.Fatalf("countActiveRuns failed: %v", err)
	}
	if active != 2 {
		t.Fatalf("active runs = %d, want 2", active)
	}
}

func TestJobsV2SchedulerAndWorkerDelayUseTimers(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	if got := mgr.nextSchedulerDelay(now); got != jobsV2SchedulerIdleDelay {
		t.Fatalf("empty scheduler delay = %v, want %v", got, jobsV2SchedulerIdleDelay)
	}
	if got := mgr.nextWorkerDelay(now); got != jobsV2WorkerIdleDelay {
		t.Fatalf("empty worker delay = %v, want %v", got, jobsV2WorkerIdleDelay)
	}

	nextJobRun := now.Add(2500 * time.Millisecond)
	_, err := mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, next_run_at, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 30, 'skip', ?, ?, ?)`,
		"job_timer_delay", "timer-delay", RunnerProgram, `{"command":"echo","args":["x"]}`, TriggerOnce, `{}`, nextJobRun, now, now)
	if err != nil {
		t.Fatalf("insert scheduled job: %v", err)
	}
	if got := mgr.nextSchedulerDelay(now); got < 2*time.Second || got > 3*time.Second {
		t.Fatalf("future scheduler delay = %v, want about 2.5s", got)
	}
	_, err = mgr.db.Exec(`UPDATE jobs_v2 SET next_run_at = ? WHERE id = ?`, now.Add(-time.Second), "job_timer_delay")
	if err != nil {
		t.Fatalf("mark scheduled job overdue: %v", err)
	}
	if got := mgr.nextSchedulerDelay(now); got != jobsV2SchedulerMinDelay {
		t.Fatalf("overdue scheduler delay = %v, want %v", got, jobsV2SchedulerMinDelay)
	}

	nextQueuedRun := now.Add(1500 * time.Millisecond)
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'retry', ?, ?, ?, ?)`,
		"run_timer_delay", "job_timer_delay", nextQueuedRun, RunQueued, now, now)
	if err != nil {
		t.Fatalf("insert queued run: %v", err)
	}
	if got := mgr.nextWorkerDelay(now); got < time.Second || got > 2*time.Second {
		t.Fatalf("future worker delay = %v, want about 1.5s", got)
	}
	_, err = mgr.db.Exec(`UPDATE job_runs_v2 SET scheduled_for = ? WHERE id = ?`, now.Add(-time.Second), "run_timer_delay")
	if err != nil {
		t.Fatalf("mark queued run overdue: %v", err)
	}
	if got := mgr.nextWorkerDelay(now); got != jobsV2WorkerMinDelay {
		t.Fatalf("overdue worker delay = %v, want %v", got, jobsV2WorkerMinDelay)
	}
}

func TestJobsV2DelayQueriesUseShortErrorDelay(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)
	if err := mgr.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := mgr.nextSchedulerDelayWithError(time.Now().UTC()); err == nil {
		t.Fatal("nextSchedulerDelayWithError returned nil error for closed DB")
	}
	if got := mgr.nextSchedulerDelay(time.Now().UTC()); got != jobsV2SchedulerErrorDelay {
		t.Fatalf("scheduler delay after query error = %v, want %v", got, jobsV2SchedulerErrorDelay)
	}

	if _, err := mgr.nextWorkerDelayWithError(time.Now().UTC()); err == nil {
		t.Fatal("nextWorkerDelayWithError returned nil error for closed DB")
	}
	if got := mgr.nextWorkerDelay(time.Now().UTC()); got != jobsV2WorkerErrorDelay {
		t.Fatalf("worker delay after query error = %v, want %v", got, jobsV2WorkerErrorDelay)
	}
}

func TestJobsV2MaybeRunCleanupRetriesAfterFailure(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)
	mgr.cleanupInterval = time.Hour
	mgr.retentionEventDays = 1

	if err := mgr.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	now := time.Now().UTC()
	if err := mgr.maybeRunCleanup(now); err == nil {
		t.Fatal("maybeRunCleanup returned nil error for closed DB")
	}
	if !mgr.lastCleanupCompleted.IsZero() {
		t.Fatalf("lastCleanupCompleted = %v, want zero after failed cleanup", mgr.lastCleanupCompleted)
	}

	if err := mgr.maybeRunCleanup(now.Add(time.Minute)); err == nil {
		t.Fatal("maybeRunCleanup suppressed retry after failed cleanup")
	}
}
