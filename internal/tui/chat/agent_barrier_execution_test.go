package chat

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm/core"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat/agentbarrier"
	"github.com/samsaffron/term-llm/internal/ui"
)

// A blocked first failure gives the test a deterministic observation point;
// recovery releases persistence, never another provider invocation.
type barrierRetryStore struct {
	*session.SQLiteStore
	kind      string
	attempts  atomic.Int32
	failed    chan struct{}
	recover   chan struct{}
	onFailure func(context.Context) error
	armed     atomic.Bool
}

func (s *barrierRetryStore) fail(ctx context.Context) error {
	if s.attempts.Add(1) == 1 {
		if s.onFailure != nil {
			if err := s.onFailure(ctx); err != nil {
				return err
			}
		}
		close(s.failed)
		return errors.New("transient delivery write failure")
	}
	select {
	case <-s.recover:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *barrierRetryStore) MarkAgentRunNotified(ctx context.Context, id string, generation int64, at time.Time) (bool, error) {
	if s.kind == "ack" {
		if err := s.fail(ctx); err != nil {
			return false, err
		}
	}
	return s.SQLiteStore.MarkAgentRunNotified(ctx, id, generation, at)
}
func (s *barrierRetryStore) ListAgentRuns(ctx context.Context, parent string) ([]session.AgentRun, error) {
	if s.kind == "turn" && s.armed.Load() {
		if err := s.fail(ctx); err != nil {
			return nil, err
		}
	}
	return s.SQLiteStore.ListAgentRuns(ctx, parent)
}
func (s *barrierRetryStore) SuppressAgentEvents(ctx context.Context, events []session.AgentRun) error {
	if s.kind == "events" {
		if err := s.fail(ctx); err != nil {
			return err
		}
	}
	return s.SQLiteStore.SuppressAgentEvents(ctx, events)
}
func waitBarrierSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery write was not attempted")
	}
}
func assertBarrierLeaseActive(t *testing.T, store *session.SQLiteStore) {
	t.Helper()
	attention, err := store.ListAttention(t.Context(), session.AttentionListOptions{Kind: session.AttentionKindRunning})
	if err != nil || len(attention.Items) != 1 {
		t.Fatalf("settlement released its lease: %+v %v", attention, err)
	}
}

func TestAgentBarrierRetriesOnlyAcknowledgement(t *testing.T) {
	for _, invalidation := range []string{"none", "collected", "new_generation", "suppressed"} {
		t.Run(invalidation, func(t *testing.T) {
			m, store, manager, c := barrierFixture(t)
			row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
			other := putBarrierChild(t, store, m.SessionID(), "other", "completed")
			retry := &barrierRetryStore{SQLiteStore: store, kind: "ack", failed: make(chan struct{}), recover: make(chan struct{})}
			retry.onFailure = func(ctx context.Context) error {
				switch invalidation {
				case "collected":
					return store.CollectAgentRunGeneration(ctx, row.ID, row.RunGeneration, time.Now())
				case "new_generation":
					row.RunGeneration++
					return store.PutAgentRun(ctx, row)
				case "suppressed":
					return store.SuppressAgentEvents(ctx, []session.AgentRun{row})
				}
				return nil
			}
			m.store = retry
			runner := &barrierTestRunner{requests: make(chan runpkg.Request, 2)}
			m.SetRunner(runner)
			visible := newTestChatModel(false)
			driveBarrierCommands(t, visible, c.checkCmd(manager))
			receiveBarrierRequest(t, runner)
			waitBarrierSignal(t, retry.failed)
			if !manager.HasActive(m.SessionID()) {
				t.Fatal("ACK failure settled execution instead of retrying ACK")
			}
			assertBarrierLeaseActive(t, store)
			driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
			close(retry.recover)
			waitBarrierRun(t, manager, m.SessionID())
			if len(runner.requests) != 0 || manager.runs[m.SessionID()].err != nil {
				t.Fatal("ACK retry repeated inference or failed the successful turn")
			}
			got, err := store.GetAgentRun(t.Context(), other.ID)
			if err != nil || got.NotifiedAt.IsZero() {
				t.Fatalf("remaining offered event was not acknowledged: %+v %v", got, err)
			}
			pending, err := tools.PendingAgentBarrier(t.Context(), store, m.SessionID())
			want := 0
			if invalidation == "new_generation" {
				want = 1
			}
			if err != nil || len(pending) != want {
				t.Fatalf("concurrent invalidation stolen/lost: %+v %v", pending, err)
			}
		})
	}
}

type barrierStopRunner struct {
	requests chan context.Context
	childEnd chan bool
}

func (r *barrierStopRunner) Run(ctx context.Context, _ runpkg.Request, _ runpkg.EventSink) (runpkg.Result, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.requests <- ctx
	<-childCtx.Done()
	explicit, owned := agentbarrier.StopRequested(childCtx)
	r.childEnd <- explicit && owned
	return runpkg.Result{}, ctx.Err()
}

func TestAgentBarrierReattachedHumanStopAndGenericCancel(t *testing.T) {
	for _, human := range []bool{true, false} {
		t.Run(map[bool]string{true: "human", false: "generic"}[human], func(t *testing.T) {
			m, store, manager, c := barrierFixture(t)
			row := putBarrierChild(t, store, m.SessionID(), "owed", "completed")
			runner := &barrierStopRunner{requests: make(chan context.Context, 1), childEnd: make(chan bool, 1)}
			m.SetRunner(runner)
			driveBarrierCommands(t, newTestChatModel(false), c.checkCmd(manager))
			var runCtx context.Context
			select {
			case runCtx = <-runner.requests:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not start")
			}
			m.attachMainRun(m.SessionID())
			m.detachMainRun() // leave the session while its execution continues
			next := newTestChatModel(false)
			next.sess, next.store = m.sess, store
			next.SetMainRunManager(manager)
			next.attachMainRun(next.SessionID())
			if human {
				next.setStreamCancelRequested(true)
				next.streamCancelFunc()
			} else {
				manager.Cancel(m.SessionID())
			}
			waitBarrierRun(t, manager, m.SessionID())
			if got := <-runner.childEnd; got != human {
				t.Fatalf("descendant stop authority = %v, want %v", got, human)
			}
			if explicit, owned := agentbarrier.StopRequested(runCtx); explicit != human || !owned {
				t.Fatalf("execution lost human stop intent: explicit=%v owned=%v", explicit, owned)
			}
			got, err := store.GetAgentRun(t.Context(), row.ID)
			if err != nil || got.WakeSuppressed != human {
				t.Fatalf("owed event suppression = %+v %v, human=%v", got, err, human)
			}
		})
	}
}

func TestAgentBarrierRetriesHumanSuppressionWithoutInference(t *testing.T) {
	for _, kind := range []string{"turn", "events"} {
		t.Run(kind, func(t *testing.T) {
			m, store, manager, c := barrierFixture(t)
			row := putBarrierChild(t, store, m.SessionID(), "owed", "completed")
			retry := &barrierRetryStore{SQLiteStore: store, kind: kind, failed: make(chan struct{}), recover: make(chan struct{})}
			m.store = retry
			runner := &barrierStopRunner{requests: make(chan context.Context, 2), childEnd: make(chan bool, 1)}
			m.SetRunner(runner)
			visible := newTestChatModel(false)
			driveBarrierCommands(t, visible, c.checkCmd(manager))
			select {
			case <-runner.requests:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not start")
			}
			retry.armed.Store(true)
			manager.Stop(m.SessionID(), manager.Status(m.SessionID()).RunID)
			waitBarrierSignal(t, retry.failed)
			if !manager.HasActive(m.SessionID()) {
				t.Fatal("failed suppression discarded human stop intent")
			}
			assertBarrierLeaseActive(t, store)
			driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
			close(retry.recover)
			waitBarrierRun(t, manager, m.SessionID())
			driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
			got, err := store.GetAgentRun(t.Context(), row.ID)
			if err != nil || !got.WakeSuppressed || len(runner.requests) != 0 {
				t.Fatalf("suppression retried inference or lost event: %+v %v", got, err)
			}
		})
	}
}

func TestMainRunHumanStopCannotTargetReplacement(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	first, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-first.Done
	started := make(chan context.Context, 1)
	second, err := manager.Start("session", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		started <- ctx
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := <-started
	if manager.Stop("session", first.RunID) {
		t.Fatal("stale Stop accepted")
	}
	if stopped, _ := agentbarrier.StopRequested(ctx); stopped || ctx.Err() != nil {
		t.Fatal("stale Stop marked/cancelled replacement")
	}
	if !manager.Stop("session", second.RunID) {
		t.Fatal("current Stop rejected")
	}
	<-second.Done
}

func barrierNoticeCount(t *testing.T, store session.Store, sessionID string) int {
	t.Helper()
	rows, err := store.GetMessages(t.Context(), sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if core.HasGoalSteeringPart(row.Parts) {
			count++
		}
	}
	return count
}

func TestAgentBarrierUnadmittedPreparationDoesNotPersistNotice(t *testing.T) {
	for _, rejection := range []string{"stale_coordinator", "session_switch", "start_rejected"} {
		t.Run(rejection, func(t *testing.T) {
			m, store, manager, c := barrierFixture(t)
			row := putBarrierChild(t, store, m.SessionID(), "owed", "completed")
			result := c.prepareCmd(manager, []session.AgentRun{row})().(AgentBarrierMsg)
			if result.err != nil || result.prepared == nil {
				t.Fatalf("prepare: %v", result.err)
			}
			if count := barrierNoticeCount(t, store, m.SessionID()); count != 0 {
				t.Fatalf("speculative preparation persisted %d notices", count)
			}
			visible := newTestChatModel(false)
			switch rejection {
			case "stale_coordinator":
				next := newTestChatModel(false)
				next.sess, next.store = m.sess, store
				next.SetMainRunManager(manager)
				driveBarrierCommands(t, visible, func() tea.Msg { return result })
			case "session_switch":
				m.sessionSwitchPending = true
				driveBarrierCommands(t, visible, func() tea.Msg { return result })
			case "start_rejected":
				cmd := result.Update(visible)
				_, err := manager.Start(m.SessionID(), MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error { <-ctx.Done(); return ctx.Err() }})
				if err != nil {
					t.Fatal(err)
				}
				driveBarrierCommands(t, visible, cmd)
			}
			if count := barrierNoticeCount(t, store, m.SessionID()); count != 0 {
				t.Fatalf("rejected preparation persisted %d notices", count)
			}
		})
	}
}

func TestAgentBarrierAdmittedExecutionPersistsOneNoticeAndUpdatesView(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	putBarrierChild(t, store, m.SessionID(), "owed", "completed")
	inbox := make(chan tea.Msg, 2)
	detach := manager.AttachUISink(m.SessionID(), func(msg tea.Msg) { inbox <- msg })
	defer detach()
	release := make(chan struct{})
	defer close(release)
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1), release: release}
	m.SetRunner(runner)
	driveBarrierCommands(t, newTestChatModel(false), c.checkCmd(manager))
	req := receiveBarrierRequest(t, runner)
	if count := barrierNoticeCount(t, store, m.SessionID()); count != 1 {
		t.Fatalf("admitted delivery persisted %d notices", count)
	}
	notice := (<-inbox).(mainRunUIEnvelope).message.(agentBarrierNoticeMsg)
	m.Update(notice)
	m.Update(notice) // reattach may have loaded the same durable row already
	count := 0
	for _, row := range m.messages {
		if core.HasGoalSteeringPart(row.Parts) {
			count++
		}
	}
	if count != 1 || !core.HasGoalSteeringPart(req.Messages[len(req.Messages)-1].Parts) {
		t.Fatalf("view/request missing the single admitted notice: view=%d", count)
	}
	live, ok := manager.ActiveLiveContext(m.SessionID())
	if !ok || !core.HasGoalSteeringPart(live[len(live)-1].Parts) {
		t.Fatal("run boundary omitted admitted notice")
	}
}

func TestAgentBarrierLostLeaseSuppressionCannotSweepSuccessor(t *testing.T) {
	m, store, _, _ := barrierFixture(t)
	offered := putBarrierChild(t, store, m.SessionID(), "offered", "completed")
	owned := putBarrierChild(t, store, m.SessionID(), "owned", "completed")
	owned.ParentResponseID = "stopped-turn"
	if err := store.PutAgentRun(t.Context(), owned); err != nil {
		t.Fatal(err)
	}
	retry := &barrierRetryStore{SQLiteStore: store, kind: "events", failed: make(chan struct{}), recover: make(chan struct{})}
	retry.onFailure = func(ctx context.Context) error {
		next := offered
		next.RunGeneration++
		next.Status, next.ParentResponseID = "running", "successor-turn"
		if err := store.PutAgentRun(ctx, next); err != nil {
			return err
		}
		return store.PutAgentRun(ctx, session.AgentRun{ID: "new-child", ParentSessionID: m.SessionID(), ParentResponseID: "successor-turn", RunGeneration: 1, Status: "running"})
	}
	ctx := core.ContextWithSessionID(session.WithResponseRunFence(t.Context(), session.ResponseRunFence{ResponseID: "stopped-turn"}), m.SessionID())
	leaseCtx, loseLease := context.WithCancel(t.Context())
	loseLease()
	stopped := &atomic.Bool{}
	stopped.Store(true)
	done := make(chan error, 1)
	go func() {
		done <- agentbarrier.Complete(ctx, leaseCtx, t.Context(), retry, stopped, []session.AgentRun{offered}, context.Canceled)
	}()
	waitBarrierSignal(t, retry.failed)
	close(retry.recover)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("suppression: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("suppression did not recover")
	}
	for _, id := range []string{offered.ID, "new-child", owned.ID} {
		got, err := store.GetAgentRun(t.Context(), id)
		if err != nil || got.WakeSuppressed != (id == owned.ID) {
			t.Fatalf("suppression swept successor or lost intent: %+v %v", got, err)
		}
	}
}

type barrierGetErrorStore struct {
	session.Store
	err error
}

func (s barrierGetErrorStore) Get(context.Context, string) (*session.Session, error) {
	return nil, s.err
}
func TestAgentBarrierDeletedParentIsQuiet(t *testing.T) {
	outage := errors.New("store unavailable")
	for _, err := range []error{session.ErrNotFound, sql.ErrNoRows, outage} {
		events, gotErr := agentbarrier.Pending(t.Context(), barrierGetErrorStore{err: err}, "deleted")
		if len(events) != 0 || (err == outage && !errors.Is(gotErr, outage)) || (err != outage && gotErr != nil) {
			t.Fatalf("Pending(%v) = %v, %v", err, events, gotErr)
		}
	}
}
