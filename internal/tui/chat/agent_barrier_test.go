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
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat/agentbarrier"
	"github.com/samsaffron/term-llm/internal/ui"
)

type barrierTestRunner struct {
	requests chan runpkg.Request
	release  <-chan struct{}
	err      error
}

func (r *barrierTestRunner) Run(ctx context.Context, req runpkg.Request, _ runpkg.EventSink) (runpkg.Result, error) {
	r.requests <- req
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return runpkg.Result{}, ctx.Err()
		}
	}
	return runpkg.Result{}, r.err
}

func barrierFixture(t *testing.T) (*Model, *session.SQLiteStore, *MainRunManager, *agentBarrierCoordinator) {
	t.Helper()
	m, store := newTurnLeaseTestModel(t)
	m.providerKey, m.modelName = m.sess.ProviderKey, m.sess.Model
	m.sideProviderFactory = func(_ string, _ string) (core.Provider, error) { return mockprovider.NewMockProvider("mock"), nil }
	manager := NewMainRunManager(t.Context())
	t.Cleanup(func() { manager.Close(time.Second) })
	m.SetMainRunManager(manager)
	return m, store, manager, manager.agentBarriers[m.SessionID()]
}

func putBarrierChild(t *testing.T, store session.AgentRunDeliveryStore, parent, id, status string) session.AgentRun {
	t.Helper()
	row := session.AgentRun{ID: id, ParentSessionID: parent, AgentName: "developer", RunGeneration: 1, Status: status,
		StopReason: status, OwnerInstanceID: "remote-live-owner", UpdatedAt: time.Now(), Output: "SECRET CHILD OUTPUT", Prompt: "SECRET CHILD PROMPT"}
	if err := store.PutAgentRun(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return row
}

// Run host commands without a real terminal. Passive timers and presentation
// messages are intentionally not executed; every state mutation is still routed
// through the same AgentBarrierMsg.Update entry used by ProgramModel.
func driveBarrierCommands(t *testing.T, visible *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, nested := range msg {
			driveBarrierCommands(t, visible, nested)
		}
	case AgentBarrierMsg:
		driveBarrierCommands(t, visible, msg.Update(visible))
	}
}
func waitBarrierRun(t *testing.T, manager *MainRunManager, id string) {
	t.Helper()
	manager.mu.RLock()
	run := manager.runs[id]
	manager.mu.RUnlock()
	if run == nil {
		t.Fatal("no main run")
	}
	select {
	case <-run.done:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not settle")
	}
}
func receiveBarrierRequest(t *testing.T, r *barrierTestRunner) runpkg.Request {
	t.Helper()
	select {
	case req := <-r.requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("no wake request")
		return runpkg.Request{}
	}
}

func TestAgentBarrierBackgroundWakePreservesDraftAndOffersAllOrigins(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	first := putBarrierChild(t, store, m.SessionID(), "unflagged", "completed")
	second := putBarrierChild(t, store, m.SessionID(), "other-origin", "completed")
	second.NotifyOrigin = "web"
	if err := store.PutAgentRun(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	m.setTextareaValue("unfinished composer draft")
	m.files = []FileAttachment{{Path: "draft.go"}}
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1), release: release}
	m.SetRunner(runner)
	visible := newTestChatModel(false)
	visible.setTextareaValue("different session draft")
	// Detached presentation may still say streaming after the manager settled.
	m.streaming = true
	driveBarrierCommands(t, visible, c.checkCmd(manager))
	req := receiveBarrierRequest(t, runner)
	notice := req.Messages[len(req.Messages)-1]
	if notice.Role != core.RoleUser || notice.ClientMessageID != "" {
		t.Fatalf("not a host synthetic message: %#v", notice)
	}
	stored, err := store.GetMessages(t.Context(), m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	synthetic := stored[len(stored)-1]
	if !core.HasGoalSteeringPart(synthetic.Parts) || synthetic.TextContent != "" || synthetic.ClientMessageID != "" {
		t.Fatalf("missing persisted synthetic provenance: %+v", synthetic)
	}
	text := core.MessageText(notice)
	for _, want := range []string{first.ID, second.ID, "All subagents"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q: %s", want, text)
		}
	}
	for _, secret := range []string{"SECRET CHILD OUTPUT", "SECRET CHILD PROMPT", "unfinished composer draft", "different session draft"} {
		if strings.Contains(text, secret) {
			t.Errorf("notice leaked %q", secret)
		}
	}
	if m.textarea.Value() != "unfinished composer draft" || len(m.files) != 1 || visible.textarea.Value() != "different session draft" {
		t.Fatal("wake consumed a composer draft or attachment")
	}
	for _, id := range []string{first.ID, second.ID} {
		row, err := store.GetAgentRun(t.Context(), id)
		if err != nil || !row.NotifiedAt.IsZero() {
			t.Fatalf("ack before execution: %+v %v", row, err)
		}
	}
	// Completion below is synchronized by the manager's done channel.
	close(release)
	waitBarrierRun(t, manager, m.SessionID())
	for _, id := range []string{first.ID, second.ID} {
		row, err := store.GetAgentRun(t.Context(), id)
		if err != nil || row.NotifiedAt.IsZero() {
			t.Fatalf("successful wake unacknowledged: %+v %v", row, err)
		}
	}
}

func TestAgentBarrierWaitsForLastSiblingAndParentFinalize(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	putBarrierChild(t, store, m.SessionID(), "done", "completed")
	sibling := putBarrierChild(t, store, m.SessionID(), "running", "running")
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1)}
	m.SetRunner(runner)
	visible := newTestChatModel(false)
	driveBarrierCommands(t, visible, c.checkCmd(manager))
	if manager.HasActive(m.SessionID()) {
		t.Fatal("wake while sibling outstanding")
	}
	sibling.Status = "completed"
	sibling.StopReason = "completed"
	if err := store.PutAgentRun(t.Context(), sibling); err != nil {
		t.Fatal(err)
	}
	entered, finalize := make(chan struct{}), make(chan struct{})
	_, err := manager.Start(m.SessionID(), MainRunExecution{Execute: func(_ context.Context, emit func(ui.StreamEvent)) error { emit(ui.DoneEvent(0)); return nil }, Finalize: func(error) { close(entered); <-finalize }})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	m.streaming = false // StreamDone can precede Finalize; it must not admit a wake.
	c.retryAt = time.Time{}
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	select {
	case <-runner.requests:
		t.Fatal("wake admitted before Finalize")
	default:
	}
	close(finalize)
	waitBarrierRun(t, manager, m.SessionID())
	c.retryAt = time.Time{}
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
}

func TestAgentBarrierReloadsStaleTranscriptAndSettingsBeforeRetry(t *testing.T) {
	m, store, path := newTurnLeaseTestModelWithHistory(t, true)
	m.providerKey, m.modelName = m.sess.ProviderKey, m.sess.Model
	m.sideProviderFactory = func(_, _ string) (core.Provider, error) { return mockprovider.NewMockProvider("mock"), nil }
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	m.SetMainRunManager(manager)
	c := manager.agentBarriers[m.SessionID()]
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	other := otherProcessStore(t, path)
	changed, err := other.Get(t.Context(), m.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	changed.Model = "remote-model"
	changed.Search = true
	changed.Tools = "read_file"
	if err := other.Update(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	if err := other.AddMessage(t.Context(), changed.ID, session.NewMessage(changed.ID, core.UserText("remote user turn"), -1)); err != nil {
		t.Fatal(err)
	}
	m.setTextareaValue("local draft")
	prepared := c.prepareCmd(manager, []session.AgentRun{row})().(AgentBarrierMsg)
	if !errors.Is(prepared.err, errTranscriptStale) {
		t.Fatalf("expected stale admission, got %v", prepared.err)
	}
	if m.modelName == "remote-model" {
		t.Fatal("worker mutated model settings before Update")
	}
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1)}
	m.SetRunner(runner)
	visible := newTestChatModel(false)
	driveBarrierCommands(t, visible, func() tea.Msg { return prepared })
	req := receiveBarrierRequest(t, runner)
	if req.Model != "remote-model" || m.modelName != "remote-model" || !m.searchEnabled || m.toolsStr != "read_file" {
		t.Fatalf("stale settings used: model=%s search=%v tools=%s", req.Model, m.searchEnabled, m.toolsStr)
	}
	found := false
	for _, msg := range req.Messages {
		if strings.Contains(core.MessageText(msg), "remote user turn") {
			found = true
		}
	}
	if !found || m.textarea.Value() != "local draft" {
		t.Fatal("stale transcript used or draft lost")
	}
	waitBarrierRun(t, manager, m.SessionID())
}

func TestAgentBarrierAdmissionCollectionRaceIsQuiet(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	if err := store.CollectAgentRunGeneration(t.Context(), row.ID, row.RunGeneration, time.Now()); err != nil {
		t.Fatal(err)
	}
	result := c.prepareCmd(manager, []session.AgentRun{row})().(AgentBarrierMsg)
	if !errors.Is(result.err, session.ErrSessionTurnOwned) {
		t.Fatalf("admitted invalidated offer: %v", result.err)
	}
	driveBarrierCommands(t, newTestChatModel(false), func() tea.Msg { return result })
	if c.failures != 0 || strings.Contains(m.footerMessage, "retry") {
		t.Fatalf("benign invalidation produced failure: %q", m.footerMessage)
	}
	if manager.HasActive(m.SessionID()) {
		t.Fatal("invalidated offer started provider")
	}
}

func TestAgentBarrierFailureBackoffAndReconciliationWithoutSignal(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	row := putBarrierChild(t, store, m.SessionID(), "child", "completed")
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 2), err: errors.New("provider unavailable")}
	m.SetRunner(runner)
	visible := newTestChatModel(false)
	driveBarrierCommands(t, visible, c.checkCmd(manager))
	receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	if c.failures != 1 || !c.retryAt.After(time.Now()) || !strings.Contains(m.footerMessage, "results are waiting") {
		t.Fatalf("no visible retry backoff: failures=%d footer=%q", c.failures, m.footerMessage)
	}
	pending, err := tools.PendingAgentBarrier(t.Context(), store, m.SessionID())
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed delivery lost generation: %+v %v", pending, err)
	}
	// No Program or completion callback is installed: periodic state reconciliation
	// alone must find the still-pending generation and retry after the backoff.
	runner.err = nil
	c.retryAt = time.Time{}
	driveBarrierCommands(t, visible, func() tea.Msg { return AgentBarrierMsg{manager: manager} })
	receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	got, err := store.GetAgentRun(t.Context(), row.ID)
	if err != nil || got.NotifiedAt.IsZero() {
		t.Fatalf("retry did not acknowledge: %+v %v", got, err)
	}
}

func TestAgentBarrierExplicitStopIsTurnScopedAndModelCancellationDoesNotSuppress(t *testing.T) {
	m, store, _, _ := barrierFixture(t)
	older := putBarrierChild(t, store, m.SessionID(), "older", "completed")
	current := putBarrierChild(t, store, m.SessionID(), "current", "completed")
	current.ParentResponseID = "current-turn"
	if err := store.PutAgentRun(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	ctx := core.ContextWithSessionID(session.WithResponseRunFence(t.Context(), session.ResponseRunFence{ResponseID: "current-turn"}), m.SessionID())
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	stopped := &atomic.Bool{}
	if err := agentbarrier.Complete(cancelled, context.WithoutCancel(cancelled), context.WithoutCancel(cancelled), store, stopped, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	got, _ := store.GetAgentRun(t.Context(), current.ID)
	if got.WakeSuppressed {
		t.Fatal("model cancellation suppressed failsafe")
	}
	stopped.Store(true)
	if err := agentbarrier.Complete(cancelled, context.WithoutCancel(cancelled), context.WithoutCancel(cancelled), store, stopped, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("explicit stop = %v", err)
	}
	got, _ = store.GetAgentRun(t.Context(), current.ID)
	if !got.WakeSuppressed {
		t.Fatal("current turn completion was not suppressed")
	}
	got, _ = store.GetAgentRun(t.Context(), older.ID)
	if got.WakeSuppressed {
		t.Fatal("stop suppressed an older unrelated completion")
	}
	stoppedCtx := agentbarrier.ContextWithStop(ctx, stopped)
	m.setStreamCancelRequested(false)
	if explicit, owned := agentbarrier.StopRequested(stoppedCtx); !explicit || !owned {
		t.Fatal("terminal cleanup erased explicit stop provenance")
	}
}

func TestAgentBarrierPreservesApprovalAndDraft(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	putBarrierChild(t, store, m.SessionID(), "child", "completed")
	m.setTextareaValue("draft")
	answers := make(chan tools.ApprovalResult, 1)
	m.Update(ApprovalRequestMsg{Path: "file.go", DoneCh: answers})
	driveBarrierCommands(t, m, c.checkCmd(manager))
	if m.approvalModel == nil || m.textarea.Value() != "draft" || manager.HasActive(m.SessionID()) {
		t.Fatal("wake disturbed approval or draft")
	}
	select {
	case <-answers:
		t.Fatal("wake resolved approval without human input")
	default:
	}
}

func TestAgentBarrierResourcesSurviveIdleNavigation(t *testing.T) {
	m, store, manager, _ := barrierFixture(t)
	putBarrierChild(t, store, m.SessionID(), "outstanding", "running")
	cleaned := false
	if !manager.AdoptAgentWakeResources(m.SessionID(), func() { cleaned = true }) {
		t.Fatal("idle session resources were not retained")
	}
	if cleaned {
		t.Fatal("navigation cleaned resources needed by background wake")
	}
	manager.Close(time.Second)
	if !cleaned {
		t.Fatal("host close leaked adopted resources")
	}
	if cmd := (AgentBarrierMsg{manager: manager, periodic: true}).Update(newTestChatModel(false)); cmd != nil {
		t.Fatal("closed host scheduled retry")
	}
}

type barrierAckOrderStore struct {
	*session.SQLiteStore
	acked             atomic.Bool
	leaseActive       chan bool
	finalizedAfterAck chan bool
}

func (s *barrierAckOrderStore) MarkAgentRunNotified(ctx context.Context, id string, generation int64, at time.Time) (bool, error) {
	running, err := s.SQLiteStore.ListAttention(ctx, session.AttentionListOptions{Kind: session.AttentionKindRunning})
	if err != nil {
		return false, err
	}
	s.leaseActive <- len(running.Items) > 0
	marked, err := s.SQLiteStore.MarkAgentRunNotified(ctx, id, generation, at)
	if err == nil {
		s.acked.Store(true)
	}
	return marked, err
}
func (s *barrierAckOrderStore) FinalizeResponseRun(ctx context.Context, terminal session.ResponseRunTerminal) (session.AttentionState, error) {
	s.finalizedAfterAck <- s.acked.Load()
	return s.SQLiteStore.FinalizeResponseRun(ctx, terminal)
}
func TestAgentBarrierAcknowledgesBeforeLeaseFinalization(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	putBarrierChild(t, store, m.SessionID(), "child", "completed")
	ordered := &barrierAckOrderStore{SQLiteStore: store, leaseActive: make(chan bool, 1), finalizedAfterAck: make(chan bool, 1)}
	m.store = ordered
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1)}
	m.SetRunner(runner)
	driveBarrierCommands(t, newTestChatModel(false), c.checkCmd(manager))
	receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	if !<-ordered.leaseActive {
		t.Fatal("wake was acknowledged after releasing lease")
	}
	if !<-ordered.finalizedAfterAck {
		t.Fatal("lease finalized before acknowledgment")
	}
}

func TestAgentBarrierFreshSessionUsesReplacementRuntime(t *testing.T) {
	for _, command := range []string{"new", "clear"} {
		t.Run(command, func(t *testing.T) {
			m, store, manager, c := barrierFixture(t)
			old := m.SessionID()
			putBarrierChild(t, store, old, "child", "completed")
			requested := make(chan SessionSwitchRequest, 1)
			m.SetSessionSwitcher(func(req SessionSwitchRequest) (*Model, error) {
				requested <- req
				return nil, errors.New("test stops before replacement")
			})
			var cmd tea.Cmd
			if command == "new" {
				_, cmd = m.cmdNew()
			} else {
				_, cmd = m.cmdClear()
			}
			if cmd == nil || m.SessionID() != old || c.model.SessionID() != old {
				t.Fatal("fresh session mutated old coordinator runtime")
			}
			if manager.agentBarriers[old] != c {
				t.Fatal("old session wake coordinator lost")
			}
		})
	}
}

func TestAgentBarrierForegroundMarkerOnBackgroundReattach(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	putBarrierChild(t, store, m.SessionID(), "child", "completed")
	runner := &barrierTestRunner{requests: make(chan runpkg.Request, 1)}
	m.SetRunner(runner)
	driveBarrierCommands(t, newTestChatModel(false), c.checkCmd(manager))
	receiveBarrierRequest(t, runner)
	waitBarrierRun(t, manager, m.SessionID())
	// Returning to the session creates a fresh view model; neither the hidden
	// synthetic row nor the detached model's footer is sufficient by itself.
	next := newTestChatModel(false)
	next.sess = m.sess
	next.store = store
	next.SetMainRunManager(manager)
	next.attachMainRun(next.SessionID())
	if next.footerMessage != agentBarrierNotice {
		t.Fatalf("missing host wake marker after navigation: %q", next.footerMessage)
	}
}

func TestAgentBarrierDoesNotRetainEmptyDetachedSession(t *testing.T) {
	m, _, manager, c := barrierFixture(t)
	if manager.AdoptAgentWakeResources(m.SessionID(), func() { t.Error("manager adopted empty runtime") }) {
		t.Fatal("empty runtime was retained")
	}
	if manager.agentBarriers[m.SessionID()] != nil {
		t.Fatal("disposed coordinator would poll closed store")
	}
	// A command already querying the old model cannot resurrect it after cleanup.
	result := AgentBarrierMsg{manager: manager, coordinator: c, checked: true}
	if cmd := result.Update(newTestChatModel(false)); cmd != nil {
		t.Fatal("disposed coordinator scheduled another check")
	}
}

func TestAgentBarrierAdmissionRejectsNewOutstandingSibling(t *testing.T) {
	m, store, manager, c := barrierFixture(t)
	offered := putBarrierChild(t, store, m.SessionID(), "finished", "completed")
	// The sibling was not present when the host assembled the offer. Only the
	// AgentBarrier admission marker can reject this still-pending generation.
	putBarrierChild(t, store, m.SessionID(), "new-sibling", "running")
	result := c.prepareCmd(manager, []session.AgentRun{offered})().(AgentBarrierMsg)
	if !errors.Is(result.err, session.ErrSessionTurnOwned) {
		t.Fatalf("barrier admitted with new outstanding sibling: %v", result.err)
	}
	driveBarrierCommands(t, newTestChatModel(false), func() tea.Msg { return result })
	if manager.HasActive(m.SessionID()) || c.failures != 0 {
		t.Fatal("busy barrier started or displayed an invalidation failure")
	}
}
