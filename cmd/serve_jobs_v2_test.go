package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
)

func TestResolveSettingsWithPlatform_JobsForServeJobsV2(t *testing.T) {
	cfg := &config.Config{}
	settings, err := ResolveSettingsWithPlatform(cfg, nil, CLIFlags{SystemMessage: "jobs={{platform}}"}, "", "", "", 0, 20, agents.TemplatePlatformJobs)
	if err != nil {
		t.Fatalf("ResolveSettingsWithPlatform() error = %v", err)
	}
	if settings.SystemPrompt != "jobs=jobs" {
		t.Fatalf("SystemPrompt = %q, want %q", settings.SystemPrompt, "jobs=jobs")
	}
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
