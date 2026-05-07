package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func BenchmarkJobsV2ListRuns(b *testing.B) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		b.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_bench_runs", "bench-runs", jobsV2RunnerProgram, `{"command":"echo","args":["x"]}`, jobsV2TriggerManual, `{}`, now, now)
	if err != nil {
		b.Fatalf("insert job: %v", err)
	}

	payload := strings.Repeat("x", 64<<10)
	const rows = 100
	const limit = 50
	for i := 0; i < rows; i++ {
		created := now.Add(-time.Duration(i) * time.Second)
		_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, started_at, finished_at, exit_code, stdout, stderr, thinking, response, exit_reason, turn_count, input_tokens, output_tokens, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, 2, 100, 20, ?, ?)`,
			fmt.Sprintf("run_bench_%03d", i), "job_bench_runs", created, jobsV2RunSucceeded, created, created, payload, payload, payload, payload, exitReasonNatural, created, created)
		if err != nil {
			b.Fatalf("insert run %d: %v", i, err)
		}
	}

	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			runs, total, err := mgr.ListRuns("job_bench_runs", limit, 0)
			if err != nil {
				b.Fatalf("ListRuns failed: %v", err)
			}
			if total != rows || len(runs) != limit {
				b.Fatalf("got total=%d len=%d, want total=%d len=%d", total, len(runs), rows, limit)
			}
			if len(runs[0].Stdout) != len(payload) || len(runs[0].Response) != len(payload) {
				b.Fatalf("full list did not load output payloads")
			}
		}
	})

	b.Run("summary", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			runs, total, err := mgr.ListRunSummaries("job_bench_runs", limit, 0)
			if err != nil {
				b.Fatalf("ListRunSummaries failed: %v", err)
			}
			if total != rows || len(runs) != limit {
				b.Fatalf("got total=%d len=%d, want total=%d len=%d", total, len(runs), rows, limit)
			}
			if runs[0].Stdout != "" || runs[0].Response != "" {
				b.Fatalf("summary list loaded output payloads")
			}
		}
	})
}

func BenchmarkJobsV2ListRunEventsSince(b *testing.B) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		b.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_bench_events", "bench-events", jobsV2RunnerProgram, `{"command":"echo","args":["x"]}`, jobsV2TriggerManual, `{}`, now, now)
	if err != nil {
		b.Fatalf("insert job: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?)`,
		"run_bench_events", "job_bench_events", now, jobsV2RunRunning, now, now)
	if err != nil {
		b.Fatalf("insert run: %v", err)
	}

	const eventsCount = 100_000
	tx, err := mgr.db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO job_run_events_v2 (run_id, event_type, message, created_at) VALUES (?, 'phase', ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		b.Fatalf("prepare event insert: %v", err)
	}
	var sinceID int64
	for i := 0; i < eventsCount; i++ {
		createdAt := now.Add(time.Duration(i) * time.Millisecond)
		res, err := stmt.Exec("run_bench_events", fmt.Sprintf("event %d", i), createdAt)
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			b.Fatalf("insert event %d: %v", i, err)
		}
		if i == eventsCount-100 {
			firstTailID, err := res.LastInsertId()
			if err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				b.Fatalf("last insert id for event %d: %v", i, err)
			}
			sinceID = firstTailID - 1
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		b.Fatalf("close event insert stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit events: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		events, total, err := mgr.ListRunEvents("run_bench_events", sinceID, 100, 0)
		if err != nil {
			b.Fatalf("ListRunEvents failed: %v", err)
		}
		if len(events) != 100 || total != 100 {
			b.Fatalf("got total=%d len=%d, want total=100 len=100", total, len(events))
		}
	}
}
