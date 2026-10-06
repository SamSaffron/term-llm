package cmd

import (
	"context"
	"database/sql"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"sync"
	"testing"
	"time"
)

func TestAgentReviewWakePreservesRequestConfiguration(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddTextResponse("done")
	srv, store, rt := newAgentWakeFixture(t, provider)
	rt.maxTurns = 7
	rt.search = true
	rt.forceExternalSearch = true
	putWakeRun(t, store, "completed")
	srv.wakeAgentParent("wake-parent")
	srv.agentWakeWG.Wait()
	reqs := provider.RecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests=%d", len(reqs))
	}
	r := reqs[0]
	if len(r.Tools) == 0 || r.MaxTurns != 7 || !r.ForceExternalSearch {
		t.Fatalf("wake dropped runtime configuration: Tools=%v MaxTurns=%d Search=%v ForceExternalSearch=%v", r.Tools, r.MaxTurns, r.Search, r.ForceExternalSearch)
	}
}

type agentSnapshotStore struct {
	*session.SQLiteStore
	once          sync.Once
	read, release chan struct{}
}

func (s *agentSnapshotStore) ListPendingAgentRunsForParent(ctx context.Context, parent string) ([]session.AgentRun, error) {
	rows, err := s.SQLiteStore.ListPendingAgentRunsForParent(ctx, parent)
	s.once.Do(func() { close(s.read); <-s.release })
	return rows, err
}
func TestAgentReviewWakeRevalidatesStopOrCollection(t *testing.T) {
	for _, operation := range []string{"stop", "collect"} {
		t.Run(operation, func(t *testing.T) {
			provider := llm.NewMockProvider("debug").AddTextResponse("unwanted automatic turn")
			srv, store, _ := newAgentWakeFixture(t, provider)
			row := putWakeRun(t, store, "completed")
			blocked := &agentSnapshotStore{SQLiteStore: store, read: make(chan struct{}), release: make(chan struct{})}
			srv.store = blocked
			srv.wakeAgentParent("wake-parent")
			<-blocked.read
			if operation == "stop" {
				if err := store.SuppressPendingAgentWakes(context.Background(), "wake-parent"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := store.CollectAgentRun(context.Background(), row.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			close(blocked.release)
			srv.agentWakeWG.Wait()
			if provider.CurrentTurn() != 0 {
				t.Fatalf("%s committed before admission but parent still ran %d model turns", operation, provider.CurrentTurn())
			}
		})
	}
}

func TestAgentReviewRestartRetriesWakeAfterOldParentLeaseExpires(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddTextResponse("recovered child")
	srv, store, _ := newAgentWakeFixture(t, provider)
	srv.shutdownCh = make(chan struct{})
	// Drive the startup and periodic lifecycle bodies synchronously, without sleeps.
	srv.responseLifecycleOnce.Do(func() {})
	ctx := context.Background()
	_, err := store.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: "dead-parent-response", SessionID: "wake-parent", OwnerInstanceID: "dead-server", RunEpoch: 1, LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	putWakeRun(t, store, "running")
	srv.sweepAndRenewResponseLifecycle(ctx, store, srv.responseOwnerID())
	srv.reconcileAgentWakes(ctx)
	srv.agentWakeWG.Wait()
	if provider.CurrentTurn() != 0 {
		t.Fatal("fixture failed: old lease should block startup wake")
	}
	path, err := session.SessionInputStoreIdentity(store)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE serve_response_lifecycle SET lease_expires_at=? WHERE response_id='dead-parent-response'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	srv.sweepAndRenewResponseLifecycle(ctx, store, srv.responseOwnerID())
	srv.agentWakeWG.Wait()
	owner, err := store.ForeignTurnOwner(ctx, "wake-parent", srv.responseOwnerID())
	if err != nil || owner != "" {
		t.Fatalf("old owner not cleared: %s %v", owner, err)
	}
	row, err := store.GetAgentRun(ctx, "wake-child")
	if err != nil {
		t.Fatal(err)
	}
	if provider.CurrentTurn() != 1 || row.NotifiedAt.IsZero() {
		t.Fatalf("periodic recovery removed expired lease but stranded wake: turns=%d notified=%v", provider.CurrentTurn(), row.NotifiedAt)
	}
}

func TestAgentWakeDoesNotRecreateDeletedParentSession(t *testing.T) {
	provider := llm.NewMockProvider("debug").AddTextResponse("spontaneous reply")
	srv, store, rt := newAgentWakeFixture(t, provider)
	putWakeRun(t, store, "completed")
	if err := store.Delete(context.Background(), "wake-parent"); err != nil {
		t.Fatal(err)
	}
	rt.sessionMeta = nil // runtime built for a missing session
	rt.history = nil
	srv.reconcileAgentWakes(context.Background())
	srv.agentWakeWG.Wait()
	sess, err := store.Get(context.Background(), "wake-parent")
	t.Logf("after reconcile: provider turns=%d session exists=%v err=%v", provider.CurrentTurn(), sess != nil, err)
	if sess != nil {
		msgs, _ := store.GetMessages(context.Background(), "wake-parent", 0, 0)
		roles := []string{}
		for _, m := range msgs {
			roles = append(roles, string(m.Role))
		}
		t.Errorf("REPRO R2: deleted parent session recreated by agent wake (origin=%s agent=%q messages=%v)", sess.Origin, sess.Agent, roles)
	}
}

func TestAgentWakeAcknowledgmentSurvivesHostWaiterExit(t *testing.T) {
	provider := llm.NewMockProvider("debug").
		AddTurn(llm.MockTurn{Text: "handled child result", Delay: 250 * time.Millisecond}).
		AddTextResponse("handled the SAME child result again")
	srv, store, _ := newAgentWakeFixture(t, provider)
	srv.shutdownCh = make(chan struct{})
	srv.responseLifecycleOnce.Do(func() {})
	t.Cleanup(func() {
		if srv.responseLifecycleCancel != nil {
			srv.responseLifecycleCancel()
		}
	})
	putWakeRun(t, store, "completed")
	srv.wakeAgentParent("wake-parent")
	waitForServeCondition(t, 2*time.Second, func() bool { return srv.responseRuns.activeRunID("wake-parent") != "" }, "wake turn running")
	close(srv.shutdownCh) // handoff begins; admitting goroutine stops waiting
	srv.agentWakeWG.Wait()
	waitForServeCondition(t, 3*time.Second, func() bool {
		return provider.CurrentTurn() >= 1 && srv.responseRuns.activeRunID("wake-parent") == ""
	}, "first wake turn finished")
	row, _ := store.GetAgentRun(context.Background(), "wake-child")
	t.Logf("after first wake turn completed: notified_at set=%v", !row.NotifiedAt.IsZero())
	// Successor process (same DB) reconciles.
	srv.shutdownCh = nil
	srv.reconcileAgentWakes(context.Background())
	srv.agentWakeWG.Wait()
	waitForServeCondition(t, 3*time.Second, func() bool { return srv.responseRuns.activeRunID("wake-parent") == "" }, "settle")
	t.Logf("provider turns=%d", provider.CurrentTurn())
	if provider.CurrentTurn() == 2 {
		t.Errorf("REPRO R7: the same completion event was delivered to the parent loop twice")
	}
}

func TestAgentHostShutdownIsNotExplicitStop(t *testing.T) {
	mgr := newServeResponseRunManager()
	cancelled := make(chan struct{})
	run := newResponseRun("host-shutdown", "parent", "", "fast", time.Now().Unix(), func() { close(cancelled) })
	if err := mgr.create(run); err != nil {
		t.Fatal(err)
	}
	mgr.CloseContext(context.Background())
	<-cancelled
	if explicitParentStop(withResponseRunContext(context.Background(), run)) {
		t.Fatal("host shutdown became a user stop")
	}
}

func TestAgentResponseTimeoutLeavesDetachedChildrenOwned(t *testing.T) {
	provider := llm.NewMockProvider("debug")
	_, _, rt := newAgentWakeFixture(t, provider)
	run := newResponseRun("timeout", "parent", "", "fast", time.Now().Unix(), func() {})
	ctx, cancel := context.WithCancelCause(withResponseRunContext(context.Background(), run))
	cancel(errResponseRunTimeout)
	if await := rt.signalStoppedChildren(ctx, "parent"); await != nil {
		t.Fatal("parent inactivity timeout imposed a child deadline")
	}
}

func TestAgentWakeReloadCarriesDeliveryAndProvenance(t *testing.T) {
	saved := webReloadFixture(t, "wake-reload", "debug")
	provider := llm.NewMockProvider("debug")
	_, _, rt := newAgentWakeFixture(t, provider)
	run := restoreWebRun(&saved, func() {})
	events := []session.AgentRun{{ID: "child", RunGeneration: 3}}
	next, err := snapshotWebRun(run, saved.streamState(), saved.Engine, rt, true, startResponseRunOptions{agentCompletion: true, agentEvents: events})
	if err != nil {
		t.Fatal(err)
	}
	if !next.AgentCompletion || len(next.AgentEvents) != 1 || next.AgentEvents[0].RunGeneration != 3 {
		t.Fatalf("reload lost wake ownership: %+v", next)
	}
}

func TestStoppingHostWakeSuppressesItsOfferedGenerations(t *testing.T) {
	provider := llm.NewMockProvider("debug")
	_, store, rt := newAgentWakeFixture(t, provider)
	event := putWakeRun(t, store, "completed")
	event.ParentResponseID = "older-user-turn"
	if err := store.PutAgentRun(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	run := newResponseRun("host-wake", "wake-parent", "", "fast", time.Now().Unix(), func() {})
	run.cancelRequested = true
	run.agentEvents = []session.AgentRun{event}
	await := rt.signalStoppedChildren(withResponseRunContext(context.Background(), run), "wake-parent")
	if await != nil {
		await(context.Background())
	}
	pending, err := tools.PendingAgentEvents(context.Background(), store, "wake-parent")
	if err != nil || len(pending) != 0 {
		t.Fatalf("stopped host wake can reawaken: %+v %v", pending, err)
	}
}
