package termhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

// Control is terminal output that must be returned to Bubble Tea's renderer.
// RefreshAfter is non-zero while the active OSC protocol needs a keepalive.
type Control struct {
	Sequence     string
	RefreshAfter time.Duration
}

type lifecycleDiagnostic struct {
	mu     sync.Mutex
	writer io.Writer
	seen   map[string]struct{}
}

func newLifecycleDiagnostic(rt runtimeContext) *lifecycleDiagnostic {
	if rt.getenv == nil || strings.TrimSpace(rt.getenv("TERM_LLM_LIFECYCLE_DEBUG")) != "1" || rt.stderr == nil {
		return nil
	}
	return &lifecycleDiagnostic{writer: rt.stderr, seen: make(map[string]struct{})}
}

func (d *lifecycleDiagnostic) report(adapter string, err error) {
	if d == nil || err == nil {
		return
	}
	class := lifecycleErrorClass(err)
	key := adapter + "\x00" + class
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.seen[key]; exists {
		return
	}
	d.seen[key] = struct{}{}
	// Adapter names are quoted and errors are reduced to a fixed class. Never
	// print argv, environment values, event/session data, or raw error strings.
	_, _ = fmt.Fprintf(d.writer, "term-llm lifecycle: adapter %q send %s\n", adapter, class)
}

func lifecycleErrorClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "was canceled"
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return "failed: executable unavailable"
	case errors.Is(err, fs.ErrPermission):
		return "failed: permission denied"
	default:
		return "failed"
	}
}

// Manager fans each lifecycle event out to independently scheduled adapters.
// Report never waits for an adapter. Each adapter retains only its latest
// pending state while its current command is running.
type Manager struct {
	mu         sync.Mutex
	workers    []*adapterWorker
	osc        *oscController
	now        func() time.Time
	metadata   lifecycle.Metadata
	executable string

	closed       bool
	claimed      bool
	lastSnapshot lifecycle.Snapshot
	sequence     int64
	closeTimeout time.Duration
	closeOnce    sync.Once
	done         chan struct{}
}

// New discovers all selected terminal hosts and starts one independent worker
// per enabled adapter. Multiple detected hosts are active simultaneously.
func New(cfg config.LifecycleConfig, legacyProgress bool) (*Manager, error) {
	rt := defaultRuntimeContext()
	found, report, err := discoverAll(cfg, legacyProgress, rt)
	if err != nil {
		return nil, err
	}
	adapters := make([]Adapter, 0, len(found))
	for _, entry := range found {
		adapters = append(adapters, entry.adapter)
	}
	var osc *oscController
	if report.Enabled && report.OSC.Enabled {
		osc = newOSCController(report.OSC, rt)
	}
	return newManager(adapters, osc, rt, defaultCloseTimeout), nil
}

func newManager(adapters []Adapter, osc *oscController, rt runtimeContext, closeTimeout time.Duration) *Manager {
	if rt.now == nil {
		rt.now = time.Now
	}
	if closeTimeout <= 0 {
		closeTimeout = defaultCloseTimeout
	}
	manager := &Manager{
		osc: osc,
		now: rt.now,
		metadata: lifecycle.Metadata{
			Producer: "term-llm",
			PID:      rt.pid,
			CWD:      rt.cwd,
		},
		closeTimeout: closeTimeout,
		executable:   strings.TrimSpace(rt.executable),
		done:         make(chan struct{}),
	}
	manager.sequence = rt.now().UnixNano() - 1
	diagnostic := newLifecycleDiagnostic(rt)
	for _, adapter := range adapters {
		if adapter != nil {
			commandTimeout := defaultCommandTimeout
			if configured, ok := adapter.(interface{ Timeout() time.Duration }); ok && configured.Timeout() > 0 {
				commandTimeout = configured.Timeout()
			}
			manager.workers = append(manager.workers, newAdapterWorker(adapter, commandTimeout, defaultReleaseTimeout, diagnostic.report))
		}
	}
	return manager
}

// Report publishes a normalized snapshot once and returns any renderer-owned
// OSC output. Calling Report with an unchanged active snapshot is useful for an
// OSC keepalive; adapters still deduplicate it independently.
func (m *Manager) Report(snapshot lifecycle.Snapshot) Control {
	if m == nil {
		return Control{}
	}
	snapshot = lifecycle.NormalizeSnapshot(snapshot)
	if !lifecycle.ValidState(snapshot.State) {
		return Control{}
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Control{}
	}
	changed := !m.claimed || snapshot != m.lastSnapshot
	if changed {
		m.claimed = true
		m.lastSnapshot = snapshot
		event := m.newEventLocked(lifecycle.KindState, snapshot)
		for _, worker := range m.workers {
			worker.enqueue(event)
		}
	}
	control := Control{}
	if m.osc != nil {
		control = m.osc.control(snapshot)
	}
	m.mu.Unlock()
	return control
}

// RestoreOSC returns a clear sequence when this manager currently owns an OSC
// indicator. The caller must write it only after Bubble Tea releases renderer
// ownership (or while performing bounded forced-exit cleanup).
func (m *Manager) RestoreOSC() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.osc == nil {
		return ""
	}
	return m.osc.restore()
}

// Close relinquishes all claimed adapters in parallel. The total wait is one
// aggregate budget, regardless of adapter count. Close is idempotent.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(m.beginClose)
	timer := time.NewTimer(m.closeTimeout)
	defer timer.Stop()
	select {
	case <-m.done:
	case <-timer.C:
	}
}

func (m *Manager) beginClose() {
	m.mu.Lock()
	m.closed = true
	claimed := m.claimed
	var release lifecycle.Event
	if claimed {
		release = m.newEventLocked(lifecycle.KindRelease, m.lastSnapshot)
	}
	workers := append([]*adapterWorker(nil), m.workers...)
	m.mu.Unlock()

	for _, worker := range workers {
		worker.beginClose(release, claimed)
	}
	go func() {
		for _, worker := range workers {
			<-worker.done
		}
		close(m.done)
	}()
}

func (m *Manager) newEventLocked(kind lifecycle.Kind, snapshot lifecycle.Snapshot) lifecycle.Event {
	now := m.now()
	next := now.UnixNano()
	if next <= m.sequence {
		next = m.sequence + 1
	}
	m.sequence = next
	metadata := m.metadata
	if snapshot.CWD != "" {
		metadata.CWD = snapshot.CWD
	}
	if snapshot.SessionID != "" && m.executable != "" {
		metadata.ResumeArgv = []string{m.executable, "chat", "--resume=" + snapshot.SessionID}
	}
	return lifecycle.NewEvent(kind, next, now, metadata, snapshot)
}

// adapterWorker owns one adapter's coalescing and execution. No worker ever
// waits for another worker.
type adapterWorker struct {
	adapter        Adapter
	commandTimeout time.Duration
	releaseTimeout time.Duration
	diagnose       func(string, error)

	mu            sync.Mutex
	closed        bool
	lastState     lifecycle.Event
	hasLastState  bool
	pending       *lifecycle.Event
	release       *lifecycle.Event
	runningCancel context.CancelFunc
	wake          chan struct{}
	done          chan struct{}
}

func newAdapterWorker(adapter Adapter, commandTimeout, releaseTimeout time.Duration, diagnose func(string, error)) *adapterWorker {
	worker := &adapterWorker{
		adapter:        adapter,
		commandTimeout: commandTimeout,
		releaseTimeout: releaseTimeout,
		diagnose:       diagnose,
		wake:           make(chan struct{}, 1),
		done:           make(chan struct{}),
	}
	go worker.loop()
	return worker
}

func (w *adapterWorker) enqueue(event lifecycle.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || (w.hasLastState && sameStateEvent(w.lastState, event)) {
		return
	}
	w.hasLastState = true
	w.lastState = event
	copyEvent := event
	w.pending = &copyEvent
	w.notifyLocked()
}

func sameStateEvent(left, right lifecycle.Event) bool {
	return left.State == right.State && left.Message == right.Message && left.SessionID == right.SessionID && left.Title == right.Title && left.CWD == right.CWD
}

func (w *adapterWorker) beginClose(release lifecycle.Event, claimed bool) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.pending = nil
	if claimed {
		copyEvent := release
		w.release = &copyEvent
	}
	cancel := w.runningCancel
	w.notifyLocked()
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *adapterWorker) loop() {
	defer close(w.done)
	for range w.wake {
		for {
			event, ok, closed := w.nextEvent()
			if !ok {
				if closed {
					return
				}
				break
			}
			w.execute(event)
		}
	}
}

func (w *adapterWorker) nextEvent() (lifecycle.Event, bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.release != nil {
		event := *w.release
		w.release = nil
		return event, true, false
	}
	if w.pending != nil {
		event := *w.pending
		w.pending = nil
		return event, true, false
	}
	return lifecycle.Event{}, false, w.closed
}

func (w *adapterWorker) execute(event lifecycle.Event) {
	timeout := w.commandTimeout
	if event.Kind == lifecycle.KindRelease {
		timeout = w.releaseTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	w.mu.Lock()
	if w.closed && event.Kind == lifecycle.KindState {
		w.mu.Unlock()
		cancel()
		return
	}
	w.runningCancel = cancel
	w.mu.Unlock()

	// Send synchronously on this dedicated worker. Built-in adapters honor ctx;
	// keeping the call in-line guarantees a timed-out or misbehaving adapter can
	// never overlap a later state or release for the same host.
	err := w.adapter.Send(ctx, event)
	contextErr := ctx.Err()
	cancel()

	w.mu.Lock()
	w.runningCancel = nil
	w.mu.Unlock()
	if err != nil && w.diagnose != nil {
		if contextErr != nil {
			err = contextErr
		}
		w.diagnose(w.adapter.Name(), err)
	}
}

func (w *adapterWorker) notifyLocked() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
