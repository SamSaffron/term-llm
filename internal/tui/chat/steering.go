package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

type SteeringCoordinator struct {
	mu                sync.Mutex
	op                *session.RushOperation
	store             session.RushStore
	engine            *llm.Engine
	owner             llm.SteeringTransition
	entries           []llm.QueuedSteering
	cancelled         atomic.Bool
	sourceInterrupted atomic.Bool
	settlementOnce    sync.Once
	source            *mainRunState
	admitted          chan struct{}
	admissionOnce     sync.Once
	cleanupMu         sync.Mutex
}
type steeringReadyMsg struct {
	sessionID   string
	operationID string
	entries     []llm.QueuedSteering
	err         error
	rows        []*session.Message
}

func (m *MainRunManager) steeringCoordinator(sessionID string) *SteeringCoordinator {
	if m == nil {
		return nil
	}
	value, ok := m.steering.Load(sessionID)
	if !ok {
		return nil
	}
	return value.(*SteeringCoordinator)
}

func (m *MainRunManager) Rush(sessionID, expectedRunID string, engine *llm.Engine, store session.Store) (tea.Cmd, error) {
	if m == nil || engine == nil {
		return nil, errors.New("run_not_consuming")
	}
	var durable session.RushStore
	if store != nil {
		var ok bool
		durable, ok = session.AsRushStore(store)
		if !ok {
			return nil, errors.New("durable_store_unavailable")
		}
	}
	m.mu.Lock()
	source := m.runs[sessionID]
	if source == nil || source.id != expectedRunID || m.steeringCoordinator(sessionID) != nil {
		m.mu.Unlock()
		return nil, errors.New("transition_in_progress")
	}
	owner := llm.SteeringTransition{OperationID: session.NewID(), Fence: int64(m.nextRun.Add(1))}
	entries, err := engine.FreezeSteering(owner)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if len(entries) == 0 {
		m.mu.Unlock()
		return nil, nil
	}
	coordinator := &SteeringCoordinator{engine: engine, owner: owner, entries: entries, source: source, store: durable, admitted: make(chan struct{})}
	m.steering.Store(sessionID, coordinator)
	m.mu.Unlock()
	return func() tea.Msg {
		result := steeringReadyMsg{sessionID: sessionID, operationID: owner.OperationID, entries: entries}
		defer coordinator.admissionOnce.Do(func() { close(coordinator.admitted) })
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		fail := func(err error) tea.Msg {
			result.err = err
			m.releaseFailedSteering(sessionID, coordinator, err)
			return result
		}
		if durable != nil {
			pendingStore, _ := session.AsPendingSteeringStore(store)
			if pendingStore == nil {
				return fail(errors.New("durable_store_unavailable"))
			}
			pending := make([]session.PendingSteering, 0, len(entries))
			for _, entry := range entries {
				row := session.PendingSteering{SessionID: sessionID, ID: entry.ID, Message: entry.Message, DisplayText: entry.DisplayText, Origin: entry.Origin, AttachmentSummary: llm.MessageAttachmentSummary(entry.Message)}
				if err := pendingStore.SavePendingSteering(ctx, row); err != nil {
					return fail(err)
				}
				pending = append(pending, row)
			}
			sess, err := store.Get(ctx, sessionID)
			if err != nil {
				return fail(err)
			}
			if sess.Goal != nil && sess.Goal.IsActive() {
				goal := sess.Goal.Clone()
				goal.Status = session.GoalStatusPaused
				goal.PausedAt = time.Now()
				goal.UpdatedAt = goal.PausedAt
				if err := session.UpdateGoal(ctx, store, sessionID, goal); err != nil {
					return fail(err)
				}
			}
			if coordinator.cancelled.Load() {
				return fail(context.Canceled)
			}
			op, err := durable.AdmitRush(ctx, session.RushOperation{SessionID: sessionID, RequestID: owner.OperationID, SourceResponseID: source.id, SourceEpoch: source.epoch, Fence: owner.Fence, ReplacementResponseID: "tui-rush-" + owner.OperationID}, pending)
			if err != nil {
				return fail(err)
			}
			coordinator.mu.Lock()
			coordinator.op = op
			coordinator.mu.Unlock()
		}
		coordinator.admissionOnce.Do(func() { close(coordinator.admitted) })
		cancelMainRunPendingUI(source)
		coordinator.sourceInterrupted.Store(true)
		source.cancel()
		select {
		case <-source.done:
		case <-ctx.Done():
			return fail(errors.New("settlement_unknown"))
		}
		if err := engine.WaitSteeringSettlement(ctx, owner); err != nil {
			return fail(err)
		}
		if coordinator.cancelled.Load() {
			return fail(context.Canceled)
		}
		rows := []*session.Message{session.NewMessage(sessionID, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: llm.SteeringInterruptionNotice}}}, -1)}
		for _, entry := range entries {
			rows = append(rows, sessionMessageForSteering(sessionID, entry.DisplayText, entry.Message))
		}
		if durable != nil {
			coordinator.mu.Lock()
			current := coordinator.op
			coordinator.mu.Unlock()
			op, err := durable.AdvanceRush(ctx, current, session.RushStarting, "")
			if err != nil {
				return fail(err)
			}
			coordinator.mu.Lock()
			coordinator.op = op
			coordinator.mu.Unlock()
			if coordinator.cancelled.Load() {
				return fail(context.Canceled)
			}
			if _, err = durable.CommitRushInitialInput(ctx, op, rows); err != nil {
				return fail(err)
			}
		}
		engine.ReleaseSteeringFreeze(owner, true)
		result.rows = rows
		return result
	}, nil
}

func (m *MainRunManager) releaseFailedSteering(sessionID string, c *SteeringCoordinator, cause error) {
	c.mu.Lock()
	op := c.op
	c.mu.Unlock()
	cleanup := func() {
		if !c.cleanupMu.TryLock() {
			return
		}
		defer c.cleanupMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if c.store != nil && op != nil {
			current, err := c.store.GetRush(ctx, sessionID, op.RequestID)
			if err != nil {
				return
			}
			if current.Status.Active() {
				status := session.RushBlocked
				if c.cancelled.Load() || errors.Is(cause, context.Canceled) {
					status = session.RushCancelled
				}
				current, err = c.store.AdvanceRush(ctx, current, status, cause.Error())
				if err != nil {
					return
				}
			}
			if current.Status != session.RushStarted {
				if err := c.store.ReleaseRush(ctx, current); err != nil {
					return
				}
			}
		}
		c.engine.ReleaseSteeringFreeze(c.owner, false)
		m.steering.CompareAndDelete(sessionID, c)
	}
	if op == nil && !c.sourceInterrupted.Load() && !c.cancelled.Load() {
		cleanup()
		return
	}
	c.source.cancel()
	// Failed admission may unfreeze the still-running source immediately, but
	// a cancelled source (including no-store mode) must settle actual tools first.
	settle := func() {
		if err := c.engine.WaitSteeringSettlement(m.ctx, c.owner); err != nil && !errors.Is(err, llm.ErrSteeringTransition) {
			return
		}
		cleanup()
	}
	select {
	case <-c.source.done:
		probe, cancel := context.WithCancel(context.Background())
		cancel()
		err := c.engine.WaitSteeringSettlement(probe, c.owner)
		if err == nil || errors.Is(err, llm.ErrSteeringTransition) {
			cleanup()
		} else {
			c.settlementOnce.Do(func() { go settle() })
		}
	default:
		c.settlementOnce.Do(func() {
			go func() {
				select {
				case <-c.source.done:
					settle()
				case <-m.ctx.Done():
				}
			}()
		})
	}
}

func (m *Model) rushPendingSteering() (tea.Model, tea.Cmd) {
	if m.steeringHandoff != "" {
		return m, nil
	}
	if m.mainRunManager == nil || m.engine == nil || !m.engine.SteeringAvailability().CanRush {
		return m, nil
	}
	command, err := m.mainRunManager.Rush(m.SessionID(), m.mainRunID, m.engine, m.store)
	if err != nil {
		return m.showFooterWarning("Could not start a steered run. Your queued messages are still pending.")
	}
	if command == nil {
		return m, nil
	}
	coordinator := m.mainRunManager.steeringCoordinator(m.SessionID())
	m.steeringHandoff = coordinator.owner.OperationID
	m.interruptNotice = "Interrupting…"
	return m, command
}

func (m *Model) handleSteeringReady(msg steeringReadyMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.SessionID() || msg.operationID != m.steeringHandoff {
		return m, nil
	}
	if msg.err != nil {
		m.steeringHandoff = ""
		for _, entry := range msg.entries {
			m.setPendingSteering(entry.ID, entry.DisplayText)
		}
		m.interruptNotice = "Rush stopped: " + msg.err.Error() + " — guidance kept"
		return m, nil
	}
	coordinator := m.mainRunManager.steeringCoordinator(msg.sessionID)
	if coordinator == nil || coordinator.cancelled.Load() {
		// Initial input was already committed: show the unanswered rows, never
		// reclassify them as pending or lose them in the last UI-tick Stop race.
		m.appendSteeringInitialRows(msg.rows)
		m.clearPendingSteering()
		m.steeringHandoff = ""
		m.interruptNotice = "Stopped before the steered run started — guidance kept in the conversation"
		return m, nil
	}
	rows := msg.rows
	// Materialize the source's partial output before starting a new generation.
	if m.streaming {
		_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled), generation: m.streamGeneration, mainRunID: m.mainRunID, mainRunSubscription: m.mainRunSubscription})
	}
	m.appendSteeringInitialRows(rows)
	m.clearPendingSteering()
	draft := m.textarea.Value()
	images := m.images
	files := m.files
	pastes := m.pasteChunks
	_, cmd := m.beginUserResponse("", fmt.Sprintf("❯ %d pending steering message(s)", len(msg.entries)), nil)
	m.setTextareaValue(draft)
	m.images = images
	m.files = files
	m.pasteChunks = pastes
	m.interruptNotice = "Starting steered run…"
	return m, cmd
}

// Source terminal processing can reload the transcript after Rush's initial
// input transaction commits. Do not append those same rows a second time.
func (m *Model) appendSteeringInitialRows(rows []*session.Message) {
	before := len(m.messages)
	for _, row := range rows {
		if slices.ContainsFunc(m.messages, func(existing session.Message) bool {
			return (row.ID > 0 && existing.ID == row.ID) ||
				(row.ClientMessageID != "" && existing.ClientMessageID == row.ClientMessageID)
		}) {
			continue
		}
		m.messages = append(m.messages, *row)
	}
	if len(m.messages) != before {
		m.invalidateHistoryCache()
		m.scrollToBottom = true
	}
}

type steeringStartFailedMsg struct {
	generation  uint64
	operationID string
	err         error
}
