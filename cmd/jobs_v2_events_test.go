package cmd

import (
	"testing"
	"time"
)

func TestJobsV2ListRunEventsSinceIDOrdersByInsertion(t *testing.T) {
	mgr := newJobsV2ManagerWithoutLoops(t)
	now := time.Now().UTC()

	_, err := mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_events_since", "events-since", jobsV2RunnerProgram, `{"command":"echo","args":["x"]}`, jobsV2TriggerManual, `{}`, now, now)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?)`,
		"run_events_since", "job_events_since", now, jobsV2RunRunning, now, now)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	insertEvent := func(message string, createdAt time.Time) int64 {
		t.Helper()
		res, err := mgr.db.Exec(`INSERT INTO job_run_events_v2 (run_id, event_type, message, created_at) VALUES (?, 'phase', ?, ?)`,
			"run_events_since", message, createdAt)
		if err != nil {
			t.Fatalf("insert event %q: %v", message, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("last insert id for %q: %v", message, err)
		}
		return id
	}

	firstID := insertEvent("first", now.Add(3*time.Second))
	secondID := insertEvent("second", now.Add(2*time.Second))
	thirdID := insertEvent("third", now.Add(time.Second))

	events, total, err := mgr.ListRunEvents("run_events_since", firstID, 10, 0)
	if err != nil {
		t.Fatalf("ListRunEvents failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].ID != secondID || events[0].Message != "second" || events[1].ID != thirdID || events[1].Message != "third" {
		t.Fatalf("events after since_id not returned in id order: %+v", events)
	}
}
