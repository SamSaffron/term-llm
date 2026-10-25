package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func newAgentWakeFixture(t *testing.T, provider *mockprovider.MockProvider) (*serveServer, *session.SQLiteStore, *serveRuntime) {
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
	registry := engine.NewToolRegistry()
	registry.Register(&echoTool{})
	rt := &serveRuntime{provider: provider, providerKey: "debug", engine: engine.NewEngine(provider, registry), store: store, defaultModel: "fast", platform: "web", sessionMeta: parent}
	mgr := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) { return rt, nil })
	putTestSession(mgr, parent.ID, rt)
	srv := &serveServer{cfg: serveServerConfig{ui: true}, store: store, sessionMgr: mgr, responseRuns: newServeResponseRunManager(), cfgRef: &config.Config{Providers: map[string]config.ProviderConfig{"debug": {Model: "fast"}}}}
	// These tests drive reconciliation explicitly; shutdown only bounds worker
	// cleanup, not a second periodic lifecycle scanner racing the assertions.
	srv.responseLifecycleOnce.Do(func() {})
	t.Cleanup(func() {
		if srv.shutdownCh != nil {
			select {
			case <-srv.shutdownCh:
			default:
				close(srv.shutdownCh)
			}
		}
		srv.agentWakeWG.Wait()
		srv.responseRuns.Close()
		mgr.Close()
	})
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
	provider := mockprovider.NewMockProvider("debug").AddToolCall("follow-on", "echo", map[string]any{"input": "continue workflow"}).AddTextResponse("synthesized result")
	srv, store, rt := newAgentWakeFixture(t, provider)
	rt.maxTurns = 7
	rt.search = true
	rt.forceExternalSearch = true
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
	if len(requests[0].Tools) == 0 || requests[0].MaxTurns != 7 || !requests[0].Search || !requests[0].ForceExternalSearch || !requests[0].ParallelToolCalls {
		t.Fatalf("wake dropped parent request settings: %+v", requests[0])
	}
	foundEvent := false
	for _, msg := range requests[0].Messages {
		if strings.Contains(core.MessageText(msg), "wake-child") {
			foundEvent = msg.Role == core.RoleUser
		}
		if strings.Contains(core.MessageText(msg), "child evidence") {
			t.Fatal("wake elevated untrusted child output")
		}
	}
	if !foundEvent {
		t.Fatalf("missing provider-visible internal event: %#v", requests[0].Messages)
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
	provider := mockprovider.NewMockProvider("debug").AddTurn(mockprovider.MockTurn{Text: "initial answer", Delay: 80 * time.Millisecond}).AddTextResponse("combined results")
	srv, store, rt := newAgentWakeFixture(t, provider)
	initial, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("initial task")}, core.Request{SessionID: "wake-parent", Model: "fast"}, "wake-parent", startResponseRunOptions{uiSession: true})
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
		text := core.MessageText(requests[1].Messages[len(requests[1].Messages)-1])
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
	provider := mockprovider.NewMockProvider("debug").AddTextResponse("I will report the failure")
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
	if len(requests) != 1 || !strings.Contains(core.MessageText(requests[0].Messages[len(requests[0].Messages)-1]), `"status":"failed"`) {
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
	if err != nil || !row.WakeSuppressed || row.StopReason != "completed" {
		t.Fatalf("late completion bypassed stop: %+v %v", row, err)
	}
	srv.wakeAgentParent("wake-parent")
	srv.agentWakeWG.Wait()
	if provider.CurrentTurn() != 1 {
		t.Fatalf("explicit stop restarted parent: %d turns", provider.CurrentTurn())
	}
}

func TestAgentInterruptedAfterRestartWakesParentWithoutResumingChild(t *testing.T) {
	provider := mockprovider.NewMockProvider("debug").AddTextResponse("I will inspect before resuming")
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
	if len(requests) != 1 || !strings.Contains(core.MessageText(requests[0].Messages[len(requests[0].Messages)-1]), `"reason":"host_restarted"`) {
		t.Fatalf("restart parent request=%#v", requests)
	}
	run, err := store.GetAgentRun(context.Background(), "wake-child")
	if err != nil || run.Status != "running" {
		t.Fatalf("child was automatically resumed/rewritten: %+v %v", run, err)
	}
}

func TestAgentBarrierWaitsForUnflaggedSiblingAndPreservesEagerCompatibility(t *testing.T) {
	for _, eager := range []bool{false, true} {
		t.Run(fmt.Sprint("eager=", eager), func(t *testing.T) {
			provider := mockprovider.NewMockProvider("debug").AddTextResponse("first lifecycle").AddTextResponse("second lifecycle")
			srv, store, _ := newAgentWakeFixture(t, provider)
			first := putWakeRun(t, store, "completed")
			first.NotifyWhenDone = eager
			if !eager {
				first.NotifyOrigin = ""
			}
			if err := store.PutAgentRun(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			second := first
			second.ID, second.Status, second.StopReason, second.NotifyWhenDone, second.OwnerInstanceID = "outstanding-sibling", "running", "", false, "remote-owner"
			if err := store.PutAgentRun(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			srv.wakeAgentParent(first.ParentSessionID)
			srv.agentWakeWG.Wait()
			want := 0
			if eager {
				want = 1
			}
			if provider.CurrentTurn() != want {
				t.Fatalf("outstanding sibling: turns=%d want=%d", provider.CurrentTurn(), want)
			}
			second.Status, second.StopReason = "completed", "completed"
			if err := store.PutAgentRun(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			// Periodic reconciliation, rather than a process-local callback,
			// observes this remote child's completion.
			srv.reconcileAgentWakes(context.Background())
			srv.agentWakeWG.Wait()
			if provider.CurrentTurn() != want+1 {
				t.Fatalf("final barrier: turns=%d", provider.CurrentTurn())
			}
			for _, id := range []string{first.ID, second.ID} {
				row, err := store.GetAgentRun(context.Background(), id)
				if err != nil || row.NotifiedAt.IsZero() {
					t.Fatalf("unacknowledged %s: %+v %v", id, row, err)
				}
			}
		})
	}
}

func TestAgentBarrierRetriesProviderFailureAndWakesAfterFailedParent(t *testing.T) {
	for _, initialHuman := range []bool{false, true} {
		t.Run(fmt.Sprint("human=", initialHuman), func(t *testing.T) {
			provider := mockprovider.NewMockProvider("debug").AddError(errors.New("injected provider failure")).AddTextResponse("recovered lifecycle")
			srv, store, rt := newAgentWakeFixture(t, provider)
			row := putWakeRun(t, store, "completed")
			row.NotifyWhenDone, row.NotifyOrigin = false, ""
			if err := store.PutAgentRun(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if initialHuman {
				run, err := srv.startResponseRun(rt, true, false, []core.Message{core.UserText("human task")}, core.Request{SessionID: row.ParentSessionID, Model: "fast"}, row.ParentSessionID, startResponseRunOptions{uiSession: true})
				if err != nil {
					t.Fatal(err)
				}
				<-run.settled
			} else {
				srv.wakeAgentParent(row.ParentSessionID)
			}
			waitForServeCondition(t, 5*time.Second, func() bool {
				got, err := store.GetAgentRun(context.Background(), row.ID)
				return err == nil && !got.NotifiedAt.IsZero()
			}, "provider retry / failed parent lifecycle")
			srv.agentWakeWG.Wait()
			if provider.CurrentTurn() != 2 {
				t.Fatalf("turns=%d", provider.CurrentTurn())
			}
		})
	}
}

type agentAckRetryStore struct {
	*session.SQLiteStore
	attempts atomic.Int32
}

func (s *agentAckRetryStore) MarkAgentRunNotified(ctx context.Context, id string, gen int64, at time.Time) (bool, error) {
	if s.attempts.Add(1) <= 2 {
		return false, errors.New("injected acknowledgment failure")
	}
	return s.SQLiteStore.MarkAgentRunNotified(ctx, id, gen, at)
}

func TestAgentWakeRetriesAcknowledgmentWithoutRepeatingInference(t *testing.T) {
	provider := mockprovider.NewMockProvider("debug").AddTextResponse("successful lifecycle")
	srv, store, _ := newAgentWakeFixture(t, provider)
	row := putWakeRun(t, store, "completed")
	flaky := &agentAckRetryStore{SQLiteStore: store}
	srv.store = flaky
	srv.wakeAgentParent(row.ParentSessionID)
	waitForServeCondition(t, 5*time.Second, func() bool {
		got, err := store.GetAgentRun(context.Background(), row.ID)
		return err == nil && !got.NotifiedAt.IsZero()
	}, "acknowledgment retry")
	srv.agentWakeWG.Wait()
	if provider.CurrentTurn() != 1 || flaky.attempts.Load() < 3 {
		t.Fatalf("inference=%d acknowledgments=%d", provider.CurrentTurn(), flaky.attempts.Load())
	}
}

func TestAgentWakeRoutesLoadedWebParentsNotNestedOrUnloadedTerminalSessions(t *testing.T) {
	for _, kind := range []string{"loaded-tui", "unloaded-tui", "nested"} {
		t.Run(kind, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("debug").AddTextResponse("web handles loaded parent")
			srv, store, _ := newAgentWakeFixture(t, provider)
			meta, err := store.Get(context.Background(), "wake-parent")
			if err != nil {
				t.Fatal(err)
			}
			meta.Origin, meta.IsSubagent = session.OriginTUI, kind == "nested"
			if kind == "nested" {
				if err := store.Create(context.Background(), &session.Session{ID: "owning-child-runner", Provider: "debug", Model: "fast"}); err != nil {
					t.Fatal(err)
				}
				meta.ParentID = "owning-child-runner"
			}
			if err := store.Update(context.Background(), meta); err != nil {
				t.Fatal(err)
			}
			if kind == "unloaded-tui" {
				srv.sessionMgr.mu.Lock()
				delete(srv.sessionMgr.sessions, meta.ID)
				srv.sessionMgr.mu.Unlock()
			}
			row := putWakeRun(t, store, "completed")
			row.NotifyWhenDone, row.NotifyOrigin = false, ""
			if err := store.PutAgentRun(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			srv.reconcileAgentWakes(context.Background())
			srv.agentWakeWG.Wait()
			want := 0
			if kind == "loaded-tui" {
				want = 1
			}
			if provider.CurrentTurn() != want {
				t.Fatalf("kind=%s turns=%d want=%d", kind, provider.CurrentTurn(), want)
			}
		})
	}
}
