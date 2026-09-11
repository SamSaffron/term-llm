package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestResponsesStreamCanResumeAfterClientDisconnect(t *testing.T) {
	provider := newStagedProvider("hello ", "world")
	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManager(),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "resume-session")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var responseID string
	lastSeq := int64(0)
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before first text delta")
		}
		if data == "[DONE]" {
			t.Fatal("stream completed before disconnect")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal SSE payload: %v", err)
		}
		if seq, ok := payload["sequence_number"].(float64); ok {
			lastSeq = int64(seq)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			responseID, _ = response["id"].(string)
		case "response.output_text.delta":
			if got := payload["delta"]; got != "hello " {
				t.Fatalf("first delta = %v, want hello ", got)
			}
			cancelReq()
			_ = resp.Body.Close()
			goto disconnected
		}
	}

disconnected:
	if responseID == "" {
		t.Fatal("missing response id before disconnect")
	}

	<-provider.firstSent
	close(provider.releaseSecond)

	statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("get response status failed: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", statusResp.StatusCode)
	}
	var statusPayload map[string]any
	if err := json.NewDecoder(statusResp.Body).Decode(&statusPayload); err != nil {
		t.Fatalf("decode response status: %v", err)
	}
	if got := statusPayload["status"]; got != "in_progress" && got != "completed" {
		t.Fatalf("status = %v, want in_progress or completed", got)
	}

	resumeResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID + "/events?after=" + strconv.FormatInt(lastSeq, 10))
	if err != nil {
		t.Fatalf("resume request failed: %v", err)
	}
	defer resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d, want 200", resumeResp.StatusCode)
	}

	resumeScanner := bufio.NewScanner(resumeResp.Body)
	var resumed []string
	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, resumeScanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal resumed SSE payload: %v", err)
		}
		switch eventName {
		case "response.output_text.delta":
			resumed = append(resumed, fmt.Sprint(payload["delta"]))
		case "response.completed":
			sawCompleted = true
		case "response.failed":
			t.Fatalf("resume stream failed: %s", data)
		}
	}

	if strings.Join(resumed, "") != "world" {
		t.Fatalf("resumed text = %q, want %q", strings.Join(resumed, ""), "world")
	}
	if !sawCompleted {
		t.Fatal("resume stream missing response.completed")
	}
}

func TestResponsesCompletedRunExpiresAfterRetention(t *testing.T) {
	provider := newStagedProvider("hello ", "world")
	close(provider.releaseSecond)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManagerWithRetention(100 * time.Millisecond),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "retention-session")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var responseID string
	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			responseID, _ = response["id"].(string)
		case "response.completed":
			sawCompleted = true
		}
	}

	if responseID == "" {
		t.Fatal("missing response id for completed run")
	}
	if !sawCompleted {
		t.Fatal("stream missing response.completed")
	}

	statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("get completed response failed: %v", err)
	}
	statusBody, _ := io.ReadAll(statusResp.Body)
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", statusResp.StatusCode, string(statusBody))
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		expireResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
		if err != nil {
			t.Fatalf("get expired response failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, expireResp.Body)
		expireResp.Body.Close()
		if expireResp.StatusCode == http.StatusNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %s still present after retention window; status=%d", responseID, expireResp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHandleResponseCancelDiscardsPendingSteering(t *testing.T) {
	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()

	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	engine.QueueSteering(llm.QueuedSteering{ID: "steer-1", Message: llm.UserText("please also do x")})
	rt := &serveRuntime{engine: engine}
	putTestSession(mgr, "sess-cancel-steer", rt)

	runCancel := func() {}
	run := newResponseRun("resp_cancel_steer", "sess-cancel-steer", "", "mock-model", time.Now().Unix(), runCancel)
	srv := &serveServer{sessionMgr: mgr, responseRuns: newServeResponseRunManager()}
	if err := srv.responseRuns.create(run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_cancel_steer/cancel", nil)
	rr := httptest.NewRecorder()
	srv.handleResponseByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	waitForServeCondition(t, time.Second, func() bool {
		return len(engine.ListPendingSteering()) == 0
	}, "pending steering to be discarded after explicit cancel")
}

func TestHandleResponseCancelAcknowledgesBeforeCleanupFinishes(t *testing.T) {
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	run := newResponseRun("resp_cancel_prompt", "cancel-session", "", "mock-model", time.Now().Unix(), func() {
		close(cleanupStarted)
		<-releaseCleanup
	})
	mgr := newServeResponseRunManagerWithRetention(time.Minute)
	if err := mgr.create(run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	srv := &serveServer{responseRuns: mgr}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_cancel_prompt/cancel", nil)
	rr := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() {
		srv.handleResponseByID(rr, req)
		close(handlerDone)
	}()

	select {
	case <-handlerDone:
		if rr.Code != http.StatusOK {
			t.Fatalf("cancel status = %d, want 200 body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		close(releaseCleanup)
		t.Fatal("cancel acknowledgement waited for cleanup")
	}
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		close(releaseCleanup)
		t.Fatal("cancel cleanup did not start")
	}
	close(releaseCleanup)
}

func TestHandleResponseCancelIsIdempotentWhileRunIsFinishing(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	run := newResponseRun("resp_cancel_once", "cancel-session", "", "mock-model", time.Now().Unix(), cancel)
	mgr := newServeResponseRunManagerWithRetention(time.Minute)
	if err := mgr.create(run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	srv := &serveServer{
		responseRuns: mgr,
	}

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_cancel_once/cancel", nil)
	firstRR := httptest.NewRecorder()
	srv.handleResponseByID(firstRR, firstReq)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first cancel status = %d, want 200", firstRR.Code)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_cancel_once/cancel", nil)
	secondRR := httptest.NewRecorder()
	srv.handleResponseByID(secondRR, secondReq)
	if secondRR.Code != http.StatusOK {
		t.Fatalf("second cancel status = %d, want 200", secondRR.Code)
	}
}

func TestHandleResponseByID_CancelStopsActiveToolRun(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_1", "slow_tool", map[string]any{})
	provider.AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&testServeDelayTool{delay: 5 * time.Second})

	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{
		responseRuns: newServeResponseRunManager(),
	}

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("sleep for a while"),
	}, llm.Request{
		SessionID:  "sess_cancel_tool",
		MaxTurns:   5,
		Tools:      []llm.ToolSpec{(&testServeDelayTool{}).Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
	}, "sess_cancel_tool", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun failed: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run.snapshot()
		recovery, ok := snapshot["recovery"].(map[string]any)
		if !ok {
			return false
		}
		messages, ok := recovery["messages"].([]map[string]any)
		if !ok {
			return false
		}
		for _, message := range messages {
			if message["role"] != "tool-group" || message["status"] != "running" {
				continue
			}
			toolsPayload, ok := message["tools"].([]map[string]any)
			if !ok {
				continue
			}
			for _, tool := range toolsPayload {
				if tool["name"] == "slow_tool" && tool["status"] == "running" {
					return true
				}
			}
		}
		return false
	}, "slow tool running in response recovery state")

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses/"+run.id+"/cancel", nil)
	firstRR := httptest.NewRecorder()
	srv.handleResponseByID(firstRR, firstReq)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first cancel status = %d, want 200 body=%s", firstRR.Code, firstRR.Body.String())
	}

	waitForServeCondition(t, time.Second, func() bool {
		if rt.hasActiveRun() {
			return false
		}
		snapshot := run.snapshot()
		status, _ := snapshot["status"].(string)
		return status != "in_progress"
	}, "tool-backed response run to stop after cancel")

	if got := provider.CurrentTurn(); got != 1 {
		t.Fatalf("provider turn index = %d, want 1 (tool run should stop before follow-up turn)", got)
	}

	snapshot := run.snapshot()
	if status, _ := snapshot["status"].(string); status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled: %#v", status, snapshot)
	}
	if _, ok := snapshot["error"]; ok {
		t.Fatalf("cancelled run should not include error payload: %#v", snapshot)
	}

	subscription := run.subscribe(0)
	if subscription.ch != nil {
		t.Fatal("terminal cancelled run should replay without live subscription channel")
	}
	var sawCancelled bool
	for _, event := range subscription.replay {
		if event.Event == "response.failed" {
			t.Fatalf("cancelled run replay unexpectedly contained response.failed: %+v", event)
		}
		if event.Event == "response.cancelled" {
			sawCancelled = true
			var payload map[string]any
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatalf("unmarshal response.cancelled payload: %v", err)
			}
			response, _ := payload["response"].(map[string]any)
			if got := response["status"]; got != "cancelled" {
				t.Fatalf("response.cancelled status = %v, want cancelled", got)
			}
		}
	}
	if !sawCancelled {
		t.Fatal("cancelled run replay missing response.cancelled terminal event")
	}
}

func TestResolveServeResponseTimeout(t *testing.T) {
	tests := []struct {
		name      string
		flagSet   bool
		flagVal   time.Duration
		configVal string
		want      time.Duration
		wantErr   string
	}{
		{
			name: "default",
			want: defaultServeRequestTimeout,
		},
		{
			name:    "flag wins",
			flagSet: true,
			flagVal: 90 * time.Minute,
			want:    90 * time.Minute,
		},
		{
			name:      "config",
			configVal: "1h15m",
			want:      75 * time.Minute,
		},
		{
			name:      "invalid config",
			configVal: "eventually",
			wantErr:   "invalid serve.response_timeout",
		},
		{
			name:      "non-positive config",
			configVal: "0s",
			wantErr:   "must be > 0",
		},
		{
			name:    "non-positive flag",
			flagSet: true,
			flagVal: 0,
			wantErr: "invalid --response-timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveServeResponseTimeout(tt.flagSet, tt.flagVal, tt.configVal)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("timeout = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestResponseRunDeadlineMessageUsesConfiguredLimitOnlyWhenRunContextExpired(t *testing.T) {
	timeout := 45 * time.Minute
	if got := responseRunDeadlineMessage(context.Background(), timeout); strings.Contains(got, "45 minutes") {
		t.Fatalf("live run context falsely claimed configured timeout: %q", got)
	}

	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := responseRunDeadlineMessage(expiredCtx, timeout); !strings.Contains(got, "no LLM response completed within 45 minutes") {
		t.Fatalf("expired run context message = %q, want configured inactivity timeout", got)
	}
}

func TestStartResponseRunProviderDeadlineDoesNotClaimRunTimeout(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddError(context.DeadlineExceeded)
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{
		cfg: serveServerConfig{
			responseTimeout: 45 * time.Minute,
		},
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("take too long"),
	}, llm.Request{SessionID: "sess_timeout"}, "sess_timeout", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun failed: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run.snapshot()
		status, _ := snapshot["status"].(string)
		return status == "failed"
	}, "deadline-exceeded response run to fail")

	snapshot := run.snapshot()
	if status, _ := snapshot["status"].(string); status != "failed" {
		t.Fatalf("run status = %q, want failed: %#v", status, snapshot)
	}
	errPayload, _ := snapshot["error"].(map[string]any)
	if got := errPayload["type"]; got != "timeout_error" {
		t.Fatalf("error type = %v, want timeout_error: %#v", got, snapshot)
	}
	message, _ := errPayload["message"].(string)
	if !strings.Contains(message, "provider request timed out before the response run deadline") {
		t.Fatalf("timeout message = %q, want provider timeout explanation", message)
	}
	if strings.Contains(message, "45 minutes") {
		t.Fatalf("provider timeout falsely claimed the configured response deadline: %q", message)
	}
	if strings.Contains(message, "context deadline exceeded") {
		t.Fatalf("timeout message leaked raw context error: %q", message)
	}

	subscription := run.subscribe(0)
	if subscription.ch != nil {
		t.Fatal("terminal failed run should replay without live subscription channel")
	}
	var sawFailed bool
	for _, event := range subscription.replay {
		if event.Event != "response.failed" {
			continue
		}
		sawFailed = true
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatalf("unmarshal response.failed payload: %v", err)
		}
		eventErr, _ := payload["error"].(map[string]any)
		if got := eventErr["message"]; got != message {
			t.Fatalf("response.failed message = %v, want %q", got, message)
		}
	}
	if !sawFailed {
		t.Fatal("deadline-exceeded run replay missing response.failed terminal event")
	}
}

func TestStartResponseRunTimeoutPreservesDeferredUIError(t *testing.T) {
	const timeout = 50 * time.Millisecond
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Delay: time.Second, Text: "too late"})
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()
	srv := &serveServer{
		cfg:          serveServerConfig{responseTimeout: timeout},
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{llm.UserText("wait")}, llm.Request{
		SessionID: "sess_ui_timeout",
	}, "sess_ui_timeout", startResponseRunOptions{uiSession: true})
	if err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}
	waitForServeCondition(t, 2*time.Second, func() bool {
		return stringValue(run.snapshot()["status"]) == "failed"
	}, "UI response run timeout")

	want := responseRunTimeoutMessage(timeout)
	if got := rt.consumeLastUIRunError(); got != want {
		t.Fatalf("deferred UI timeout error = %q, want %q", got, want)
	}
}

func TestStartResponseRunExplicitCancelClearsDeferredUIError(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Delay: time.Second, Text: "too late"})
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()
	srv := &serveServer{
		cfg:          serveServerConfig{responseTimeout: time.Minute},
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{llm.UserText("wait")}, llm.Request{
		SessionID: "sess_ui_cancel",
	}, "sess_ui_cancel", startResponseRunOptions{uiSession: true})
	if err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}
	rt.setLastUIRunError("stale error")
	if !run.cancelRun() {
		t.Fatal("explicit response cancellation was not accepted")
	}
	waitForServeCondition(t, 2*time.Second, func() bool {
		return stringValue(run.snapshot()["status"]) == "cancelled"
	}, "UI response cancellation")
	if got := rt.consumeLastUIRunError(); got != "" {
		t.Fatalf("explicit cancellation retained deferred UI error %q", got)
	}
}

func TestResponseOwnerIDIsStableAcrossConcurrentInitialization(t *testing.T) {
	srv := &serveServer{}
	ids := make(chan string, 64)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- srv.responseOwnerID()
		}()
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id == "" || id != first {
			t.Fatalf("owner IDs diverged: first=%q got=%q", first, id)
		}
	}
}

func TestClassifiedCancelPreservesCompletedToolContextForFollowUp(t *testing.T) {
	const sessionID = "sess-cancel-preserves-tools"
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := &cancelPreserveProvider{secondStarted: make(chan struct{})}
	registry := llm.NewToolRegistry()
	tool := &testServeDelayTool{}
	registry.Register(tool)
	runtime := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       llm.NewEngine(provider, registry),
		defaultModel: "test-model",
		store:        store,
	}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{
		store:        store,
		sessionMgr:   manager,
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(runtime, true, false, []llm.Message{llm.UserText("gather context")}, llm.Request{
		SessionID:  sessionID,
		MaxTurns:   5,
		Tools:      []llm.ToolSpec{tool.Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
	}, sessionID, startResponseRunOptions{uiSession: true})
	if err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}

	select {
	case <-provider.secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not reach the post-tool turn")
	}

	action, _, err := runtime.InterruptMessage(context.Background(), llm.UserText("/stop"), "/stop", "msg-stop-now", nil, interruptDeliveryAuto)
	if err != nil {
		t.Fatalf("InterruptMessage: %v", err)
	}
	if action != llm.InterruptCancel {
		t.Fatalf("action = %q, want cancel", action)
	}

	waitForServeCondition(t, 2*time.Second, func() bool {
		return run.snapshot()["status"] == "cancelled"
	}, "classified cancellation to become terminal")

	snapshot := run.snapshot()
	continuationID, _ := snapshot["continuation_response_id"].(string)
	if !strings.HasPrefix(continuationID, durableResponseMessagePrefix) {
		t.Fatalf("continuation_response_id = %q, want durable message cursor; snapshot=%#v", continuationID, snapshot)
	}

	stored, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	foundToolResult := false
	for _, message := range stored {
		for _, part := range message.Parts {
			if message.Role == llm.RoleTool && part.ToolResult != nil && strings.Contains(part.ToolResult.Content, "slept") {
				foundToolResult = true
			}
		}
	}
	if !foundToolResult {
		t.Fatalf("completed tool result was lost on cancellation: %#v", stored)
	}

	code, response := doResponses(t, srv, `{"input":"use what you gathered","previous_response_id":"`+continuationID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("follow-up status = %d, want 200 body=%#v", code, response)
	}
	requests := provider.Requests()
	if len(requests) < 3 {
		t.Fatalf("provider requests = %d, want at least 3", len(requests))
	}
	foundPriorToolResult := false
	for _, message := range requests[len(requests)-1].Messages {
		for _, part := range message.Parts {
			if message.Role == llm.RoleTool && part.ToolResult != nil && strings.Contains(part.ToolResult.Content, "slept") {
				foundPriorToolResult = true
			}
		}
	}
	if !foundPriorToolResult {
		t.Fatalf("follow-up provider request lost completed tool context: %#v", requests[len(requests)-1].Messages)
	}
}

func TestHandleResponseByID_CancelStopsShellToolRun(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_1", tools.ShellToolName, map[string]any{
		"command": "sleep 10",
	})
	provider.AddTextResponse("done")

	shellTool := tools.NewShellTool(nil, nil, tools.DefaultOutputLimits())
	registry := llm.NewToolRegistry()
	registry.Register(shellTool)

	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{
		responseRuns: newServeResponseRunManager(),
	}

	run, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("sleep for a while"),
	}, llm.Request{
		SessionID:  "sess_cancel_shell_tool",
		MaxTurns:   5,
		Tools:      []llm.ToolSpec{shellTool.Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
	}, "sess_cancel_shell_tool", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun failed: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run.snapshot()
		recovery, ok := snapshot["recovery"].(map[string]any)
		if !ok {
			return false
		}
		messages, ok := recovery["messages"].([]map[string]any)
		if !ok {
			return false
		}
		for _, message := range messages {
			if message["role"] != "tool-group" || message["status"] != "running" {
				continue
			}
			toolsPayload, ok := message["tools"].([]map[string]any)
			if !ok {
				continue
			}
			for _, tool := range toolsPayload {
				if tool["name"] == tools.ShellToolName && tool["status"] == "running" {
					return true
				}
			}
		}
		return false
	}, "shell tool running in response recovery state")

	start := time.Now()
	cancelReq := httptest.NewRequest(http.MethodPost, "/v1/responses/"+run.id+"/cancel", nil)
	cancelRR := httptest.NewRecorder()
	srv.handleResponseByID(cancelRR, cancelReq)
	if cancelRR.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200 body=%s", cancelRR.Code, cancelRR.Body.String())
	}

	waitForServeCondition(t, 2*time.Second, func() bool {
		if rt.hasActiveRun() {
			return false
		}
		snapshot := run.snapshot()
		status, _ := snapshot["status"].(string)
		return status != "in_progress"
	}, "shell-backed response run to stop after cancel")

	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("shell tool cancel took too long: %s", elapsed)
	}
	if got := provider.CurrentTurn(); got != 1 {
		t.Fatalf("provider turn index = %d, want 1 (shell tool run should stop before follow-up turn)", got)
	}

	snapshot := run.snapshot()
	if status, _ := snapshot["status"].(string); status == "in_progress" {
		t.Fatalf("run status remained in_progress after shell-tool cancel: %#v", snapshot)
	}
}

func TestResponsesCompactedRunRequiresSnapshotRecovery(t *testing.T) {
	longText := strings.Repeat("abcdefghij", 3000)
	provider := llm.NewMockProvider("mock").AddTextResponse(longText)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManager(),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "compaction-session")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}

	scanner := bufio.NewScanner(resp.Body)
	var responseID string
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before response.created")
		}
		if data == "[DONE]" {
			t.Fatal("stream ended before response.created")
		}
		if eventName != "response.created" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal response.created payload: %v", err)
		}
		response, _ := payload["response"].(map[string]any)
		responseID, _ = response["id"].(string)
		break
	}
	_ = resp.Body.Close()

	if responseID == "" {
		t.Fatal("missing response id")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
		if err != nil {
			t.Fatalf("get response status failed: %v", err)
		}
		var statusPayload map[string]any
		if err := json.NewDecoder(statusResp.Body).Decode(&statusPayload); err != nil {
			statusResp.Body.Close()
			t.Fatalf("decode response status: %v", err)
		}
		statusResp.Body.Close()
		if statusPayload["status"] == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("response %s did not complete in time", responseID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	replayResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID + "/events?after=0")
	if err != nil {
		t.Fatalf("replay request failed: %v", err)
	}
	defer replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(replayResp.Body)
		t.Fatalf("replay status = %d, want 409 (body=%s)", replayResp.StatusCode, string(body))
	}

	var replayErr map[string]any
	if err := json.NewDecoder(replayResp.Body).Decode(&replayErr); err != nil {
		t.Fatalf("decode replay error: %v", err)
	}
	if got := replayErr["snapshot_required"]; got != true {
		t.Fatalf("snapshot_required = %v, want true", got)
	}

	snapshotResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("snapshot request failed: %v", err)
	}
	defer snapshotResp.Body.Close()
	if snapshotResp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", snapshotResp.StatusCode)
	}

	var snapshotPayload map[string]any
	if err := json.NewDecoder(snapshotResp.Body).Decode(&snapshotPayload); err != nil {
		t.Fatalf("decode snapshot payload: %v", err)
	}
	recovery, _ := snapshotPayload["recovery"].(map[string]any)
	if recovery == nil {
		t.Fatal("missing recovery payload")
	}
	messages, _ := recovery["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("recovery message count = %d, want 1", len(messages))
	}
	message, _ := messages[0].(map[string]any)
	if got := message["role"]; got != "assistant" {
		t.Fatalf("recovery message role = %v, want assistant", got)
	}
	if got := message["content"]; got != longText {
		t.Fatalf("recovery message content length = %d, want %d", len(fmt.Sprint(got)), len(longText))
	}
}

func TestResponseToSessionMap_CleanedOnEviction(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(50*time.Millisecond, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	// Create a session and get a response ID
	code, resp := doResponsesWithHeader(t, srv, `{"input":"hi"}`, "evict-test")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	respID, _ := resp["id"].(string)

	// Verify mapping exists
	if _, ok := srv.responseToSession.Load(respID); !ok {
		t.Fatalf("responseToSession should contain %q after request", respID)
	}

	// Wait for TTL expiry, then evict explicitly.
	time.Sleep(75 * time.Millisecond)
	mgr.evictExpired()

	// Mapping should be cleaned up
	if _, ok := srv.responseToSession.Load(respID); ok {
		t.Fatalf("responseToSession should be cleaned up after eviction")
	}
}

func TestStreamResponses_EmitsSteeringEvent(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_1", "slow_tool", map[string]any{})
	provider.AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&testServeDelayTool{delay: 20 * time.Millisecond})

	engine := llm.NewEngine(provider, registry)
	engine.Steer("keep sleeping")

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "mock",
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	mgr := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "sess_steer")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: response.steering") {
		t.Fatalf("expected response.steering event in stream, got:\n%s", body)
	}
	if !strings.Contains(body, `"text":"keep sleeping"`) {
		t.Fatalf("expected steering payload in stream, got:\n%s", body)
	}
}

func TestParsePreviousResponseID(t *testing.T) {
	var req responsesCreateRequest
	body := `{"input":"hello","previous_response_id":"resp_abc123"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.PreviousResponseID != "resp_abc123" {
		t.Fatalf("previous_response_id = %q, want resp_abc123", req.PreviousResponseID)
	}
}

func TestResponsesHandler_ReasoningEffortFlowsToProvider(t *testing.T) {
	var capturedProvider *llm.MockProvider
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		capturedProvider = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "openai",
			engine:       engine,
			defaultModel: "gpt-5.6-sol",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{"input":"hello","reasoning_effort":"high","reasoning":{"mode":"pro","context":"all_turns"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if capturedProvider == nil {
		t.Fatal("expected runtime factory to have been called")
	}
	if len(capturedProvider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(capturedProvider.Requests))
	}
	if got := capturedProvider.Requests[0].ReasoningEffort; got != "high" {
		t.Fatalf("ReasoningEffort = %q, want %q", got, "high")
	}
	responses := capturedProvider.Requests[0].Responses
	if responses == nil || responses.ReasoningMode != "pro" || responses.ReasoningContext != "all_turns" {
		t.Fatalf("Responses = %+v, want pro/all_turns", responses)
	}
}

func TestResponsesHandler_NormalizesSuffixedModelAndExplicitEffort(t *testing.T) {
	var capturedProvider *llm.MockProvider
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		capturedProvider = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "chatgpt",
			engine:       engine,
			defaultModel: "gpt-5.5",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	defer mgr.Close()
	srv := &serveServer{
		sessionMgr: mgr,
		cfgRef: &config.Config{
			DefaultProvider: "chatgpt",
		},
	}

	body := `{"provider":"chatgpt","model":"gpt-5.5-medium","reasoning_effort":"xhigh","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if capturedProvider == nil || len(capturedProvider.Requests) != 1 {
		t.Fatalf("expected one provider request, got provider=%v", capturedProvider)
	}
	captured := capturedProvider.Requests[0]
	if captured.Model != "gpt-5.5" || captured.ReasoningEffort != "xhigh" {
		t.Fatalf("captured runtime = model %q effort %q, want gpt-5.5/xhigh", captured.Model, captured.ReasoningEffort)
	}
	if strings.Contains(rr.Body.String(), "gpt-5.5-medium") {
		t.Fatalf("response leaked suffixed model: %s", rr.Body.String())
	}
}

// After the first message of a web session pins reasoning_effort=high and
// model=first-model, chained requests on the same session must be silently
// overridden back to the persisted values. Mid-session switches of
// effort/model are disallowed — the first message of a conversation is the
// only place where client values are honored.
func TestHandleResponses_ModelAndEffortLockedAfterFirstMessage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	providers := make([]*llm.MockProvider, 0, 4)
	newRuntime := func() *serveRuntime {
		provider := llm.NewMockProvider("mock")
		provider.AddTextResponse("r1").AddTextResponse("r2").AddTextResponse("r3")
		providers = append(providers, provider)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-default-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime(), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
		store:      store,
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hi","model":"first-model","reasoning_effort":"high"}`, "lock-effort")
	if code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	code, _ = doResponses(t, srv, `{"input":"again","model":"second-model","reasoning_effort":"low","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second status = %d", code)
	}

	if len(providers) == 0 {
		t.Fatal("expected runtime factory to have been called")
	}
	last := providers[len(providers)-1]
	if len(last.Requests) == 0 {
		t.Fatal("expected a provider request on second call")
	}
	lastReq := last.Requests[len(last.Requests)-1]
	if lastReq.Model != "first-model" {
		t.Fatalf("Model on second request = %q, want first-model (locked)", lastReq.Model)
	}
	if lastReq.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort on second request = %q, want high (locked)", lastReq.ReasoningEffort)
	}

	sess, err := store.Get(context.Background(), "lock-effort")
	if err != nil || sess == nil {
		t.Fatalf("Get session: err=%v sess=%v", err, sess)
	}
	if sess.Model != "first-model" {
		t.Fatalf("persisted Model = %q, want first-model", sess.Model)
	}
	if sess.ReasoningEffort != "high" {
		t.Fatalf("persisted ReasoningEffort = %q, want high", sess.ReasoningEffort)
	}
}

func TestHandleResponses_FreshConversationReusedSessionIDUpdatesPersistedModelAndEffort(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	providers := make([]*llm.MockProvider, 0, 4)
	var createNum atomic.Int32
	newRuntime := func() *serveRuntime {
		n := createNum.Add(1)
		provider := llm.NewMockProvider("mock").AddTextResponse(fmt.Sprintf("runtime %d", n))
		providers = append(providers, provider)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-default-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime(), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
		store:      store,
	}

	code, _ := doResponsesWithHeader(t, srv, `{"input":"hi","model":"first-model","reasoning_effort":"high"}`, "reuse-model-effort")
	if code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}

	code, resp2 := doResponsesWithHeader(t, srv, `{"input":"restart","model":"second-model","reasoning_effort":"low"}`, "reuse-model-effort")
	if code != http.StatusOK {
		t.Fatalf("fresh status = %d", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("fresh response missing id")
	}

	sess, err := store.Get(context.Background(), "reuse-model-effort")
	if err != nil || sess == nil {
		t.Fatalf("Get session: err=%v sess=%v", err, sess)
	}
	if sess.Model != "second-model" {
		t.Fatalf("persisted Model after fresh restart = %q, want second-model", sess.Model)
	}
	if sess.ReasoningEffort != "low" {
		t.Fatalf("persisted ReasoningEffort after fresh restart = %q, want low", sess.ReasoningEffort)
	}

	manager.mu.Lock()
	evicted := manager.sessions["reuse-model-effort"]
	delete(manager.sessions, "reuse-model-effort")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, _ = doResponses(t, srv, `{"input":"continue","model":"ignored-model","reasoning_effort":"high","previous_response_id":"`+respID2+`"}`)
	if code != http.StatusOK {
		t.Fatalf("resume status = %d", code)
	}

	if len(providers) < 3 {
		t.Fatalf("provider count = %d, want at least 3 runtimes", len(providers))
	}
	last := providers[len(providers)-1]
	if len(last.Requests) == 0 {
		t.Fatal("expected a provider request on resumed call")
	}
	lastReq := last.Requests[len(last.Requests)-1]
	if lastReq.Model != "second-model" {
		t.Fatalf("Model on resumed request = %q, want second-model", lastReq.Model)
	}
	if lastReq.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort on resumed request = %q, want low", lastReq.ReasoningEffort)
	}
}

func TestResponsesHandler_ReasoningEffortDefaultNormalizedToEmpty(t *testing.T) {
	var capturedProvider *llm.MockProvider
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		capturedProvider = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{"input":"hello","reasoning_effort":"default"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(capturedProvider.Requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(capturedProvider.Requests))
	}
	if got := capturedProvider.Requests[0].ReasoningEffort; got != "" {
		t.Fatalf("ReasoningEffort = %q, want empty (normalized from %q)", got, "default")
	}
}

func TestServeRuntime_CumulativeUsageAccumulates(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddTurn(llm.MockTurn{Text: "a", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}}).
		AddTurn(llm.MockTurn{Text: "b", Usage: llm.Usage{InputTokens: 20, OutputTokens: 8}})
	engine := llm.NewEngine(provider, nil)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{SessionID: "cumul-test", MaxTurns: 1}

	result1, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("first"),
	}, req)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if result1.SessionUsage.InputTokens != 10 {
		t.Fatalf("session input tokens after 1st run = %d, want 10", result1.SessionUsage.InputTokens)
	}
	if result1.SessionUsage.OutputTokens != 5 {
		t.Fatalf("session output tokens after 1st run = %d, want 5", result1.SessionUsage.OutputTokens)
	}

	result2, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("second"),
	}, req)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if result2.SessionUsage.InputTokens != 30 {
		t.Fatalf("session input tokens after 2nd run = %d, want 30", result2.SessionUsage.InputTokens)
	}
	if result2.SessionUsage.OutputTokens != 13 {
		t.Fatalf("session output tokens after 2nd run = %d, want 13", result2.SessionUsage.OutputTokens)
	}
}

func TestServeRuntime_CumulativeUsageResetsOnFreshConversation(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddTurn(llm.MockTurn{Text: "a", Usage: llm.Usage{InputTokens: 100, OutputTokens: 50}}).
		AddTurn(llm.MockTurn{Text: "b", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}})
	engine := llm.NewEngine(provider, nil)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{SessionID: "reset-test", MaxTurns: 1}

	// First run accumulates usage
	result1, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("first"),
	}, req)
	if err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if result1.SessionUsage.InputTokens != 100 {
		t.Fatalf("after run 1: session input = %d, want 100", result1.SessionUsage.InputTokens)
	}

	// Second run with replaceHistory=true should reset cumulative usage
	result2, err := rt.Run(context.Background(), true, true, []llm.Message{
		llm.UserText("fresh start"),
	}, req)
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	// Should only reflect this run's usage, not accumulated
	if result2.SessionUsage.InputTokens != 10 {
		t.Fatalf("after fresh run: session input = %d, want 10 (reset)", result2.SessionUsage.InputTokens)
	}
	if result2.SessionUsage.OutputTokens != 5 {
		t.Fatalf("after fresh run: session output = %d, want 5 (reset)", result2.SessionUsage.OutputTokens)
	}
}

func TestRegisterResponseID_CapsAtMax(t *testing.T) {
	srv := &serveServer{}
	rt := &serveRuntime{}

	// Register more than maxResponseIDs
	for i := 0; i < maxResponseIDs+5; i++ {
		id := fmt.Sprintf("resp_%d", i)
		srv.registerResponseID(rt, id, "sess-1")
	}

	// Should be capped
	if got := len(rt.getResponseIDs()); got != maxResponseIDs {
		t.Fatalf("responseIDs len = %d, want %d", got, maxResponseIDs)
	}

	// First 5 IDs should be pruned from the map
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("resp_%d", i)
		if _, ok := srv.responseToSession.Load(id); ok {
			t.Fatalf("pruned ID %q still in responseToSession map", id)
		}
	}

	// Latest IDs should still be in the map
	for i := 5; i < maxResponseIDs+5; i++ {
		id := fmt.Sprintf("resp_%d", i)
		if _, ok := srv.responseToSession.Load(id); !ok {
			t.Fatalf("retained ID %q missing from responseToSession map", id)
		}
	}

	// lastResponseID should be the most recent
	expected := fmt.Sprintf("resp_%d", maxResponseIDs+4)
	if got := rt.getLastResponseID(); got != expected {
		t.Fatalf("lastResponseID = %q, want %q", got, expected)
	}
}

func TestResponseRunManagerCloseWaitsForDetachedRunShutdown(t *testing.T) {
	provider := newShutdownBlockingProvider()
	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{responseRuns: newServeResponseRunManager()}

	if _, err := srv.startResponseRun(rt, false, true, []llm.Message{
		llm.UserText("hi"),
	}, llm.Request{Model: "mock-model"}, "", startResponseRunOptions{}); err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}

	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response run to start streaming")
	}

	closeDone := make(chan struct{})
	go func() {
		srv.responseRuns.Close()
		close(closeDone)
	}()

	select {
	case <-provider.cancelled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response run cancellation")
	}

	select {
	case <-closeDone:
		t.Fatal("Close returned before detached response run finished unwinding")
	case <-time.After(100 * time.Millisecond):
	}

	select {
	case <-provider.cleanupDone:
		t.Fatal("stateless runtime cleanup completed before blocked run was released")
	default:
	}

	close(provider.release)

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for detached response run goroutine to finish")
	}

	select {
	case <-provider.cleanupDone:
	default:
		t.Fatal("stateless runtime cleanup was not finished when Close returned")
	}
}

func TestStartResponseRun_StatelessCleanupRemovesResponseIDMapping(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddTextResponse("hello")

	rt := &serveRuntime{
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	run, err := srv.startResponseRun(rt, false, true, []llm.Message{
		llm.UserText("hi"),
	}, llm.Request{Model: "mock-model"}, "", startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return run.status == "completed"
	}, "stateless response run completion")

	waitForServeCondition(t, time.Second, func() bool {
		_, ok := srv.responseToSession.Load(run.id)
		return !ok
	}, "stateless response ID cleanup")
}

func TestStartResponseRun_BusyConcurrentRunKeepsActiveSessionTracking(t *testing.T) {
	provider := newStagedProvider("hello ", "world")
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "staged",
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
	}
	rt.Touch()

	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	const sessionID = "sess_busy_active_tracking"
	run1, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("first"),
	}, llm.Request{SessionID: sessionID}, sessionID, startResponseRunOptions{})
	if err != nil {
		t.Fatalf("startResponseRun first: %v", err)
	}

	waitForServeCondition(t, time.Second, func() bool {
		return rt.hasActiveRun() && srv.responseRuns.activeRunID(sessionID) == run1.id
	}, "first response run to become active")

	run2, err := srv.startResponseRun(rt, true, false, []llm.Message{
		llm.UserText("second"),
	}, llm.Request{SessionID: sessionID}, sessionID, startResponseRunOptions{})
	if !errors.Is(err, errServeSessionBusy) || run2 != nil {
		t.Fatalf("busy admission returned run=%v err=%v", run2, err)
	}

	if got := srv.responseRuns.activeRunID(sessionID); got != run1.id {
		t.Fatalf("activeRunID after concurrent busy run = %q, want %q", got, run1.id)
	}

	close(provider.releaseSecond)

	waitForServeCondition(t, time.Second, func() bool {
		snapshot := run1.snapshot()
		status, _ := snapshot["status"].(string)
		return status == "completed"
	}, "first response run completion")

	waitForServeCondition(t, time.Second, func() bool {
		return srv.responseRuns.activeRunID(sessionID) == ""
	}, "active response run cleanup")
}

func TestServeSessionManager_Get_ExistingSession(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	// Create a session first.
	created, err := mgr.GetOrCreate(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Get should find it.
	got, ok := mgr.Get("sess-1")
	if !ok {
		t.Fatal("Get returned false for existing session")
	}
	if got != created {
		t.Fatal("Get returned different runtime than GetOrCreate")
	}
}

func TestServeSessionManager_Get_MissingSession(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		t.Fatal("factory should not be called")
		return nil, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	rt, ok := mgr.Get("nonexistent")
	if ok {
		t.Fatal("Get returned true for nonexistent session")
	}
	if rt != nil {
		t.Fatal("Get returned non-nil runtime for nonexistent session")
	}
}

func TestServeSessionManager_GetOrCreate_RespectsContextCancel(t *testing.T) {
	// Factory blocks until told to proceed.
	proceed := make(chan struct{})
	factory := func(ctx context.Context) (*serveRuntime, error) {
		<-proceed
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	// First call triggers factory (blocks).
	go func() {
		_, _ = mgr.GetOrCreate(context.Background(), "slow-sess")
	}()
	// Give time for in-flight to be registered.
	time.Sleep(20 * time.Millisecond)

	// Second call with a cancelled context should return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mgr.GetOrCreate(ctx, "slow-sess")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	// Unblock factory so cleanup works.
	close(proceed)
}

func TestServeServer_RuntimeForRequest_StatefulRespectsRequestCancel(t *testing.T) {
	started := make(chan struct{})
	factory := func(ctx context.Context) (*serveRuntime, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := srv.runtimeForRequest(ctx, "slow-sess")
		errCh <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runtimeForRequest did not return after request cancellation")
	}

	if _, ok := mgr.Get("slow-sess"); ok {
		t.Fatal("cancelled runtime creation should not store a session runtime")
	}
}

func TestServeServer_RuntimeForProviderRequest_StatefulRespectsRequestCancel(t *testing.T) {
	started := make(chan struct{})
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return nil, fmt.Errorf("default session manager factory should not be used")
	})
	defer mgr.Close()

	srv := &serveServer{
		sessionMgr: mgr,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			if providerName != "test-provider" {
				return nil, fmt.Errorf("expected provider test-provider, got %q", providerName)
			}
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := srv.runtimeForProviderRequest(ctx, "slow-sess", "test-provider")
		errCh <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runtimeForProviderRequest did not return after request cancellation")
	}

	if _, ok := mgr.Get("slow-sess"); ok {
		t.Fatal("cancelled runtime creation should not store a session runtime")
	}
}

func TestServeSessionManager_EvictionCallbackCleansResponseIDs(t *testing.T) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(50*time.Millisecond, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	rt, err := mgr.GetOrCreate(context.Background(), "evict-sess")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Simulate registering response IDs.
	srv.registerResponseID(rt, "resp_a", "evict-sess")
	srv.registerResponseID(rt, "resp_b", "evict-sess")

	// Verify mappings exist.
	if _, ok := srv.responseToSession.Load("resp_a"); !ok {
		t.Fatal("resp_a should exist before eviction")
	}
	if _, ok := srv.responseToSession.Load("resp_b"); !ok {
		t.Fatal("resp_b should exist before eviction")
	}

	// Wait for TTL, then evict explicitly.
	time.Sleep(75 * time.Millisecond)
	mgr.evictExpired()

	// Mappings should be cleaned up.
	if _, ok := srv.responseToSession.Load("resp_a"); ok {
		t.Fatal("resp_a should be cleaned up after eviction")
	}
	if _, ok := srv.responseToSession.Load("resp_b"); ok {
		t.Fatal("resp_b should be cleaned up after eviction")
	}
}

func TestServeSessionManager_GetOrCreate_ConcurrentDedup(t *testing.T) {
	var factoryCalls atomic.Int32
	factory := func(ctx context.Context) (*serveRuntime, error) {
		factoryCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()

	const workers = 20
	runtimes := make([]*serveRuntime, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			rt, err := mgr.GetOrCreate(context.Background(), "dedup-id")
			runtimes[idx] = rt
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d error: %v", i, err)
		}
	}

	// All workers should get the same runtime.
	first := runtimes[0]
	for i := 1; i < workers; i++ {
		if runtimes[i] != first {
			t.Fatalf("worker %d got different runtime pointer", i)
		}
	}

	// Factory should only have been called once.
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("factory called %d times, want 1", got)
	}
}

func TestHandlePushSubscribe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	t.Run("POST saves subscription", func(t *testing.T) {
		srv := &serveServer{store: store}
		body := `{"endpoint":"https://push.example.com/sub1","keys":{"p256dh":"keydata","auth":"authdata"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
		}

		// Verify subscription was persisted
		subs, err := store.ListPushSubscriptions(context.Background())
		if err != nil {
			t.Fatalf("ListPushSubscriptions: %v", err)
		}
		if len(subs) != 1 {
			t.Fatalf("subscription count = %d, want 1", len(subs))
		}
		if subs[0].Endpoint != "https://push.example.com/sub1" {
			t.Fatalf("endpoint = %q, want %q", subs[0].Endpoint, "https://push.example.com/sub1")
		}
	})

	t.Run("DELETE removes subscription", func(t *testing.T) {
		srv := &serveServer{store: store}
		body := `{"endpoint":"https://push.example.com/sub1"}`
		req := httptest.NewRequest(http.MethodDelete, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
		}

		subs, err := store.ListPushSubscriptions(context.Background())
		if err != nil {
			t.Fatalf("ListPushSubscriptions: %v", err)
		}
		if len(subs) != 0 {
			t.Fatalf("subscription count = %d, want 0", len(subs))
		}
	})

	t.Run("POST missing fields returns 400", func(t *testing.T) {
		srv := &serveServer{store: store}
		body := `{"endpoint":"https://push.example.com/sub2"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("no store returns 503", func(t *testing.T) {
		srv := &serveServer{}
		body := `{"endpoint":"https://push.example.com/sub3","keys":{"p256dh":"k","auth":"a"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("auth required returns 401 without token", func(t *testing.T) {
		srv := &serveServer{
			store: store,
			cfg:   serveServerConfig{requireAuth: true, token: "secret"},
		}
		body := `{"endpoint":"https://push.example.com/sub4","keys":{"p256dh":"k","auth":"a"}}`
		req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.auth(srv.handlePushSubscribe)(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("GET not allowed", func(t *testing.T) {
		srv := &serveServer{store: store}
		req := httptest.NewRequest(http.MethodGet, "/v1/push/subscribe", nil)
		rr := httptest.NewRecorder()
		srv.handlePushSubscribe(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405; body: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestPushSubscribe_EndToEnd stores a subscription via the handler using
// base64url-encoded keys (matching real browser toJSON() output), then calls
// sendWebPush against a local httptest push server. This validates the full
// chain: handler -> DB -> webpush-go encrypt+send.
func TestPushSubscribe_EndToEnd(t *testing.T) {
	// Generate a real P-256 ECDH key pair (simulates the browser's key).
	browserKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate browser key: %v", err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(browserKey.PublicKey().Bytes())

	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	auth := base64.RawURLEncoding.EncodeToString(authSecret)

	// Mock push service that records whether it received a request.
	var pushReceived atomic.Bool
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushReceived.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	defer pushServer.Close()

	// Store via handler (same JSON shape as subscription.toJSON()).
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	srv := &serveServer{store: store}
	subBody := fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`,
		pushServer.URL, p256dh, auth)
	req := httptest.NewRequest(http.MethodPost, "/v1/push/subscribe", strings.NewReader(subBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handlePushSubscribe(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("subscribe status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	// Read the stored subscription back
	subs, err := store.ListPushSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subscription count = %d, want 1", len(subs))
	}

	// Verify keys were stored in base64url (no +, /, or trailing =)
	for _, key := range []string{subs[0].KeyP256DH, subs[0].KeyAuth} {
		if strings.ContainsAny(key, "+/=") {
			t.Fatalf("stored key %q contains standard base64 characters; expected base64url", key)
		}
	}

	// Generate VAPID keys and send a push notification via sendWebPush.
	vapidPriv, vapidPub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("generate VAPID keys: %v", err)
	}

	payload, _ := json.Marshal(map[string]string{"title": "test", "body": "hello"})
	opts := &webpush.Options{
		VAPIDPublicKey:  vapidPub,
		VAPIDPrivateKey: vapidPriv,
		Subscriber:      "mailto:test@example.com",
		TTL:             30,
	}

	status, err := sendWebPush(context.Background(), &subs[0], payload, opts)
	if err != nil {
		t.Fatalf("sendWebPush error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", status)
	}
	if !pushReceived.Load() {
		t.Fatal("mock push server never received a request")
	}
}

func TestHandleProviders_ReturnsList(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "anthropic",
		Providers: map[string]config.ProviderConfig{
			"anthropic": {Model: "claude-sonnet-4-6"},
			"openai":    {Model: "gpt-5", ServiceTier: "fast"},
		},
	}
	srv := &serveServer{cfgRef: cfg}
	req := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	rr := httptest.NewRecorder()
	srv.handleProviders(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var result struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if result.Object != "list" {
		t.Fatalf("object = %q, want list", result.Object)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected at least one provider")
	}
	// Check that the default provider is marked and configured tiers are normalized.
	found := false
	foundFast := false
	for _, p := range result.Data {
		if p["name"] == "anthropic" {
			if p["is_default"] != true {
				t.Errorf("anthropic should be marked as default")
			}
			found = true
		}
		if p["name"] == "openai" {
			if p["service_tier"] != llm.ServiceTierFast {
				t.Errorf("openai service_tier = %#v, want %q", p["service_tier"], llm.ServiceTierFast)
			}
			foundFast = true
		}
	}
	if !found {
		t.Error("expected anthropic in provider list")
	}
	if !foundFast {
		t.Error("expected openai in provider list")
	}
}

func TestHandleProviders_MethodNotAllowed(t *testing.T) {
	srv := &serveServer{cfgRef: &config.Config{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/providers", nil)
	rr := httptest.NewRecorder()
	srv.handleProviders(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestHandleModels_WithProviderParam(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "anthropic",
		Providers: map[string]config.ProviderConfig{
			"anthropic": {Model: "claude-sonnet-4-6"},
		},
	}
	// Pre-seed the cache so getModelsProvider doesn't try to construct a real
	// Anthropic provider (which requires auth not present in CI).
	mock := llm.NewMockProvider("anthropic")
	srv := &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]llm.Provider{"anthropic": mock},
	}

	// Without provider param — uses default
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected at least one model for default provider")
	}

	// With unknown provider param — returns error
	req = httptest.NewRequest(http.MethodGet, "/v1/models?provider=nonexistent", nil)
	rr = httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown provider", rr.Code)
	}
}

func TestHandleModels_PrefersUpstreamListOverLocalFallback(t *testing.T) {
	provider := &countingListModelsProvider{
		name:   "acme",
		models: []llm.ModelInfo{{ID: "from-upstream", InputPrice: 0.22, OutputPrice: 0.59}},
	}
	srv := &serveServer{
		cfgRef: &config.Config{
			DefaultProvider: "acme",
			Providers: map[string]config.ProviderConfig{
				"acme": {
					Type:    config.ProviderTypeOpenAICompat,
					BaseURL: "http://example.invalid/v1",
					Model:   "acme-pro",
					Models:  []string{"acme-fast", "acme-pro"},
				},
			},
		},
		modelsProviders: map[string]llm.Provider{"acme": provider},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if calls := provider.CallCount(); calls != 1 {
		t.Fatalf("ListModels call count = %d, want 1", calls)
	}

	var result struct {
		Data []struct {
			ID          string  `json:"id"`
			InputPrice  float64 `json:"input_price"`
			OutputPrice float64 `json:"output_price"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	got := make([]string, 0, len(result.Data))
	for _, item := range result.Data {
		got = append(got, item.ID)
	}
	if !reflect.DeepEqual(got, []string{"from-upstream"}) {
		t.Fatalf("model ids = %v, want [from-upstream]", got)
	}
	if result.Data[0].InputPrice != 0.22 || result.Data[0].OutputPrice != 0.59 {
		t.Fatalf("pricing = %g/%g, want 0.22/0.59", result.Data[0].InputPrice, result.Data[0].OutputPrice)
	}
}

func TestHandleModels_CachesUpstreamListResults(t *testing.T) {
	provider := &countingListModelsProvider{
		name: "custom",
		models: []llm.ModelInfo{
			{ID: "custom-a"},
			{ID: "custom-b"},
		},
	}
	srv := &serveServer{
		cfgRef: &config.Config{
			DefaultProvider: "custom",
			Providers: map[string]config.ProviderConfig{
				"custom": {
					Type:    config.ProviderTypeOpenAICompat,
					BaseURL: "http://example.invalid/v1",
				},
			},
		},
		modelsProviders: map[string]llm.Provider{"custom": provider},
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rr := httptest.NewRecorder()
		srv.handleModels(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200; body: %s", i+1, rr.Code, rr.Body.String())
		}
	}
	if calls := provider.CallCount(); calls != 1 {
		t.Fatalf("ListModels call count = %d, want 1", calls)
	}
}

func TestHandleModels_ReportsFastModeMetadata(t *testing.T) {
	provider := &countingListModelsProvider{
		name: "chatgpt",
		models: []llm.ModelInfo{{
			ID:                   "gpt-fast",
			ServiceTiers:         []llm.ModelServiceTier{{ID: llm.ServiceTierFast, Name: "fast"}},
			AdditionalSpeedTiers: []string{"fast"},
		}},
	}
	srv := &serveServer{
		cfgRef: &config.Config{
			DefaultProvider: "chatgpt",
			Providers:       map[string]config.ProviderConfig{"chatgpt": {Model: "gpt-fast"}},
		},
		modelsProviders: map[string]llm.Provider{"chatgpt": provider},
	}

	rr := httptest.NewRecorder()
	srv.handleModels(rr, httptest.NewRequest(http.MethodGet, "/v1/models?provider=chatgpt", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []struct {
			ID                   string                 `json:"id"`
			ServiceTiers         []llm.ModelServiceTier `json:"service_tiers"`
			AdditionalSpeedTiers []string               `json:"additional_speed_tiers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].ID != "gpt-fast" {
		t.Fatalf("models = %#v", result.Data)
	}
	if len(result.Data[0].ServiceTiers) != 1 || result.Data[0].ServiceTiers[0].ID != llm.ServiceTierFast {
		t.Fatalf("service_tiers = %#v", result.Data[0].ServiceTiers)
	}
	if len(result.Data[0].AdditionalSpeedTiers) != 1 || result.Data[0].AdditionalSpeedTiers[0] != "fast" {
		t.Fatalf("additional_speed_tiers = %#v", result.Data[0].AdditionalSpeedTiers)
	}
}

func TestHandleModels_DropsEffortSuffixDuplicates(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "claude-bin",
		Providers:       map[string]config.ProviderConfig{"claude-bin": {}},
	}
	// Use the MockProvider so ListModels returns nothing and we fall back
	// to the curated claude-bin list (which contains opus-low/medium/high/...).
	mock := llm.NewMockProvider("claude-bin")
	srv := &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]llm.Provider{"claude-bin": mock},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?provider=claude-bin", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	got := make(map[string]map[string]any, len(result.Data))
	for _, m := range result.Data {
		id, _ := m["id"].(string)
		got[id] = m
	}
	// Base models must remain.
	for _, want := range []string{"opus", "sonnet", "haiku"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected %q in models response, got %v", want, got)
		}
	}
	// Effort-suffixed aliases must be filtered out — the web UI has a
	// dedicated reasoning-effort selector for these.
	for _, banned := range []string{"opus-low", "opus-medium", "opus-high", "opus-xhigh", "opus-max", "sonnet-low", "sonnet-medium", "sonnet-high"} {
		if _, ok := got[banned]; ok {
			t.Errorf("unexpected %q in models response (should be deduped by effort selector)", banned)
		}
	}
	opusEfforts, _ := got["opus"]["reasoning_efforts"].([]any)
	if !stringAnySliceContains(opusEfforts, "max") {
		t.Fatalf("opus reasoning_efforts = %#v, want max", got["opus"]["reasoning_efforts"])
	}
	sonnetEfforts, _ := got["sonnet"]["reasoning_efforts"].([]any)
	if stringAnySliceContains(sonnetEfforts, "max") {
		t.Fatalf("sonnet reasoning_efforts = %#v, want no max", got["sonnet"]["reasoning_efforts"])
	}
}

func TestHandleModels_ReportsConfiguredReasoningDefault(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "cdck_deepseek",
		Providers: map[string]config.ProviderConfig{
			"cdck_deepseek": {
				Model:  "deepseek-ai/DeepSeek-V4-Flash",
				Models: []string{"deepseek-v4-flash"},
				ModelConfigs: []config.ProviderModelConfig{{
					ID:                     "deepseek-ai/DeepSeek-V4-Flash",
					Alias:                  "deepseek-v4-flash",
					ReasoningEfforts:       []string{"none", "low", "high", "max"},
					DefaultReasoningEffort: "high",
				}},
			},
		},
	}
	srv := &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]llm.Provider{"cdck_deepseek": llm.NewMockProvider("cdck_deepseek")},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?provider=cdck_deepseek", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	for _, model := range result.Data {
		if model["id"] != "deepseek-ai/DeepSeek-V4-Flash" {
			continue
		}
		if model["display_name"] != "deepseek-v4-flash" {
			t.Fatalf("display_name = %#v, want configured alias", model["display_name"])
		}
		if model["default_reasoning_effort"] != "high" {
			t.Fatalf("default_reasoning_effort = %#v", model["default_reasoning_effort"])
		}
		efforts, _ := model["reasoning_efforts"].([]any)
		for _, want := range []string{"none", "low", "high", "max"} {
			if !stringAnySliceContains(efforts, want) {
				t.Fatalf("reasoning_efforts = %#v, missing %q", efforts, want)
			}
		}
		return
	}
	t.Fatalf("configured model missing in %s", rr.Body.String())
}

func TestHandleModels_ReportsGPT5EffortsWithoutMax(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "chatgpt",
		Providers:       map[string]config.ProviderConfig{"chatgpt": {Model: "gpt-5.5"}},
	}
	mock := llm.NewMockProvider("chatgpt")
	srv := &serveServer{
		cfgRef:          cfg,
		modelsProviders: map[string]llm.Provider{"chatgpt": mock},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?provider=chatgpt", nil)
	rr := httptest.NewRecorder()
	srv.handleModels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	var efforts []any
	for _, m := range result.Data {
		if m["id"] == "gpt-5.5" {
			efforts, _ = m["reasoning_efforts"].([]any)
			break
		}
	}
	if len(efforts) == 0 {
		t.Fatalf("gpt-5.5 reasoning_efforts missing in %s", rr.Body.String())
	}
	for _, want := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		if !stringAnySliceContains(efforts, want) {
			t.Fatalf("gpt-5.5 reasoning_efforts = %#v, missing %q", efforts, want)
		}
	}
	if stringAnySliceContains(efforts, "max") {
		t.Fatalf("gpt-5.5 reasoning_efforts = %#v, want no max", efforts)
	}
}

func stringAnySliceContains(values []any, want string) bool {
	for _, v := range values {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

func TestHandleResponses_WithProviderField(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	manager := newServeSessionManager(time.Minute, 10, factory)
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			engine := llm.NewEngine(provider, nil)
			rt := &serveRuntime{
				provider:     provider,
				providerKey:  providerName,
				engine:       engine,
				defaultModel: "mock-model",
			}
			rt.Touch()
			return rt, nil
		},
	}

	// Request with non-default provider creates session with that provider
	body := `{"input":"hello","provider":"other"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleResponses_PreservesClientDefinedPassthroughTools(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_passthrough_1", "client_tool", map[string]any{"value": "ok"})

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	manager := newServeSessionManager(time.Minute, 10, factory)
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "mock"},
		sessionMgr: manager,
	}

	code, _ := doResponses(t, srv, `{
		"input":"hello",
		"tools":[{
			"type":"function",
			"name":"client_tool",
			"description":"Client-defined passthrough tool",
			"parameters":{
				"type":"object",
				"properties":{"value":{"type":"string"}},
				"required":["value"]
			}
		}],
		"tool_choice":{"type":"function","name":"client_tool"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}

	req := provider.Requests[0]
	if len(req.Tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Name != "client_tool" {
		t.Fatalf("tool name = %q, want client_tool", req.Tools[0].Name)
	}
	if req.Tools[0].Description != "Client-defined passthrough tool" {
		t.Fatalf("tool description = %q", req.Tools[0].Description)
	}
	if req.ToolChoice.Mode != llm.ToolChoiceName || req.ToolChoice.Name != "client_tool" {
		t.Fatalf("tool choice = %#v, want name client_tool", req.ToolChoice)
	}
	props, ok := req.Tools[0].Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool schema properties missing: %#v", req.Tools[0].Schema)
	}
	valueProp, ok := props["value"].(map[string]interface{})
	if !ok || valueProp["type"] != "string" {
		t.Fatalf("tool schema value property = %#v, want string type", props["value"])
	}
}

func TestSessionManager_GetOrCreateWithDeduplication(t *testing.T) {
	var calls int32
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return &serveRuntime{providerKey: "default"}, nil
	})
	defer manager.Close()

	const workers = 12
	results := make(chan *serveRuntime, workers)
	errs := make(chan error, workers)

	customFactory := func(ctx context.Context) (*serveRuntime, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(25 * time.Millisecond)
		rt := &serveRuntime{providerKey: "custom"}
		rt.Touch()
		return rt, nil
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			rt, err := manager.GetOrCreateWith(context.Background(), "same-id", customFactory)
			if err != nil {
				errs <- err
				return
			}
			results <- rt
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("GetOrCreateWith error: %v", err)
	}

	var first *serveRuntime
	for rt := range results {
		if first == nil {
			first = rt
			continue
		}
		if rt != first {
			t.Fatalf("expected all calls to return same runtime pointer")
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("factory calls = %d, want 1", got)
	}
	if first.providerKey != "custom" {
		t.Fatalf("providerKey = %q, want custom", first.providerKey)
	}
}

// ---------------------------------------------------------------------------
// Anthropic Messages API tests
// ---------------------------------------------------------------------------

func TestParseAnthropicMessages_SimpleText(t *testing.T) {
	msgs, err := parseAnthropicMessages([]anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"Hello"`)},
	})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser {
		t.Fatalf("role = %s, want user", msgs[0].Role)
	}
	if msgs[0].Parts[0].Text != "Hello" {
		t.Fatalf("text = %q, want Hello", msgs[0].Parts[0].Text)
	}
}

func TestParseAnthropicMessages_ContentBlocks(t *testing.T) {
	msgs, err := parseAnthropicMessages([]anthropicMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"Hi"},{"type":"text","text":" there"}]`)},
	})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	if len(msgs[0].Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(msgs[0].Parts))
	}
	if msgs[0].Parts[0].Text != "Hi" || msgs[0].Parts[1].Text != " there" {
		t.Fatalf("unexpected text parts")
	}
}

func TestParseAnthropicMessages_ImageContentSavesPath(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	b64 := base64.StdEncoding.EncodeToString([]byte("png bytes"))
	content := json.RawMessage(fmt.Sprintf(`[{ 
		"type":"image",
		"source":{"type":"base64","media_type":"image/png","data":%q}
	}]`, b64))

	msgs, err := parseAnthropicMessages([]anthropicMessage{{Role: "user", Content: content}})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Parts) != 1 || msgs[0].Parts[0].Type != llm.PartImage {
		t.Fatalf("messages = %#v, want one image part", msgs)
	}
	part := msgs[0].Parts[0]
	if part.ImagePath == "" {
		t.Fatal("ImagePath is empty, want saved upload path")
	}
	if filepath.Ext(part.ImagePath) != ".png" {
		t.Fatalf("ImagePath = %q, want .png extension", part.ImagePath)
	}
	if !strings.HasPrefix(part.ImagePath, filepath.Join(dataHome, "term-llm", "uploads")) {
		t.Fatalf("ImagePath = %q, want under uploads dir", part.ImagePath)
	}
}

func TestParseAnthropicMessages_InvalidImageBase64ReturnsError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	content := json.RawMessage(`[{
		"type":"image",
		"source":{"type":"base64","media_type":"image/png","data":"not base64!!!"}
	}]`)

	_, err := parseAnthropicMessages([]anthropicMessage{{Role: "user", Content: content}})
	if err == nil || !strings.Contains(err.Error(), "decode image attachment") {
		t.Fatalf("parseAnthropicMessages error = %v, want decode image attachment", err)
	}
}

func TestParseAnthropicMessages_ToolUseRoundTrip(t *testing.T) {
	msgs, err := parseAnthropicMessages([]anthropicMessage{
		{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"a.txt"}}]`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_1","content":"file contents"}]`)},
	})
	if err != nil {
		t.Fatalf("parseAnthropicMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleAssistant {
		t.Fatalf("first role = %s, want assistant", msgs[0].Role)
	}
	if msgs[0].Parts[0].ToolCall == nil || msgs[0].Parts[0].ToolCall.Name != "read_file" {
		t.Fatalf("missing tool call")
	}
	if msgs[1].Parts[0].ToolResult == nil || msgs[1].Parts[0].ToolResult.ID != "call_1" {
		t.Fatalf("missing tool result")
	}
	if msgs[1].Parts[0].ToolResult.Content != "file contents" {
		t.Fatalf("tool result content = %q", msgs[1].Parts[0].ToolResult.Content)
	}
}

func TestParseAnthropicSystem(t *testing.T) {
	// String form
	if got := parseAnthropicSystem(json.RawMessage(`"Be helpful"`)); got != "Be helpful" {
		t.Fatalf("string system = %q", got)
	}
	// Array form
	if got := parseAnthropicSystem(json.RawMessage(`[{"type":"text","text":"System prompt"}]`)); got != "System prompt" {
		t.Fatalf("array system = %q", got)
	}
	// Empty
	if got := parseAnthropicSystem(nil); got != "" {
		t.Fatalf("nil system = %q", got)
	}
}

func TestParseAnthropicToolChoice(t *testing.T) {
	if got := parseAnthropicToolChoice(json.RawMessage(`{"type":"auto"}`)); got.Mode != llm.ToolChoiceAuto {
		t.Fatalf("auto mode = %s", got.Mode)
	}
	if got := parseAnthropicToolChoice(json.RawMessage(`{"type":"any"}`)); got.Mode != llm.ToolChoiceRequired {
		t.Fatalf("any mode = %s", got.Mode)
	}
	if got := parseAnthropicToolChoice(json.RawMessage(`{"type":"tool","name":"shell"}`)); got.Mode != llm.ToolChoiceName || got.Name != "shell" {
		t.Fatalf("tool mode = %#v", got)
	}
	if got := parseAnthropicToolChoice(nil); got.Mode != llm.ToolChoiceAuto {
		t.Fatalf("nil mode = %s", got.Mode)
	}
}

func TestHandleAnthropicMessages_NonStreaming(t *testing.T) {
	srv := newTestServeServer("Hello from Anthropic!")
	body := `{"model":"test","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["type"] != "message" {
		t.Fatalf("type = %v, want message", result["type"])
	}
	if result["role"] != "assistant" {
		t.Fatalf("role = %v, want assistant", result["role"])
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content empty or wrong type")
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Hello from Anthropic!" {
		t.Fatalf("content block = %v", block)
	}
	if result["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", result["stop_reason"])
	}
}

func TestHandleAnthropicMessages_DoesNotExposeUnrequestedServerTools(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(_ context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{
		"model": "test",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{
			"name": "client_tool",
			"description": "Client-defined passthrough tool",
			"input_schema": {
				"type": "object",
				"properties": {
					"query": {"type": "string"}
				},
				"required": ["query"]
			}
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(provider.Requests))
	}
	tools := provider.Requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("expected 1 passthrough tool, got %d", len(tools))
	}
	if tools[0].Name != "client_tool" {
		t.Fatalf("expected tool name client_tool, got %q", tools[0].Name)
	}
	if tools[0].Description != "Client-defined passthrough tool" {
		t.Fatalf("unexpected tool description: %q", tools[0].Description)
	}
}

func TestHandleAnthropicMessages_NoToolsDoesNotExposeServerTools(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(_ context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{
		"model": "test",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hi"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(provider.Requests))
	}
	if got := len(provider.Requests[0].Tools); got != 0 {
		t.Fatalf("expected 0 tools, got %d", got)
	}
}

func TestHandleAnthropicMessages_StreamText(t *testing.T) {
	srv := newTestServeServer("streamed text")
	body := `{"model":"test","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	output := rr.Body.String()

	// Verify required SSE events are present
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in SSE output", want)
		}
	}

	// Verify text_delta contains our text (mock chunks into ~10 char pieces)
	if !strings.Contains(output, "streamed") {
		t.Errorf("missing 'streamed' in output")
	}
	if !strings.Contains(output, "text") {
		t.Errorf("missing 'text' in output")
	}

	// Verify stop_reason is end_turn (no tool calls)
	if !strings.Contains(output, `"stop_reason":"end_turn"`) {
		t.Errorf("missing end_turn stop_reason")
	}
}

func TestHandleAnthropicMessages_Auth_XApiKey(t *testing.T) {
	srv := newTestServeServer("ok")
	srv.cfg.requireAuth = true
	srv.cfg.token = "secret-token"

	body := `{"model":"test","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`

	// No auth → 401
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.auth(srv.handleAnthropicMessages)(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", rr.Code)
	}

	// x-api-key → 200
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "secret-token")
	rr = httptest.NewRecorder()
	srv.auth(srv.handleAnthropicMessages)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("x-api-key: status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	// Bearer token also still works
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")
	rr = httptest.NewRecorder()
	srv.auth(srv.handleAnthropicMessages)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer: status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAnthropicMessages_MethodNotAllowed(t *testing.T) {
	srv := newTestServeServer("ok")
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleAnthropicMessages(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

// newTestServeServerWithToolMap creates a test serve server with a registered
// "echo" tool and the given toolMap for testing --tool-map behavior.
func newTestServeServerWithToolMap(toolMap map[string]string, responses ...string) *serveServer {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		for _, r := range responses {
			provider.AddTextResponse(r)
		}
		registry := llm.NewToolRegistry()
		registry.Register(&echoTool{})
		engine := llm.NewEngine(provider, registry)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
			toolMap:      toolMap,
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	return srv
}

func TestSelectTools_ResolvesToolMapNames(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{}) // registers as "echo"
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider: provider,
		engine:   engine,
		toolMap:  map[string]string{"MyEcho": "echo"},
	}

	// Requesting the client name "MyEcho" should resolve to server tool "echo"
	tools := rt.selectTools(map[string]bool{"MyEcho": true})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Fatalf("expected tool name 'echo', got %q", tools[0].Name)
	}

	// Requesting by server name directly should also work
	tools = rt.selectTools(map[string]bool{"echo": true})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool when using server name, got %d", len(tools))
	}

	// Requesting an unknown name should return nothing
	tools = rt.selectTools(map[string]bool{"nonexistent": true})
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools for unknown name, got %d", len(tools))
	}

	// No filter returns all
	tools = rt.selectTools(nil)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool for nil filter, got %d", len(tools))
	}
}

func TestSelectTools_OmitsManageWorkspaceInYoloWithoutUnregisteringIt(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	registry := llm.NewToolRegistry()
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	registry.Register(tools.NewManageWorkspaceTool(approval))
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{
		provider: provider,
		engine:   engine,
		toolMgr:  &tools.ToolManager{ApprovalMgr: approval},
	}

	if got := rt.selectTools(nil); len(got) != 2 {
		t.Fatalf("prompt tools = %v, want manage_workspace and echo", serveToolSpecNames(got))
	}

	approval.SetApprovalMode(tools.ModeYolo)
	got := rt.selectTools(nil)
	if len(got) != 1 || got[0].Name != "echo" {
		t.Fatalf("yolo tools = %v, want only echo", serveToolSpecNames(got))
	}
	if _, ok := engine.Tools().Get(tools.ManageWorkspaceToolName); !ok {
		t.Fatal("manage_workspace executor was unregistered in yolo mode")
	}

	approval.SetApprovalMode(tools.ModePrompt)
	if got := rt.selectTools(nil); len(got) != 2 {
		t.Fatalf("restored prompt tools = %v, want manage_workspace and echo", serveToolSpecNames(got))
	}
}

func serveToolSpecNames(specs []llm.ToolSpec) []string {
	names := make([]string, len(specs))
	for i, spec := range specs {
		names[i] = spec.Name
	}
	return names
}

func TestToolMap_ChatCompletions(t *testing.T) {
	srv := newTestServeServerWithToolMap(
		map[string]string{"MyEcho": "echo"},
		"mapped tool works",
	)

	body := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{"type": "function", "function": {"name": "MyEcho"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices in response")
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "mapped tool works" {
		t.Fatalf("unexpected content: %v", msg["content"])
	}
}

func TestChatCompletions_PreservesClientPassthroughTools(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	engine := llm.NewEngine(provider, nil)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "client_tool",
				"description": "Client-defined passthrough tool",
				"parameters": {
					"type": "object",
					"properties": {
						"query": {"type": "string"}
					},
					"required": ["query"]
				}
			}
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request, got %d", len(provider.Requests))
	}
	tools := provider.Requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("expected 1 passthrough tool, got %d", len(tools))
	}
	if tools[0].Name != "client_tool" {
		t.Fatalf("expected tool name client_tool, got %q", tools[0].Name)
	}
	if tools[0].Description != "Client-defined passthrough tool" {
		t.Fatalf("unexpected tool description: %q", tools[0].Description)
	}
	if got := tools[0].Schema["type"]; got != "object" {
		t.Fatalf("expected tool schema type object, got %#v", got)
	}
	props, ok := tools[0].Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected schema properties map, got %#v", tools[0].Schema["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Fatalf("expected query property in schema, got %#v", props)
	}
}

func TestChatCompletions_ToolResultWithoutReplayedToolCallKeepsNameFromSessionHistory(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "client_tool", map[string]any{"query": "hi"}).
		AddTextResponse("all set")
	engine := llm.NewEngine(provider, nil)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	firstBody := `{
		"model": "test",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [{"type": "function", "function": {"name": "client_tool"}}]
	}`
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(firstBody))
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("session_id", "tool-history-session")
	firstResp := httptest.NewRecorder()
	srv.handleChatCompletions(firstResp, firstReq)
	if firstResp.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body: %s", firstResp.Code, firstResp.Body.String())
	}

	secondBody := `{
		"model": "test",
		"messages": [{"role": "tool", "tool_call_id": "call-1", "content": "done"}]
	}`
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(secondBody))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("session_id", "tool-history-session")
	secondResp := httptest.NewRecorder()
	srv.handleChatCompletions(secondResp, secondReq)
	if secondResp.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body: %s", secondResp.Code, secondResp.Body.String())
	}

	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}
	if len(provider.Requests[1].Messages) != 3 {
		t.Fatalf("second provider request message count = %d, want 3", len(provider.Requests[1].Messages))
	}
	if provider.Requests[1].Messages[0].Role != llm.RoleUser || len(provider.Requests[1].Messages[0].Parts) != 1 || provider.Requests[1].Messages[0].Parts[0].Type != llm.PartText || provider.Requests[1].Messages[0].Parts[0].Text != "Hi" {
		t.Fatalf("second request message[0] = %+v, want original user message", provider.Requests[1].Messages[0])
	}
	if provider.Requests[1].Messages[1].Role != llm.RoleAssistant || len(provider.Requests[1].Messages[1].Parts) != 1 || provider.Requests[1].Messages[1].Parts[0].Type != llm.PartToolCall || provider.Requests[1].Messages[1].Parts[0].ToolCall == nil || provider.Requests[1].Messages[1].Parts[0].ToolCall.ID != "call-1" {
		t.Fatalf("second request message[1] = %+v, want assistant tool call", provider.Requests[1].Messages[1])
	}
	if provider.Requests[1].Messages[2].Role != llm.RoleTool || len(provider.Requests[1].Messages[2].Parts) != 1 || provider.Requests[1].Messages[2].Parts[0].Type != llm.PartToolResult || provider.Requests[1].Messages[2].Parts[0].ToolResult == nil || provider.Requests[1].Messages[2].Parts[0].ToolResult.ID != "call-1" {
		t.Fatalf("second request message[2] = %+v, want tool result", provider.Requests[1].Messages[2])
	}

	var toolResultName string
	for _, msg := range provider.Requests[1].Messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.ID != "call-1" {
				continue
			}
			toolResultName = part.ToolResult.Name
		}
	}
	if toolResultName != "client_tool" {
		t.Fatalf("tool result name = %q, want %q", toolResultName, "client_tool")
	}
}

func TestToolMap_Responses(t *testing.T) {
	srv := newTestServeServerWithToolMap(
		map[string]string{"MyEcho": "echo"},
		"mapped response works",
	)

	body := `{
		"model": "test",
		"input": "Hi",
		"tools": [{"type": "function", "name": "MyEcho"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output, ok := result["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("expected output in response")
	}
	msg := output[0].(map[string]any)
	content := msg["content"].([]any)[0].(map[string]any)
	if content["text"] != "mapped response works" {
		t.Fatalf("unexpected text: %v", content["text"])
	}
}

func TestResolvePlatforms_API(t *testing.T) {
	got, err := resolvePlatforms([]string{"api"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "api" {
		t.Fatalf("got %v, want [api]", got)
	}
}

func TestSingleServeTemplatePlatform_API(t *testing.T) {
	if got := singleServeTemplatePlatform([]string{"api"}); got != "api" {
		t.Fatalf("got %q, want %q", got, "api")
	}
}

func TestServeHTTPHandler_MountsAPIOnlyUnderBasePath(t *testing.T) {
	srv := &serveServer{
		cfg:        serveServerConfig{basePath: "/ui", api: true},
		sessionMgr: newServeSessionManager(time.Minute, 10, nil),
	}
	handler := srv.httpHandler()
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// API routes should be reachable under basePath
	resp, err := http.Get(ts.URL + "/ui/healthz")
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	// Root should not redirect (no UI)
	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("root request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusTemporaryRedirect {
		t.Fatalf("api-only should not redirect root to basePath")
	}
}

func TestNonStreamingChat_ShowsServerExecutedToolCalls(t *testing.T) {
	// Script: model calls the echo tool (server-executed), then returns "done".
	// The chat completions endpoint is used by the web UI, which needs to see
	// tool calls so it can display them. They must NOT be filtered here.
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{"model":"test","messages":[{"role":"user","content":"call echo"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := result["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	// "echo" is server-executed but the chat completions handler must pass tool
	// calls through so the web UI can display them.
	if msg["tool_calls"] == nil {
		t.Fatalf("expected tool_calls to be visible in chat completions response, got nil")
	}
	if msg["content"] != "done" {
		t.Fatalf("expected final text 'done', got %v", msg["content"])
	}
}

func TestChatCompletionFinalResponse_UsageIncludesCachedPromptTokens(t *testing.T) {
	var result serveRunResult
	result.Text.WriteString("done")
	result.Usage = llm.Usage{
		InputTokens:       100,
		CachedInputTokens: 20,
		CacheWriteTokens:  5,
		OutputTokens:      7,
	}

	response := chatCompletionFinalResponse(result, "test-model")
	usage := response["usage"].(map[string]any)

	if got := usage["prompt_tokens"]; got != 120 {
		t.Fatalf("prompt_tokens = %v, want 120", got)
	}
	if got := usage["completion_tokens"]; got != 7 {
		t.Fatalf("completion_tokens = %v, want 7", got)
	}
	if got := usage["total_tokens"]; got != 127 {
		t.Fatalf("total_tokens = %v, want 127", got)
	}

	details := usage["prompt_tokens_details"].(map[string]any)
	if got := details["cached_tokens"]; got != 20 {
		t.Fatalf("cached_tokens = %v, want 20", got)
	}
	if got := details["cache_write_tokens"]; got != 5 {
		t.Fatalf("cache_write_tokens = %v, want 5", got)
	}
}

func TestStreamingChatIncludeUsage_UsageIncludesCachedPromptTokens(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{
		Text: "done",
		Usage: llm.Usage{
			InputTokens:       100,
			CachedInputTokens: 20,
			CacheWriteTokens:  5,
			OutputTokens:      7,
		},
	})
	engine := llm.NewEngine(provider, nil)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}

	body := `{
		"model": "test",
		"stream": true,
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "Hi"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleChatCompletions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	scanner := bufio.NewScanner(bytes.NewReader(rr.Body.Bytes()))
	for {
		_, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before usage chunk")
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal stream chunk: %v", err)
		}

		usageVal, ok := payload["usage"]
		if !ok {
			continue
		}
		usage := usageVal.(map[string]any)
		if got := usage["prompt_tokens"].(float64); got != 120 {
			t.Fatalf("prompt_tokens = %v, want 120", got)
		}
		if got := usage["completion_tokens"].(float64); got != 7 {
			t.Fatalf("completion_tokens = %v, want 7", got)
		}
		if got := usage["total_tokens"].(float64); got != 127 {
			t.Fatalf("total_tokens = %v, want 127", got)
		}
		details := usage["prompt_tokens_details"].(map[string]any)
		if got := details["cached_tokens"].(float64); got != 20 {
			t.Fatalf("cached_tokens = %v, want 20", got)
		}
		if got := details["cache_write_tokens"].(float64); got != 5 {
			t.Fatalf("cache_write_tokens = %v, want 5", got)
		}
		return
	}

	t.Fatal("did not find usage chunk in stream")
}

func TestNonStreamingResponses_FiltersServerExecutedToolCalls(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddTextResponse("done")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	factory := func(ctx context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr, cfg: serveServerConfig{suppressServerTools: true}}

	body := `{"model":"test","input":"call echo"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	var result map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output := result["output"].([]any)
	// Should have only the text message, no function_call items
	for _, item := range output {
		m := item.(map[string]any)
		if m["type"] == "function_call" {
			t.Fatalf("server-executed tool calls should be filtered; got function_call in output")
		}
	}
	if len(output) != 1 {
		t.Fatalf("expected 1 output item (text), got %d", len(output))
	}
}

func TestWriteJSONConditional_SetsETagAndCacheControl(t *testing.T) {
	payload := map[string]any{"models": []string{"gpt-4", "gpt-3.5-turbo"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr := httptest.NewRecorder()
	writeJSONConditional(rr, req, http.StatusOK, payload)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	if etag := rr.Header().Get("ETag"); etag == "" {
		t.Fatal("ETag header missing")
	}
}

func TestWriteJSONConditional_Returns304OnMatch(t *testing.T) {
	payload := map[string]any{"models": []string{"gpt-4"}}

	// First request: get the ETag.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr1 := httptest.NewRecorder()
	writeJSONConditional(rr1, req1, http.StatusOK, payload)
	etag := rr1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response has no ETag")
	}

	// Second request: send If-None-Match.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	writeJSONConditional(rr2, req2, http.StatusOK, payload)

	if rr2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rr2.Code)
	}
	if rr2.Body.Len() != 0 {
		t.Fatalf("body should be empty on 304, got %d bytes", rr2.Body.Len())
	}
}

func TestWriteJSONConditional_Returns200OnMismatch(t *testing.T) {
	payload := map[string]any{"models": []string{"gpt-4"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("If-None-Match", `"stale-etag-value"`)
	rr := httptest.NewRecorder()
	writeJSONConditional(rr, req, http.StatusOK, payload)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() == 0 {
		t.Fatal("body should be non-empty on cache miss")
	}
}

func TestHandleModels_ETagConditional(t *testing.T) {
	mock := llm.NewMockProvider("anthropic")
	srv := &serveServer{
		cfgRef: &config.Config{
			DefaultProvider: "anthropic",
			Providers:       map[string]config.ProviderConfig{"anthropic": {Model: "claude-sonnet-4-6"}},
		},
		modelsProviders: map[string]llm.Provider{"anthropic": mock},
	}

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr1 := httptest.NewRecorder()
	srv.handleModels(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body: %s", rr1.Code, rr1.Body.String())
	}
	etag := rr1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("handleModels: no ETag on first response")
	}
	if cc := rr1.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req2.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	srv.handleModels(rr2, req2)
	if rr2.Code != http.StatusNotModified {
		t.Fatalf("second request (same etag) status = %d, want 304", rr2.Code)
	}
}

func TestHandleProviders_ETagConditional(t *testing.T) {
	srv := &serveServer{
		cfgRef: &config.Config{},
	}

	req1 := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	rr1 := httptest.NewRecorder()
	srv.handleProviders(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rr1.Code)
	}
	etag := rr1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("handleProviders: no ETag on first response")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/providers", nil)
	req2.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	srv.handleProviders(rr2, req2)
	if rr2.Code != http.StatusNotModified {
		t.Fatalf("second request (same etag) status = %d, want 304", rr2.Code)
	}
}

func TestHandleSessions_ETagConditional(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		sess := &session.Session{
			ID:        fmt.Sprintf("s-%d", i),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	srv := &serveServer{store: store, cfgRef: &config.Config{}}

	req1 := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rr1 := httptest.NewRecorder()
	srv.handleSessions(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body: %s", rr1.Code, rr1.Body.String())
	}
	etag := rr1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("handleSessions: no ETag on first response")
	}
	if cc := rr1.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req2.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	srv.handleSessions(rr2, req2)
	if rr2.Code != http.StatusNotModified {
		t.Fatalf("second request (same etag) status = %d, want 304", rr2.Code)
	}
}

func TestResolveServeToken(t *testing.T) {
	gen := func() (string, error) { return "generated-token", nil }
	genErr := func() (string, error) { return "", errors.New("rng broken") }

	tests := []struct {
		name        string
		flag        string
		env         string
		requireAuth bool
		generate    func() (string, error)
		wantToken   string
		wantSource  string
		wantErr     bool
	}{
		{
			name:        "auth disabled returns empty",
			flag:        "flag-token",
			env:         "env-token",
			requireAuth: false,
			generate:    gen,
			wantToken:   "",
			wantSource:  tokenSourceNone,
		},
		{
			name:        "flag wins over env",
			flag:        "flag-token",
			env:         "env-token",
			requireAuth: true,
			generate:    gen,
			wantToken:   "flag-token",
			wantSource:  tokenSourceFlag,
		},
		{
			name:        "env used when no flag",
			flag:        "",
			env:         "env-token",
			requireAuth: true,
			generate:    gen,
			wantToken:   "env-token",
			wantSource:  tokenSourceEnv,
		},
		{
			name:        "env whitespace trimmed",
			flag:        "",
			env:         "  env-token  ",
			requireAuth: true,
			generate:    gen,
			wantToken:   "env-token",
			wantSource:  tokenSourceEnv,
		},
		{
			name:        "flag whitespace falls back to env",
			flag:        "   ",
			env:         "env-token",
			requireAuth: true,
			generate:    gen,
			wantToken:   "env-token",
			wantSource:  tokenSourceEnv,
		},
		{
			name:        "auto-generated when nothing set",
			flag:        "",
			env:         "",
			requireAuth: true,
			generate:    gen,
			wantToken:   "generated-token",
			wantSource:  tokenSourceGenerated,
		},
		{
			name:        "generate error propagates",
			flag:        "",
			env:         "",
			requireAuth: true,
			generate:    genErr,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tok, source, err := resolveServeToken(tc.flag, tc.env, tc.requireAuth, tc.generate)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok != tc.wantToken {
				t.Errorf("token = %q, want %q", tok, tc.wantToken)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

func TestNewServeEngineWithTools_AutoModeInstallsGuardian(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "mock", Guardian: config.GuardianConfig{Provider: "debug", Model: "mock-model"}, Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	settings := SessionSettings{Tools: tools.ShellToolName}
	provider := llm.NewMockProvider("mock")

	_, toolMgr, err := newServeEngineWithTools(cfg, settings, provider, "mock", "mock-model", false, true, nil, nil)
	if err != nil {
		t.Fatalf("newServeEngineWithTools failed: %v", err)
	}
	if toolMgr == nil {
		t.Fatal("toolMgr = nil")
	}
	if got := toolMgr.ApprovalMgr.ApprovalMode(); got != tools.ModeAuto {
		t.Fatalf("approval mode = %v, want auto", got)
	}
	if !toolMgr.ApprovalMgr.GuardianReviewerAvailable() {
		t.Fatal("guardian reviewer not installed")
	}
}

func TestServeRuntimeEmitGuardianReviewUsesApprovalEventStream(t *testing.T) {
	rt := &serveRuntime{}
	var gotEvent string
	var gotData map[string]any
	rt.approvalEventFunc = func(event string, data map[string]any) error {
		gotEvent = event
		gotData = data
		return nil
	}

	event := tools.GuardianEvent{ToolCallID: "shell-1", Command: "rm file", WorkDir: "/tmp", Message: "guardian: denied: nope", Outcome: tools.GuardianDenied}
	rt.emitGuardianReview(event)

	if gotEvent != "response.guardian.review" {
		t.Fatalf("event = %q, want response.guardian.review", gotEvent)
	}
	if gotData["message"] != "guardian: denied: nope" || gotData["tool_call_id"] != "shell-1" || gotData["command"] != "rm file" || gotData["workdir"] != "/tmp" || gotData["outcome"] != tools.GuardianDenied {
		t.Fatalf("guardian payload = %#v", gotData)
	}
}

func TestServeSessionIdleMetadataMutationDoesNotWaitForSessionOperation(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	})
	defer manager.Close()
	op := manager.sessionOperation("session-a")
	op.Lock()
	defer op.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, err := manager.lockIdleMetadataMutation("session-a")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errServeSessionBusy) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("idle metadata mutation waited for an active session operation")
	}
}

func TestServeSessionMetadataMutationDoesNotBlockOtherSessionAdmission(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	runtimeA := &serveRuntime{}
	runtimeA.Touch()
	manager.mu.Lock()
	manager.sessions["session-a"] = runtimeA
	manager.mu.Unlock()

	_, release, err := manager.lockIdleMetadataMutation("session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if got, err := manager.GetOrCreate(context.Background(), "session-a"); err != nil || got != runtimeA {
		t.Fatalf("existing reserved runtime lookup = %p, %v; want %p, nil", got, err, runtimeA)
	}

	done := make(chan error, 1)
	go func() {
		_, err := manager.GetOrCreate(context.Background(), "session-b")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("session B admission waited for session A metadata mutation")
	}
}
