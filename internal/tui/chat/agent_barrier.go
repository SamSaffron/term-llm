package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat/agentbarrier"
)

const agentBarrierInterval = time.Second
const agentBarrierNotice = "[host] subagents finished — resuming"

// Coordinator mutation belongs to Update; execution remains manager-owned.
type agentBarrierCoordinator struct {
	model       *Model
	sessionID   string
	checking    bool
	failures    int
	retryAt     time.Time
	lastSettled string
}

type agentBarrierTurn struct {
	coordinator *agentBarrierCoordinator
	events      []session.AgentRun
	background  bool
}

// AgentBarrierMsg must be routed before displayed-session command scoping.
type AgentBarrierMsg struct {
	manager     *MainRunManager
	coordinator *agentBarrierCoordinator
	periodic    bool
	checked     bool
	events      []session.AgentRun
	result      tea.Msg
	err         error
	prepared    *agentBarrierPrepared
}

func (m *MainRunManager) registerAgentBarrier(model *Model) {
	if model == nil || model.SessionID() == "" || session.AsAgentRunDeliveryStore(model.store) == nil {
		return
	}
	if _, ok := session.AsServeResponseLifecycleStore(model.store); !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if m.agentBarriers == nil {
		m.agentBarriers = make(map[string]*agentBarrierCoordinator)
	}
	if old := m.agentBarriers[model.SessionID()]; old == nil || old.model != model {
		m.agentBarriers[model.SessionID()] = &agentBarrierCoordinator{model: model, sessionID: model.SessionID()}
	}
}

// AdoptAgentWakeResources retains needed detached resources until host shutdown.
func (m *MainRunManager) AdoptAgentWakeResources(sessionID string, cleanup func()) bool {
	if m == nil || cleanup == nil {
		return false
	}
	m.mu.RLock()
	c := m.agentBarriers[sessionID]
	closed := m.closed
	m.mu.RUnlock()
	if closed || c == nil {
		return false
	}
	needed := m.agentBarrierResourcesNeeded(c)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	if run := m.runs[sessionID]; run != nil {
		run.mu.Lock()
		needed = needed || run.active
		run.mu.Unlock()
	}
	needed = needed || c.model.agentBarrierStarting.Load() || c.model.agentBarrierAdmitting.Load() || c.model.agentBarrierSkillWork.Load() > 0
	if !needed {
		if m.agentBarriers[sessionID] == c {
			delete(m.agentBarriers, sessionID)
		}
		return false
	}
	m.agentWakeCleanups = append(m.agentWakeCleanups, cleanup)
	return true
}

func (m *MainRunManager) agentBarrierResourcesNeeded(c *agentBarrierCoordinator) bool {
	model := c.model
	if m.HasActive(c.sessionID) || model.agentBarrierStarting.Load() || model.agentBarrierAdmitting.Load() ||
		model.agentBarrierSkillWork.Load() > 0 || model.runtimeOperations.activeCount() > 0 {
		return true
	}
	if spawn := model.toolMgr.GetSpawnAgentTool(); spawn != nil && len(spawn.OutstandingAgentIDs()) > 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(m.ctx, turnLeaseAdmitTimeout)
	defer cancel()
	return agentbarrier.NeedsResources(ctx, session.AsAgentRunDeliveryStore(model.store), c.sessionID)
}

func (m *MainRunManager) signalAgentBarrier(parent string) {
	m.mu.RLock()
	coordinator := m.agentBarriers[parent]
	send := m.agentWakeSend
	m.mu.RUnlock()
	if coordinator == nil || send == nil || m.ctx.Err() != nil {
		return
	}
	// Never block child completion or finalization on Program.Send.
	go send(AgentBarrierMsg{manager: m})
}

// AgentBarrierReconcileCmd polls durable state; signals are latency hints only.
func (m *Model) AgentBarrierReconcileCmd() tea.Cmd {
	if m.mainRunManager == nil {
		return nil
	}
	manager := m.mainRunManager
	return func() tea.Msg { return AgentBarrierMsg{manager: manager, periodic: true} }
}

func (msg AgentBarrierMsg) Update(visible *Model) tea.Cmd {
	manager := msg.manager
	if manager == nil || manager.ctx.Err() != nil {
		msg.releasePrepared()
		return nil
	}
	manager.mu.RLock()
	coordinators := make([]*agentBarrierCoordinator, 0, len(manager.agentBarriers))
	for _, c := range manager.agentBarriers {
		coordinators = append(coordinators, c)
	}
	current := msg.coordinator == nil || manager.agentBarriers[msg.coordinator.sessionID] == msg.coordinator
	manager.mu.RUnlock()
	var cmds []tea.Cmd
	if msg.periodic {
		cmds = append(cmds, PassiveCommand(func() tea.Msg {
			timer := time.NewTimer(agentBarrierInterval)
			defer timer.Stop()
			select {
			case <-manager.ctx.Done():
				return nil
			case <-timer.C:
				return AgentBarrierMsg{manager: manager, periodic: true}
			}
		}))
	}
	if !current {
		msg.releasePrepared()
		return tea.Batch(cmds...)
	}
	if c := msg.coordinator; c != nil {
		c.checking = false
		if !msg.checked {
			c.model.agentBarrierAdmitting.Store(false)
		}
		if msg.checked {
			c.retryAt = time.Now().Add(agentBarrierInterval)
		}
		switch {
		case msg.result != nil:
			if failure, ok := msg.result.(streamEventMsg); ok && failure.event.Err != nil {
				c.model.streaming = false
				cmds = append(cmds, c.failed(visible, failure.event.Err))
			} else if c.model == visible {
				_, cmd := visible.Update(msg.result)
				cmds = append(cmds, cmd)
			}
		case msg.prepared != nil:
			if !c.idle(manager, visible) {
				msg.releasePrepared()
				break
			}
			if err := c.model.applyAgentBarrierPrepared(msg.prepared); err != nil {
				msg.releasePrepared()
				cmds = append(cmds, c.failed(visible, err))
				break
			}
			if errors.Is(msg.err, errTranscriptStale) {
				c.retryAt = time.Time{}
				break
			}
			c.model.pendingTurnLease.Store(msg.prepared.lease)
			c.checking = true
			turn := &agentBarrierTurn{coordinator: c, events: msg.events, background: c.model != visible}
			_, cmd := c.model.beginResponse("", agentBarrierNotice, nil, turn)
			cmds = append(cmds, cmd)
			if c.model == visible {
				_, notice := visible.showFooterMuted(agentBarrierNotice)
				cmds = append(cmds, notice)
			}
		case msg.err != nil:
			cmds = append(cmds, c.failed(visible, msg.err))
		case msg.checked && len(msg.events) > 0 && c.idle(manager, visible):
			c.checking = true
			c.model.agentBarrierAdmitting.Store(true)
			cmds = append(cmds, c.prepareCmd(manager, msg.events))
		}
	}

	for _, c := range coordinators {
		cmds = append(cmds, c.observeSettled(manager, visible))
		if c.checking || time.Now().Before(c.retryAt) || !c.idle(manager, visible) {
			continue
		}
		c.checking = true
		cmds = append(cmds, c.checkCmd(manager))
	}
	return tea.Batch(cmds...)
}

func (c *agentBarrierCoordinator) idle(manager *MainRunManager, visible *Model) bool {
	m := c.model
	if m.SessionID() != c.sessionID || m.quitting || manager.HasActive(c.sessionID) || manager.steeringCoordinator(c.sessionID) != nil || m.steeringHandoff != "" || m.agentBarrierStarting.Load() {
		return false
	}
	// Detached streaming bits are presentation only; the manager owns execution.
	if m == visible && m.streaming {
		return false
	}
	return (m.dialog == nil || !m.dialog.IsOpen()) && !m.blockingPromptActive() && !m.pausedForExternalUI && !m.externalProcessActive &&
		m.directShellRun == nil && !m.branchContextInFlight() && !m.worktreeOperationBusy() &&
		!m.transcriptMutationInFlight && m.sessionTransition == nil && !m.sessionSwitchPending &&
		m.activeSkillRunCount() == 0 && !m.sideQuestion.Running && !m.shareInFlight
}

func (c *agentBarrierCoordinator) checkCmd(manager *MainRunManager) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(manager.ctx, turnLeaseAdmitTimeout)
		defer cancel()
		result := AgentBarrierMsg{manager: manager, coordinator: c, checked: true}
		result.events, result.err = agentbarrier.Pending(ctx, c.model.store, c.sessionID)
		return result
	}
}

func (c *agentBarrierCoordinator) failed(visible *Model, err error) tea.Cmd {
	if errors.Is(err, session.ErrSessionTurnOwned) || errors.Is(err, session.ErrNotFound) || errors.Is(err, context.Canceled) {
		c.retryAt = time.Now().Add(agentBarrierInterval)
		return nil
	}
	delays := [...]time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	c.retryAt = time.Now().Add(delays[min(c.failures, len(delays)-1)])
	c.failures++
	if c.failures != 1 {
		return nil
	}
	text := fmt.Sprintf("[host] Subagent results are waiting; automatic resume will retry: %v", err)
	_, cmd := c.model.showFooterWarning(text)
	if c.model == visible {
		return cmd
	}
	return nil
}

type agentBarrierPrepared struct {
	session            *session.Session
	messages           []session.Message
	compactionIdx      int
	provider           core.Provider
	providerKey, model string
	rev                int64
	lease              *turnLease
}

func (msg AgentBarrierMsg) releasePrepared() {
	if msg.coordinator != nil {
		msg.coordinator.model.agentBarrierAdmitting.Store(false)
	}
	if msg.prepared != nil && msg.prepared.lease != nil {
		msg.coordinator.model.releaseTurnLease(msg.prepared.lease, session.ResponseRunCancelled)
	}
}

func (c *agentBarrierCoordinator) prepareCmd(manager *MainRunManager, events []session.AgentRun) tea.Cmd {
	// Capture settings on Update; the worker never mutates the Tea model.
	m := c.model
	store, providerKey, model := m.store, m.providerKey, m.modelName
	factory := m.sideProviderFactory
	if factory == nil {
		cfg := m.config
		factory = func(key, model string) (core.Provider, error) { return llm.NewProviderByName(cfg, key, model) }
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(manager.ctx, turnLeaseAdmitTimeout)
		defer cancel()
		result := AgentBarrierMsg{manager: manager, coordinator: c, events: events}
		lease, admitErr := m.acquireAgentTurnLease(ctx, c.sessionID, events)
		if admitErr != nil && !errors.Is(admitErr, errTranscriptStale) {
			result.err = admitErr
			return result
		}
		if admitErr == nil && lease == nil {
			result.err = errors.New("agent wake requires a durable turn lease")
			return result
		}
		prepared := &agentBarrierPrepared{lease: lease}
		fail := func(err error) tea.Msg {
			m.releaseTurnLease(lease, session.ResponseRunFailed)
			result.err = err
			return result
		}
		var err error
		prepared.session, err = store.Get(ctx, c.sessionID)
		if err != nil {
			return fail(err)
		}
		if prepared.session == nil || prepared.session.Archived {
			return fail(session.ErrNotFound)
		}
		prepared.messages, prepared.compactionIdx, err = loadSessionMessagesForScrollback(ctx, store, prepared.session)
		if err != nil {
			return fail(err)
		}
		prepared.providerKey, prepared.model = agentbarrier.ProviderSettings(prepared.session, providerKey, model)
		if prepared.providerKey != providerKey || prepared.model != model {
			prepared.provider, err = factory(prepared.providerKey, prepared.model)
			if err != nil {
				return fail(fmt.Errorf("reload agent wake provider: %w", err))
			}
		}
		indexer, _ := store.(session.TranscriptIndexer)
		prepared.rev, _ = transcriptRevOf(ctx, indexer, c.sessionID)
		result.prepared, result.err = prepared, admitErr
		return result
	}
}

func (m *Model) applyAgentBarrierPrepared(prepared *agentBarrierPrepared) error {
	if err := m.admitModelSelection(prepared.providerKey, prepared.model, false); err != nil {
		return err
	}
	if prepared.provider != nil {
		allowed, present := m.engine.AllowedToolsFilter()
		m.closeSideLane()
		m.provider = prepared.provider
		m.replaceEngine(engine.NewEngine(prepared.provider, m.engine.Tools()))
		m.engine.RestoreAllowedToolsFilter(allowed, present)
		m.providerName = prepared.provider.Name()
	}
	m.providerKey, m.modelName = prepared.providerKey, prepared.model
	m.sess = prepared.session
	m.applyLoadedScrollback(prepared.messages, prepared.compactionIdx)
	m.setTranscriptRevision(m.sess.ID, prepared.rev)
	m.searchEnabled = m.sess.Search
	m.toolsStr, m.mcpStr = m.sess.Tools, m.sess.MCP
	m.localTools = tools.ParseToolsFlag(m.toolsStr)
	if m.approvalMgr != nil && m.sess.ApprovalMode != "" {
		mode := tools.ModePrompt
		switch m.sess.ApprovalMode {
		case session.ApprovalModeAuto:
			mode = tools.ModeAuto
		case session.ApprovalModeYolo:
			mode = tools.ModeYolo
		}
		m.approvalMgr.SetApprovalMode(mode)
	}
	return m.admitModelSelection(m.providerKey, m.modelName, true)
}

// Observe manager finalization, never StreamDone (which can precede ACK).
func (c *agentBarrierCoordinator) observeSettled(manager *MainRunManager, visible *Model) tea.Cmd {
	manager.mu.RLock()
	run := manager.runs[c.sessionID]
	manager.mu.RUnlock()
	if run == nil {
		return nil
	}
	run.mu.Lock()
	id, active, barrier, err := run.id, run.active, run.agentBarrier, run.err
	run.mu.Unlock()
	if active || id == c.lastSettled {
		return nil
	}
	c.lastSettled = id
	if barrier && err != nil {
		return c.failed(visible, err)
	}
	c.failures = 0
	c.retryAt = time.Time{}
	return nil
}

// SetProgram attaches the program for host-lifetime wake messages.
func (m *Model) SetProgram(p *tea.Program) {
	m.program = p
	if m.mainRunManager != nil && p != nil {
		m.mainRunManager.mu.Lock()
		m.mainRunManager.agentWakeSend = p.Send
		m.mainRunManager.mu.Unlock()
	}
}

// SetMainRunManager attaches process-scoped execution ownership.
func (m *Model) SetMainRunManager(manager *MainRunManager) {
	m.mainRunManager = manager
	if manager != nil {
		manager.registerAgentBarrier(m)
		if m.program != nil {
			m.SetProgram(m.program)
		}
	}
}
