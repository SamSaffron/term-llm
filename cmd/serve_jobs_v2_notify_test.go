package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/providerhttp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type jobsV2NotifyTransientTestError struct{}

func (jobsV2NotifyTransientTestError) Error() string {
	return "transient notify failure"
}

func (jobsV2NotifyTransientTestError) RetryAfterDelay() (time.Duration, bool) {
	return time.Millisecond, true
}

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

	waitForServeCondition(t, 2*time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.messages["sess-origin"]) == 1
	}, "completion notification to idle web session")

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

func TestJobsV2NotifyWhenDoneContinuesLoadedIdleWebSession(t *testing.T) {
	store := newServeRuntimeTestStore()
	provider := llm.NewMockProvider("mock").AddTextResponse("I saw the queued job finish.")
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
		store:        store,
		platform:     "web",
	}
	mgr := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	if _, err := mgr.GetOrCreate(context.Background(), "sess-origin"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	srv := &serveServer{
		store:        store,
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	message := "Queued job job_123 (developer) succeeded: hello world"
	if err := srv.notifyQueuedAgentWeb(context.Background(), "run_123", "sess-origin", message); err != nil {
		t.Fatalf("notifyQueuedAgentWeb: %v", err)
	}

	waitForServeCondition(t, 2*time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.messages["sess-origin"]) >= 2
	}, "notification continuation to persist user notice and assistant response")

	store.mu.Lock()
	msgs := append([]session.Message(nil), store.messages["sess-origin"]...)
	store.mu.Unlock()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || !strings.Contains(msgs[0].TextContent, message) {
		t.Fatalf("first message = (%s, %q), want user notification", msgs[0].Role, msgs[0].TextContent)
	}
	if msgs[1].Role != llm.RoleAssistant || !strings.Contains(msgs[1].TextContent, "I saw the queued job finish") {
		t.Fatalf("second message = (%s, %q), want assistant continuation", msgs[1].Role, msgs[1].TextContent)
	}
	if provider.CurrentTurn() != 1 {
		t.Fatalf("provider turns = %d, want 1", provider.CurrentTurn())
	}
}

func TestNotifyQueuedAgentWeb_ActiveRunOwnsNotificationExactlyOnce(t *testing.T) {
	store := newServeRuntimeTestStore()
	if err := store.Create(context.Background(), &session.Session{ID: "sess-active", Origin: session.OriginWeb, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	provider := llm.NewMockProvider("active").
		AddTurn(llm.MockTurn{Text: "working", Delay: 100 * time.Millisecond}).
		AddTextResponse("notification acknowledged")
	engine := llm.NewEngine(provider, nil)
	stream, err := engine.Stream(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("work")},
		Tools:    []llm.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	rt := &serveRuntime{engine: engine, provider: provider, providerKey: "active", store: store}
	state := &runtimeInterruptState{done: make(chan struct{}), cancel: func() {}}
	rt.setActiveInterrupt(state)
	mgr := newServeSessionManager(time.Minute, 10, nil)
	putTestSession(mgr, "sess-active", rt)
	srv := &serveServer{store: store, sessionMgr: mgr}
	defer func() {
		rt.clearActiveInterrupt(state)
		_ = stream.Close()
		mgr.Close()
	}()

	message := "Queued job job_123 (developer) succeeded: hello world"
	if err := srv.notifyQueuedAgentWeb(context.Background(), "run-active", "sess-active", message); err != nil {
		t.Fatalf("notifyQueuedAgentWeb: %v", err)
	}
	steeringEvents := 0
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if event.Type == llm.EventSteering && event.SteeringID == "job_notify_run-active" {
			steeringEvents++
		}
	}
	if steeringEvents != 1 {
		t.Fatalf("job notification steering events = %d, want exactly one", steeringEvents)
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("active run retained consumed notification: %#v", pending)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want initial plus notification continuation", len(requests))
	}
	notificationMessages := 0
	for _, request := range requests {
		for _, requestMessage := range request.Messages {
			if requestMessage.ClientMessageID == "job_notify_run-active" && llm.MessageText(requestMessage) == message {
				notificationMessages++
			}
		}
	}
	if notificationMessages != 1 {
		t.Fatalf("provider notification messages = %d, want exactly one", notificationMessages)
	}
	store.mu.Lock()
	persisted := append([]session.Message(nil), store.messages["sess-active"]...)
	store.mu.Unlock()
	if len(persisted) != 0 || len(rt.history) != 0 {
		t.Fatalf("active-run notification also fell back: persisted=%#v history=%#v", persisted, rt.history)
	}
}

func TestNotifyQueuedAgentWeb_RunFinishedTeardownFallsBackWithoutPhantom(t *testing.T) {
	store := newServeRuntimeTestStore()
	if err := store.Create(context.Background(), &session.Session{ID: "sess-teardown", Origin: session.OriginWeb, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	provider := llm.NewMockProvider("teardown").AddTextResponse("finished")
	engine := llm.NewEngine(provider, nil)
	stream, err := engine.Stream(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("work")},
		Tools:    []llm.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
	}

	rt := &serveRuntime{engine: engine, provider: provider, providerKey: "teardown", store: store}
	// Retain activeInterrupt to model the window after the engine closes its final
	// boundary but before response-run teardown clears runtime ownership.
	state := &runtimeInterruptState{done: make(chan struct{}), cancel: func() {}}
	rt.setActiveInterrupt(state)
	mgr := newServeSessionManager(time.Minute, 10, nil)
	putTestSession(mgr, "sess-teardown", rt)
	srv := &serveServer{store: store, sessionMgr: mgr}
	defer func() {
		rt.clearActiveInterrupt(state)
		mgr.Close()
	}()

	message := "Queued job job_456 (developer) succeeded: complete"
	if err := srv.notifyQueuedAgentWeb(context.Background(), "run-finished", "sess-teardown", message); err != nil {
		t.Fatalf("notifyQueuedAgentWeb: %v", err)
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("run-finished fallback retained phantom steering: %#v", pending)
	}
	store.mu.Lock()
	persisted := append([]session.Message(nil), store.messages["sess-teardown"]...)
	store.mu.Unlock()
	if len(persisted) != 1 || persisted[0].Role != llm.RoleAssistant || persisted[0].TextContent != message {
		t.Fatalf("fallback notifications = %#v, want exactly one assistant notice", persisted)
	}
	if len(rt.history) != 1 || rt.history[0].Role != llm.RoleAssistant || llm.MessageText(rt.history[0]) != message {
		t.Fatalf("runtime history = %#v, want one assistant notice", rt.history)
	}
}

func TestJobsV2NotifyRetriesTransientFailures(t *testing.T) {
	attempts := 0
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		attempts++
		if attempts < jobsV2NotifyMaxAttempts {
			return jobsV2NotifyTransientTestError{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-retry-success",
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

	mgr.notifyRunDone(run, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, exitReasonNatural, false, "")
	if attempts != jobsV2NotifyMaxAttempts {
		t.Fatalf("notification attempts = %d, want %d", attempts, jobsV2NotifyMaxAttempts)
	}
	events, _, err := mgr.ListRunEvents(run.ID, 0, 20, 0)
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			t.Fatalf("unexpected notify_failed event after successful retry: %#v", events)
		}
	}
}

func TestJobsV2NotifyPermanentHTTPFailureDoesNotRetry(t *testing.T) {
	attempts := 0
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		attempts++
		return providerhttp.NewStatusErrorString("Telegram", http.StatusBadRequest, "400 Bad Request", nil, "bad chat")
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-permanent-failure",
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

	mgr.notifyRunDone(run, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, exitReasonNatural, false, "")
	if attempts != 1 {
		t.Fatalf("notification attempts = %d, want 1 for permanent HTTP failure", attempts)
	}
	events, _, err := mgr.ListRunEvents(run.ID, 0, 20, 0)
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	notifyFailures := 0
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			notifyFailures++
		}
	}
	if notifyFailures != 1 {
		t.Fatalf("notify_failed events = %d, want 1: %#v", notifyFailures, events)
	}
}

func TestJobsV2NotifyFailureDoesNotChangeRunStatus(t *testing.T) {
	var attempts atomic.Int32
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		attempts.Add(1)
		return jobsV2NotifyTransientTestError{}
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
	var events []jobsV2RunEvent
	waitForServeCondition(t, 2*time.Second, func() bool {
		var err error
		events, _, err = mgr.ListRunEvents(run.ID, 0, 20, 0)
		if err != nil {
			return false
		}
		for _, ev := range events {
			if ev.EventType == "notify_failed" {
				return true
			}
		}
		return false
	}, "async notify_failed event")
	notifyFailures := 0
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			notifyFailures++
		}
	}
	if got := attempts.Load(); got != jobsV2NotifyMaxAttempts {
		t.Fatalf("notification attempts = %d, want %d", got, jobsV2NotifyMaxAttempts)
	}
	if notifyFailures != 1 {
		t.Fatalf("notify_failed events = %d, want 1: %#v", notifyFailures, events)
	}
}

func TestJobsV2FinishRunDoesNotBlockOnCompletionNotification(t *testing.T) {
	notifyStarted := make(chan struct{})
	releaseNotify := make(chan struct{})
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(ctx context.Context, run jobsV2Run, job jobsV2Job, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) error {
		close(notifyStarted)
		select {
		case <-releaseNotify:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	defer close(releaseNotify)

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-nonblocking",
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

	started := time.Now()
	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, nil, run.Attempt); err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("finishRun blocked on notification for %v", elapsed)
	}

	select {
	case <-notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("completion notification did not start asynchronously")
	}
}

func TestJobsV2CloseCancelsCompletionNotification(t *testing.T) {
	notifyStarted := make(chan struct{}, 1)
	var attempts atomic.Int32
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(ctx context.Context, _ jobsV2Run, _ jobsV2Job, _ jobsV2RunStatus, _ jobsV2RunResult, _ string, _ bool, _ string) error {
		attempts.Add(1)
		notifyStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-shutdown",
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

	select {
	case <-notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("completion notification did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.CloseContext(closeCtx); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("notification attempts after shutdown = %d, want 1", got)
	}
}
