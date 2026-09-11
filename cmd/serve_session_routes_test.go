package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/widgets"
)

func TestHandleSessionRuntimeGoalMutatesPersistentGoal(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	const sessionID = "sess-goal-runtime"
	if err := store.Create(context.Background(), &session.Session{
		ID:        sessionID,
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	rt := &serveRuntime{sessionMeta: &session.Session{ID: sessionID}}
	putTestSession(mgr, sessionID, rt)

	srv := &serveServer{sessionMgr: mgr, store: store}
	postGoal := func(body string) *session.Goal {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/runtime/goal", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Goal *session.Goal `json:"goal"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.Goal
	}

	goal := postGoal(`{"action":"set","objective":"ship web goal UX","token_budget":123}`)
	if goal == nil || goal.Objective != "ship web goal UX" || goal.TokenBudget != 123 || goal.Status != session.GoalStatusActive {
		t.Fatalf("set goal response = %+v", goal)
	}
	rt.mu.Lock()
	runtimeGoal := rt.sessionMeta.Goal.Clone()
	rt.mu.Unlock()
	if runtimeGoal == nil || runtimeGoal.Objective != "ship web goal UX" {
		t.Fatalf("runtime goal = %+v, want set goal", runtimeGoal)
	}

	goal = postGoal(`{"action":"pause"}`)
	if goal == nil || goal.Status != session.GoalStatusPaused {
		t.Fatalf("pause goal response = %+v", goal)
	}
	goal = postGoal(`{"action":"resume"}`)
	if goal == nil || goal.Status != session.GoalStatusActive {
		t.Fatalf("resume goal response = %+v", goal)
	}
	goal = postGoal(`{"action":"clear"}`)
	if goal != nil {
		t.Fatalf("clear goal response = %+v, want nil", goal)
	}
	persisted, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if persisted.Goal != nil {
		t.Fatalf("persisted goal after clear = %+v, want nil", persisted.Goal)
	}
	rt.mu.Lock()
	runtimeGoal = rt.sessionMeta.Goal.Clone()
	rt.mu.Unlock()
	if runtimeGoal != nil {
		t.Fatalf("runtime goal after clear = %+v, want nil", runtimeGoal)
	}
}

func TestHandleSessionRuntimeGoalRejectsBudgetLimitedResume(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	const sessionID = "sess-goal-budget-resume"
	goal := session.NewGoal("spent", 10, time.Now())
	goal.TokensUsed = 10
	goal.Status = session.GoalStatusBudgetLimited
	if err := store.Create(context.Background(), &session.Session{
		ID:        sessionID,
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
		Goal:      goal,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/runtime/goal", strings.NewReader(`{"action":"resume"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	persisted, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if persisted.Goal == nil || persisted.Goal.Status != session.GoalStatusBudgetLimited {
		t.Fatalf("persisted goal = %+v, want budget_limited", persisted.Goal)
	}
}

func TestHandleSessionRuntimeGoalDoesNotBlockWhenRuntimeBusy(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	const sessionID = "sess-goal-runtime-busy"
	if err := store.Create(context.Background(), &session.Session{
		ID:        sessionID,
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	rt := &serveRuntime{sessionMeta: &session.Session{ID: sessionID}}
	putTestSession(mgr, sessionID, rt)
	server := &serveServer{sessionMgr: mgr, store: store}

	rt.mu.Lock()
	locked := true
	release := func() {
		if locked {
			locked = false
			rt.mu.Unlock()
		}
	}
	defer release()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/runtime/goal", strings.NewReader(`{"action":"set","objective":"do not block"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		server.handleSessionByID(rr, req)
		done <- rr
	}()

	select {
	case rr := <-done:
		release()
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("handleSessionRuntimeGoal blocked while rt.mu was held by a run")
	}
	persisted, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if persisted.Goal == nil || persisted.Goal.Objective != "do not block" {
		t.Fatalf("persisted goal = %+v, want updated goal", persisted.Goal)
	}
}

func TestHandleSessionRuntimeGoalCreatesDraftSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	const sessionID = "sess-goal-draft"
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return &serveRuntime{store: store, providerKey: "mock", defaultModel: "mock-model"}, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr, store: store}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/runtime/goal", strings.NewReader(`{"action":"set","objective":"draft goal"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	persisted, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if persisted == nil || persisted.Goal == nil || persisted.Goal.Objective != "draft goal" {
		t.Fatalf("persisted draft goal = %+v", persisted)
	}
}

func TestHandleSessionRuntimeEffortQueuesEngineSwitch(t *testing.T) {
	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	provider := newStagedProvider("ok", "")
	close(provider.releaseSecond)
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{engine: engine, provider: provider, providerKey: "staged", defaultModel: "gpt-5.4"}
	state := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{}), model: "gpt-5.4", reasoningEffort: "medium"}
	rt.setActiveInterrupt(state)
	defer rt.clearActiveInterrupt(state)
	putTestSession(mgr, "sess-effort", rt)

	srv := &serveServer{sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-effort/runtime/effort", strings.NewReader(`{"model":"gpt-5.4","reasoning_effort":"high"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"reasoning_effort":"high"`) {
		t.Fatalf("response body = %s, want queued high", rr.Body.String())
	}

	stream, err := engine.Stream(context.Background(), llm.Request{
		Model:           "gpt-5.4",
		ReasoningEffort: "medium",
		Messages:        []llm.Message{llm.UserText("hello")},
		Tools:           []llm.ToolSpec{{Name: "noop", Description: "unused", Schema: map[string]interface{}{"type": "object"}}},
		MaxTurns:        1,
	})
	if err != nil {
		t.Fatalf("engine stream: %v", err)
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
	}
	_ = stream.Close()

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	if got := provider.requests[0].ReasoningEffort; got != "high" {
		t.Fatalf("provider reasoning effort = %q, want high", got)
	}
}

func TestHandleSessionRuntimeEffortRejectsModelMismatch(t *testing.T) {
	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	rt := &serveRuntime{engine: engine, providerKey: "mock", defaultModel: "gpt-5.4"}
	state := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{}), model: "gpt-5.4", reasoningEffort: "medium"}
	rt.setActiveInterrupt(state)
	defer rt.clearActiveInterrupt(state)
	putTestSession(mgr, "sess-effort-mismatch", rt)

	srv := &serveServer{sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-effort-mismatch/runtime/effort", strings.NewReader(`{"model":"foreign-model","reasoning_effort":"high"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "active model") {
		t.Fatalf("response body = %s, want active model error", rr.Body.String())
	}
}

func TestServeRuntimeRunClearsUnappliedRuntimeSwitch(t *testing.T) {
	provider := newStagedProvider("hello ", "done")
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{engine: engine, provider: provider, providerKey: "staged", defaultModel: "gpt-5.4"}

	runDone := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), true, false, []llm.Message{llm.UserText("hello")}, llm.Request{
			Model:           "gpt-5.4",
			ReasoningEffort: "medium",
		})
		runDone <- err
	}()

	select {
	case <-provider.firstSent:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if err := rt.QueueActiveRunRuntimeSwitch("gpt-5.4", "high"); err != nil {
		t.Fatalf("queue runtime switch: %v", err)
	}
	close(provider.releaseSecond)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not finish")
	}

	stream, err := engine.Stream(context.Background(), llm.Request{
		Model:           "gpt-5.4",
		ReasoningEffort: "low",
		Messages:        []llm.Message{llm.UserText("next")},
	})
	if err != nil {
		t.Fatalf("engine stream: %v", err)
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
	}
	_ = stream.Close()

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	if got := provider.requests[1].ReasoningEffort; got != "low" {
		t.Fatalf("next request reasoning effort = %q, want low (stale queued switch cleared)", got)
	}
}

func TestServeRuntimeRun_RejectsSteeringDuringSimpleStream(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	provider := newStagedProvider("hello ", "world")
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  "staged",
		engine:       engine,
		store:        store,
		defaultModel: "staged-model",
	}
	rt.Touch()

	errCh := make(chan error, 1)
	go func() {
		_, runErr := rt.Run(context.Background(), true, false, []llm.Message{
			llm.UserText("original request"),
		}, llm.Request{SessionID: "serve-steer-persist", MaxTurns: 3})
		errCh <- runErr
	}()

	select {
	case <-provider.firstSent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first streamed chunk")
	}

	action, err := rt.Interrupt(context.Background(), "also remember this", nil)
	if action != llm.InterruptSteer {
		t.Fatalf("Interrupt action = %v, want %v", action, llm.InterruptSteer)
	}
	if err == nil || !strings.Contains(err.Error(), "active run finished") {
		t.Fatalf("Interrupt error = %v, want non-consuming simple-stream error", err)
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("simple stream retained phantom steering: %#v", pending)
	}

	close(provider.releaseSecond)

	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("Run failed: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to finish")
	}

	if len(rt.history) != 2 {
		t.Fatalf("history len = %d, want 2", len(rt.history))
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("pending steering = %#v, want none", pending)
	}

	msgs, err := store.GetMessages(context.Background(), "serve-steer-persist", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("persisted message count = %d, want 2", len(msgs))
	}
}

func TestServeRuntimeRun_PersistsMessagesOnErrorExit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// Turn 1: tool call (succeeds, callbacks persist).
	// Turn 2: error (run returns error — deferred persist must save turn 1).
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "echo", map[string]any{"input": "hi"}).
		AddError(errors.New("provider exploded"))

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{
		SessionID: "error-persist-test",
		MaxTurns:  5,
		Tools:     []llm.ToolSpec{(&echoTool{}).Spec()},
	}
	_, runErr := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("call the echo tool"),
	}, req)
	if runErr == nil {
		t.Fatal("expected Run to return an error")
	}

	// Despite the error, turn 1 messages must be persisted.
	msgs, err := store.GetMessages(context.Background(), "error-persist-test", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}

	var hasToolCall, hasToolResult bool
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolCall && p.ToolCall != nil && p.ToolCall.Name == "echo" {
				hasToolCall = true
			}
			if p.Type == llm.PartToolResult && p.ToolResult != nil && p.ToolResult.ID == "call-1" {
				hasToolResult = true
			}
		}
	}
	if !hasToolCall {
		t.Fatalf("persisted messages missing assistant tool_call after error exit; messages: %d", len(msgs))
	}
	if !hasToolResult {
		t.Fatalf("persisted messages missing tool_result after error exit; messages: %d", len(msgs))
	}
}

func TestServeRuntimeRun_ImmediateErrorDoesNotDesyncState(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer store.Close()

	// The provider fails on the very first call — no callbacks fire.
	provider := llm.NewMockProvider("mock").
		AddError(errors.New("immediate boom")).
		AddTextResponse("recovered")

	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
	}
	rt.Touch()

	sid := "immediate-error-test"
	req := llm.Request{
		SessionID: sid,
		MaxTurns:  5,
		Tools:     []llm.ToolSpec{(&echoTool{}).Spec()},
	}

	// First run: immediate error, no produced messages.
	_, runErr := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("hello"),
	}, req)
	if runErr == nil {
		t.Fatal("expected Run to return an error")
	}

	// rt.history must be empty — no callbacks ran, no state committed.
	if len(rt.history) != 0 {
		t.Fatalf("history len after immediate error = %d, want 0", len(rt.history))
	}

	// DB should have the submitted user message immediately. This keeps the web
	// session view stable while the provider is slow or fails before emitting.
	msgs, err := store.GetMessages(context.Background(), sid, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages failed: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != llm.RoleUser || msgs[0].TextContent != "hello" {
		t.Fatalf("persisted messages after immediate error = %+v, want durable user prompt", msgs)
	}

	// Second run: succeeds — must not be confused by stale DB state.
	_, runErr = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("hello again"),
	}, req)
	if runErr != nil {
		t.Fatalf("second Run failed: %v", runErr)
	}

	msgs, err = store.GetMessages(context.Background(), sid, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after recovery failed: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected persisted messages after successful second run")
	}
}

func TestServeRuntimeRun_ReplaceHistoryRestoresInMemoryHistoryOnEarlyFailure(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddTextResponse("old reply").
		AddError(errors.New("fresh-start boom")).
		AddTextResponse("recovered")
	engine := llm.NewEngine(provider, nil)

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		defaultModel: "mock-model",
	}
	rt.Touch()

	req := llm.Request{SessionID: "replace-history-early-failure", MaxTurns: 1}

	_, err := rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("old context"),
	}, req)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	if len(rt.history) == 0 {
		t.Fatal("expected first Run to populate history")
	}

	_, err = rt.Run(context.Background(), true, true, []llm.Message{
		llm.UserText("fresh start"),
	}, req)
	if err == nil || !strings.Contains(err.Error(), "fresh-start boom") {
		t.Fatalf("replaceHistory Run error = %v, want fresh-start boom", err)
	}
	if len(rt.history) != 2 {
		t.Fatalf("history len after failed replaceHistory run = %d, want 2 restored messages", len(rt.history))
	}

	_, err = rt.Run(context.Background(), true, false, []llm.Message{
		llm.UserText("after failure"),
	}, req)
	if err != nil {
		t.Fatalf("third Run failed: %v", err)
	}
	if len(provider.Requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(provider.Requests))
	}
	if len(provider.Requests[2].Messages) != 3 {
		t.Fatalf("third request message count = %d, want 3 restored-history messages", len(provider.Requests[2].Messages))
	}
	if provider.Requests[2].Messages[0].Role != llm.RoleUser {
		t.Fatalf("third request[0] role = %s, want user", provider.Requests[2].Messages[0].Role)
	}
	if got := provider.Requests[2].Messages[0].Parts[0].Text; got != "old context" {
		t.Fatalf("third request[0] text = %q, want %q", got, "old context")
	}
	if provider.Requests[2].Messages[1].Role != llm.RoleAssistant {
		t.Fatalf("third request[1] role = %s, want assistant", provider.Requests[2].Messages[1].Role)
	}
	if got := provider.Requests[2].Messages[1].Parts[0].Text; got != "old reply" {
		t.Fatalf("third request[1] text = %q, want %q", got, "old reply")
	}
	if provider.Requests[2].Messages[2].Role != llm.RoleUser {
		t.Fatalf("third request[2] role = %s, want user", provider.Requests[2].Messages[2].Role)
	}
	if got := provider.Requests[2].Messages[2].Parts[0].Text; got != "after failure" {
		t.Fatalf("third request[2] text = %q, want %q", got, "after failure")
	}
}

func TestServeRuntimeRun_ReinjectsPlatformDeveloperMessageAfterFailedFirstRun(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddError(errors.New("boom")).
		AddTextResponse("hello from serve")
	engine := llm.NewEngine(provider, nil)
	devText := "telegram developer instructions"
	input := []llm.Message{llm.UserText("hello")}

	rt := &serveRuntime{
		provider:         provider,
		engine:           engine,
		platform:         "telegram",
		platformMessages: agents.PlatformMessagesConfig{Telegram: devText},
	}
	rt.Touch()

	_, err := rt.Run(context.Background(), true, false, input, llm.Request{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("first Run error = %v, want boom", err)
	}
	if rt.lastInjectedPlatform != "" {
		t.Fatalf("lastInjectedPlatform = %q, want empty after failed first run", rt.lastInjectedPlatform)
	}
	if len(rt.history) != 0 {
		t.Fatalf("history len = %d, want 0 after failed first run", len(rt.history))
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("request count after first run = %d, want 1", len(provider.Requests))
	}
	if len(provider.Requests[0].Messages) != 2 {
		t.Fatalf("first request message count = %d, want 2", len(provider.Requests[0].Messages))
	}
	if provider.Requests[0].Messages[0].Role != llm.RoleDeveloper || provider.Requests[0].Messages[0].Parts[0].Text != devText {
		t.Fatalf("first request did not include injected developer message: %+v", provider.Requests[0].Messages)
	}

	_, err = rt.Run(context.Background(), true, false, input, llm.Request{})
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}
	if rt.lastInjectedPlatform != "telegram" {
		t.Fatalf("lastInjectedPlatform = %q, want telegram after successful run", rt.lastInjectedPlatform)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("request count after second run = %d, want 2", len(provider.Requests))
	}
	if len(provider.Requests[1].Messages) != 2 {
		t.Fatalf("second request message count = %d, want 2", len(provider.Requests[1].Messages))
	}
	if provider.Requests[1].Messages[0].Role != llm.RoleDeveloper || provider.Requests[1].Messages[0].Parts[0].Text != devText {
		t.Fatalf("second request did not re-include injected developer message: %+v", provider.Requests[1].Messages)
	}
	if len(rt.history) < 3 {
		t.Fatalf("history len = %d, want at least 3 after successful run", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleDeveloper || llm.MessageText(rt.history[0]) != devText || !llm.IsPlatformContextMessage(rt.history[0]) {
		t.Fatalf("history missing injected developer message: %+v", rt.history)
	}
}

func TestHandleResponses_GeneratesSessionIDHeaderWhenMissing(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr: manager,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := strings.TrimSpace(rr.Header().Get("x-session-id")); got == "" {
		t.Fatalf("x-session-id header missing")
	}
}

func TestHandleResponses_FirstPartyRequiresDedicatedClientMessageID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	req.Header.Set("Idempotency-Key", "request-not-a-message")
	rr := httptest.NewRecorder()
	(&serveServer{}).handleResponses(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "client_message_id") {
		t.Fatalf("status/body = %d %s, want missing client_message_id", rr.Code, rr.Body.String())
	}
}

func TestHandleResponses_UIFollowUpClaimsQueuedSteering(t *testing.T) {
	const (
		sessionID = "sess_steering_handoff"
		messageID = "msg_steering_handoff"
	)

	provider := llm.NewMockProvider("mock").AddTextResponse("follow-up response")
	engine := llm.NewEngine(provider, nil)
	runtime := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		defaultModel: "mock-model",
	}
	runtime.Touch()
	engine.QueueSteering(llm.QueuedSteering{
		ID:          messageID,
		Message:     llm.UserText("same logical message"),
		DisplayText: "same logical message",
	})

	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)

	srv := &serveServer{
		sessionMgr:   manager,
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req:                responsesCreateRequest{Stream: true, ClientMessageID: messageID},
		inputMessages:      []llm.Message{llm.UserText("same logical message")},
		sessionID:          sessionID,
		previousResponseID: "resp_previous",
		previousDurable:    true,
		idempotencyKey:     "request_steering_handoff",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("pending steering after follow-up handoff = %#v, want none", pending)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}
}

func TestHandleResponses_UIQueuedFollowUpBatchRunsOnceAndPersistsDistinctRows(t *testing.T) {
	const sessionID = "sess-ui-follow-up-batch"
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").AddTextResponse("initial answer").AddTextResponse("one batched answer")
	runtime := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
		store:        store,
	}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) { return runtime, nil })
	defer manager.Close()
	srv := &serveServer{store: store, sessionMgr: manager, cfgRef: &config.Config{DefaultProvider: "mock"}}

	code, first := doResponsesFirstParty(t, srv, `{"input":"start","client_message_id":"msg-start","model":"mock-model"}`, sessionID)
	if code != http.StatusOK {
		t.Fatalf("initial status = %d body=%#v", code, first)
	}
	previousID, _ := first["id"].(string)
	if previousID == "" {
		t.Fatalf("initial response missing id: %#v", first)
	}

	for _, queued := range []struct{ id, text string }{{"msg-first", "first queued"}, {"msg-second", "second queued"}} {
		runtime.engine.QueueSteering(llm.QueuedSteering{ID: queued.id, Message: llm.UserText(queued.text), DisplayText: queued.text})
	}
	body := `{"input":[` +
		`{"type":"message","role":"user","client_message_id":"msg-first","content":"first queued"},` +
		`{"type":"message","role":"user","client_message_id":"msg-second","content":"second queued"}` +
		`],"client_message_id":"msg-second","previous_response_id":"` + previousID + `"}`
	code, second := doResponsesFirstParty(t, srv, body)
	if code != http.StatusOK {
		t.Fatalf("batch status = %d body=%#v", code, second)
	}
	if pending := runtime.engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("batched follow-up retained engine queue ownership: %#v", pending)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want initial + one batch", len(provider.Requests))
	}
	lastRequest := provider.Requests[1]
	var batchUsers []llm.Message
	for _, message := range lastRequest.Messages {
		if message.Role == llm.RoleUser && (message.ClientMessageID == "msg-first" || message.ClientMessageID == "msg-second") {
			batchUsers = append(batchUsers, message)
		}
	}
	if len(batchUsers) != 2 || batchUsers[0].ClientMessageID != "msg-first" || batchUsers[1].ClientMessageID != "msg-second" {
		t.Fatalf("provider batch users = %#v", batchUsers)
	}

	stored, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	var storedBatch []session.Message
	batchAssistantCount := 0
	for _, message := range stored {
		if message.ClientMessageID == "msg-first" || message.ClientMessageID == "msg-second" {
			storedBatch = append(storedBatch, message)
		}
		if message.Role == llm.RoleAssistant && message.TextContent == "one batched answer" {
			batchAssistantCount++
		}
	}
	if len(storedBatch) != 2 || storedBatch[0].ClientMessageID != "msg-first" || storedBatch[1].ClientMessageID != "msg-second" {
		t.Fatalf("stored batch users = %#v", storedBatch)
	}
	if batchAssistantCount != 1 {
		t.Fatalf("batched assistant response count = %d, want 1; messages=%#v", batchAssistantCount, stored)
	}
}

func TestHandleResponses_UIFollowUpStartFailureReleasesClaim(t *testing.T) {
	const (
		sessionID = "sess-follow-up-start-failure"
		messageID = "msg-follow-up-start-failure"
	)
	provider := &serveRuntimeErrorProvider{err: errors.New("provider failed before response")}
	engine := llm.NewEngine(provider, nil)
	entry := llm.QueuedSteering{ID: messageID, Message: llm.UserText("retry me")}
	engine.QueueSteering(entry)
	runtime := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: engine, defaultModel: "mock-model"}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	runs := newServeResponseRunManager()
	runs.Close()
	srv := &serveServer{sessionMgr: manager, responseRuns: runs}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req: responsesCreateRequest{Stream: true, ClientMessageID: messageID}, inputMessages: []llm.Message{{Role: llm.RoleUser, ClientMessageID: messageID}},
		sessionID: sessionID, previousResponseID: "resp_previous", previousDurable: true, idempotencyKey: "request_start_failure",
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rr.Code, rr.Body.String())
	}
	if _, status := engine.QueueSteeringWithStatus(entry); status != llm.SteeringQueueQueued {
		t.Fatalf("claim was not released after start failure: %q", status)
	}
}

func TestHandleResponses_UIFollowUpRunFailureReleasesClaim(t *testing.T) {
	const (
		sessionID = "sess-follow-up-run-failure"
		messageID = "msg-follow-up-run-failure"
	)
	provider := &serveRuntimeErrorProvider{err: errors.New("provider failed before response")}
	engine := llm.NewEngine(provider, nil)
	entry := llm.QueuedSteering{ID: messageID, Message: llm.UserText("retry me")}
	engine.QueueSteering(entry)
	runtime := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: engine, defaultModel: "mock-model"}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{sessionMgr: manager, responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req: responsesCreateRequest{Stream: true, ClientMessageID: messageID}, inputMessages: []llm.Message{{Role: llm.RoleUser, ClientMessageID: messageID}},
		sessionID: sessionID, previousResponseID: "resp_previous", previousDurable: true, idempotencyKey: "request_run_failure",
	})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "event: response.failed") {
		t.Fatalf("run failure response status=%d body=%s", rr.Code, rr.Body.String())
	}
	if status := engine.ClaimSteeringEntry(messageID); status != llm.SteeringClaimNotFound {
		t.Fatalf("claim was not released after run failure: %q", status)
	}
	if _, status := engine.QueueSteeringWithStatus(entry); status != llm.SteeringQueueRunFinished {
		t.Fatalf("finished run unexpectedly reclaimed retry ownership: %q", status)
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("finished run retained retry as phantom steering: %#v", pending)
	}
}

func TestHandleResponses_UIFollowUpRejectsCommittedSteering(t *testing.T) {
	const (
		sessionID = "sess_committed_steering_handoff"
		messageID = "msg_committed_steering_handoff"
	)

	provider := llm.NewMockProvider("mock").AddTextResponse("must not run")
	engine := llm.NewEngine(provider, nil)
	engine.QueueSteering(llm.QueuedSteering{
		ID:          messageID,
		Message:     llm.UserText("already committed"),
		DisplayText: "already committed",
	})
	if drained := engine.DrainSteering(); len(drained) != 1 {
		t.Fatalf("drained steering = %#v, want one", drained)
	}
	runtime := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		defaultModel: "mock-model",
	}
	runtime.Touch()

	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{sessionMgr: manager, responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req:                responsesCreateRequest{Stream: true, ClientMessageID: messageID},
		inputMessages:      []llm.Message{llm.UserText("already committed")},
		sessionID:          sessionID,
		previousResponseID: "resp_previous",
		previousDurable:    true,
		idempotencyKey:     "request_committed_steering_handoff",
	})

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"type":"client_message_already_committed"`) {
		t.Fatalf("body missing typed committed conflict: %s", rr.Body.String())
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider request count = %d, want 0", len(provider.Requests))
	}
}

func TestHandleResponses_UIFollowUpAllowsTrailingUnansweredRetry(t *testing.T) {
	const (
		sessionID = "sess_trailing_retry"
		messageID = "msg_trailing_retry"
	)
	provider := llm.NewMockProvider("mock").AddTextResponse("retry succeeded")
	engine := llm.NewEngine(provider, nil)
	trailing := llm.UserText("retry me")
	trailing.ClientMessageID = messageID
	runtime := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		defaultModel: "mock-model",
		history:      []llm.Message{llm.UserText("earlier"), llm.AssistantText("done"), trailing},
	}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{sessionMgr: manager, responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req:                responsesCreateRequest{Stream: true, ClientMessageID: messageID},
		inputMessages:      []llm.Message{llm.UserText("retry me")},
		sessionID:          sessionID,
		previousResponseID: "resp_previous",
		previousDurable:    true,
		idempotencyKey:     "request_trailing_retry",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}
}

func TestHandleResponses_UIFollowUpAllowsTrailingUnansweredBatchRetry(t *testing.T) {
	const sessionID = "sess-trailing-batch-retry"
	provider := llm.NewMockProvider("mock").AddTextResponse("batch retry succeeded")
	engine := llm.NewEngine(provider, nil)
	first := llm.UserText("first")
	first.ClientMessageID = "msg-first"
	second := llm.UserText("second")
	second.ClientMessageID = "msg-second"
	for _, message := range []llm.Message{first, second} {
		engine.QueueSteering(llm.QueuedSteering{ID: message.ClientMessageID, Message: message})
	}
	engine.ClaimSteering([]string{first.ClientMessageID, second.ClientMessageID})
	runtime := &serveRuntime{
		provider: provider, providerKey: provider.Name(), engine: engine, defaultModel: "mock-model",
		history: []llm.Message{llm.UserText("earlier"), llm.AssistantText("done"), first, second},
	}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{sessionMgr: manager, responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req: responsesCreateRequest{Stream: true, ClientMessageID: second.ClientMessageID}, inputMessages: []llm.Message{first, second},
		sessionID: sessionID, previousResponseID: "resp_previous", previousDurable: true, idempotencyKey: "request_batch_retry",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}
}

func TestHandleResponses_UIFollowUpRejectsMixedOwnedAndPendingBatch(t *testing.T) {
	const sessionID = "sess-mixed-batch-ownership"
	provider := llm.NewMockProvider("mock").AddTextResponse("must not run")
	engine := llm.NewEngine(provider, nil)
	first := llm.UserText("first")
	first.ClientMessageID = "msg-owned"
	second := llm.UserText("second")
	second.ClientMessageID = "msg-pending"
	for _, message := range []llm.Message{first, second} {
		engine.QueueSteering(llm.QueuedSteering{ID: message.ClientMessageID, Message: message})
	}
	engine.ClaimSteeringEntry(first.ClientMessageID)
	runtime := &serveRuntime{
		provider: provider, providerKey: provider.Name(), engine: engine, defaultModel: "mock-model",
		history: []llm.Message{llm.UserText("earlier"), llm.AssistantText("done"), first, second},
	}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{sessionMgr: manager, responseRuns: newServeResponseRunManager()}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req: responsesCreateRequest{Stream: true, ClientMessageID: second.ClientMessageID}, inputMessages: []llm.Message{first, second},
		sessionID: sessionID, previousResponseID: "resp_previous", previousDurable: true, idempotencyKey: "request_mixed_ownership",
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider request count = %d, want 0", len(provider.Requests))
	}
	if _, status := engine.QueueSteeringWithStatus(llm.QueuedSteering{ID: second.ClientMessageID, Message: second}); status != llm.SteeringQueueAlreadyQueued {
		t.Fatalf("pending batch member was transferred despite mixed ownership: %q", status)
	}
}

func TestHandleResponses_UIFollowUpRejectsColdDurableDuplicate(t *testing.T) {
	const (
		sessionID = "sess_cold_durable_duplicate"
		messageID = "msg_cold_durable_duplicate"
	)
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "mock", Model: "mock-model", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	persisted := session.NewMessage(sessionID, llm.UserText("already durable"), -1)
	persisted.ClientMessageID = messageID
	if err := store.AddMessage(ctx, sessionID, persisted); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.AddMessage(ctx, sessionID, session.NewMessage(sessionID, llm.AssistantText("already answered"), -1)); err != nil {
		t.Fatalf("Add assistant: %v", err)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("must not run")
	runtime := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
		store:        store,
	}
	runtime.Touch()
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	putTestSession(manager, sessionID, runtime)
	srv := &serveServer{sessionMgr: manager, responseRuns: newServeResponseRunManager(), store: store}
	defer srv.responseRuns.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	rr := httptest.NewRecorder()
	srv.handleResolvedResponses(rr, req, req.Context(), resolvedResponsesRequest{
		req:                responsesCreateRequest{Stream: true, ClientMessageID: messageID},
		inputMessages:      []llm.Message{llm.UserText("already durable")},
		sessionID:          sessionID,
		previousResponseID: "resp_previous",
		previousDurable:    true,
		idempotencyKey:     "request_cold_durable_duplicate",
	})

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider request count = %d, want 0", len(provider.Requests))
	}
}

func TestHandleResponses_StreamIdempotencyKeyReplaysExistingRun(t *testing.T) {
	const (
		sessionID      = "sess_stream_idempotent"
		idempotencyKey = "req-grab-tuner"
	)

	provider := llm.NewMockProvider("mock").AddTextResponse("first response")
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  provider.Name(),
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr:   manager,
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()

	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"stream":true,"input":"grab tuner"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("session_id", sessionID)
		req.Header.Set("Idempotency-Key", idempotencyKey)
		return req
	}

	first := httptest.NewRecorder()
	srv.handleResponses(first, newRequest())
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200; body: %s", first.Code, first.Body.String())
	}
	respID := strings.TrimSpace(first.Header().Get("x-response-id"))
	if respID == "" {
		t.Fatalf("first response missing x-response-id")
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count after first response = %d, want 1", len(provider.Requests))
	}

	second := httptest.NewRecorder()
	srv.handleResponses(second, newRequest())
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body: %s", second.Code, second.Body.String())
	}
	if got := strings.TrimSpace(second.Header().Get("x-response-id")); got != respID {
		t.Fatalf("second x-response-id = %q, want original %q", got, respID)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count after replay = %d, want 1", len(provider.Requests))
	}
	if got := srv.responseRuns.Diagnostics().IdempotencyReplays; got != 1 {
		t.Fatalf("idempotency replay diagnostics = %d, want 1", got)
	}
}

func TestHandleResponses_StreamIdempotencyKeyReattachesInProgressRun(t *testing.T) {
	const (
		sessionID      = "sess_stream_idempotent_live"
		idempotencyKey = "req-live-grab-tuner"
	)

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := newStagedProvider("first ", "second")
	releaseSecond := sync.OnceFunc(func() { close(provider.releaseSecond) })
	defer releaseSecond()

	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  provider.Name(),
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr:   manager,
		store:        store,
		responseRuns: newServeResponseRunManager(),
	}
	defer srv.responseRuns.Close()
	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	newRequest := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"stream":true,"input":"grab tuner"}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("session_id", sessionID)
		req.Header.Set("Idempotency-Key", idempotencyKey)
		return req
	}

	client := ts.Client()
	first, err := client.Do(newRequest())
	if err != nil {
		t.Fatalf("first Do: %v", err)
	}
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(first.Body)
		t.Fatalf("first status = %d, want 200; body: %s", first.StatusCode, string(body))
	}
	respID := strings.TrimSpace(first.Header.Get("x-response-id"))
	if respID == "" {
		t.Fatalf("first response missing x-response-id")
	}
	sessionNumber := strings.TrimSpace(first.Header.Get("x-session-number"))
	if sessionNumber == "" {
		t.Fatalf("first response missing x-session-number")
	}

	select {
	case <-provider.firstSent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-progress run to stream first chunk")
	}

	second, err := client.Do(newRequest())
	if err != nil {
		t.Fatalf("second Do: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(second.Body)
		t.Fatalf("second status = %d, want 200; body: %s", second.StatusCode, string(body))
	}
	if got := strings.TrimSpace(second.Header.Get("x-response-id")); got != respID {
		t.Fatalf("second x-response-id = %q, want original %q", got, respID)
	}
	if got := strings.TrimSpace(second.Header.Get("x-session-number")); got != sessionNumber {
		t.Fatalf("second x-session-number = %q, want original %q", got, sessionNumber)
	}

	provider.mu.Lock()
	requestCount := len(provider.requests)
	provider.mu.Unlock()
	if requestCount != 1 {
		t.Fatalf("provider request count while duplicate is attached = %d, want 1", requestCount)
	}

	releaseSecond()
	body1 := readResponseBodyWithTimeout(t, first.Body, 2*time.Second, "first idempotent stream")
	body2 := readResponseBodyWithTimeout(t, second.Body, 2*time.Second, "second idempotent stream")
	for name, body := range map[string][]byte{"first": body1, "second": body2} {
		if !bytes.Contains(body, []byte("first")) || !bytes.Contains(body, []byte("second")) {
			t.Fatalf("%s stream body missing expected replayed deltas: %s", name, string(body))
		}
	}
}

func readResponseBodyWithTimeout(t *testing.T, body io.Reader, timeout time.Duration, label string) []byte {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(body)
		ch <- result{data: data, err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("ReadAll %s: %v", label, res.err)
		}
		return res.data
	case <-time.After(timeout):
		t.Fatalf("timed out reading %s", label)
		return nil
	}
}

func TestStreamUIResponses_SetsSessionNumberHeader(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("hi")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{
		sessionMgr:   manager,
		store:        store,
		responseRuns: newServeResponseRunManager(),
	}

	body := `{"stream":true,"input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("session_id", "sess_test_number")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)

	numStr := strings.TrimSpace(rr.Header().Get("x-session-number"))
	if numStr == "" {
		t.Fatalf("x-session-number header missing from streaming UI response")
	}
	num, parseErr := strconv.ParseInt(numStr, 10, 64)
	if parseErr != nil || num <= 0 {
		t.Fatalf("x-session-number = %q, want positive integer", numStr)
	}
}

func TestResponsesUIBareSessionIDAppendsServerHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	const sessionID = "sess_ui_append"
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), sessionID, []session.Message{
		*session.NewMessage(sessionID, llm.UserText("old question"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("old answer"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("new answer")
	// A fresh post-restart web runtime still has a tool manager. Restoring its
	// persisted BaseDir loads session metadata before Run; that metadata must not
	// make Run mistake the otherwise empty runtime for a hydrated conversation.
	toolCfg := tools.DefaultToolConfig()
	toolCfg.Enabled = []string{tools.ReadFileToolName}
	toolMgr, err := tools.NewToolManager(&toolCfg, &config.Config{})
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			toolMgr:      toolMgr,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	seed, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("seed GetMessages: %v", err)
	}
	previousID := durableResponseIDForMessageID(seed[len(seed)-1].ID)
	srv := &serveServer{sessionMgr: manager, store: store}
	body := fmt.Sprintf(`{"input":"new question","previous_response_id":%q}`, previousID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("session_id", sessionID)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	msgs, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("stored message count = %d, want 4 appended messages", len(msgs))
	}
	want := []struct {
		role llm.Role
		text string
	}{
		{llm.RoleUser, "old question"},
		{llm.RoleAssistant, "old answer"},
		{llm.RoleUser, "new question"},
		{llm.RoleAssistant, "new answer"},
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].TextContent != w.text {
			t.Fatalf("message[%d] = (%s, %q), want (%s, %q)", i, msgs[i].Role, msgs[i].TextContent, w.role, w.text)
		}
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}
	if got := len(provider.Requests[0].Messages); got != 3 {
		t.Fatalf("provider message count = %d, want 3 server-owned history messages", got)
	}
}

func TestResponsesMessageBackedPreviousResponseAcceptsBatchedToolOutputs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").
		AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{
			{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
			{ID: "call_2", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.txt"}`)},
		}}).
		AddTextResponse("done")
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager, store: store}
	const sessionID = "sess_durable_tool_batch"
	firstBody := `{"input":"read both files","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
	code, firstResponse := doResponsesWithHeader(t, srv, firstBody, sessionID)
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	previousID, _ := firstResponse["id"].(string)
	if !strings.HasPrefix(previousID, durableResponseMessagePrefix) {
		t.Fatalf("first response id = %q, want %s*", previousID, durableResponseMessagePrefix)
	}

	secondBody := fmt.Sprintf(`{"previous_response_id":%q,"input":[{"type":"function_call_output","call_id":"call_1","output":"alpha"},{"type":"function_call_output","call_id":"call_2","output":"beta"}]}`, previousID)
	code, _ = doResponses(t, srv, secondBody)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}

	gotResults := map[string]llm.ToolResult{}
	for _, msg := range provider.Requests[1].Messages {
		for _, part := range msg.Parts {
			if part.Type == llm.PartToolResult && part.ToolResult != nil {
				gotResults[part.ToolResult.ID] = *part.ToolResult
			}
		}
	}
	for id, content := range map[string]string{"call_1": "alpha", "call_2": "beta"} {
		result, ok := gotResults[id]
		if !ok {
			t.Fatalf("second provider request missing tool result %q", id)
		}
		if result.Name != "read_file" || result.Content != content {
			t.Fatalf("tool result %q = (%q, %q), want (%q, %q)", id, result.Name, result.Content, "read_file", content)
		}
	}
}

func TestResponsesMessageBackedPreviousResponseIgnoresStaleRuntimeTail(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	const sessionID = "sess_durable_runtime_tail"
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), sessionID, []session.Message{
		*session.NewMessage(sessionID, llm.UserText("old question"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("old answer"), -1),
		*session.NewMessage(sessionID, llm.UserText("newer question"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("newer answer"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}
	seed, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	staleRuntimeID := durableResponseIDForMessageID(seed[1].ID)
	previousID := durableResponseIDForMessageID(seed[len(seed)-1].ID)

	provider := llm.NewMockProvider("mock").AddTextResponse("continued")
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.addResponseID(staleRuntimeID)
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager, store: store}
	srv.responseToSession.Store(staleRuntimeID, sessionID)
	srv.sessionToResponse.Store(sessionID, staleRuntimeID)

	body := fmt.Sprintf(`{"input":"continue","previous_response_id":%q}`, previousID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("session_id", sessionID)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider request count = %d, want 1", len(provider.Requests))
	}
}

func TestResponsesMessageBackedPreviousResponseRejectsReplayShape(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	const sessionID = "sess_ui_replay"
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), sessionID, []session.Message{
		*session.NewMessage(sessionID, llm.UserText("old"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("old answer"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}
	seed, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	previousID := durableResponseIDForMessageID(seed[len(seed)-1].ID)
	provider := llm.NewMockProvider("mock").AddTextResponse("should not run")
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		return &serveRuntime{provider: provider, engine: engine, store: store, defaultModel: "mock-model"}, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager, store: store}
	body := fmt.Sprintf(`{"previous_response_id":%q,"input":[{"type":"message","role":"user","content":"old"},{"type":"message","role":"assistant","content":"old answer"},{"type":"message","role":"user","content":"new"}]}`, previousID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider request count = %d, want 0", len(provider.Requests))
	}
	msgs, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after reject: %v", err)
	}
	if len(msgs) != 2 || msgs[0].TextContent != "old" || msgs[1].TextContent != "old answer" {
		t.Fatalf("messages changed after rejected replay: %#v", msgs)
	}
}

func TestResponsesMessageBackedPreviousResponseRejectsStaleTail(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	const sessionID = "sess_stale_durable"
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), sessionID, []session.Message{
		*session.NewMessage(sessionID, llm.UserText("first"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("first answer"), -1),
		*session.NewMessage(sessionID, llm.UserText("second"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("second answer"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}
	seed, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	staleID := durableResponseIDForMessageID(seed[1].ID)

	provider := llm.NewMockProvider("mock").AddTextResponse("should not run")
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		return &serveRuntime{provider: provider, engine: engine, store: store, defaultModel: "mock-model"}, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager, store: store}
	body := fmt.Sprintf(`{"input":"third","previous_response_id":%q}`, staleID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider request count = %d, want 0", len(provider.Requests))
	}
	msgs, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after stale reject: %v", err)
	}
	if len(msgs) != len(seed) {
		t.Fatalf("message count changed after stale reject: got %d want %d", len(msgs), len(seed))
	}
}

func TestResponsesMessageBackedPreviousResponseResolvesThroughLoggingStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	const sessionID = "sess_logging_wrapper"
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Status: session.StatusActive}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), sessionID, []session.Message{
		*session.NewMessage(sessionID, llm.UserText("hello"), -1),
		*session.NewMessage(sessionID, llm.AssistantText("hi"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}
	seed, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	previousID := durableResponseIDForMessageID(seed[len(seed)-1].ID)

	wrapped := session.NewLoggingStore(store, t.Logf)
	srv := &serveServer{store: wrapped}
	body := fmt.Sprintf(`{"input":"wrong session","previous_response_id":%q}`, previousID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", "other-session")
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 after durable ID resolves through LoggingStore; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "conflicts with previous_response_id session") {
		t.Fatalf("body = %s, want session conflict", rr.Body.String())
	}
}

func TestResponsesMessageBackedPreviousResponseRejectsConflictingSessionHeader(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	const sessionA = "sess_conflict_a"
	const sessionB = "sess_conflict_b"
	for _, id := range []string{sessionA, sessionB} {
		if err := store.Create(context.Background(), &session.Session{ID: id, Status: session.StatusActive}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	if err := store.ReplaceMessages(context.Background(), sessionA, []session.Message{
		*session.NewMessage(sessionA, llm.UserText("hello"), -1),
		*session.NewMessage(sessionA, llm.AssistantText("hi"), -1),
	}); err != nil {
		t.Fatalf("seed ReplaceMessages: %v", err)
	}
	seed, err := store.GetMessages(context.Background(), sessionA, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	previousID := durableResponseIDForMessageID(seed[len(seed)-1].ID)

	srv := &serveServer{store: store}
	body := fmt.Sprintf(`{"input":"wrong session","previous_response_id":%q}`, previousID)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", sessionB)
	rr := httptest.NewRecorder()

	srv.handleResponses(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestResolveServeAuthMode(t *testing.T) {
	mode, err := resolveServeAuthMode(false, "bearer", false, false)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "bearer" {
		t.Fatalf("mode = %q, want bearer", mode)
	}

	mode, err = resolveServeAuthMode(false, "none", false, false)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "none" {
		t.Fatalf("mode = %q, want none", mode)
	}

	mode, err = resolveServeAuthMode(false, "bearer", true, true)
	if err != nil {
		t.Fatalf("resolveServeAuthMode returned error: %v", err)
	}
	if mode != "none" {
		t.Fatalf("mode = %q, want none", mode)
	}

	if _, err := resolveServeAuthMode(true, "bearer", true, true); err == nil {
		t.Fatalf("expected conflict error when --auth and --allow-no-auth disagree")
	}

	if _, err := resolveServeAuthMode(true, "invalid", false, false); err == nil {
		t.Fatalf("expected invalid auth mode error")
	}
}

func TestServeAuthMiddleware_CookieFallback(t *testing.T) {
	srv := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "secret"}}
	h := srv.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// No credentials → 401
	req := httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", rr.Code)
	}

	// Valid cookie → allowed
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("valid cookie: status = %d, want 204", rr.Code)
	}

	// Wrong cookie → 401
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "wrong"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: status = %d, want 401", rr.Code)
	}

	// Bearer still works
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("bearer: status = %d, want 204", rr.Code)
	}

	// Cookie on POST to API → rejected (cookie fallback is not enabled for mutating API routes)
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cookie on API POST: status = %d, want 401", rr.Code)
	}

	// Cookie on widget mutating requests → allowed so iframe/widget fetches work.
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		req = httptest.NewRequest(method, "/widgets/demo/state", nil)
		req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "secret"})
		rr = httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("cookie on widget %s: status = %d, want 204", method, rr.Code)
		}
	}

	// URL-encoded cookie → decoded and accepted
	srv2 := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "se+cret/val="}}
	h2 := srv2.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req = httptest.NewRequest(http.MethodGet, "/ui/images/test.png", nil)
	req.AddCookie(&http.Cookie{Name: "term_llm_token", Value: "se%2Bcret%2Fval%3D"})
	rr = httptest.NewRecorder()
	h2(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("url-encoded cookie: status = %d, want 204", rr.Code)
	}
}

func TestHandleImage_ServesFileAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cat.png"), []byte("fake-png"), 0644); err != nil {
		t.Fatalf("write test image: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "benchmarks", "go"), 0755); err != nil {
		t.Fatalf("mkdir nested image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "benchmarks", "go", "board.png"), []byte("nested-png"), 0644); err != nil {
		t.Fatalf("write nested image: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = dir

	// Paths as seen by handler after StripPrefix removes basePath.
	// Valid file
	req := httptest.NewRequest(http.MethodGet, "/images/cat.png", nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "fake-png" {
		t.Fatalf("body = %q, want %q", got, "fake-png")
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q, want 'private'", cc)
	}
	if vary := rr.Header().Get("Vary"); vary == "" {
		t.Fatalf("missing Vary header")
	}

	// Nested file
	req = httptest.NewRequest(http.MethodGet, "/images/benchmarks/go/board.png", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nested file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "nested-png" {
		t.Fatalf("nested body = %q, want %q", got, "nested-png")
	}

	// Path traversal with ..
	req = httptest.NewRequest(http.MethodGet, "/images/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("traversal: status = %d, want 404", rr.Code)
	}

	// Empty filename
	req = httptest.NewRequest(http.MethodGet, "/images/", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty: status = %d, want 404", rr.Code)
	}

	// Nonexistent file
	req = httptest.NewRequest(http.MethodGet, "/images/nope.png", nil)
	rr = httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing: status = %d, want 404", rr.Code)
	}
}

func TestEnsureImageServeable_RejectsExternalFile(t *testing.T) {
	outputDir := t.TempDir()
	externalDir := t.TempDir()

	// Create a file outside the image output directory.
	externalImg := filepath.Join(externalDir, "photo.png")
	if err := os.WriteFile(externalImg, []byte("external-image-data"), 0644); err != nil {
		t.Fatalf("write external image: %v", err)
	}

	// Create a file already inside the output directory.
	internalImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(internalImg, []byte("internal-image-data"), 0644); err != nil {
		t.Fatalf("write internal image: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui"}, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = outputDir

	// External files should be rejected instead of republished.
	if result, ok := srv.ensureImageServeable(externalImg); ok || result != "" {
		t.Fatalf("ensureImageServeable should reject external image, got result=%q ok=%v", result, ok)
	}

	// Internal file should be returned as an absolute, serveable path.
	result, ok := srv.ensureImageServeable(internalImg)
	if !ok {
		t.Fatal("ensureImageServeable should succeed for internal image")
	}
	absInternalImg, err := filepath.EvalSymlinks(internalImg)
	if err != nil {
		t.Fatalf("resolve internal image: %v", err)
	}
	if result != absInternalImg {
		t.Fatalf("internal image should be unchanged apart from canonicalization, got %q want %q", result, absInternalImg)
	}

	// Verify the internal file is actually serveable via handleImage.
	req := httptest.NewRequest(http.MethodGet, "/images/"+filepath.Base(result), nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("serve internal image: status = %d, want 200", rr.Code)
	}
}

func TestMaterializeInlineSessionImageIsBoundedAndPrivate(t *testing.T) {
	outputDir := t.TempDir()
	srv := &serveServer{cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = outputDir

	path, ok := srv.materializeInlineSessionImage("image/png", base64.StdEncoding.EncodeToString([]byte("png")))
	if !ok {
		t.Fatal("materializeInlineSessionImage returned false for small image")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat materialized image: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("permissions = %v, want 0600", got)
	}

	tooLarge := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, maxMaterializedSessionImageBytes+1))
	if _, ok := srv.materializeInlineSessionImage("image/png", tooLarge); ok {
		t.Fatal("materializeInlineSessionImage should reject oversized inline images")
	}
}

func TestEnsureImageServeable_CopiesFromWriteDir(t *testing.T) {
	outputDir := t.TempDir()
	writeDir := t.TempDir()
	externalDir := t.TempDir()

	writeDirImg := filepath.Join(writeDir, "tool-output.png")
	if err := os.WriteFile(writeDirImg, []byte("tool-png"), 0644); err != nil {
		t.Fatalf("write writeDir image: %v", err)
	}
	externalImg := filepath.Join(externalDir, "secret.png")
	if err := os.WriteFile(externalImg, []byte("secret-png"), 0644); err != nil {
		t.Fatalf("write external image: %v", err)
	}
	largeImg := filepath.Join(writeDir, "huge.png")
	largeFile, err := os.Create(largeImg)
	if err != nil {
		t.Fatalf("create large image: %v", err)
	}
	if err := largeFile.Truncate(maxMaterializedSessionImageBytes + 1); err != nil {
		largeFile.Close()
		t.Fatalf("truncate large image: %v", err)
	}
	if err := largeFile.Close(); err != nil {
		t.Fatalf("close large image: %v", err)
	}

	srv := &serveServer{
		cfg: serveServerConfig{
			basePath:  "/ui",
			writeDirs: []string{writeDir},
		},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	result, ok := srv.ensureImageServeable(writeDirImg)
	if !ok {
		t.Fatal("ensureImageServeable should accept images from configured writeDirs")
	}
	if result == writeDirImg {
		t.Fatal("writeDir image should have been copied into imageOutputDir")
	}
	absResult, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("resolve copied image: %v", err)
	}
	absOutputDir, err := filepath.EvalSymlinks(outputDir)
	if err != nil {
		t.Fatalf("resolve output dir: %v", err)
	}
	if !strings.HasPrefix(absResult, absOutputDir+string(filepath.Separator)) {
		t.Fatalf("copied image %q should be under output dir %q", absResult, absOutputDir)
	}

	resultAgain, ok := srv.ensureImageServeable(writeDirImg)
	if !ok {
		t.Fatal("second ensureImageServeable should accept images from configured writeDirs")
	}
	if resultAgain != result {
		t.Fatalf("second ensureImageServeable result = %q, want stable %q", resultAgain, result)
	}

	// The new copy should round-trip through handleImage.
	req := httptest.NewRequest(http.MethodGet, "/images/"+filepath.Base(result), nil)
	rr := httptest.NewRecorder()
	srv.handleImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("serve copied image: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "tool-png" {
		t.Fatalf("body = %q, want %q", got, "tool-png")
	}

	if _, ok := srv.ensureImageServeable(largeImg); ok {
		t.Fatal("oversized images from approved dirs must be rejected")
	}

	if _, ok := srv.ensureImageServeable(externalImg); ok {
		t.Fatal("images outside all approved dirs must still be rejected")
	}
}

func TestEnsureImageServeable_ReusesExistingMaterializedCopyWithoutDirectoryWrites(t *testing.T) {
	outputDir := t.TempDir()
	writeDir := t.TempDir()
	writeDirImg := filepath.Join(writeDir, "tool-output.png")
	if err := os.WriteFile(writeDirImg, []byte("tool-png"), 0644); err != nil {
		t.Fatalf("write writeDir image: %v", err)
	}

	srv := &serveServer{
		cfg:    serveServerConfig{writeDirs: []string{writeDir}},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	result, ok := srv.ensureImageServeable(writeDirImg)
	if !ok {
		t.Fatal("first ensureImageServeable should accept images from configured writeDirs")
	}

	fixedTime := time.Unix(123, 0)
	if err := os.Chtimes(outputDir, fixedTime, fixedTime); err != nil {
		t.Fatalf("set output dir times: %v", err)
	}

	resultAgain, ok := srv.ensureImageServeable(writeDirImg)
	if !ok {
		t.Fatal("second ensureImageServeable should accept images from configured writeDirs")
	}
	if resultAgain != result {
		t.Fatalf("second ensureImageServeable result = %q, want stable %q", resultAgain, result)
	}

	info, err := os.Stat(outputDir)
	if err != nil {
		t.Fatalf("stat output dir: %v", err)
	}
	if !info.ModTime().Equal(fixedTime) {
		t.Fatalf("output dir modtime = %v, want unchanged %v", info.ModTime(), fixedTime)
	}
}

func deterministicServeName(data []byte, base string) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("serve-%s-%s", hex.EncodeToString(sum[:16]), base)
}

func TestEnsureImageServeable_RejectsDirectoryAtDeterministicDestination(t *testing.T) {
	outputDir := t.TempDir()
	writeDir := t.TempDir()
	data := []byte("tool-png")
	writeDirImg := filepath.Join(writeDir, "tool-output.png")
	if err := os.WriteFile(writeDirImg, data, 0644); err != nil {
		t.Fatalf("write writeDir image: %v", err)
	}
	if err := os.Mkdir(filepath.Join(outputDir, deterministicServeName(data, filepath.Base(writeDirImg))), 0755); err != nil {
		t.Fatalf("create directory at deterministic destination: %v", err)
	}

	srv := &serveServer{
		cfg:    serveServerConfig{writeDirs: []string{writeDir}},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	if result, ok := srv.ensureImageServeable(writeDirImg); ok || result != "" {
		t.Fatalf("ensureImageServeable should reject directory destination, got result=%q ok=%v", result, ok)
	}
}

func TestHandleFile_ServesFileAndRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "video.mp4"), []byte("fake-video"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "qwen36-go-benchmark"), 0755); err != nil {
		t.Fatalf("mkdir nested file dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen36-go-benchmark", "board.html"), []byte("<html>nested-board</html>"), 0644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}
	// index.html must be served verbatim — http.ServeFile would otherwise
	// 301-redirect any URL ending in /index.html to "./", which then hits
	// the SPA catch-all and returns the app shell.
	indexHTML := "<html>served-file-index</html>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qwen36-go-benchmark", "index.html"), []byte("<html>nested-index</html>"), 0644); err != nil {
		t.Fatalf("write nested index.html: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui", filesDir: dir}}

	// Valid file
	req := httptest.NewRequest(http.MethodGet, "/files/video.mp4", nil)
	rr := httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "fake-video" {
		t.Fatalf("body = %q, want %q", got, "fake-video")
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q, want 'private'", cc)
	}

	// Nested file
	req = httptest.NewRequest(http.MethodGet, "/files/qwen36-go-benchmark/board.html", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nested file: status = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "<html>nested-board</html>" {
		t.Fatalf("nested body = %q, want %q", got, "<html>nested-board</html>")
	}

	// index.html at the root of files-dir — must not redirect to "./".
	req = httptest.NewRequest(http.MethodGet, "/files/index.html", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("index.html: status = %d, want 200 (body=%q, location=%q)", rr.Code, rr.Body.String(), rr.Header().Get("Location"))
	}
	if got := rr.Body.String(); got != indexHTML {
		t.Fatalf("index.html body = %q, want %q", got, indexHTML)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'" {
		t.Fatalf("unsafe file CSP: %q", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("file nosniff = %q", got)
	}

	// Nested index.html — same redirect trap.
	req = httptest.NewRequest(http.MethodGet, "/files/qwen36-go-benchmark/index.html", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("nested index.html: status = %d, want 200 (location=%q)", rr.Code, rr.Header().Get("Location"))
	}
	if got := rr.Body.String(); got != "<html>nested-index</html>" {
		t.Fatalf("nested index.html body = %q, want %q", got, "<html>nested-index</html>")
	}

	// Path traversal
	req = httptest.NewRequest(http.MethodGet, "/files/..%2Fetc%2Fpasswd", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("traversal: status = %d, want 404", rr.Code)
	}

	// Empty filename
	req = httptest.NewRequest(http.MethodGet, "/files/", nil)
	rr = httptest.NewRecorder()
	srv.handleFile(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("empty: status = %d, want 404", rr.Code)
	}

	// No files-dir configured → 404
	srv2 := &serveServer{cfg: serveServerConfig{basePath: "/ui"}}
	req = httptest.NewRequest(http.MethodGet, "/files/video.mp4", nil)
	rr = httptest.NewRecorder()
	srv2.handleFile(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("no files-dir: status = %d, want 404", rr.Code)
	}
}

func TestServeRoutePath_PreservesNestedRelativePaths(t *testing.T) {
	baseDir := t.TempDir()
	servedPath := filepath.Join(baseDir, "qwen36-go-benchmark", "board.html")
	if err := os.MkdirAll(filepath.Dir(servedPath), 0755); err != nil {
		t.Fatalf("mkdir served path dir: %v", err)
	}
	if err := os.WriteFile(servedPath, []byte("ok"), 0644); err != nil {
		t.Fatalf("write served path file: %v", err)
	}

	got := serveRoutePath("/chat/files/", baseDir, servedPath)
	want := "/chat/files/qwen36-go-benchmark/board.html"
	if got != want {
		t.Fatalf("serveRoutePath() = %q, want %q", got, want)
	}
}

func TestEnsureFileServeable_RejectsExternalFile(t *testing.T) {
	filesDir := t.TempDir()
	externalDir := t.TempDir()

	externalFile := filepath.Join(externalDir, "report.pdf")
	if err := os.WriteFile(externalFile, []byte("pdf-data"), 0644); err != nil {
		t.Fatalf("write external file: %v", err)
	}

	internalFile := filepath.Join(filesDir, "existing.mp4")
	if err := os.WriteFile(internalFile, []byte("video-data"), 0644); err != nil {
		t.Fatalf("write internal file: %v", err)
	}

	srv := &serveServer{cfg: serveServerConfig{basePath: "/ui", filesDir: filesDir}}

	// External files should be rejected instead of republished.
	if result, ok := srv.ensureFileServeable(externalFile); ok || result != "" {
		t.Fatalf("ensureFileServeable should reject external file, got result=%q ok=%v", result, ok)
	}

	// Internal file should be returned as an absolute, serveable path.
	result, ok := srv.ensureFileServeable(internalFile)
	if !ok {
		t.Fatal("ensureFileServeable should succeed for internal file")
	}
	absInternalFile, err := filepath.EvalSymlinks(internalFile)
	if err != nil {
		t.Fatalf("resolve internal file: %v", err)
	}
	if result != absInternalFile {
		t.Fatalf("internal file should be unchanged apart from canonicalization, got %q want %q", result, absInternalFile)
	}

	// No files-dir → fails.
	srv2 := &serveServer{cfg: serveServerConfig{basePath: "/ui"}}
	_, ok = srv2.ensureFileServeable(externalFile)
	if ok {
		t.Fatal("ensureFileServeable should fail when filesDir is empty")
	}
}

func TestEnsureFileServeable_CopiesFromImageOutputDir(t *testing.T) {
	filesDir := t.TempDir()
	outputDir := t.TempDir()
	generatedImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(generatedImg, []byte("image-data"), 0644); err != nil {
		t.Fatalf("write generated image: %v", err)
	}

	srv := &serveServer{
		cfg:    serveServerConfig{basePath: "/ui", filesDir: filesDir},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	result, ok := srv.ensureFileServeable(generatedImg)
	if !ok {
		t.Fatal("ensureFileServeable should allow files from image output dir")
	}
	if result == generatedImg {
		t.Fatal("image output file should have been copied into filesDir")
	}
	absResult, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("resolve copied file: %v", err)
	}
	absFilesDir, err := filepath.EvalSymlinks(filesDir)
	if err != nil {
		t.Fatalf("resolve files dir: %v", err)
	}
	if !strings.HasPrefix(absResult, absFilesDir+string(filepath.Separator)) {
		t.Fatalf("copied file %q should be under files dir %q", absResult, absFilesDir)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "image-data" {
		t.Fatalf("copied data = %q, want %q", string(data), "image-data")
	}

	secondResult, ok := srv.ensureFileServeable(generatedImg)
	if !ok {
		t.Fatal("second ensureFileServeable should also succeed")
	}
	if secondResult != result {
		t.Fatalf("repeat ensureFileServeable should reuse the same copied path, got %q want %q", secondResult, result)
	}
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		t.Fatalf("read files dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("files dir should contain exactly one copied file after repeat calls, got %d", len(entries))
	}
}

func TestEnsureFileServeable_ReusesExistingMaterializedCopyWithoutDirectoryWrites(t *testing.T) {
	filesDir := t.TempDir()
	outputDir := t.TempDir()
	data := []byte("image-data")
	generatedImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(generatedImg, data, 0644); err != nil {
		t.Fatalf("write generated image: %v", err)
	}

	srv := &serveServer{
		cfg:    serveServerConfig{basePath: "/ui", filesDir: filesDir},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	result, ok := srv.ensureFileServeable(generatedImg)
	if !ok {
		t.Fatal("first ensureFileServeable should allow files from image output dir")
	}

	fixedTime := time.Unix(456, 0)
	if err := os.Chtimes(filesDir, fixedTime, fixedTime); err != nil {
		t.Fatalf("set files dir times: %v", err)
	}

	resultAgain, ok := srv.ensureFileServeable(generatedImg)
	if !ok {
		t.Fatal("second ensureFileServeable should allow files from image output dir")
	}
	if resultAgain != result {
		t.Fatalf("second ensureFileServeable result = %q, want stable %q", resultAgain, result)
	}

	info, err := os.Stat(filesDir)
	if err != nil {
		t.Fatalf("stat files dir: %v", err)
	}
	if !info.ModTime().Equal(fixedTime) {
		t.Fatalf("files dir modtime = %v, want unchanged %v", info.ModTime(), fixedTime)
	}
}

func TestEnsureFileServeable_RejectsDirectoryAtDeterministicDestination(t *testing.T) {
	filesDir := t.TempDir()
	outputDir := t.TempDir()
	data := []byte("image-data")
	generatedImg := filepath.Join(outputDir, "generated.png")
	if err := os.WriteFile(generatedImg, data, 0644); err != nil {
		t.Fatalf("write generated image: %v", err)
	}
	if err := os.Mkdir(filepath.Join(filesDir, deterministicServeName(data, filepath.Base(generatedImg))), 0755); err != nil {
		t.Fatalf("create directory at deterministic destination: %v", err)
	}

	srv := &serveServer{
		cfg:    serveServerConfig{basePath: "/ui", filesDir: filesDir},
		cfgRef: &config.Config{},
	}
	srv.cfgRef.Image.OutputDir = outputDir

	if result, ok := srv.ensureFileServeable(generatedImg); ok || result != "" {
		t.Fatalf("ensureFileServeable should reject directory destination, got result=%q ok=%v", result, ok)
	}
}

func TestEnsureFileServeable_CopiesFromWriteDir(t *testing.T) {
	filesDir := t.TempDir()
	writeDir := t.TempDir()
	externalDir := t.TempDir()

	toolOutput := filepath.Join(writeDir, "tool-output.pdf")
	if err := os.WriteFile(toolOutput, []byte("pdf-data"), 0644); err != nil {
		t.Fatalf("write tool output: %v", err)
	}
	external := filepath.Join(externalDir, "secret.txt")
	if err := os.WriteFile(external, []byte("secret"), 0644); err != nil {
		t.Fatalf("write external: %v", err)
	}

	srv := &serveServer{
		cfg: serveServerConfig{
			basePath:  "/ui",
			filesDir:  filesDir,
			writeDirs: []string{writeDir},
		},
		cfgRef: &config.Config{},
	}

	result, ok := srv.ensureFileServeable(toolOutput)
	if !ok {
		t.Fatal("ensureFileServeable should allow files from configured writeDirs")
	}
	if result == toolOutput {
		t.Fatal("writeDir source should have been copied into filesDir")
	}
	absResult, err := filepath.EvalSymlinks(result)
	if err != nil {
		t.Fatalf("resolve copied file: %v", err)
	}
	absFilesDir, err := filepath.EvalSymlinks(filesDir)
	if err != nil {
		t.Fatalf("resolve files dir: %v", err)
	}
	if !strings.HasPrefix(absResult, absFilesDir+string(filepath.Separator)) {
		t.Fatalf("copied file %q should be under files dir %q", absResult, absFilesDir)
	}

	if _, ok := srv.ensureFileServeable(external); ok {
		t.Fatal("paths outside all approved dirs must still be rejected")
	}
}

func TestHandleSessions_ListsFromStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "test-session-1",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Summary:   "hello world",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Sessions []struct {
			ID            string `json:"id"`
			ShortTitle    string `json:"short_title"`
			LongTitle     string `json:"long_title"`
			CreatedAt     int64  `json:"created_at"`
			LastMessageAt int64  `json:"last_message_at"`
			MsgCount      int    `json:"message_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(body.Sessions))
	}
	if body.Sessions[0].ID != "test-session-1" {
		t.Fatalf("id = %q, want %q", body.Sessions[0].ID, "test-session-1")
	}
	if body.Sessions[0].ShortTitle != "hello world" {
		t.Fatalf("short_title = %q, want %q", body.Sessions[0].ShortTitle, "hello world")
	}
	if body.Sessions[0].LastMessageAt == 0 {
		t.Fatalf("last_message_at = 0, want non-zero (falling back to created_at)")
	}
	if body.Sessions[0].LastMessageAt != body.Sessions[0].CreatedAt {
		t.Fatalf("last_message_at = %d, want %d (fallback to created_at when no messages)", body.Sessions[0].LastMessageAt, body.Sessions[0].CreatedAt)
	}
}

func TestHandleSessions_SideloadsExactWidgetStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	widgetsDir := t.TempDir()
	widgetDir := filepath.Join(widgetsDir, "private-widget-id")
	if err := os.MkdirAll(widgetDir, 0o755); err != nil {
		t.Fatalf("mkdir widget: %v", err)
	}
	manifest := `title: "Startup Metrics"
mount: metrics
command: ["widget-server", "--socket", "$SOCKET"]
description: "Safe sidebar description"
`
	if err := os.WriteFile(filepath.Join(widgetDir, "widget.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write widget manifest: %v", err)
	}
	manager := widgets.NewManager(widgetsDir, "/ui")
	defer manager.Close()

	srv := &serveServer{
		cfg:        serveServerConfig{basePath: "/ui", ui: true},
		store:      store,
		widgetsMgr: manager,
	}

	sideloadReq := httptest.NewRequest(http.MethodGet, "/v1/sessions?include_widget_status=1", nil)
	sideloadRec := httptest.NewRecorder()
	srv.handleSessions(sideloadRec, sideloadReq)
	if sideloadRec.Code != http.StatusOK {
		t.Fatalf("sideload status = %d, want 200; body=%s", sideloadRec.Code, sideloadRec.Body.String())
	}
	var sideload map[string]any
	if err := json.Unmarshal(sideloadRec.Body.Bytes(), &sideload); err != nil {
		t.Fatalf("decode sideload: %v", err)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/admin/widgets/status", nil)
	statusRec := httptest.NewRecorder()
	srv.handleAdminWidgetsStatus(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("widget status = %d, want 200; body=%s", statusRec.Code, statusRec.Body.String())
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusRec.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("decode widget status: %v", err)
	}

	if !reflect.DeepEqual(sideload["widget_status"], statusPayload) {
		t.Fatalf("sideloaded widget_status = %#v, want exact endpoint shape %#v", sideload["widget_status"], statusPayload)
	}
	widgetStatus, ok := sideload["widget_status"].(map[string]any)
	if !ok {
		t.Fatalf("widget_status type = %T, want object", sideload["widget_status"])
	}
	entries, ok := widgetStatus["widgets"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("widget_status.widgets = %#v, want one entry", widgetStatus["widgets"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok || entry["id"] != "private-widget-id" || entry["mount"] != "metrics" || entry["title"] != "Startup Metrics" || entry["description"] != "Safe sidebar description" || entry["state"] != "stopped" {
		t.Fatalf("serialized widget entry = %#v", entries[0])
	}
	if strings.Contains(sideloadRec.Body.String(), widgetsDir) || strings.Contains(sideloadRec.Body.String(), "widget-server") || strings.Contains(sideloadRec.Body.String(), "$SOCKET") {
		t.Fatalf("sideload exposed filesystem or command configuration: %s", sideloadRec.Body.String())
	}

	index := string(srv.buildIndexHTML(""))
	for _, sensitive := range []string{"private-widget-id", "Startup Metrics", "Safe sidebar description", widgetsDir} {
		if strings.Contains(index, sensitive) {
			t.Fatalf("public bootstrap HTML exposed widget status/config value %q", sensitive)
		}
	}

	// The sideload remains behind the same auth wrapper as the sessions API;
	// the public shell must not become an alternate widget-discovery surface.
	srv.cfg.requireAuth = true
	srv.cfg.token = "startup-secret"
	handler := srv.httpHandler()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/ui/v1/sessions?include_widget_status=1", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated sideload status = %d, want 401", unauthorized.Code)
	}
	if strings.Contains(unauthorized.Body.String(), "Startup Metrics") {
		t.Fatalf("unauthenticated sideload leaked widget status: %s", unauthorized.Body.String())
	}
	authorizedReq := httptest.NewRequest(http.MethodGet, "/ui/v1/sessions?include_widget_status=1", nil)
	authorizedReq.Header.Set("Authorization", "Bearer startup-secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedReq)
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), "Startup Metrics") {
		t.Fatalf("authenticated sideload status = %d, body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestHandleSessions_SideloadsAuthoritativeEmptyWidgetStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?include_widget_status=1", nil)
	rec := httptest.NewRecorder()
	srv.handleSessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		WidgetStatus struct {
			Widgets []json.RawMessage `json:"widgets"`
		} `json:"widget_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.WidgetStatus.Widgets == nil {
		t.Fatalf("widget_status.widgets = nil, want authoritative [] in %s", rec.Body.String())
	}
	if len(payload.WidgetStatus.Widgets) != 0 {
		t.Fatalf("widget_status.widgets = %d entries, want 0", len(payload.WidgetStatus.Widgets))
	}
}

func TestHandleSessionsSearch_UsesFTSAndReturnsSessionSummaries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:                  "search-session-1",
		Provider:            "mock",
		Model:               "mock-model",
		Mode:                session.ModeChat,
		GeneratedShortTitle: "Linux Mount Configuration",
		GeneratedLongTitle:  "Linux Mount Configuration Details",
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
		Status:              session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := store.AddMessage(ctx, sess.ID, &session.Message{
		Role:        llm.RoleUser,
		TextContent: "How do I configure linux mount entries in fstab with UUIDs?",
		Parts:       []llm.Part{{Type: llm.PartText, Text: "How do I configure linux mount entries in fstab with UUIDs?"}},
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	lifecycle, ok := session.AsServeResponseLifecycleStore(store)
	if !ok {
		t.Fatal("SQLite store missing response lifecycle capability")
	}
	lease, err := lifecycle.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "search-response", SessionID: sess.ID, RunEpoch: 1, OwnerInstanceID: "owner", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := lifecycle.FinalizeResponseRun(ctx, session.ResponseRunTerminal{ResponseID: "search-response", OwnerInstanceID: "owner", FencingToken: lease.FencingToken, Outcome: session.ResponseRunCompleted, FinalRev: 1})
	if err != nil {
		t.Fatal(err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/search?q=linux%20mount", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionsSearch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Sessions []struct {
			ID                       string `json:"id"`
			ShortTitle               string `json:"short_title"`
			Snippet                  string `json:"snippet"`
			AttentionStoreInstanceID string `json:"attention_store_instance_id"`
			AttentionSeq             int64  `json:"attention_seq"`
			AttentionUnseen          bool   `json:"attention_unseen"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1; body: %s", len(body.Sessions), rr.Body.String())
	}
	if body.Sessions[0].ID != sess.ID {
		t.Fatalf("id = %q, want %q", body.Sessions[0].ID, sess.ID)
	}
	if body.Sessions[0].ShortTitle != "Linux Mount Configuration" {
		t.Fatalf("short_title = %q", body.Sessions[0].ShortTitle)
	}
	if !strings.Contains(body.Sessions[0].Snippet, "linux") && !strings.Contains(body.Sessions[0].Snippet, "Linux") {
		t.Fatalf("snippet = %q, want matched text", body.Sessions[0].Snippet)
	}
	if body.Sessions[0].AttentionStoreInstanceID != marker.StoreInstanceID ||
		body.Sessions[0].AttentionSeq != marker.LatestAttentionSeq || !body.Sessions[0].AttentionUnseen {
		t.Fatalf("search attention projection = %+v, marker=%+v", body.Sessions[0], marker)
	}
}

func TestHandleSessionsSearch_DoesNotDropOlderMatchesOutsideRecent2000ListWindow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	base := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	matching := &session.Session{
		ID:        "older-match",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Summary:   "older matching session",
		CreatedAt: base,
		UpdatedAt: base,
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, matching); err != nil {
		t.Fatalf("Create matching session: %v", err)
	}
	if err := store.AddMessage(ctx, matching.ID, &session.Message{
		Role:        llm.RoleUser,
		TextContent: "needle older searchable session",
		Parts:       []llm.Part{{Type: llm.PartText, Text: "needle older searchable session"}},
		CreatedAt:   base,
	}); err != nil {
		t.Fatalf("AddMessage matching session: %v", err)
	}

	for i := 0; i < 2000; i++ {
		createdAt := base.Add(time.Duration(i+1) * time.Second)
		sess := &session.Session{
			ID:        fmt.Sprintf("newer-%04d", i),
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Summary:   fmt.Sprintf("newer session %d", i),
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
			Status:    session.StatusActive,
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create newer session %d: %v", i, err)
		}
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/search?q=needle&limit=5", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionsSearch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	var body struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1; body: %s", len(body.Sessions), rr.Body.String())
	}
	if body.Sessions[0].ID != matching.ID {
		t.Fatalf("id = %q, want %q", body.Sessions[0].ID, matching.ID)
	}
}

func TestHandleSessions_GzipCompressed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	for i := range 12 {
		sess := &session.Session{
			ID:          fmt.Sprintf("test-session-%02d", i),
			Provider:    "mock",
			ProviderKey: "mock",
			Model:       "mock-model",
			Mode:        session.ModeChat,
			Summary:     "compressible session title " + strings.Repeat("abc", 20),
			CreatedAt:   now.Add(time.Duration(i) * time.Second),
			UpdatedAt:   now.Add(time.Duration(i) * time.Second),
			Status:      session.StatusActive,
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create session %d: %v", i, err)
		}
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control = %q, want no-cache", got)
	}
	if rr.Header().Get("ETag") == "" {
		t.Fatalf("expected ETag")
	}

	zr, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("ReadAll gzip: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("Close gzip: %v", err)
	}
	var body struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(decompressed, &body); err != nil {
		t.Fatalf("decode decompressed JSON: %v", err)
	}
	if len(body.Sessions) != 12 {
		t.Fatalf("session count = %d, want 12", len(body.Sessions))
	}
}

func TestHandleSessions_FiltersByCategoriesAndArchived(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	sessions := []*session.Session{
		{
			ID:        "sess-tui",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Origin:    session.OriginTUI,
			Pinned:    true,
			Summary:   "tui chat",
			CreatedAt: now,
			UpdatedAt: now,
			Status:    session.StatusActive,
		},
		{
			ID:        "sess-web",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Origin:    session.OriginWeb,
			Summary:   "web chat",
			CreatedAt: now.Add(time.Second),
			UpdatedAt: now.Add(time.Second),
			Status:    session.StatusActive,
		},
		{
			ID:        "sess-ask",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeAsk,
			Origin:    session.OriginTUI,
			Summary:   "ask run",
			CreatedAt: now.Add(2 * time.Second),
			UpdatedAt: now.Add(2 * time.Second),
			Status:    session.StatusActive,
		},
		{
			ID:        "sess-hidden",
			Provider:  "mock",
			Model:     "mock-model",
			Mode:      session.ModeChat,
			Origin:    session.OriginWeb,
			Summary:   "hidden web chat",
			CreatedAt: now.Add(3 * time.Second),
			UpdatedAt: now.Add(3 * time.Second),
			Status:    session.StatusActive,
			Archived:  true,
		},
	}
	for _, sess := range sessions {
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s): %v", sess.ID, err)
		}
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?categories=chat,web", nil)
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Sessions []struct {
			ID       string                `json:"id"`
			Mode     session.SessionMode   `json:"mode"`
			Origin   session.SessionOrigin `json:"origin"`
			Archived bool                  `json:"archived"`
			Pinned   bool                  `json:"pinned"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("session count = %d, want 2", len(body.Sessions))
	}
	if body.Sessions[0].ID != "sess-tui" || !body.Sessions[0].Pinned {
		t.Fatalf("first session = %+v, want pinned sess-tui first", body.Sessions[0])
	}
	gotIDs := []string{body.Sessions[0].ID, body.Sessions[1].ID}
	sort.Strings(gotIDs)
	if strings.Join(gotIDs, ",") != "sess-tui,sess-web" {
		t.Fatalf("ids = %v, want [sess-tui sess-web]", gotIDs)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sessions?categories=web&include_archived=1", nil)
	rr = httptest.NewRecorder()
	srv.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("include_archived status = %d, want 200", rr.Code)
	}
	body = struct {
		Sessions []struct {
			ID       string                `json:"id"`
			Mode     session.SessionMode   `json:"mode"`
			Origin   session.SessionOrigin `json:"origin"`
			Archived bool                  `json:"archived"`
			Pinned   bool                  `json:"pinned"`
		} `json:"sessions"`
	}{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode include_archived: %v", err)
	}
	if len(body.Sessions) != 2 {
		t.Fatalf("include_archived session count = %d, want 2", len(body.Sessions))
	}
}

func TestHandleSessionByID_PatchRenameAndArchive(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-rename",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		Summary:   "hello world",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	srv := &serveServer{store: store}
	body := strings.NewReader(`{"name":"Renamed session","archived":true,"pinned":true}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess-rename", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}

	updated, err := store.Get(ctx, "sess-rename")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Name != "Renamed session" {
		t.Fatalf("Name = %q, want %q", updated.Name, "Renamed session")
	}
	if !updated.Archived {
		t.Fatal("Archived = false, want true")
	}
	if !updated.Pinned {
		t.Fatal("Pinned = false, want true")
	}

	mgr := newServeSessionManager(time.Minute, 10, nil)
	defer mgr.Close()
	cached := *updated
	rt := &serveRuntime{sessionMeta: &cached}
	putTestSession(mgr, sess.ID, rt)
	srv.sessionMgr = mgr

	// runOnce holds this mutex for the entire provider stream. Renaming is a
	// durable metadata write and must not wait for that stream to finish merely
	// to refresh the runtime's cache.
	rt.mu.Lock()
	streamingBody := strings.NewReader(`{"name":"Renamed while streaming"}`)
	streamingReq := httptest.NewRequest(http.MethodPatch, "/v1/sessions/sess-rename", streamingBody)
	streamingReq.Header.Set("Content-Type", "application/json")
	streamingRR := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handleSessionByID(streamingRR, streamingReq)
		close(done)
	}()

	select {
	case <-done:
		rt.mu.Unlock()
	case <-time.After(time.Second):
		rt.mu.Unlock()
		<-done
		t.Fatal("metadata PATCH waited for the active runtime lock")
	}
	if streamingRR.Code != http.StatusOK {
		t.Fatalf("streaming rename status = %d, want 200 body=%s", streamingRR.Code, streamingRR.Body.String())
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after streaming rename: %v", err)
	}
	if persisted.Name != "Renamed while streaming" {
		t.Fatalf("persisted Name = %q, want streaming rename", persisted.Name)
	}
	if cached.Name != "Renamed session" {
		t.Fatalf("busy runtime cache changed without its lock: %q", cached.Name)
	}
}

func TestHandleTranscribe_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("path = %q, want /audio/transcriptions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want Bearer test-key", got)
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		f, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer f.Close()
		if header.Filename == "" {
			t.Fatal("expected filename")
		}
		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(data) != "fake audio" {
			t.Fatalf("audio payload = %q, want fake audio", string(data))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "hello from audio"})
	}))
	defer upstream.Close()

	srv := &serveServer{cfgRef: &config.Config{
		Providers: map[string]config.ProviderConfig{
			"openai": {
				ResolvedAPIKey: "test-key",
				BaseURL:        upstream.URL,
			},
		},
	}}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "voice-note.webm")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte("fake audio")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()

	srv.handleTranscribe(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Text != "hello from audio" {
		t.Fatalf("text = %q, want %q", payload.Text, "hello from audio")
	}
}

func TestHandleTranscribe_RejectsUnsupportedType(t *testing.T) {
	srv := &serveServer{cfgRef: &config.Config{}}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="voice-note.bin"`},
		"Content-Type":        {"application/octet-stream"},
	})
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := fw.Write([]byte("nope")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()

	srv.handleTranscribe(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unsupported") {
		t.Fatalf("body = %q, want unsupported error", rr.Body.String())
	}
}
