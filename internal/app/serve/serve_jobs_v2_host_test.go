package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	progressive "github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestJobsV2OnceProgramRunLifecycle(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager failed: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	srv := &serveServer{jobsV2: mgr}

	runAt := time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
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
		time.Sleep(10 * time.Millisecond)
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
	payload := strings.Repeat("payload", 1024)
	completions := make(chan jobsV2Run, 1)
	mgr := newJobsV2HostManager(t, 1, map[jobsV2RunnerType]jobsV2Runner{
		jobsV2RunnerProgram: jobsV2HostRunnerFunc(func(context.Context, jobsV2Job, progressWriter) (jobsV2RunResult, error) {
			return jobsV2RunResult{
				ExitCode:     7,
				Stdout:       payload,
				Stderr:       payload,
				Thinking:     payload,
				Response:     payload,
				ExitReason:   exitReasonException,
				Truncated:    true,
				TurnCount:    3,
				InputTokens:  100,
				OutputTokens: 20,
			}, errors.New("sample error")
		}),
	}, func(_ context.Context, run jobsV2Run, _ jobsV2Job, _ jobsV2RunStatus, _ jobsV2RunResult, _ string, _ bool, _ string) error {
		completions <- run
		return nil
	})

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

	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("trigger job: %v", err)
	}
	completed := waitJobsV2HostCompletion(t, completions)
	if completed.ID != run.ID {
		t.Fatalf("completed run ID = %q, want %q", completed.ID, run.ID)
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

	summaryDetailReq := httptest.NewRequest(http.MethodGet, "/v2/runs/"+run.ID+"?summary=true", nil)
	summaryDetailRR := httptest.NewRecorder()
	srv.handleRunV2ByID(summaryDetailRR, summaryDetailReq)
	if summaryDetailRR.Code != http.StatusOK {
		t.Fatalf("summary detail status = %d, want 200 body=%s", summaryDetailRR.Code, summaryDetailRR.Body.String())
	}
	var summaryDetail jobsV2Run
	if err := json.Unmarshal(summaryDetailRR.Body.Bytes(), &summaryDetail); err != nil {
		t.Fatalf("decode summary detail: %v", err)
	}
	if summaryDetail.Stdout != "" || summaryDetail.Stderr != "" || summaryDetail.Thinking != "" || summaryDetail.Response != "" {
		t.Fatalf("summary detail included output payloads: stdout=%d stderr=%d thinking=%d response=%d", len(summaryDetail.Stdout), len(summaryDetail.Stderr), len(summaryDetail.Thinking), len(summaryDetail.Response))
	}
	if summaryDetail.Status != jobsV2RunFailed || summaryDetail.Error != "sample error" || summaryDetail.ExitReason != exitReasonException {
		t.Fatalf("summary detail lost status metadata: %+v", summaryDetail)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/v2/runs/"+run.ID, nil)
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

func TestJobsV2LLMProgressiveRunStoresEnvelopeAndProgressEvents(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 1, func(ctx context.Context, cfg JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
		if cfg.AgentName != "planner" {
			t.Fatalf("agent_name = %q, want %q", cfg.AgentName, "planner")
		}
		if strings.TrimSpace(cfg.SessionID) == "" {
			t.Fatal("expected session_id to be set for llm job runs")
		}
		onEvent(core.Event{
			Type: core.EventToolCall,
			Tool: &core.ToolCall{
				ID:        "progress-1",
				Name:      "update_progress",
				Arguments: json.RawMessage(`{"state":{"step":"draft"},"reason":"milestone","message":"draft ready"}`),
			},
		})
		onEvent(core.Event{
			Type:        core.EventToolExecEnd,
			ToolCallID:  "progress-1",
			ToolName:    "update_progress",
			ToolSuccess: true,
		})
		return serveJobsExecResult{
			Progressive: &progressive.RunResult{
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
			"cwd":".",
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
	mgr, err := newJobsV2Manager(":memory:", 1, func(ctx context.Context, cfg JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
		return serveJobsExecResult{
			Progressive: &progressive.RunResult{
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
			"cwd":".",
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

func TestJobsV2LLMRunnerAccumulatesStreamingDeltas(t *testing.T) {
	runner := &jobsV2LLMRunner{exec: func(ctx context.Context, cfg JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
		onEvent(core.Event{Type: core.EventReasoningDelta, Text: "thinking"})
		onEvent(core.Event{Type: core.EventTextDelta, Text: "hel"})
		onEvent(core.Event{Type: core.EventTextDelta, Text: "lo"})
		onEvent(core.Event{Type: core.EventUsage, Use: &core.Usage{InputTokens: 3, OutputTokens: 5}})
		return serveJobsExecResult{}, nil
	}}
	job := jobsV2Job{RunnerConfig: json.RawMessage(`{"agent_name":"test","instructions":"say hello","cwd":"."}`)}

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

func TestJobsV2ManagerWrapperKeepsNilExecutorFailure(t *testing.T) {
	notifications := make(chan struct {
		status  jobsV2RunStatus
		errText string
	}, 1)
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 1, nil, func(_ context.Context, _ jobsV2Run, _ jobsV2Job, status jobsV2RunStatus, _ jobsV2RunResult, _ string, _ bool, errText string) error {
		notifications <- struct {
			status  jobsV2RunStatus
			errText string
		}{status: status, errText: errText}
		return nil
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "nil-executor",
		Enabled:       true,
		RunnerType:    jobsV2RunnerLLM,
		RunnerConfig:  json.RawMessage(`{"agent_name":"developer","instructions":"work","cwd":"."}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}

	select {
	case got := <-notifications:
		if got.status != jobsV2RunFailed || got.errText != "llm runner is not configured" {
			t.Fatalf("notification = status %q error %q, want failed nil-executor error", got.status, got.errText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nil-executor completion notification was not called")
	}
	stored, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Status != jobsV2RunFailed || stored.Error != "llm runner is not configured" {
		t.Fatalf("stored run = status %q error %q, want failed nil-executor error", stored.Status, stored.Error)
	}
}

// TestJobsV2LLMRunnerRequiresCwd asserts an llm job is rejected before execution
// when cwd is missing, so a run never silently inherits the jobs server's working
// directory. The executor must not be invoked.
func TestJobsV2LLMRunnerRequiresCwd(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{name: "cwd omitted", config: `{"agent_name":"test","instructions":"hi"}`},
		{name: "cwd blank", config: `{"agent_name":"test","instructions":"hi","cwd":"   "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			runner := &jobsV2LLMRunner{exec: func(context.Context, JobsV2LLMConfig, func(core.Event)) (serveJobsExecResult, error) {
				called = true
				return serveJobsExecResult{}, nil
			}}
			job := jobsV2Job{RunnerConfig: json.RawMessage(tc.config)}
			_, err := runner.Run(context.Background(), job, nil)
			if err == nil || !strings.Contains(err.Error(), "cwd is required") {
				t.Fatalf("Run err = %v, want it to mention cwd is required", err)
			}
			if called {
				t.Fatal("executor was invoked despite missing cwd")
			}
		})
	}
}

func BenchmarkJobsV2LLMRunnerTextDeltaAccumulation(b *testing.B) {
	const deltas = 4096
	const delta = "0123456789abcdef"
	wantLen := deltas * len(delta)

	runner := &jobsV2LLMRunner{exec: func(ctx context.Context, cfg JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
		for i := 0; i < deltas; i++ {
			onEvent(core.Event{Type: core.EventTextDelta, Text: delta})
		}
		return serveJobsExecResult{}, nil
	}}
	job := jobsV2Job{RunnerConfig: json.RawMessage(`{"agent_name":"bench","instructions":"bench","cwd":"."}`)}

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
