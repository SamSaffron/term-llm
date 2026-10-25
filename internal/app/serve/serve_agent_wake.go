package serve

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// wakeAgentParent schedules a single host-owned continuation for the original
// web session. It never sends child output directly to the user, and does not
// create a competing turn while a response is active. An incremented signal
// closes the scan/finish race when another child settles during a delivery.
func (s *serveServer) wakeAgentParent(parent string) {
	if s == nil || !s.cfg.ui || parent == "" || s.sessionMgr == nil || session.AsAgentRunDeliveryStore(s.store) == nil {
		return
	}
	s.agentWakeMu.Lock()
	if s.shutdownCh != nil {
		select {
		case <-s.shutdownCh:
			s.agentWakeMu.Unlock()
			return
		default:
		}
	}
	if s.agentWakeActive == nil {
		s.agentWakeActive = make(map[string]bool)
	}
	// Separate sequence from the worker flag so a concurrent completion cannot
	// disappear between the final DB read and worker exit.
	if s.agentWakeSignals == nil {
		s.agentWakeSignals = make(map[string]uint64)
	}
	s.agentWakeSignals[parent]++
	if s.agentWakeActive[parent] {
		s.agentWakeMu.Unlock()
		return
	}
	s.agentWakeActive[parent] = true
	s.agentWakeWG.Add(1)
	s.agentWakeMu.Unlock()
	go s.runAgentWake(parent)
}

func (s *serveServer) runAgentWake(parent string) {
	defer s.agentWakeWG.Done()
	attempt := 0
	for {
		s.agentWakeMu.Lock()
		generation := s.agentWakeSignals[parent]
		s.agentWakeMu.Unlock()
		if !s.deliverAgentWake(parent) && s.waitAgentWakeRetry(attempt) {
			attempt++
			continue
		}
		attempt = 0
		s.agentWakeMu.Lock()
		if generation == s.agentWakeSignals[parent] {
			delete(s.agentWakeActive, parent)
			delete(s.agentWakeSignals, parent)
			s.agentWakeMu.Unlock()
			return
		}
		s.agentWakeMu.Unlock()
	}
}

// waitAgentWakeRetry backs off provider/runtime failures without losing the
// durable event. Shutdown always interrupts the wait.
func (s *serveServer) waitAgentWakeRetry(attempt int) bool {
	delays := [...]time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	if attempt == 0 {
		log.Printf("[agents] parent lifecycle delivery failed; results remain pending and will be retried")
	}
	timer := time.NewTimer(delays[min(attempt, len(delays)-1)])
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.shutdownCh:
		return false
	}
}

// deliverAgentWake returns false for retryable host/provider failures. Busy
// foreign leases and outstanding remote children are reconciled periodically.
func (s *serveServer) deliverAgentWake(parent string) bool {
	// Automatic UI turns belong to a web host, not a jobs-only/API-only peer
	// sharing its store. Explicit API inference remains available on all hosts.
	if s == nil || !s.cfg.ui {
		return true
	}
	store := session.AsAgentRunDeliveryStore(s.store)
	if !s.waitForIdleAgentParent(parent) {
		return true // shutdown or explicit human stop, not a provider failure
	}
	ctx := context.Background()
	meta, err := s.store.Get(ctx, parent)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, session.ErrNotFound) {
		return true // Deletion permanently retires this parent's delivery worker.
	}
	if err != nil {
		return false
	}
	if !s.ownsAgentWakeParent(meta) {
		return true // Never resurrect a deleted/archived/non-web parent.
	}
	pending, barrier, err := pendingWebAgentWake(ctx, store, parent)
	if err != nil {
		log.Printf("[agents] list parent events for %s: %v", parent, err)
		return false
	}
	if len(pending) == 0 {
		return true
	}
	// Reloading a session uses its persisted agent, provider, permissions, and
	// workspace through the same runtime selection as a normal web follow-up.
	rt, _, err := s.runtimeForRequest(ctx, parent)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			return true // periodic reconciliation handles foreign parent ownership
		}
		log.Printf("[agents] restore parent %s: %v", parent, err)
		return false
	}
	if rt == nil || rt.platform != "web" {
		return true
	}
	previous := strings.TrimSpace(rt.getLastResponseID())
	if previous == "" {
		previous = s.latestDurableResponseIDForSession(ctx, parent)
	}
	// Reuse the durable synthetic-prompt marker: provider-visible user content,
	// never user-authored speech and never developer authority for child data.
	msg := tools.AgentLifecycleMessage(pending, barrier)
	request := responsesCreateRequest{Model: rt.defaultModel}
	s.prepareResponseRuntimePlan(ctx, &request, parent, false)
	llmRequest := s.buildResponsesLLMRequest(request, rt, parent, true)
	run, err := s.startResponseRun(rt, true, false, []core.Message{msg}, llmRequest, parent, startResponseRunOptions{
		previousResponseID: previous, uiSession: true, agentCompletion: true, agentEvents: pending, agentBarrier: barrier,
		notificationTurn: &session.NotificationTurnAdmission{PreviousResponseID: previous, AwaitedEvents: pending},
		admissionCheck:   func() bool { return s.agentWakeStillPending(ctx, parent, pending, barrier) },
	})
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			return true
		}
		log.Printf("[agents] parent wake %s: %v", parent, err)
		return false
	}
	if run == nil {
		return false
	}
	if !s.waitAgentResponseSettled(run) {
		return false
	}
	run.mu.Lock()
	completed := run.status == "completed" && !run.cancelRequested
	stopped := run.cancelRequested || run.status == "cancelled"
	run.mu.Unlock()
	if stopped {
		return true
	}
	if !completed {
		return false
	}

	// Retry ACK, not inference, after successful provider execution. The
	// execution finalizer already attempted this; a transient DB failure must
	// not cause a second synthesis of the same successful lifecycle turn.
	for attempt := 0; ; attempt++ {
		if err := tools.AcknowledgeAgentEvents(ctx, store, pending); err == nil {
			break
		} else {
			log.Printf("[agents] retry acknowledge parent %s: %v", parent, err)
		}
		if !agentEventsNeedAcknowledgment(ctx, store, pending) {
			return true // collection, a newer generation, or human stop won
		}
		if !s.waitAgentWakeRetry(attempt) {
			return true
		}
	}
	return true
}

func agentEventsNeedAcknowledgment(ctx context.Context, store session.AgentRunDeliveryStore, events []session.AgentRun) bool {
	for _, event := range events {
		row, err := store.GetAgentRun(ctx, event.ID)
		if err != nil {
			return true // DB outage is not permission to discard an ACK
		}
		if row.RunGeneration == event.RunGeneration && row.NotifiedAt.IsZero() && row.CollectedAt.IsZero() && !row.WakeSuppressed && row.StopReason != "parent_stopped" {
			return true
		}
	}
	return false
}

// Persisted origin selects startup ownership, not barrier membership. A
// top-level session opened on web can be serviced here regardless of where it
// was created. Never take nested orchestration from its owning child runner.
func (s *serveServer) ownsAgentWakeParent(meta *session.Session) bool {
	// A loaded web runtime alone does not give a headless host UI ownership.
	if s == nil || !s.cfg.ui || meta == nil || meta.Archived || meta.IsSubagent || sessionIsDelegatedChild(meta) {
		return false
	}
	if meta.Origin == session.OriginWeb {
		return true
	}
	if s.sessionMgr != nil {
		rt, ok := s.sessionMgr.Get(meta.ID)
		return ok && rt != nil && rt.platform == "web"
	}
	return false
}

func pendingWebAgentWake(ctx context.Context, store session.AgentRunDeliveryStore, parent string) ([]session.AgentRun, bool, error) {
	// Eager notifications are a separate compatibility path, never barrier
	// membership. Prefer the complete barrier when it is ready.
	eager, err := tools.PendingAgentEvents(ctx, store, parent)
	if err != nil {
		return nil, false, err
	}
	barrier, err := tools.PendingAgentBarrier(ctx, store, parent)
	if err != nil {
		return nil, false, err
	}
	if len(barrier) > 0 {
		return barrier, true, nil
	}
	return eager, false, nil
}

func (s *serveServer) waitForIdleAgentParent(parent string) bool {
	for {
		if s.shutdownCh != nil {
			select {
			case <-s.shutdownCh:
				return false
			default:
			}
		}
		mgr := s.ensureResponseRuns()
		id := mgr.activeRunID(parent)
		if id == "" {
			return true
		}
		run, ok := mgr.get(id)
		if !ok || !s.waitAgentResponseSettled(run) {
			return false
		}
		run.mu.Lock()
		stopped := run.cancelRequested || run.status == "cancelled"
		run.mu.Unlock()
		if stopped {
			return false
		}
	}
}

func (s *serveServer) waitAgentResponseSettled(run *responseRun) bool {
	if run == nil || run.settled == nil {
		return false
	}
	if s.shutdownCh == nil {
		<-run.settled
		return true
	}
	select {
	case <-run.settled:
		return true
	case <-s.shutdownCh:
		return false
	}
}

// reconcileAgentWakes runs at startup on the existing response lifecycle
// path, not on a second scheduler. Only proven-dead owners yield restart
// interruption events.
func (s *serveServer) reconcileAgentWakes(ctx context.Context) {
	// Leave shared durable events pending for a host that can present UI input.
	if s == nil || !s.cfg.ui {
		return
	}
	s.agentWakeMu.Lock()
	restoring := s.restoringWebRuns
	s.agentWakeMu.Unlock()
	if restoring {
		return
	}
	store := session.AsAgentRunDeliveryStore(s.store)
	if store == nil {
		return
	}
	rows, err := store.ListPendingAgentRuns(ctx)
	if err != nil {
		log.Printf("[agents] reconcile pending events: %v", err)
		return
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		if seen[row.ParentSessionID] {
			continue
		}
		seen[row.ParentSessionID] = true
		// Outstanding remote owners may settle without a local callback; the
		// periodic lifecycle tick keeps scanning until their barrier opens.
		s.wakeAgentParent(row.ParentSessionID)
	}
}

// The admission check runs under the same session active-slot boundary used by
// user turns. Collection/stop in a preceding turn invalidates an old snapshot.
func (s *serveServer) agentEventsStillPending(ctx context.Context, parent string, offered []session.AgentRun) bool {
	return s.agentWakeStillPending(ctx, parent, offered, false)
}

func (s *serveServer) agentWakeStillPending(ctx context.Context, parent string, offered []session.AgentRun, barrier bool) bool {
	meta, err := s.store.Get(ctx, parent)
	if err != nil || !s.ownsAgentWakeParent(meta) {
		return false
	}
	store := session.AsAgentRunDeliveryStore(s.store)
	var current []session.AgentRun
	if barrier {
		current, err = tools.PendingAgentBarrier(ctx, store, parent)
	} else {
		current, err = tools.PendingAgentEvents(ctx, store, parent)
	}
	if err != nil {
		return false
	}
	for _, event := range offered {
		found := false
		for _, row := range current {
			if row.ID == event.ID && row.RunGeneration == event.RunGeneration {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(offered) > 0
}
