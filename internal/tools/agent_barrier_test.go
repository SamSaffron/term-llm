package tools

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

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func barrierStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPendingAgentBarrier(t *testing.T) {
	host, _ := os.Hostname()
	for _, tc := range []struct {
		name   string
		change func(*session.AgentRun)
		want   int
	}{
		{"unflagged_no_origin", func(*session.AgentRun) {}, 1},
		{"foreign_origin", func(r *session.AgentRun) { r.NotifyOrigin = "telegram" }, 1},
		{"collected", func(r *session.AgentRun) { r.CollectedAt = time.Now() }, 0},
		{"notified", func(r *session.AgentRun) { r.NotifiedAt = time.Now() }, 0},
		{"suppressed", func(r *session.AgentRun) { r.WakeSuppressed = true }, 0},
		{"parent_stopped", func(r *session.AgentRun) { r.StopReason = "parent_stopped" }, 0},
		{"unknown_owner", func(r *session.AgentRun) { r.Status = "running"; r.OwnerInstanceID = "remote-host:1:1" }, 0},
		{"dead_owner", func(r *session.AgentRun) { r.Status = "running"; r.OwnerInstanceID = host + ":99999999:1" }, 1},
		{"same_process_orphan", func(r *session.AgentRun) { r.Status = "queued"; r.OwnerInstanceID = processAgentOwner() }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := barrierStore(t)
			r := session.AgentRun{ID: session.NewID(), ParentSessionID: "barrier-parent", Status: "completed", RunGeneration: 2, UpdatedAt: time.Now()}
			tc.change(&r)
			if err := store.PutAgentRun(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			events, err := PendingAgentBarrier(context.Background(), store, r.ParentSessionID)
			if err != nil || len(events) != tc.want {
				t.Fatalf("events=%+v err=%v, want %d", events, err, tc.want)
			}
			if tc.want == 1 && r.Status != "completed" && events[0].StopReason != "host_restarted" {
				t.Fatalf("missing restart provenance: %+v", events)
			}
		})
	}
}

func TestPendingAgentBarrierCountsObservedOutstandingSiblings(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint("local=", local), func(t *testing.T) {
			store := barrierStore(t)
			parent := session.NewID()
			done := session.AgentRun{ID: session.NewID(), ParentSessionID: parent, Status: "completed", UpdatedAt: time.Now()}
			if err := store.PutAgentRun(context.Background(), done); err != nil {
				t.Fatal(err)
			}
			active := done
			active.ID, active.Status, active.OwnerInstanceID = session.NewID(), "awaiting_approval", "unknown-owner"
			// Observation/suppression must not remove an outstanding sibling from
			// barrier membership, even though it removes it from pending queries.
			active.CollectedAt, active.NotifiedAt, active.WakeSuppressed = time.Now(), time.Now(), true
			if local {
				m := newAgentManager(SpawnConfig{MaxParallel: 1})
				m.store = store // a failed initial write in this same durable scope
				e := &agentEntry{record: active, done: make(chan struct{}), manager: m}
				processAgentEntries.Store(active.ID, e)
				t.Cleanup(func() { processAgentEntries.Delete(active.ID) })
			} else if err := store.PutAgentRun(context.Background(), active); err != nil {
				t.Fatal(err)
			}
			events, err := PendingAgentBarrier(context.Background(), store, parent)
			if err != nil || events != nil {
				t.Fatalf("outstanding sibling ignored: %+v %v", events, err)
			}
		})
	}
}

func TestUnconditionalFinishSignalsAfterDoneAndWaitDoesNotDrain(t *testing.T) {
	store := barrierStore(t)
	runner := &lifecycleRunner{entered: make(chan string, 2), release: make(chan struct{})}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: runner, store: store})
	parent := session.NewID()
	ctx := core.ContextWithSessionID(context.Background(), parent)
	woke := make(chan error, 2)
	ctx = ContextWithAgentLifecycleWake(ctx, func(got string) {
		var wakeErr error
		processAgentEntries.Range(func(_, value any) bool {
			e := value.(*agentEntry)
			e.manager.mu.Lock()
			belongs := e.record.ParentSessionID == got
			e.manager.mu.Unlock()
			if belongs {
				select {
				case <-e.done:
				default:
					wakeErr = errors.New("wake before done")
				}
			}
			return true
		})
		woke <- wakeErr
	})
	first := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"first","wait":0}`))
	<-runner.entered
	close(runner.release)
	if err := tool.WaitForChildren(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := <-woke; err != nil {
		t.Fatal(err)
	}
	pending, err := PendingAgentBarrier(ctx, store, parent)
	if err != nil || len(pending) != 1 || pending[0].ID != first.AgentID {
		t.Fatalf("unconditional pending=%+v %v", pending, err)
	}
	// A second spawn succeeds; a draining wait would permanently forbid it.
	second := lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"second","wait":0}`)
	if second.IsError {
		t.Fatal(second.Content)
	}
	if err := tool.WaitForChildren(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := <-woke; err != nil {
		t.Fatal(err)
	}
}

type flakyTerminalStore struct {
	*session.SQLiteStore
	fail atomic.Bool
}

func (s *flakyTerminalStore) PutAgentRun(ctx context.Context, r session.AgentRun) error {
	if agentTerminal(r.Status) && s.fail.Load() {
		return errors.New("injected terminal write failure")
	}
	return s.SQLiteStore.PutAgentRun(ctx, r)
}

func TestAgentBarrierRetriesFailedTerminalPersistence(t *testing.T) {
	store := &flakyTerminalStore{SQLiteStore: barrierStore(t)}
	store.fail.Store(true)
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: runner, store: store})
	parent := session.NewID()
	ctx := core.ContextWithSessionID(context.Background(), parent)
	wake := make(chan struct{}, 1)
	ctx = ContextWithAgentLifecycleWake(ctx, func(string) { wake <- struct{}{} })
	result := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-runner.entered
	close(runner.release)
	<-wake
	if _, err := PendingAgentBarrier(ctx, store, parent); err == nil {
		t.Fatal("broken DB claimed durable delivery")
	}
	store.fail.Store(false)
	pending, err := PendingAgentBarrier(ctx, store, parent)
	if err != nil || len(pending) != 1 || pending[0].ID != result.AgentID || pending[0].Status != "completed" {
		t.Fatalf("retry=%+v %v", pending, err)
	}
}

func TestAgentLifecycleMessageMetadataAndRestrictions(t *testing.T) {
	msg := AgentLifecycleMessage([]session.AgentRun{{ID: "child", AgentName: "developer", Status: "completed", Output: "secret-output", Prompt: "secret-prompt", Error: "secret-error"}}, true)
	text := core.MessageText(msg)
	if msg.Role != core.RoleUser || !core.IsGoalSteeringMessage(msg) || !strings.Contains(text, "All subagents") || !strings.Contains(text, `"agent_name":"developer"`) {
		t.Fatalf("message=%+v", msg)
	}
	for _, secret := range []string{"secret-output", "secret-prompt", "secret-error"} {
		if strings.Contains(text, secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	ctx := ContextWithAgentLifecycleRestricted(core.ContextWithSessionID(context.Background(), "parent"))
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	for _, tool := range []core.Tool{spawn, &agentControlTool{name: ContinueAgentToolName, spawn: spawn}} {
		out := lifecycleCall(t, tool, ctx, `{}`)
		if !out.IsError || !strings.Contains(out.Content, "host lifecycle turn cannot") {
			t.Fatalf("restriction bypassed: %+v", out)
		}
	}
}

func TestHostLifecycleContinuationPolicy(t *testing.T) {
	for _, status := range []string{"completed", "turn_limit", "cancelled", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			store := barrierStore(t)
			r := session.AgentRun{ID: session.NewID(), ParentSessionID: "parent", Status: status, Started: true, UpdatedAt: time.Now()}
			if err := store.PutAgentRun(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
			tool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, store: store})
			ctx := ContextWithAgentRecoveryNotice(core.ContextWithSessionID(context.Background(), "parent"))
			out := lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: tool}, ctx, `{"agent_id":"`+r.ID+`","wait":1}`)
			allowed := status == "completed" || status == "turn_limit"
			if out.IsError == allowed {
				t.Fatalf("status=%s output=%+v", status, out)
			}
			if err := tool.WaitForChildren(ctx, "parent"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentBarrierWithoutStoreAcknowledgesOnlyOfferedGeneration(t *testing.T) {
	parent := session.NewID()
	runner := &lifecycleRunner{entered: make(chan string, 2), release: make(chan struct{})}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(runner)
	ctx := core.ContextWithSessionID(context.Background(), parent)
	first := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"local work","wait":0}`))
	t.Cleanup(func() { processAgentEntries.Delete(first.AgentID) })
	<-runner.entered
	if events, err := PendingAgentBarrier(ctx, nil, parent); err != nil || events != nil {
		t.Fatalf("active local barrier=%+v %v", events, err)
	}
	close(runner.release)
	if err := tool.WaitForChildren(ctx, parent); err != nil {
		t.Fatal(err)
	}
	events, err := PendingAgentBarrier(ctx, nil, parent)
	if err != nil || len(events) != 1 || events[0].ID != first.AgentID {
		t.Fatalf("local barrier=%+v %v", events, err)
	}
	if err := AcknowledgeAgentEvents(ctx, nil, events); err != nil {
		t.Fatal(err)
	}
	if pending, err := PendingAgentBarrier(ctx, nil, parent); err != nil || pending != nil {
		t.Fatalf("local ACK ineffective: %+v %v", pending, err)
	}
	// Notification does not collect or release output. A later continuation
	// creates a new independently deliverable generation.
	record, e, err := tool.manager.get(ctx, first.AgentID, parent)
	if err != nil || e == nil || record.Output != "done" || !record.CollectedAt.IsZero() {
		t.Fatalf("ACK discarded result: %+v %v", record, err)
	}
	entry, err := tool.manager.start(ctx, record.AgentName, record.Prompt, "", "", nil, nil, runner, 1, true, "follow up", record)
	if err != nil {
		t.Fatal(err)
	}
	<-entry.done
	if err := AcknowledgeAgentEvents(ctx, nil, events); err != nil {
		t.Fatal(err)
	}
	next, err := PendingAgentBarrier(ctx, nil, parent)
	if err != nil || len(next) != 1 || next[0].RunGeneration == events[0].RunGeneration {
		t.Fatalf("stale ACK stole generation: %+v %v", next, err)
	}
}

func TestModelCancelAgentDoesNotSuppressLifecycleEvent(t *testing.T) {
	parent := session.NewID()
	runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(runner)
	ctx := core.ContextWithSessionID(context.Background(), parent)
	wake := make(chan string, 1)
	ctx = ContextWithAgentLifecycleWake(ctx, func(parent string) { wake <- parent })
	first := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"local work","wait":0}`))
	t.Cleanup(func() { processAgentEntries.Delete(first.AgentID) })
	<-runner.entered
	out := lifecycleCall(t, &agentControlTool{name: CancelAgentToolName, spawn: tool}, ctx, `{"agent_id":"`+first.AgentID+`"}`)
	if out.IsError {
		t.Fatal(out.Content)
	}
	if got := <-wake; got != parent {
		t.Fatalf("wake=%q", got)
	}
	events, err := PendingAgentBarrier(ctx, nil, parent)
	if err != nil || len(events) != 1 || events[0].Status != "cancelled" || events[0].WakeSuppressed {
		t.Fatalf("model cancellation suppressed barrier: %+v %v", events, err)
	}
}

type observedAgentStore struct {
	*session.SQLiteStore
	read chan struct{}
}

func (s *observedAgentStore) ListAgentRuns(ctx context.Context, parent string) ([]session.AgentRun, error) {
	runs, err := s.SQLiteStore.ListAgentRuns(ctx, parent)
	select {
	case s.read <- struct{}{}:
	default:
	}
	return runs, err
}

func TestWaitForChildrenWaitsForRemoteOwnersAndHonorsCancellation(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelWait), func(t *testing.T) {
			store := &observedAgentStore{SQLiteStore: barrierStore(t), read: make(chan struct{}, 1)}
			parent := session.NewID()
			record := session.AgentRun{ID: session.NewID(), ParentSessionID: parent, Status: "running", OwnerInstanceID: "unknown-remote-owner", UpdatedAt: time.Now()}
			if err := store.PutAgentRun(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
			tool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, store: store})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- tool.WaitForChildren(ctx, parent) }()
			<-store.read
			select {
			case err := <-result:
				t.Fatalf("remote child returned early: %v", err)
			default:
			}
			if cancelWait {
				cancel()
				if err := <-result; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel=%v", err)
				}
			} else {
				record.Status = "completed"
				if err := store.PutAgentRun(ctx, record); err != nil {
					t.Fatal(err)
				}
				if err := <-result; err != nil {
					t.Fatalf("terminal remote wait=%v", err)
				}
			}
		})
	}
}

func TestHumanStopSuppressesAlreadyTerminalLocalEventWithoutDiscardingOutput(t *testing.T) {
	parent := session.NewID()
	m := newAgentManager(SpawnConfig{MaxParallel: 1})
	e := &agentEntry{record: session.AgentRun{ID: session.NewID(), ParentSessionID: parent, Status: "completed", Output: "collectable result", RunGeneration: 1}, done: make(chan struct{}), manager: m}
	close(e.done)
	processAgentEntries.Store(e.record.ID, e)
	t.Cleanup(func() { processAgentEntries.Delete(e.record.ID) })
	StopAgentsForParent(parent)(context.Background())
	events, err := PendingAgentBarrier(context.Background(), nil, parent)
	if err != nil || events != nil {
		t.Fatalf("human stop did not suppress local completion: %+v %v", events, err)
	}
	record, _, err := m.get(context.Background(), e.record.ID, parent)
	if err != nil || record.Output != "collectable result" || !record.CollectedAt.IsZero() {
		t.Fatalf("stop discarded result: %+v %v", record, err)
	}
}

func TestSpawnBindsTrustedParentResponseAcrossHostOrigins(t *testing.T) {
	for _, origin := range []string{"web", "tui", "agent"} {
		t.Run(origin, func(t *testing.T) {
			store := barrierStore(t)
			parent := session.NewID()
			runner := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
			tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
			tool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: runner, store: store})
			ctx := core.ContextWithSessionID(context.Background(), parent)
			ctx = ContextWithQueueAgentOrigin(ctx, QueueAgentOriginContext{Origin: origin, SessionID: parent, ResponseID: "human-parent-turn"})
			first := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
			<-runner.entered
			record, err := store.GetAgentRun(ctx, first.AgentID)
			close(runner.release)
			if err := tool.WaitForChildren(ctx, parent); err != nil {
				t.Fatal(err)
			}
			if err != nil || record.ParentResponseID != "human-parent-turn" {
				t.Fatalf("host origin=%s binding=%+v %v", origin, record, err)
			}
			// A completion racing a human stop can now be suppressed by the
			// same turn-scoped storage operation used by every host surface.
			if err := store.SuppressAgentWakesForTurn(ctx, parent, "human-parent-turn"); err != nil {
				t.Fatal(err)
			}
			pending, err := PendingAgentBarrier(ctx, store, parent)
			if err != nil || pending != nil {
				t.Fatalf("stopped originating turn still pending: %+v %v", pending, err)
			}
		})
	}
}

func TestPendingAgentBarrierDoesNotImportOtherStoreChildrenWithReusedParent(t *testing.T) {
	for _, otherDurable := range []bool{false, true} {
		for _, status := range []string{"running", "interrupted"} {
			t.Run(fmt.Sprintf("otherDurable=%v/%s", otherDurable, status), func(t *testing.T) {
				store := barrierStore(t)
				parent := "reused-parent"
				own := session.AgentRun{ID: session.NewID(), ParentSessionID: parent, Status: "completed", RunGeneration: 1, UpdatedAt: time.Now()}
				if err := store.PutAgentRun(context.Background(), own); err != nil {
					t.Fatal(err)
				}
				m := newAgentManager(SpawnConfig{MaxParallel: 1})
				if otherDurable {
					m.store = barrierStore(t)
				}
				foreign := own
				foreign.ID, foreign.Status, foreign.StopReason = session.NewID(), status, "host_restarted"
				e := &agentEntry{record: foreign, done: make(chan struct{}), manager: m}
				if agentTerminal(status) {
					close(e.done)
				}
				processAgentEntries.Store(foreign.ID, e)
				t.Cleanup(func() { processAgentEntries.Delete(foreign.ID) })
				events, err := PendingAgentBarrier(context.Background(), store, parent)
				if err != nil || len(events) != 1 || events[0].ID != own.ID {
					t.Fatalf("cross-store contamination: %+v %v", events, err)
				}
				rows, err := store.ListAgentRuns(context.Background(), parent)
				if err != nil || len(rows) != 1 {
					t.Fatalf("foreign row imported into DB: %+v %v", rows, err)
				}
			})
		}
	}
}

func TestLifecycleResultHintsRespectContinuationPermissions(t *testing.T) {
	for _, mode := range []string{"human", "recovery", "restricted"} {
		for _, status := range []string{"completed", "turn_limit", "interrupted", "cancelled"} {
			t.Run(mode+"/"+status, func(t *testing.T) {
				ctx := t.Context()
				switch mode {
				case "recovery":
					ctx = ContextWithAgentRecoveryNotice(ctx)
				case "restricted":
					ctx = ContextWithAgentLifecycleRestricted(ctx)
				}
				m := newAgentManager(SpawnConfig{MaxParallel: 1})
				out := m.deliver(ctx, session.AgentRun{ID: "child", Status: status}, nil)
				result := lifecycleResult(t, out)
				allowed := mode == "human" || (mode == "recovery" && (status == "completed" || status == "turn_limit"))
				if result.Resumable != allowed {
					t.Fatalf("resumable=%t, want %t: %s", result.Resumable, allowed, out.Content)
				}
				if !allowed && strings.Contains(result.Next, "continue_agent") {
					t.Fatalf("result recommends a forbidden tool: %s", out.Content)
				}
			})
		}
	}
}
