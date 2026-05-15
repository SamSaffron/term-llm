package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestNewJobsV2ManagerDoesNotForceSingleConnectionForFileBackedDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs_v2.db")
	mgr, err := newJobsV2Manager(dbPath, 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	if got := mgr.db.Stats().MaxOpenConnections; got != 0 {
		t.Fatalf("MaxOpenConnections = %d, want 0 for file-backed DB", got)
	}
}

func TestJobsV2RunSummaryUsesCoveringIndex(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	plan := jobsV2ExplainPlan(t, mgr.db, "EXPLAIN QUERY PLAN SELECT "+jobsV2RunSummaryColumns+" FROM job_runs_v2 WHERE job_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?", "job_test", 50, 0)
	if !strings.Contains(plan, "USING COVERING INDEX "+jobsV2RunSummaryIndexName) {
		t.Fatalf("summary query plan = %q, want covering summary index", plan)
	}
}

func TestJobsV2GlobalRunSummaryUsesCoveringIndex(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	plan := jobsV2ExplainPlan(t, mgr.db, "EXPLAIN QUERY PLAN SELECT "+jobsV2RunSummaryColumns+" FROM job_runs_v2 ORDER BY created_at DESC LIMIT ? OFFSET ?", 50, 0)
	if !strings.Contains(plan, "USING COVERING INDEX "+jobsV2RunGlobalSummaryIndexName) {
		t.Fatalf("global summary query plan = %q, want covering global summary index", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("global summary query plan = %q, want no temp sort", plan)
	}
}

func jobsV2ExplainPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return strings.Join(details, "\n")
}

func TestJobsV2OnceProgramRunLifecycle(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}

	runAt := time.Now().Add(1 * time.Second).UTC().Format(time.RFC3339)
	body := `{
		"name":"once-run",
		"enabled":true,
		"runner_type":"program",
		"runner_config":{"command":"echo","args":["hello-jobs-v2"]},
		"trigger_type":"once",
		"trigger_config":{"run_at":"` + runAt + `"},
		"timeout_seconds":30
	}`

	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d body=%s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var created jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("missing job id")
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		listReq := httptest.NewRequest(http.MethodGet, "/v2/runs?job_id="+created.ID, nil)
		listRR := httptest.NewRecorder()
		srv.handleRunsV2(listRR, listReq)
		if listRR.Code != http.StatusOK {
			t.Fatalf("runs status = %d, want 200 body=%s", listRR.Code, listRR.Body.String())
		}

		var listResp struct {
			Data []jobsV2Run `json:"data"`
		}
		if err := json.Unmarshal(listRR.Body.Bytes(), &listResp); err != nil {
			t.Fatalf("decode runs list: %v", err)
		}
		if len(listResp.Data) > 0 {
			run := listResp.Data[0]
			if run.Status == jobsV2RunSucceeded {
				if !strings.Contains(run.Stdout, "hello-jobs-v2") {
					t.Fatalf("stdout = %q, want contains hello-jobs-v2", run.Stdout)
				}
				break
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for one-off job run")
		}
		time.Sleep(100 * time.Millisecond)
	}

	jobReq := httptest.NewRequest(http.MethodGet, "/v2/jobs/"+created.ID, nil)
	jobRR := httptest.NewRecorder()
	srv.handleJobV2ByID(jobRR, jobReq)
	if jobRR.Code != http.StatusOK {
		t.Fatalf("job get status = %d, want 200", jobRR.Code)
	}
	var job jobsV2Job
	if err := json.Unmarshal(jobRR.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.Enabled {
		t.Fatalf("once job should auto-disable after scheduling")
	}
}

func TestJobsV2RunsSummaryOmitsOutputPayload(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "summary-payload",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	now := time.Now().UTC()
	payload := strings.Repeat("payload", 1024)
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, started_at, finished_at, exit_code, error, stdout, stderr, thinking, response, exit_reason, truncated, turn_count, input_tokens, output_tokens, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, 7, 'sample error', ?, ?, ?, ?, ?, 1, 3, 100, 20, ?, ?)`,
		"run_summary_payload", job.ID, now, jobsV2RunFailed, now, now, payload, payload, payload, payload, exitReasonException, now, now)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	srv := &serveServer{jobsV2: mgr}
	listReq := httptest.NewRequest(http.MethodGet, "/v2/runs?summary=1&job_id="+job.ID, nil)
	listRR := httptest.NewRecorder()
	srv.handleRunsV2(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("summary runs status = %d, want 200 body=%s", listRR.Code, listRR.Body.String())
	}
	var listResp struct {
		Data []jobsV2Run `json:"data"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode summary runs list: %v", err)
	}
	if len(listResp.Data) != 1 {
		t.Fatalf("summary runs len = %d, want 1", len(listResp.Data))
	}
	summary := listResp.Data[0]
	if summary.Stdout != "" || summary.Stderr != "" || summary.Thinking != "" || summary.Response != "" {
		t.Fatalf("summary included output payloads: stdout=%d stderr=%d thinking=%d response=%d", len(summary.Stdout), len(summary.Stderr), len(summary.Thinking), len(summary.Response))
	}
	if summary.Error != "sample error" || summary.ExitReason != exitReasonException || !summary.Truncated || summary.TurnCount != 3 || summary.InputTokens != 100 || summary.OutputTokens != 20 {
		t.Fatalf("summary lost metadata: %+v", summary)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/v2/runs/run_summary_payload", nil)
	detailRR := httptest.NewRecorder()
	srv.handleRunV2ByID(detailRR, detailReq)
	if detailRR.Code != http.StatusOK {
		t.Fatalf("detail run status = %d, want 200 body=%s", detailRR.Code, detailRR.Body.String())
	}
	var detail jobsV2Run
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail run: %v", err)
	}
	if detail.Stdout != payload || detail.Stderr != payload || detail.Thinking != payload || detail.Response != payload {
		t.Fatalf("detail run did not preserve full payloads")
	}
}

func TestJobsV2ManualTriggerAndCancel(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}

	createBody := `{
		"name":"manual-sleep",
		"enabled":true,
		"runner_type":"program",
		"runner_config":{"command":"sleep","args":["5"]},
		"trigger_type":"manual",
		"trigger_config":{},
		"timeout_seconds":30
	}`
	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var job jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}

	triggerReq := httptest.NewRequest(http.MethodPost, "/v2/jobs/"+job.ID+"/trigger", nil)
	triggerRR := httptest.NewRecorder()
	srv.handleJobV2ByID(triggerRR, triggerReq)
	if triggerRR.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202 body=%s", triggerRR.Code, triggerRR.Body.String())
	}
	var run jobsV2Run
	if err := json.Unmarshal(triggerRR.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		current, err := mgr.GetRun(run.ID)
		if err == nil && (current.Status == jobsV2RunRunning || current.Status == jobsV2RunClaimed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not start in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/v2/runs/"+run.ID+"/cancel", nil)
	cancelRR := httptest.NewRecorder()
	srv.handleRunV2ByID(cancelRR, cancelReq)
	if cancelRR.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200 body=%s", cancelRR.Code, cancelRR.Body.String())
	}

	// Allow generous time for: SIGKILL delivery → process exit → WaitDelay pipe
	// drain → cmd.Output() return → ctx.Err() check → finishRun DB write → poll.
	deadline = time.Now().Add(10 * time.Second)
	for {
		current, err := mgr.GetRun(run.ID)
		if err == nil && current.Status == jobsV2RunCancelled {
			break
		}
		if time.Now().After(deadline) {
			current, _ := mgr.GetRun(run.ID)
			t.Fatalf("run did not cancel in time, last status=%s", current.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestJobsV2CreateDefaultsEnabledWhenOmitted(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}
	body := `{
		"name":"default-enabled",
		"runner_type":"program",
		"runner_config":{"command":"echo","args":["hello"]},
		"trigger_type":"manual",
		"trigger_config":{}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var job jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if !job.Enabled {
		t.Fatalf("created job should default enabled=true when enabled is omitted")
	}
}

func TestJobsV2CreateRespectsExplicitDisabled(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}
	body := `{
		"name":"explicit-disabled",
		"enabled":false,
		"runner_type":"program",
		"runner_config":{"command":"echo","args":["hello"]},
		"trigger_type":"manual",
		"trigger_config":{}
	}`

	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var job jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.Enabled {
		t.Fatalf("created job should respect explicit enabled=false")
	}
}

func TestJobsV2PatchPreservesEnabledWhenOmitted(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}
	created, err := mgr.CreateJob(jobsV2Job{
		Name:          "patch-enabled",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/v2/jobs/"+created.ID, strings.NewReader(`{"name":"renamed"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobV2ByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	var job jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if !job.Enabled {
		t.Fatalf("patch without enabled should preserve enabled=true")
	}
	if job.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", job.Name)
	}
}

func TestJobsV2PatchRespectsExplicitDisabled(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}
	created, err := mgr.CreateJob(jobsV2Job{
		Name:          "patch-disable",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPatch, "/v2/jobs/"+created.ID, strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobV2ByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	var job jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.Enabled {
		t.Fatalf("patch should respect explicit enabled=false")
	}
}

func TestJobsV2RecoverRunsCancelsCancelRequestedRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "recover-cancel-requested",
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

	startedAt := time.Now().UTC().Add(-time.Second)
	_, err = mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, started_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunCancelRequested, startedAt, run.ID)
	if err != nil {
		t.Fatalf("mark cancel_requested failed: %v", err)
	}

	if err := mgr.recoverRuns(); err != nil {
		t.Fatalf("recoverRuns failed: %v", err)
	}

	recovered, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}
	if recovered.Status != jobsV2RunCancelled {
		t.Fatalf("status = %s, want %s", recovered.Status, jobsV2RunCancelled)
	}
	if recovered.FinishedAt == nil {
		t.Fatalf("finished_at was not set for recovered cancelled run")
	}
}

func TestJobsV2CronValidation(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	_, err = mgr.CreateJob(jobsV2Job{
		Name:         "cron-job",
		Enabled:      true,
		RunnerType:   jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:  jobsV2TriggerCron,
		TriggerConfig: json.RawMessage(`{
			"expression":"0 0 * * *",
			"timezone":"America/Los_Angeles"
		}`),
	})
	if err != nil {
		t.Fatalf("CreateJob cron failed: %v", err)
	}

	_, err = mgr.CreateJob(jobsV2Job{
		Name:         "bad-cron-job",
		Enabled:      true,
		RunnerType:   jobsV2RunnerProgram,
		RunnerConfig: json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:  jobsV2TriggerCron,
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
	cfg, err := parseTriggerConfig(jobsV2TriggerCron, json.RawMessage(`{
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

	job, err := mgr.CreateJob(jobsV2Job{
		Name:             "cron-with-schedule-timezone",
		Enabled:          true,
		RunnerType:       jobsV2RunnerProgram,
		RunnerConfig:     json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:      jobsV2TriggerCron,
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

func TestJobsV2RunEventsEndpoint(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}

	createBody := `{
		"name":"events-echo",
		"enabled":true,
		"runner_type":"program",
		"runner_config":{"command":"echo","args":["hello-events"]},
		"trigger_type":"manual",
		"trigger_config":{},
		"timeout_seconds":30
	}`
	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var job jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}

	triggerReq := httptest.NewRequest(http.MethodPost, "/v2/jobs/"+job.ID+"/trigger", nil)
	triggerRR := httptest.NewRecorder()
	srv.handleJobV2ByID(triggerRR, triggerReq)
	if triggerRR.Code != http.StatusAccepted {
		t.Fatalf("trigger status = %d, want 202 body=%s", triggerRR.Code, triggerRR.Body.String())
	}
	var run jobsV2Run
	if err := json.Unmarshal(triggerRR.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := mgr.GetRun(run.ID)
		if err == nil && current.Status == jobsV2RunSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for run success")
		}
		time.Sleep(50 * time.Millisecond)
	}

	eventsReq := httptest.NewRequest(http.MethodGet, "/v2/runs/"+run.ID+"/events", nil)
	eventsRR := httptest.NewRecorder()
	srv.handleRunV2ByID(eventsRR, eventsReq)
	if eventsRR.Code != http.StatusOK {
		t.Fatalf("events status = %d, want 200 body=%s", eventsRR.Code, eventsRR.Body.String())
	}
	var eventsResp struct {
		Data []jobsV2RunEvent `json:"data"`
	}
	if err := json.Unmarshal(eventsRR.Body.Bytes(), &eventsResp); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(eventsResp.Data) == 0 {
		t.Fatalf("expected run events")
	}

	seen := map[string]bool{}
	for _, ev := range eventsResp.Data {
		seen[ev.EventType] = true
	}
	for _, required := range []string{"queued", "claimed", "running", "succeeded"} {
		if !seen[required] {
			t.Fatalf("expected event type %q in timeline", required)
		}
	}
}

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

func TestJobsV2RetentionPrunesOldData(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	// Tight retention for test.
	mgr.retentionRunDays = 1
	mgr.retentionEventDays = 1
	mgr.retentionMaxRunsJob = 1

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	_, err = mgr.db.Exec(`INSERT INTO jobs_v2 (id, name, enabled, runner_type, runner_config, trigger_type, trigger_config, concurrency_policy, max_concurrent_runs, timeout_seconds, misfire_policy, created_at, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?, 'forbid', 1, 60, 'skip', ?, ?)`,
		"job_retention_1", "retention-job", jobsV2RunnerProgram, `{"command":"echo","args":["x"]}`, jobsV2TriggerManual, `{}`, now, now)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}

	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_old", "job_retention_1", old, jobsV2RunSucceeded, old, old, old)
	if err != nil {
		t.Fatalf("insert old run: %v", err)
	}
	_, err = mgr.db.Exec(`INSERT INTO job_runs_v2 (id, job_id, attempt, trigger, scheduled_for, status, finished_at, created_at, updated_at) VALUES (?, ?, 1, 'manual', ?, ?, ?, ?, ?)`,
		"run_recent", "job_retention_1", recent, jobsV2RunSucceeded, recent, recent, recent)
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

func TestJobsV2LLMProgressiveRunStoresEnvelopeAndProgressEvents(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error) {
		if cfg.AgentName != "planner" {
			t.Fatalf("agent_name = %q, want %q", cfg.AgentName, "planner")
		}
		if strings.TrimSpace(cfg.SessionID) == "" {
			t.Fatal("expected session_id to be set for llm job runs")
		}
		onEvent(llm.Event{
			Type: llm.EventToolCall,
			Tool: &llm.ToolCall{
				ID:        "progress-1",
				Name:      "update_progress",
				Arguments: json.RawMessage(`{"state":{"step":"draft"},"reason":"milestone","message":"draft ready"}`),
			},
		})
		onEvent(llm.Event{
			Type:        llm.EventToolExecEnd,
			ToolCallID:  "progress-1",
			ToolName:    "update_progress",
			ToolSuccess: true,
		})
		return serveJobsExecResult{
			Progressive: &progressiveRunResult{
				ExitReason: exitReasonNatural,
				Finalized:  true,
				Progress: map[string]any{
					"step": "draft",
				},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:       "progressive-llm",
		Enabled:    true,
		RunnerType: jobsV2RunnerLLM,
		RunnerConfig: json.RawMessage(`{
			"agent_name":"planner",
			"instructions":"Plan carefully",
			"progressive":true
		}`),
		TriggerType:    jobsV2TriggerManual,
		TriggerConfig:  json.RawMessage(`{}`),
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := mgr.GetRun(run.ID)
		if err == nil && current.Status == jobsV2RunSucceeded {
			run = current
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for progressive llm run")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Response should be the human-readable output from progressiveOutputText.
	// With no FinalResponse or FallbackText, it falls back to JSON-marshaled progress state.
	var progressState map[string]any
	if err := json.Unmarshal([]byte(run.Response), &progressState); err != nil {
		t.Fatalf("response is not progress JSON: %v", err)
	}
	if got := progressState["step"]; got != "draft" {
		t.Fatalf("progress step = %#v, want %q", got, "draft")
	}
	if run.ExitReason != exitReasonNatural {
		t.Fatalf("exit_reason = %q, want %q", run.ExitReason, exitReasonNatural)
	}
	if strings.TrimSpace(run.SessionID) == "" {
		t.Fatal("expected persisted run session_id to be populated")
	}

	events, _, err := mgr.ListRunEvents(run.ID, 0, 100, 0)
	if err != nil {
		t.Fatalf("ListRunEvents failed: %v", err)
	}

	foundProgressUpdate := false
	for _, ev := range events {
		if ev.EventType == "progress_update" {
			foundProgressUpdate = true
			break
		}
	}
	if !foundProgressUpdate {
		t.Fatalf("expected progress_update event, got %+v", events)
	}
}

func TestJobsV2_ProgressiveLLM_FinalResponseProse(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error) {
		return serveJobsExecResult{
			Progressive: &progressiveRunResult{
				ExitReason:    exitReasonTimeout,
				Finalized:     true,
				FinalResponse: "Here are the top 10 sci-fi audiobooks released in the past year.",
				Progress: map[string]any{
					"entries": []string{"Book A", "Book B"},
				},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:       "progressive-prose",
		Enabled:    true,
		RunnerType: jobsV2RunnerLLM,
		RunnerConfig: json.RawMessage(`{
			"agent_name":"planner",
			"instructions":"Find audiobooks",
			"progressive":true
		}`),
		TriggerType:    jobsV2TriggerManual,
		TriggerConfig:  json.RawMessage(`{}`),
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := mgr.GetRun(run.ID)
		if err == nil && current.Status == jobsV2RunTimedOut {
			run = current
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for progressive prose run")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// When FinalResponse is set, Response should be the prose, not JSON.
	expected := "Here are the top 10 sci-fi audiobooks released in the past year."
	if run.Response != expected {
		t.Fatalf("Response = %q, want prose %q", run.Response, expected)
	}
}

func TestJobsV2TriggerJobRespectsMaxConcurrentRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:              "manual-bounded-concurrency",
		Enabled:           true,
		RunnerType:        jobsV2RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:       jobsV2TriggerManual,
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
			runID, job.ID, now, jobsV2RunRunning, now, now)
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

	job, err := mgr.CreateJob(jobsV2Job{
		Name:              "serialized-concurrency-gate",
		Enabled:           true,
		RunnerType:        jobsV2RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:       jobsV2TriggerManual,
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
		firstRunID, _, firstQueued, firstErr = mgr.enqueueRunWithConcurrencyLimit(job, "manual", time.Now().UTC(), nil, func(string) error {
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

func TestJobsV2ScheduleOneRespectsMaxConcurrentRuns(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:              "cron-bounded-concurrency",
		Enabled:           true,
		RunnerType:        jobsV2RunnerProgram,
		RunnerConfig:      json.RawMessage(`{"command":"echo","args":["x"]}`),
		TriggerType:       jobsV2TriggerCron,
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
			runID, job.ID, now, jobsV2RunRunning, now, now)
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
		"job_timer_delay", "timer-delay", jobsV2RunnerProgram, `{"command":"echo","args":["x"]}`, jobsV2TriggerOnce, `{}`, nextJobRun, now, now)
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
		"run_timer_delay", "job_timer_delay", nextQueuedRun, jobsV2RunQueued, now, now)
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

func newJobsV2ManagerWithoutLoops(t *testing.T) *jobsV2Manager {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := execJobsV2Schema(db); err != nil {
		_ = db.Close()
		t.Fatalf("execJobsV2Schema failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &jobsV2Manager{
		db:                 db,
		workerID:           "worker_test",
		schedulerIdleDelay: jobsV2SchedulerIdleDelay,
		workerIdleDelay:    jobsV2WorkerIdleDelay,
		cleanupInterval:    0,
		cancels:            make(map[string]context.CancelFunc),
	}
}

type testJobsV2Runner struct {
	called bool
}

func (r *testJobsV2Runner) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
	r.called = true
	return jobsV2RunResult{}, nil
}

func TestJobsV2ExecuteRunDoesNotStartWhenManagerClosed(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := execJobsV2Schema(db); err != nil {
		t.Fatalf("execJobsV2Schema failed: %v", err)
	}

	runner := &testJobsV2Runner{}
	mgr := &jobsV2Manager{
		db:       db,
		workerID: "worker_test",
		runners: map[jobsV2RunnerType]jobsV2Runner{
			jobsV2RunnerProgram: runner,
		},
		cancels: make(map[string]context.CancelFunc),
		closed:  true,
	}

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

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob failed: %v", err)
	}
	_, err = mgr.db.Exec(`UPDATE job_runs_v2 SET status = ?, worker_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, jobsV2RunClaimed, mgr.workerID, run.ID)
	if err != nil {
		t.Fatalf("mark run claimed failed: %v", err)
	}
	run, err = mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun failed: %v", err)
	}

	mgr.executeRun(run)

	if runner.called {
		t.Fatal("runner was invoked after manager shutdown")
	}
	if len(mgr.cancels) != 0 {
		t.Fatalf("cancels still tracked after early return: %d", len(mgr.cancels))
	}

	updated, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun after executeRun failed: %v", err)
	}
	if updated.Status != jobsV2RunClaimed {
		t.Fatalf("status = %s, want %s", updated.Status, jobsV2RunClaimed)
	}
	if updated.StartedAt != nil {
		t.Fatal("started_at was set even though closed manager should not start the run")
	}
}

func TestJobsV2LLMRunnerAccumulatesStreamingDeltas(t *testing.T) {
	runner := &jobsV2LLMRunner{exec: func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error) {
		onEvent(llm.Event{Type: llm.EventReasoningDelta, Text: "thinking"})
		onEvent(llm.Event{Type: llm.EventTextDelta, Text: "hel"})
		onEvent(llm.Event{Type: llm.EventTextDelta, Text: "lo"})
		onEvent(llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 3, OutputTokens: 5}})
		return serveJobsExecResult{}, nil
	}}
	job := jobsV2Job{RunnerConfig: json.RawMessage(`{"agent_name":"test","instructions":"say hello"}`)}

	var flushes []string
	res, err := runner.Run(context.Background(), job, func(eventType, message string, data any) {
		if eventType == "response_flush" {
			flushes = append(flushes, message)
		}
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.Response != "hello" {
		t.Fatalf("Response = %q, want %q", res.Response, "hello")
	}
	if res.Thinking != "thinking" {
		t.Fatalf("Thinking = %q, want %q", res.Thinking, "thinking")
	}
	if len(flushes) != 1 || flushes[0] != "hello" {
		t.Fatalf("response flushes = %#v, want [hello]", flushes)
	}
	if res.TurnCount != 1 || res.InputTokens != 3 || res.OutputTokens != 5 {
		t.Fatalf("usage = turns:%d input:%d output:%d, want turns:1 input:3 output:5", res.TurnCount, res.InputTokens, res.OutputTokens)
	}
}

func BenchmarkJobsV2LLMRunnerTextDeltaAccumulation(b *testing.B) {
	const deltas = 4096
	const delta = "0123456789abcdef"
	wantLen := deltas * len(delta)

	runner := &jobsV2LLMRunner{exec: func(ctx context.Context, cfg jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error) {
		for i := 0; i < deltas; i++ {
			onEvent(llm.Event{Type: llm.EventTextDelta, Text: delta})
		}
		return serveJobsExecResult{}, nil
	}}
	job := jobsV2Job{RunnerConfig: json.RawMessage(`{"agent_name":"bench","instructions":"bench"}`)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := runner.Run(context.Background(), job, nil)
		if err != nil {
			b.Fatalf("Run failed: %v", err)
		}
		if len(res.Response) != wantLen {
			b.Fatalf("response length = %d, want %d", len(res.Response), wantLen)
		}
	}
}
