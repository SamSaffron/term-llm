package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func newAgentWakeFixture(t *testing.T, provider *llm.MockProvider) (*serveServer, *session.SQLiteStore, *serveRuntime) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	parent := &session.Session{ID: "wake-parent", Provider: "debug", ProviderKey: "debug", Model: "fast", Origin: session.OriginWeb, Status: session.StatusActive}
	if err := store.Create(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	rt := &serveRuntime{provider: provider, providerKey: "debug", engine: llm.NewEngine(provider, registry), store: store, defaultModel: "fast", platform: "web", sessionMeta: parent}
	mgr := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) { return rt, nil })
	putTestSession(mgr, parent.ID, rt)
	srv := &serveServer{store: store, sessionMgr: mgr, responseRuns: newServeResponseRunManager(), cfgRef: &config.Config{Providers: map[string]config.ProviderConfig{"debug": {Model: "fast"}}}}
	t.Cleanup(func() { srv.agentWakeWG.Wait(); srv.responseRuns.Close(); mgr.Close() })
	return srv, store, rt
}

func putWakeRun(t *testing.T, store *session.SQLiteStore, status string) session.AgentRun {
	t.Helper()
	owner := "finished"
	if status == "running" {
		host, _ := os.Hostname()
		owner = host + ":99999999:1"
	}
	run := session.AgentRun{ID: "wake-child", ParentSessionID: "wake-parent", AgentName: "developer", Prompt: "work", Status: status, StopReason: status, NotifyWhenDone: true, NotifyOrigin: tools.QueueAgentOriginWeb, Output: "child evidence", OwnerInstanceID: owner, UpdatedAt: time.Now()}
	if err := store.PutAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestAgentCompletionWakesParentLoopAndExecutesFollowOnTool(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddToolCall("follow-on", "echo", map[string]any{"input": "continue workflow"}).AddTextResponse("synthesized result")
	srv, store, _ := newAgentWakeFixture(t, provider)
	putWakeRun(t, store, "completed")
	srv.wakeAgentParent("wake-parent")
	waitForServeCondition(t, 3*time.Second, func() bool {
		run, err := store.GetAgentRun(context.Background(), "wake-child")
		return err == nil && !run.NotifiedAt.IsZero()
	}, "parent follow-on and acknowledgment")
	srv.agentWakeWG.Wait()
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("parent model turns=%d, want tool and synthesis", len(requests))
	}
	foundEvent, foundUser := false, false
	for _, msg := range requests[0].Messages {
		if strings.Contains(llm.MessageText(msg), "child evidence") {
			foundEvent = msg.Role == llm.RoleDeveloper
			foundUser = msg.Role == llm.RoleUser
		}
	}
	if !foundEvent || foundUser {
		t.Fatalf("internal completion was not developer provenance: %#v", requests[0].Messages)
	}
	msgs, err := store.GetMessages(context.Background(), "wake-parent", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	toolSeen := false
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			if part.ToolCall != nil && part.ToolCall.Name == "echo" {
				toolSeen = true
			}
		}
	}
	if !toolSeen {
		t.Fatalf("parent did not execute follow-on tool: %#v", msgs)
	}
	srv.wakeAgentParent("wake-parent")
	srv.agentWakeWG.Wait()
	if provider.CurrentTurn() != 2 {
		t.Fatalf("duplicate parent continuation: turns=%d", provider.CurrentTurn())
	}
}

func TestAgentWakeCoalescesCompletionsBehindBusyParent(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddTurn(llm.MockTurn{Text: "initial answer", Delay: 80 * time.Millisecond}).AddTextResponse("combined results")
	srv, store, rt := newAgentWakeFixture(t, provider)
	initial, err := srv.startResponseRun(rt, true, false, []llm.Message{llm.UserText("initial task")}, llm.Request{SessionID: "wake-parent", Model: "fast"}, "wake-parent", startResponseRunOptions{uiSession: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForServeCondition(t, time.Second, func() bool { return srv.responseRuns.activeRunID("wake-parent") == initial.id }, "parent to own turn")
	first := putWakeRun(t, store, "completed")
	second := first
	second.ID = "wake-child-two"
	second.Output = "second child evidence"
	if err := store.PutAgentRun(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	srv.wakeAgentParent("wake-parent")
	srv.wakeAgentParent("wake-parent")
	waitForServeCondition(t, 3*time.Second, func() bool {
		one, e1 := store.GetAgentRun(context.Background(), first.ID)
		two, e2 := store.GetAgentRun(context.Background(), second.ID)
		return e1 == nil && e2 == nil && !one.NotifiedAt.IsZero() && !two.NotifiedAt.IsZero()
	}, "coalesced parent continuation")
	srv.agentWakeWG.Wait()
	if requests := provider.RecordedRequests(); len(requests) != 2 {
		t.Fatalf("parent turns=%d, want initial + one wake", len(requests))
	} else {
		text := llm.MessageText(requests[1].Messages[len(requests[1].Messages)-1])
		if !strings.Contains(text, first.ID) || !strings.Contains(text, second.ID) {
			t.Fatalf("wake did not coalesce children: %s", text)
		}
	}
}

func TestExplicitParentStopDistinguishesReloadFromHumanStop(t *testing.T) {
	run := newResponseRun("resp-test", "wake-parent", "", "fast", time.Now().Unix(), func() {})
	ctx := withResponseRunContext(context.Background(), run)
	if explicitParentStop(ctx) {
		t.Fatal("ordinary parent response looked explicitly stopped")
	}
	run.mu.Lock()
	run.cancelRequested = true
	run.mu.Unlock()
	if !explicitParentStop(ctx) {
		t.Fatal("human cancellation failed to suppress wake")
	}
	reloadCtx, cancel := context.WithCancelCause(ctx)
	cancel(restart.ErrInterrupt)
	if explicitParentStop(reloadCtx) {
		t.Fatal("reload suppressed recovery visibility")
	}
}

func TestAgentWakeFailureAndExplicitStopSuppression(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddTextResponse("I will report the failure")
	srv, store, _ := newAgentWakeFixture(t, provider)
	run := putWakeRun(t, store, "failed")
	run.Error = "child failed"
	if err := store.PutAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	srv.wakeAgentParent("wake-parent")
	waitForServeCondition(t, 3*time.Second, func() bool {
		row, err := store.GetAgentRun(context.Background(), run.ID)
		return err == nil && !row.NotifiedAt.IsZero()
	}, "failure event")
	srv.agentWakeWG.Wait()
	requests := provider.RecordedRequests()
	if len(requests) != 1 || !strings.Contains(llm.MessageText(requests[0].Messages[len(requests[0].Messages)-1]), "child failed") {
		t.Fatalf("failure context=%#v", requests)
	}
	other := run
	other.ID = "stopped-child"
	other.NotifiedAt = time.Time{}
	other.Output = "not delivered"
	if err := store.PutAgentRun(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := store.SuppressPendingAgentWakes(context.Background(), "wake-parent"); err != nil {
		t.Fatal(err)
	}
	// A late terminal write racing the stop must not reinstate the wake.
	other.StopReason = "completed"
	if err := store.PutAgentRun(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetAgentRun(context.Background(), other.ID)
	if err != nil || row.StopReason != "parent_stopped" {
		t.Fatalf("late completion bypassed stop: %+v %v", row, err)
	}
	srv.wakeAgentParent("wake-parent")
	srv.agentWakeWG.Wait()
	if provider.CurrentTurn() != 1 {
		t.Fatalf("explicit stop restarted parent: %d turns", provider.CurrentTurn())
	}
}

func TestAgentInterruptedAfterRestartWakesParentWithoutResumingChild(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddTextResponse("I will inspect before resuming")
	srv, store, _ := newAgentWakeFixture(t, provider)
	run := putWakeRun(t, store, "running")
	run.NotifyWhenDone = false
	if err := store.PutAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	srv.reconcileAgentWakes(context.Background())
	waitForServeCondition(t, 3*time.Second, func() bool {
		run, err := store.GetAgentRun(context.Background(), "wake-child")
		return err == nil && !run.NotifiedAt.IsZero()
	}, "restart interruption event")
	requests := provider.RecordedRequests()
	if len(requests) != 1 || !strings.Contains(llm.MessageText(requests[0].Messages[len(requests[0].Messages)-1]), `"reason":"host_restarted"`) {
		t.Fatalf("restart parent request=%#v", requests)
	}
	run, err := store.GetAgentRun(context.Background(), "wake-child")
	if err != nil || run.Status != "running" {
		t.Fatalf("child was automatically resumed/rewritten: %+v %v", run, err)
	}
}
