package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueAgentCreatesAndTriggersJobsBackedLLMJob(t *testing.T) {
	var sawCreate bool
	var sawTrigger bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/jobs":
			sawCreate = true
			var payload jobsV2AgentJobPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			if payload.RunnerType != "llm" {
				t.Fatalf("runner_type = %q, want llm", payload.RunnerType)
			}
			if payload.RunnerConfig["agent_name"] != "developer" {
				t.Fatalf("agent_name = %#v, want developer", payload.RunnerConfig["agent_name"])
			}
			if !strings.Contains(payload.RunnerConfig["instructions"].(string), "do it") {
				t.Fatalf("instructions missing prompt: %#v", payload.RunnerConfig["instructions"])
			}
			if !strings.Contains(payload.RunnerConfig["instructions"].(string), "STATUS: COMPLETE") {
				t.Fatalf("instructions missing status footer: %#v", payload.RunnerConfig["instructions"])
			}
			if payload.RunnerConfig["cwd"] != "/tmp/work" {
				t.Fatalf("cwd = %#v, want /tmp/work", payload.RunnerConfig["cwd"])
			}
			if payload.RunnerConfig["model"] != "fast" {
				t.Fatalf("model = %#v, want fast", payload.RunnerConfig["model"])
			}
			if payload.TimeoutSeconds != 60 {
				t.Fatalf("timeout = %d, want 60", payload.TimeoutSeconds)
			}
			writeJSON(t, w, jobsV2AgentJobResponse{ID: "job_123"})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/jobs/job_123/trigger":
			sawTrigger = true
			writeJSON(t, w, jobsV2AgentRunResponse{ID: "run_123", JobID: "job_123", Status: "queued"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}
	tool := NewQueueAgentToolWithClient(client)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"do it","timeout":60,"model":"fast","cwd":"/tmp/work"}`))
	if err != nil {
		t.Fatalf("queue Execute() error = %v", err)
	}
	var result QueueAgentResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatalf("queue output is not JSON: %v; %s", err, out.Content)
	}
	if result.JobID != "job_123" || result.AgentName != "developer" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(out.Content, "run_id") {
		t.Fatalf("queue output should not expose run_id: %s", out.Content)
	}
	if !sawCreate || !sawTrigger {
		t.Fatalf("sawCreate=%v sawTrigger=%v", sawCreate, sawTrigger)
	}
}

func TestQueueAgentNotifyWhenDonePersistsTrustedOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/jobs":
			var payload jobsV2AgentJobPayload
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create payload: %v", err)
			}
			if payload.RunnerConfig["notify_when_done"] != true {
				t.Fatalf("notify_when_done = %#v, want true", payload.RunnerConfig["notify_when_done"])
			}
			if _, exists := payload.RunnerConfig["notify_origin"]; exists {
				t.Fatalf("notify_origin should not be caller-controlled in runner_config: %#v", payload.RunnerConfig["notify_origin"])
			}
			if r.Header.Get(QueueAgentNotifyOriginHeader) != QueueAgentOriginWeb || r.Header.Get(QueueAgentNotifySessionIDHeader) != "sess-web-1" {
				t.Fatalf("notify origin headers = origin:%q session:%q, want web sess-web-1", r.Header.Get(QueueAgentNotifyOriginHeader), r.Header.Get(QueueAgentNotifySessionIDHeader))
			}
			if got := r.Header.Get(QueueAgentNotifyTelegramChatIDHeader); got != "" {
				t.Fatalf("telegram header = %q, want empty", got)
			}
			writeJSON(t, w, jobsV2AgentJobResponse{ID: "job_notify"})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/jobs/job_notify/trigger":
			writeJSON(t, w, jobsV2AgentRunResponse{ID: "run_notify", JobID: "job_notify", Status: "queued"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := &jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}
	tool := NewQueueAgentToolWithClient(client)
	ctx := ContextWithQueueAgentOrigin(context.Background(), QueueAgentOriginContext{Origin: QueueAgentOriginWeb, SessionID: "sess-web-1"})
	out, err := tool.Execute(ctx, json.RawMessage(`{"agent_name":"developer","prompt":"do it","cwd":"/tmp/work","notify_when_done":true}`))
	if err != nil {
		t.Fatalf("queue Execute() error = %v", err)
	}
	var result QueueAgentResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatalf("queue output is not JSON: %v; %s", err, out.Content)
	}
	if result.JobID != "job_notify" {
		t.Fatalf("job_id = %q, want job_notify", result.JobID)
	}
}

func TestWaitForJobsPollsUntilTerminal(t *testing.T) {
	var polls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/runs" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("job_id") != "job_123" {
			t.Fatalf("job_id query = %q, want job_123", r.URL.Query().Get("job_id"))
		}
		if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("offset") != "0" {
			t.Fatalf("unexpected pagination query: %s", r.URL.RawQuery)
		}
		count := atomic.AddInt32(&polls, 1)
		if count == 1 {
			writeJSON(t, w, jobsV2AgentRunsListResponse{Data: []jobsV2AgentRunResponse{{ID: "run_123", JobID: "job_123", Status: "running"}}})
			return
		}
		exitCode := 0
		turnCount := 1
		inputTokens := 10
		outputTokens := 3
		writeJSON(t, w, jobsV2AgentRunsListResponse{Data: []jobsV2AgentRunResponse{{
			ID:           "run_123",
			JobID:        "job_123",
			Status:       "succeeded",
			ExitReason:   "natural_completion",
			TurnCount:    &turnCount,
			InputTokens:  &inputTokens,
			OutputTokens: &outputTokens,
			Response:     "STATUS: COMPLETE\nOK",
			ExitCode:     &exitCode,
			StartedAt:    "2026-06-07T07:15:51.314202856Z",
			FinishedAt:   "2026-06-07T07:16:49.259958355Z",
		}}})
	}))
	defer server.Close()

	client := &jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}
	tool := NewWaitForJobsToolWithClient(client)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"job_ids":["job_123"],"poll_interval_seconds":1}`))
	if err != nil {
		t.Fatalf("wait Execute() error = %v", err)
	}
	var results []QueuedJobResult
	if err := json.Unmarshal([]byte(out.Content), &results); err != nil {
		t.Fatalf("wait output is not JSON: %v; %s", err, out.Content)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	result := results[0]
	if result.JobID != "job_123" || result.Status != "succeeded" || result.Response != "STATUS: COMPLETE\nOK" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if strings.Contains(out.Content, "run_id") {
		t.Fatalf("wait output should not expose run_id: %s", out.Content)
	}
	if result.DurationSeconds == nil || *result.DurationSeconds < 57.9 || *result.DurationSeconds > 58.0 {
		t.Fatalf("duration = %#v, want about 57.9", result.DurationSeconds)
	}
	if polls != 2 {
		t.Fatalf("polls = %d, want 2", polls)
	}
}

func TestQueueAgentSurfacesJobsErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"llm runner_config.cwd is required","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	client := &jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}
	tool := NewQueueAgentToolWithClient(client)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"do it","cwd":"/tmp/work"}`))
	if err != nil {
		t.Fatalf("queue Execute() error = %v", err)
	}
	if !strings.Contains(out.Content, "llm runner_config.cwd is required") {
		t.Fatalf("output did not include jobs error body: %s", out.Content)
	}
}

func TestWaitForJobsContextCancellationReturnsPartialError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, jobsV2AgentRunsListResponse{Data: []jobsV2AgentRunResponse{{ID: "run_123", JobID: "job_123", Status: "running"}}})
	}))
	defer server.Close()

	client := &jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}
	tool := NewWaitForJobsToolWithClient(client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	out, err := tool.Execute(ctx, json.RawMessage(`{"job_ids":["job_123"],"poll_interval_seconds":1}`))
	if err != nil {
		t.Fatalf("wait Execute() error = %v", err)
	}
	var results []QueuedJobResult
	if err := json.Unmarshal([]byte(out.Content), &results); err != nil {
		t.Fatalf("wait output is not JSON: %v; %s", err, out.Content)
	}
	if len(results) != 1 || results[0].JobID != "job_123" || !strings.Contains(results[0].Error, "context deadline exceeded") {
		t.Fatalf("unexpected result: %+v", results)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
