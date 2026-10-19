package chat

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm/core"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

type barrierAckCountStore struct {
	*session.SQLiteStore
	acks atomic.Int32
}

func (s *barrierAckCountStore) MarkAgentRunNotified(ctx context.Context, id string, generation int64, at time.Time) (bool, error) {
	s.acks.Add(1)
	return s.SQLiteStore.MarkAgentRunNotified(ctx, id, generation, at)
}

func TestAgentBarrierProviderRetryReusesNoticeButNotAnotherGeneration(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	counted := &barrierAckCountStore{SQLiteStore: store}
	m.store = counted
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 3), err: errors.New("provider unavailable")}
	m.SetRunner(runner)
	visible := newTestChatModel(false)
	driveBarrierCommands(t, visible, c.checkCmd(manager))
	receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	runner.err = nil
	c.retryAt = time.Time{}
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	req := receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	if counted.acks.Load() != 1 {
		t.Fatalf("ACK count=%d, want one successful delivery", counted.acks.Load())
	}
	assertNotices := func(want int) {
		t.Helper()
		messages, err := store.GetMessages(t.Context(), m.SessionID(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		stored, sent := 0, 0
		for _, msg := range messages {
			if msg.IsGoalSteering() {
				stored++
			}
		}
		for _, msg := range req.Messages {
			if strings.HasPrefix(core.MessageText(msg), "All subagents for this session have finished.") {
				sent++
			}
		}
		if stored != want || sent != want {
			t.Fatalf("notice count stored=%d sent=%d, want %d", stored, sent, want)
		}
	}
	assertNotices(1)
	row.RunGeneration++
	if err := store.PutAgentRun(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	c.retryAt = time.Time{}
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	req = receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	assertNotices(2)
	if counted.acks.Load() != 2 {
		t.Fatalf("new generation was not independently acknowledged: %d", counted.acks.Load())
	}
}

func TestMainRunStopBeforeRegistrationCancelsDetachedExecution(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	stopped := &atomic.Bool{}
	stopped.Store(true)
	if manager.Stop("parent", "pending-run") {
		t.Fatal("unregistered run unexpectedly found")
	}
	observed := make(chan error, 1)
	snapshot, err := manager.Start("parent", MainRunExecution{
		RunID: "pending-run", Context: t.Context(), UserStop: stopped,
		Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
			observed <- ctx.Err()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-snapshot.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("stopped execution did not settle")
	}
	if err := <-observed; !errors.Is(err, context.Canceled) {
		t.Fatalf("human stop was lost during detached admission: %v", err)
	}
}

type archivedBarrierReadStore struct{ *session.SQLiteStore }

func (s *archivedBarrierReadStore) Get(ctx context.Context, id string) (*session.Session, error) {
	row, err := s.SQLiteStore.Get(ctx, id)
	if row != nil {
		row.Archived = true
	}
	return row, err
}

func TestAgentBarrierArchiveBetweenDiscoveryAndPrepareIsQuiet(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	m.store = &archivedBarrierReadStore{SQLiteStore: store}
	result := c.prepareCmd(manager, []session.AgentRun{row})().(AgentBarrierMsg)
	if !errors.Is(result.err, session.ErrNotFound) {
		t.Fatalf("archived parent was classified as retryable: %v", result.err)
	}
	driveBarrierCommands(t, newTestChatModel(false), func() tea.Msg { return result })
	if c.failures != 0 || m.footerMessage != "" {
		t.Fatalf("archived parent produced failure notice: failures=%d footer=%q", c.failures, m.footerMessage)
	}
}

func TestAgentBarrierHumanStopBeforeCommandStartsStillSuppresses(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1)}
	m.SetRunner(runner)
	prepared := c.prepareCmd(manager, []session.AgentRun{row})().(AgentBarrierMsg)
	if prepared.err != nil {
		t.Fatal(prepared.err)
	}
	visible := newTestChatModel(false)
	command := prepared.Update(visible)
	m.setStreamCancelRequested(true)
	m.streamCancelFunc() // The view is active, but its worker has not registered yet.
	driveBarrierCommands(t, visible, command)
	waitBarrierRun(t, manager, m.SessionID())
	got, err := store.GetAgentRun(t.Context(), row.ID)
	if err != nil || !got.WakeSuppressed || len(runner.requests) != 0 {
		t.Fatalf("pre-admission stop lost intent or ran inference: %+v err=%v calls=%d", got, err, len(runner.requests))
	}
	messages, err := store.GetMessages(t.Context(), m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.IsGoalSteering() {
			t.Fatalf("stopped command persisted a notice: %+v", message)
		}
	}
}

type barrierStopAfterNoticeStore struct {
	*session.SQLiteStore
	stop func(context.Context)
}

func (s *barrierStopAfterNoticeStore) AddMessage(ctx context.Context, id string, row *session.Message) error {
	if err := s.SQLiteStore.AddMessage(ctx, id, row); err != nil {
		return err
	}
	if row.IsGoalSteering() {
		s.stop(ctx)
	}
	return nil
}

func TestAgentBarrierStopFlagAfterNoticeStillSkipsInference(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	var live atomic.Bool
	m.store = &barrierStopAfterNoticeStore{SQLiteStore: store, stop: func(ctx context.Context) {
		live.Store(ctx.Err() == nil)
		m.agentUserStop.Load().Store(true) // Stop is recorded, but run.cancel is still queued.
	}}
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1)}
	m.SetRunner(runner)
	driveBarrierCommands(t, newTestChatModel(false), c.checkCmd(manager))
	waitBarrierRun(t, manager, m.SessionID())
	got, err := store.GetAgentRun(t.Context(), row.ID)
	if !live.Load() || err != nil || !got.WakeSuppressed || len(runner.requests) != 0 {
		t.Fatalf("late stop ran inference or lost suppression: live=%t row=%+v err=%v calls=%d", live.Load(), got, err, len(runner.requests))
	}
}
