package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestJobsV2CreateJobSanitizesNotifyOriginFromBody(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	srv := &serveServer{jobsV2: mgr}
	body := `{
		"name":"sanitize-notify-origin",
		"enabled":true,
		"runner_type":"llm",
		"runner_config":{
			"agent_name":"developer",
			"instructions":"do it",
			"cwd":"/tmp/work",
			"notify_when_done":true,
			"notify_origin":{"origin":"web","session_id":"attacker-session"}
		},
		"trigger_type":"manual",
		"trigger_config":{},
		"timeout_seconds":30
	}`
	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var created jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created job: %v", err)
	}
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(created.RunnerConfig, &cfg); err != nil {
		t.Fatalf("decode runner config: %v", err)
	}
	if !cfg.NotifyWhenDone {
		t.Fatal("notify_when_done was not preserved")
	}
	if cfg.NotifyOrigin != nil {
		t.Fatalf("notify_origin should be stripped from request body, got %+v", cfg.NotifyOrigin)
	}
}

func TestJobsV2CreateJobInjectsLoopbackNotifyOriginHeaders(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	srv := &serveServer{jobsV2: mgr}
	body := `{
		"name":"inject-notify-origin",
		"enabled":true,
		"runner_type":"llm",
		"runner_config":{
			"agent_name":"developer",
			"instructions":"do it",
			"cwd":"/tmp/work",
			"notify_when_done":true
		},
		"trigger_type":"manual",
		"trigger_config":{},
		"timeout_seconds":30
	}`
	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tools.QueueAgentNotifyOriginHeader, tools.QueueAgentOriginWeb)
	req.Header.Set(tools.QueueAgentNotifySessionIDHeader, "sess-from-runtime")
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var created jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created job: %v", err)
	}
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(created.RunnerConfig, &cfg); err != nil {
		t.Fatalf("decode runner config: %v", err)
	}
	if cfg.NotifyOrigin == nil || cfg.NotifyOrigin.Origin != "web" || cfg.NotifyOrigin.SessionID != "sess-from-runtime" {
		t.Fatalf("notify_origin = %+v, want injected web session", cfg.NotifyOrigin)
	}
}

func TestJobsV2CreateJobIgnoresNonLoopbackNotifyOriginHeaders(t *testing.T) {
	mgr, err := newJobsV2Manager(":memory:", 0, nil)
	if err != nil {
		t.Fatalf("newJobsV2Manager: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	srv := &serveServer{jobsV2: mgr}
	body := `{
		"name":"ignore-remote-notify-origin",
		"enabled":true,
		"runner_type":"llm",
		"runner_config":{
			"agent_name":"developer",
			"instructions":"do it",
			"cwd":"/tmp/work",
			"notify_when_done":true
		},
		"trigger_type":"manual",
		"trigger_config":{},
		"timeout_seconds":30
	}`
	req := httptest.NewRequest(http.MethodPost, "/v2/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tools.QueueAgentNotifyOriginHeader, tools.QueueAgentOriginWeb)
	req.Header.Set(tools.QueueAgentNotifySessionIDHeader, "attacker-session")
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	srv.handleJobsV2(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", rr.Code, rr.Body.String())
	}
	var created jobsV2Job
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created job: %v", err)
	}
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(created.RunnerConfig, &cfg); err != nil {
		t.Fatalf("decode runner config: %v", err)
	}
	if !cfg.NotifyWhenDone {
		t.Fatal("notify_when_done was not preserved")
	}
	if cfg.NotifyOrigin != nil {
		t.Fatalf("notify_origin should ignore non-loopback headers, got %+v", cfg.NotifyOrigin)
	}
}

func TestJobsV2NotifyWhenDoneAppendsWebNotificationToIdleSession(t *testing.T) {
	store := newServeRuntimeTestStore()
	if err := store.Create(context.Background(), &session.Session{ID: "sess-origin", Origin: session.OriginWeb, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	srv := &serveServer{store: store}
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, srv.notifyJobsV2RunDone)
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	runnerConfig, _ := json.Marshal(jobsV2LLMConfig{
		AgentName:      "developer",
		Instructions:   "do work",
		Cwd:            "/tmp/work",
		NotifyWhenDone: true,
		NotifyOrigin:   &jobsV2NotifyOrigin{Origin: "web", SessionID: "sess-origin"},
	})
	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-web",
		Enabled:       true,
		RunnerType:    jobsV2RunnerLLM,
		RunnerConfig:  runnerConfig,
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

	result := jobsV2RunResult{Response: "STATUS: COMPLETE\nImplemented the feature and verified tests."}
	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, result, nil, run.Attempt); err != nil {
		t.Fatalf("finishRun: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	msgs := store.messages["sess-origin"]
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1: %#v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("notification role = %s, want assistant", msgs[0].Role)
	}
	text := msgs[0].TextContent
	if !strings.Contains(text, job.ID) || !strings.Contains(text, "developer") || !strings.Contains(text, "succeeded") || !strings.Contains(text, "Implemented the feature") {
		t.Fatalf("notification text = %q, missing compact job summary", text)
	}
	if strings.Contains(text, "STATUS: COMPLETE") {
		t.Fatalf("notification text should omit status footer, got %q", text)
	}
}

func TestJobsV2NotifyFailureDoesNotChangeRunStatus(t *testing.T) {
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		return errors.New("notify failed")
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-failure",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{}`),
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

	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, nil, run.Attempt); err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	updated, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if updated.Status != jobsV2RunSucceeded {
		t.Fatalf("status = %s, want succeeded", updated.Status)
	}
	events, _, err := mgr.ListRunEvents(run.ID, 0, 20, 0)
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	foundNotifyFailure := false
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			foundNotifyFailure = true
		}
	}
	if !foundNotifyFailure {
		t.Fatalf("expected notify_failed event, got %#v", events)
	}
}
