package chat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

const (
	mainRunReplayLimit      = 4096
	mainRunSubscriberBuffer = 256
)

// MainRunEvent is one retained event from a process-scoped TUI main run.
type MainRunEvent struct {
	Sequence uint64
	Event    ui.StreamEvent
}

// MainRunSnapshot describes an active or recently terminal session run.
type MainRunSnapshot struct {
	RunID           string
	SessionID       string
	Active          bool
	StartedAt       time.Time
	CompletedAt     time.Time
	LastSequence    uint64
	AnchorMessageID int64
	Done            <-chan struct{}
	Err             error
}

// MainRunExecution owns one session's execution after the visible TUI detaches.
// Execute must publish normalized stream events through emit and return only
// after provider/tool work and persistence callbacks have stopped.
type MainRunExecution struct {
	Execute              func(ctx context.Context, emit func(ui.StreamEvent)) error
	Cancel               context.CancelFunc
	Finalize             func(error)
	Cleanup              func()
	QueueInterjection    func(llm.QueuedInterjection) llm.InterjectionQueueStatus
	CancelInterjection   func(string) bool
	DiscardInterjections func()
	ListInterjections    func() []llm.QueuedInterjection
	DrainInterjections   func() []llm.QueuedInterjection
	AnchorMessageID      int64
}

type mainRunSubscriber struct {
	id uint64
	ch chan MainRunEvent
}

type mainRunUISink struct {
	id   uint64
	send func(tea.Msg)
}

type mainRunPresentation struct {
	runID           string
	sequence        uint64
	tracker         *ui.ToolTracker
	subagentTracker *ui.SubagentTracker
}

type mainRunState struct {
	id              string
	sessionID       string
	anchorMessageID int64
	startedAt       time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu                   sync.Mutex
	active               bool
	completedAt          time.Time
	err                  error
	nextSeq              uint64
	events               []MainRunEvent
	subscribers          map[uint64]*mainRunSubscriber
	pendingUI            []tea.Msg
	uiSink               func(tea.Msg)
	uiSinkID             uint64
	presentation         *mainRunPresentation
	cleanup              func()
	queueInterjection    func(llm.QueuedInterjection) llm.InterjectionQueueStatus
	cancelInterjection   func(string) bool
	discardInterjections func()
	listInterjections    func() []llm.QueuedInterjection
	drainInterjections   func() []llm.QueuedInterjection
	cleanupOnce          sync.Once
}

// MainRunManager owns all process-lifetime TUI main runs. It is independent of
// Bubble Tea terminal ownership; visible models only subscribe to retained/live
// events and may detach without cancelling execution.
type MainRunManager struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.RWMutex
	runs     map[string]*mainRunState
	uiSinks  map[string]mainRunUISink
	nextRun  atomic.Uint64
	nextSub  atomic.Uint64
	nextSink atomic.Uint64
	closed   bool
	changed  chan struct{}
}

// NewMainRunManager creates a process-scoped run manager.
func NewMainRunManager(parent context.Context) *MainRunManager {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &MainRunManager{
		ctx: ctx, cancel: cancel, runs: make(map[string]*mainRunState),
		uiSinks: make(map[string]mainRunUISink), changed: make(chan struct{}, 1),
	}
}

// Start creates one active run for sessionID. Concurrent main runs are allowed
// across sessions, but a session can own only one active run.
func (m *MainRunManager) Start(sessionID string, execution MainRunExecution) (MainRunSnapshot, error) {
	if m == nil || sessionID == "" || execution.Execute == nil {
		return MainRunSnapshot{}, errors.New("invalid main run")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return MainRunSnapshot{}, errors.New("main run manager is closed")
	}
	if existing := m.runs[sessionID]; existing != nil {
		existing.mu.Lock()
		active := existing.active
		existing.mu.Unlock()
		if active {
			m.mu.Unlock()
			return MainRunSnapshot{}, fmt.Errorf("session %s already has an active run", sessionID)
		}
	}
	runCtx, runCancel := context.WithCancel(m.ctx)
	if execution.Cancel != nil {
		providerCancel := execution.Cancel
		baseCancel := runCancel
		runCancel = func() {
			providerCancel()
			baseCancel()
		}
	}
	sink := m.uiSinks[sessionID]
	run := &mainRunState{
		id: fmt.Sprintf("tui-run-%d", m.nextRun.Add(1)), sessionID: sessionID,
		anchorMessageID: execution.AnchorMessageID,
		startedAt:       time.Now(), ctx: runCtx, cancel: runCancel, done: make(chan struct{}),
		active: true, subscribers: make(map[uint64]*mainRunSubscriber), cleanup: execution.Cleanup,
		uiSink: sink.send, uiSinkID: sink.id, queueInterjection: execution.QueueInterjection,
		cancelInterjection: execution.CancelInterjection, discardInterjections: execution.DiscardInterjections,
		listInterjections: execution.ListInterjections, drainInterjections: execution.DrainInterjections,
	}
	m.runs[sessionID] = run
	m.mu.Unlock()
	m.signalChanged()

	go m.execute(run, execution)
	return snapshotMainRun(run), nil
}

func (m *MainRunManager) execute(run *mainRunState, execution MainRunExecution) {
	defer close(run.done)
	err := execution.Execute(run.ctx, func(event ui.StreamEvent) { m.publish(run, event) })
	if execution.Finalize != nil {
		execution.Finalize(err)
	}
	run.mu.Lock()
	if run.active {
		run.active = false
		run.completedAt = time.Now()
		run.err = err
	}
	run.presentation = nil
	cleanup := run.cleanup
	for id, subscriber := range run.subscribers {
		close(subscriber.ch)
		delete(run.subscribers, id)
	}
	run.mu.Unlock()
	run.cleanupOnce.Do(func() {
		if cleanup != nil {
			cleanup()
		}
	})
	m.signalChanged()
}

func (m *MainRunManager) publish(run *mainRunState, event ui.StreamEvent) {
	if run == nil {
		return
	}
	run.mu.Lock()
	run.nextSeq++
	envelope := MainRunEvent{Sequence: run.nextSeq, Event: event}
	run.events = append(run.events, envelope)
	if len(run.events) > mainRunReplayLimit {
		copy(run.events, run.events[len(run.events)-mainRunReplayLimit:])
		run.events = run.events[:mainRunReplayLimit]
	}
	for id, subscriber := range run.subscribers {
		select {
		case subscriber.ch <- envelope:
		default:
			close(subscriber.ch)
			delete(run.subscribers, id)
		}
	}
	run.mu.Unlock()
}

// Subscribe returns retained events after afterSequence and a live stream. If
// the requested sequence predates retained history, snapshotRequired is true.
func (m *MainRunManager) Subscribe(sessionID string, afterSequence uint64) (replay []MainRunEvent, live <-chan MainRunEvent, snapshotRequired bool, snapshot MainRunSnapshot, detach func()) {
	if m == nil {
		return nil, nil, false, MainRunSnapshot{}, func() {}
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return nil, nil, false, MainRunSnapshot{}, func() {}
	}
	run.mu.Lock()
	if len(run.events) > 0 && afterSequence != ^uint64(0) && afterSequence+1 < run.events[0].Sequence {
		snapshotRequired = true
	}
	for _, event := range run.events {
		if event.Sequence > afterSequence {
			replay = append(replay, event)
		}
	}
	var subscriberID uint64
	var ch chan MainRunEvent
	if run.active {
		subscriberID = m.nextSub.Add(1)
		ch = make(chan MainRunEvent, mainRunSubscriberBuffer)
		run.subscribers[subscriberID] = &mainRunSubscriber{id: subscriberID, ch: ch}
	}
	snapshot = snapshotMainRunLocked(run)
	run.mu.Unlock()

	var once sync.Once
	detach = func() {
		once.Do(func() {
			if subscriberID == 0 {
				return
			}
			run.mu.Lock()
			if subscriber := run.subscribers[subscriberID]; subscriber != nil {
				close(subscriber.ch)
				delete(run.subscribers, subscriberID)
			}
			run.mu.Unlock()
		})
	}
	return replay, ch, snapshotRequired, snapshot, detach
}

// RetainPresentation preserves the visible model's in-memory tool/subagent
// presentation when navigation detaches from an active run. The sequence cursor
// binds that snapshot to the retained stream suffix that has not yet been
// applied to it.
func (m *MainRunManager) RetainPresentation(sessionID, runID string, sequence uint64, tracker *ui.ToolTracker, subagentTracker *ui.SubagentTracker) bool {
	if m == nil || sessionID == "" || runID == "" || tracker == nil || subagentTracker == nil {
		return false
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.active || run.id != runID {
		return false
	}
	if retained := run.presentation; retained != nil && retained.runID == runID && retained.sequence > sequence {
		return false
	}
	run.presentation = &mainRunPresentation{
		runID: runID, sequence: sequence, tracker: tracker, subagentTracker: subagentTracker,
	}
	return true
}

// TakePresentation transfers a retained presentation snapshot to the fresh
// model reattaching to sessionID. The manager keeps stream events, while the
// snapshot owns everything through its sequence cursor.
func (m *MainRunManager) TakePresentation(sessionID string) *mainRunPresentation {
	if m == nil || sessionID == "" {
		return nil
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if !run.active || run.presentation == nil || run.presentation.runID != run.id {
		return nil
	}
	presentation := run.presentation
	run.presentation = nil
	return presentation
}

// AttachUISink makes sink the current interactive prompt destination for a
// session. Pending approval/ask/handover messages are delivered in order.
func (m *MainRunManager) AttachUISink(sessionID string, sink func(tea.Msg)) (detach func()) {
	if m == nil || sink == nil || sessionID == "" {
		return func() {}
	}
	sinkID := m.nextSink.Add(1)
	m.mu.Lock()
	m.uiSinks[sessionID] = mainRunUISink{id: sinkID, send: sink}
	run := m.runs[sessionID]
	m.mu.Unlock()
	var pending []tea.Msg
	if run != nil {
		run.mu.Lock()
		run.uiSink = sink
		run.uiSinkID = sinkID
		pending = append(pending, run.pendingUI...)
		run.pendingUI = nil
		run.mu.Unlock()
	}
	if len(pending) > 0 {
		// Deliver asynchronously (in order) so a sink backed by the blocking
		// Program.Send can be attached from the Bubble Tea update loop itself,
		// e.g. while an in-process session switch installs the new model.
		go func() {
			for _, message := range pending {
				sink(mainRunUIEnvelope{sessionID: sessionID, sinkID: sinkID, message: message})
			}
		}()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if current := m.uiSinks[sessionID]; current.id == sinkID {
				delete(m.uiSinks, sessionID)
			}
			m.mu.Unlock()
			if run != nil {
				run.mu.Lock()
				if run.uiSinkID == sinkID {
					run.uiSink = nil
					run.uiSinkID = 0
				}
				run.mu.Unlock()
			}
		})
	}
}

// DeliverUI routes an interactive request to the attached model or retains it
// until that session is reattached.
func (m *MainRunManager) DeliverUI(sessionID string, message tea.Msg) error {
	if m == nil || message == nil {
		return errors.New("invalid interactive request")
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return errors.New("main run not found")
	}
	run.mu.Lock()
	sink, sinkID := run.uiSink, run.uiSinkID
	if sink == nil {
		run.pendingUI = append(run.pendingUI, message)
		run.mu.Unlock()
		return nil
	}
	run.mu.Unlock()
	// Deliver outside run.mu: sinks forward into the Bubble Tea message loop
	// via the blocking Program.Send, and that loop takes run.mu through manager
	// methods (HasActive, Subscribe, Cancel, interjection routing). The envelope
	// lets the receiving model reject and retain a delivery whose sink detached
	// while this goroutine was blocked in Send.
	sink(mainRunUIEnvelope{sessionID: sessionID, sinkID: sinkID, message: message})
	return nil
}

// RetainUI requeues a delivery rejected by a model that no longer owns its
// captured sink. Clearing the stale sink prevents subsequent requests from
// being sent to the wrong visible session before its detach callback runs.
func (m *MainRunManager) RetainUI(sessionID string, sinkID uint64, message tea.Msg) {
	if m == nil || sessionID == "" || message == nil {
		return
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return
	}
	run.mu.Lock()
	if run.uiSinkID == sinkID {
		run.uiSink = nil
		run.uiSinkID = 0
	}
	currentSink, currentSinkID := run.uiSink, run.uiSinkID
	if currentSink == nil {
		run.pendingUI = append(run.pendingUI, message)
	}
	run.mu.Unlock()
	m.mu.Lock()
	if current := m.uiSinks[sessionID]; current.id == sinkID {
		delete(m.uiSinks, sessionID)
	}
	m.mu.Unlock()
	if currentSink != nil {
		currentSink(mainRunUIEnvelope{sessionID: sessionID, sinkID: currentSinkID, message: message})
	}
}

// IsUISinkCurrent reports whether sinkID is still attached to sessionID.
func (m *MainRunManager) IsUISinkCurrent(sessionID string, sinkID uint64) bool {
	if m == nil || sessionID == "" || sinkID == 0 {
		return false
	}
	m.mu.RLock()
	current := m.uiSinks[sessionID]
	m.mu.RUnlock()
	return current.id == sinkID
}

// AdoptResources transfers cleanup ownership for a session runtime that has
// detached while its run remains active. The manager invokes cleanup only after
// execution and persistence have stopped.
func (m *MainRunManager) AdoptResources(sessionID string, cleanup func()) bool {
	if m == nil || cleanup == nil {
		return false
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return false
	}
	run.mu.Lock()
	if !run.active || run.cleanup != nil {
		run.mu.Unlock()
		return false
	}
	run.cleanup = cleanup
	run.mu.Unlock()
	return true
}

// HasActive reports whether sessionID currently owns a running main execution.
func (m *MainRunManager) HasActive(sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.active
}

// ActiveCount returns the number of process-lifetime runs still executing.
func (m *MainRunManager) ActiveCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	runs := make([]*mainRunState, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	m.mu.RUnlock()
	count := 0
	for _, run := range runs {
		run.mu.Lock()
		if run.active {
			count++
		}
		run.mu.Unlock()
	}
	return count
}

// Changes is notified whenever the active run count may have changed.
func (m *MainRunManager) Changes() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.changed
}

func (m *MainRunManager) signalChanged() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

// QueueInterjection routes steering to the engine owned by sessionID's run.
func (m *MainRunManager) QueueInterjection(sessionID string, interjection llm.QueuedInterjection) llm.InterjectionQueueStatus {
	if m == nil {
		return llm.InterjectionQueueRunFinished
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return llm.InterjectionQueueRunFinished
	}
	run.mu.Lock()
	active, queue := run.active, run.queueInterjection
	run.mu.Unlock()
	if !active || queue == nil {
		return llm.InterjectionQueueRunFinished
	}
	return queue(interjection)
}

// CancelInterjection removes queued steering from the owning run.
func (m *MainRunManager) CancelInterjection(sessionID, interjectionID string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return false
	}
	run.mu.Lock()
	cancel := run.cancelInterjection
	run.mu.Unlock()
	return cancel != nil && cancel(interjectionID)
}

// DiscardInterjections clears all queued steering for the owning run.
func (m *MainRunManager) DiscardInterjections(sessionID string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return
	}
	run.mu.Lock()
	discard := run.discardInterjections
	run.mu.Unlock()
	if discard != nil {
		discard()
	}
}

// ListInterjections returns a snapshot of steering queued on the owning run.
func (m *MainRunManager) ListInterjections(sessionID string) []llm.QueuedInterjection {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return nil
	}
	run.mu.Lock()
	list := run.listInterjections
	run.mu.Unlock()
	if list == nil {
		return nil
	}
	return list()
}

// DrainInterjections removes and returns steering queued on the owning run.
func (m *MainRunManager) DrainInterjections(sessionID string) []llm.QueuedInterjection {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return nil
	}
	run.mu.Lock()
	drain := run.drainInterjections
	run.mu.Unlock()
	if drain == nil {
		return nil
	}
	return drain()
}

func cancelMainRunPendingUI(run *mainRunState) {
	if run == nil {
		return
	}
	run.mu.Lock()
	pending := append([]tea.Msg(nil), run.pendingUI...)
	run.pendingUI = nil
	run.mu.Unlock()
	for _, message := range pending {
		switch request := message.(type) {
		case ApprovalRequestMsg:
			select {
			case request.DoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}:
			default:
			}
		case AskUserRequestMsg:
			select {
			case request.DoneCh <- nil:
			default:
			}
		case HandoverRequestMsg:
			select {
			case request.DoneCh <- false:
			default:
			}
		}
	}
}

// Cancel stops one session run without affecting other sessions.
func (m *MainRunManager) Cancel(sessionID string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	run := m.runs[sessionID]
	m.mu.RUnlock()
	if run == nil {
		return false
	}
	cancelMainRunPendingUI(run)
	run.cancel()
	return true
}

// Close cancels all runs and waits up to timeout for persistence/cleanup.
func (m *MainRunManager) Close(timeout time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel()
	}
	runs := make([]*mainRunState, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	m.mu.Unlock()
	for _, run := range runs {
		cancelMainRunPendingUI(run)
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, run := range runs {
		select {
		case <-run.done:
		case <-deadline.C:
			return
		}
	}
}

func snapshotMainRun(run *mainRunState) MainRunSnapshot {
	run.mu.Lock()
	defer run.mu.Unlock()
	return snapshotMainRunLocked(run)
}

func snapshotMainRunLocked(run *mainRunState) MainRunSnapshot {
	return MainRunSnapshot{
		RunID: run.id, SessionID: run.sessionID, Active: run.active,
		StartedAt: run.startedAt, CompletedAt: run.completedAt,
		LastSequence: run.nextSeq, AnchorMessageID: run.anchorMessageID, Done: run.done, Err: run.err,
	}
}
