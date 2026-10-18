package engine

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"strings"
)

// Steer queues a text user message to be inserted after the current turn's tool results,
// right before the next LLM turn begins. Safe to call from any goroutine.
func (e *Engine) Steer(text string) {
	e.SteerWithID("", text)
}

// SteerWithID behaves like Steer but preserves a caller-supplied stable
// identifier.
func (e *Engine) SteerWithID(id, text string) {
	_ = e.QueueSteering(QueuedSteering{
		ID:          id,
		Message:     core.UserText(text),
		DisplayText: text,
	})
}

// QueueSteering appends a structured steering to the FIFO pending queue
// and returns its stable ID. The message role is normalized to RoleUser.
func (e *Engine) QueueSteering(entry QueuedSteering) string {
	id, _ := e.QueueSteeringWithStatus(entry)
	return id
}

// QueueSteeringWithStatus reports whether the stable identity was newly
// queued for an accepting agentic run, rejected because the current/latest run
// cannot consume it, already queued, transferred to a follow-up, or committed.
func (e *Engine) QueueSteeringWithStatus(entry QueuedSteering) (string, SteeringQueueStatus) {
	e.callbackMu.Lock()
	id, status := e.queueSteeringWithStatusLocked(entry)
	e.callbackMu.Unlock()
	if status == SteeringQueueQueued {
		e.requestInlineFlush()
	}
	return id, status
}

func (e *Engine) queueSteeringWithStatusLocked(entry QueuedSteering) (string, SteeringQueueStatus) {
	if e.steeringTransition != nil {
		return entry.ID, SteeringQueueTransitioning
	}

	entry.ID = strings.TrimSpace(entry.ID)
	if entry.ID == "" {
		entry.ID = nextEngineSteeringID()
	}
	if _, claimed := e.claimedSteeringIDs[entry.ID]; claimed {
		return entry.ID, SteeringQueueFollowUpOwned
	}
	if _, committed := e.committedSteeringIDs[entry.ID]; committed {
		return entry.ID, SteeringQueueCommitted
	}
	for i := range e.pendingSteering {
		if e.pendingSteering[i].ID == entry.ID {
			return entry.ID, SteeringQueueAlreadyQueued
		}
	}
	if e.steeringRunState == steeringRunNonConsuming {
		return entry.ID, SteeringQueueRunFinished
	}
	entry.Message.Role = core.RoleUser
	if strings.TrimSpace(entry.Message.ClientMessageID) == "" {
		entry.Message.ClientMessageID = entry.ID
	}
	if entry.DisplayText == "" {
		entry.DisplayText = core.MessageText(entry.Message)
		if strings.TrimSpace(entry.DisplayText) == "" {
			entry.DisplayText = core.MessageAttachmentSummary(entry.Message)
		}
	}
	entry.Status = core.SteeringQueued
	e.pendingSteering = append(e.pendingSteering, entry)
	return entry.ID, SteeringQueueQueued
}

// ClaimSteering atomically transfers queued steering to one normal
// follow-up request. IDs must be unique. If any ID is already committed or
// owned by another follow-up, no queued entry is removed.
func (e *Engine) ClaimSteering(ids []string) []SteeringClaimStatus {
	statuses := make([]SteeringClaimStatus, len(ids))
	if len(ids) == 0 {
		return statuses
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		out := make([]SteeringClaimStatus, len(ids))
		for i := range out {
			out[i] = SteeringClaimRushOwned
		}
		return out
	}

	pending := make(map[string]struct{}, len(e.pendingSteering))
	for i := range e.pendingSteering {
		pending[e.pendingSteering[i].ID] = struct{}{}
	}
	claimable := true
	claimedIDs := make(map[string]struct{}, len(ids))
	for i, rawID := range ids {
		id := strings.TrimSpace(rawID)
		_, isPending := pending[id]
		_, isClaimed := e.claimedSteeringIDs[id]
		_, isCommitted := e.committedSteeringIDs[id]
		switch {
		case id == "":
			statuses[i] = SteeringClaimNotFound
		case isPending:
			statuses[i] = SteeringClaimed
			claimedIDs[id] = struct{}{}
		case isClaimed:
			statuses[i] = SteeringClaimFollowUpOwned
			claimable = false
		case isCommitted:
			statuses[i] = SteeringClaimCommitted
			claimable = false
		default:
			statuses[i] = SteeringClaimNotFound
		}
	}
	if !claimable || len(claimedIDs) == 0 {
		return statuses
	}

	kept := e.pendingSteering[:0]
	for i := range e.pendingSteering {
		entry := e.pendingSteering[i]
		if _, claimed := claimedIDs[entry.ID]; claimed {
			e.rememberClaimedSteeringIDLocked(entry.ID)
			continue
		}
		kept = append(kept, entry)
	}
	for i := len(kept); i < len(e.pendingSteering); i++ {
		e.pendingSteering[i] = QueuedSteering{}
	}
	e.pendingSteering = kept
	return statuses
}

// ClaimSteeringEntry atomically transfers a queued steering to a normal
// follow-up request. A committed result means the engine already drained the ID
// and the caller must not submit it again.
func (e *Engine) ClaimSteeringEntry(id string) SteeringClaimStatus {
	statuses := e.ClaimSteering([]string{id})
	if len(statuses) == 0 {
		return SteeringClaimNotFound
	}
	return statuses[0]
}

// CancelSteering removes a queued, not-yet-committed steering without
// transferring its identity to follow-up ownership.
func (e *Engine) CancelSteering(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		return false
	}
	for i := range e.pendingSteering {
		if e.pendingSteering[i].ID != id {
			continue
		}
		copy(e.pendingSteering[i:], e.pendingSteering[i+1:])
		e.pendingSteering[len(e.pendingSteering)-1] = QueuedSteering{}
		e.pendingSteering = e.pendingSteering[:len(e.pendingSteering)-1]
		return true
	}
	return false
}

// ReleaseClaimedSteering ends temporary follow-up ownership. Durable
// history remains authoritative for requests that reached persistence.
func (e *Engine) ReleaseClaimedSteering(ids []string) {
	if len(ids) == 0 {
		return
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	released := make(map[string]struct{}, len(ids))
	for _, rawID := range ids {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		delete(e.claimedSteeringIDs, id)
		released[id] = struct{}{}
	}
	kept := e.claimedSteeringOrder[:0]
	for _, id := range e.claimedSteeringOrder {
		if _, drop := released[id]; !drop {
			kept = append(kept, id)
		}
	}
	for i := len(kept); i < len(e.claimedSteeringOrder); i++ {
		e.claimedSteeringOrder[i] = ""
	}
	e.claimedSteeringOrder = kept
}

// DiscardPendingSteering removes all queued, not-yet-committed
// steering and returns how many were discarded.
func (e *Engine) DiscardPendingSteering() int {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		return 0
	}
	count := len(e.pendingSteering)
	for i := range e.pendingSteering {
		e.pendingSteering[i] = QueuedSteering{}
	}
	e.pendingSteering = nil
	return count
}

// SteeringIdentityStatus reports engine ownership for a stable ID without
// mutating the queue. The boolean is false when the engine has never seen it.
func (e *Engine) SteeringIdentityStatus(id string) (SteeringQueueStatus, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		for _, entry := range e.pendingSteering {
			if entry.ID == id {
				return SteeringQueueRushOwned, true
			}
		}
	}
	if _, claimed := e.claimedSteeringIDs[id]; claimed {
		return SteeringQueueFollowUpOwned, true
	}
	if _, committed := e.committedSteeringIDs[id]; committed {
		return SteeringQueueCommitted, true
	}
	for i := range e.pendingSteering {
		if e.pendingSteering[i].ID == id {
			return SteeringQueueAlreadyQueued, true
		}
	}
	return "", false
}

// ListPendingSteering returns a snapshot of queued, cancellable steering.
func (e *Engine) ListPendingSteering() []QueuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.steeringTransition != nil {
		return nil
	}
	out := make([]QueuedSteering, len(e.pendingSteering))
	copy(out, e.pendingSteering)
	return out
}

// DrainSteeringText returns pending steering text and drains all queued
// steering. It is retained for legacy recovery paths; new callers should
// use DrainSteering or ListPendingSteering.
func (e *Engine) DrainSteeringText() string {
	entries := e.DrainSteering()
	var b strings.Builder
	for _, entry := range entries {
		text := entry.DisplayText
		if text == "" {
			text = core.MessageText(entry.Message)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// DrainSteering drains all queued steering and marks returned entries
// committed. Draining is the atomic handoff after which cancellation fails.
func (e *Engine) DrainSteering() []QueuedSteering {
	return e.drainSteering()
}

// PeekSteering returns a text summary of currently pending steering.
func (e *Engine) PeekSteering() string {
	entries := e.ListPendingSteering()
	var b strings.Builder
	for _, entry := range entries {
		text := entry.DisplayText
		if text == "" {
			text = core.MessageText(entry.Message)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

func rememberBoundedSteeringID(ids map[string]struct{}, order []string, id string) (map[string]struct{}, []string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ids, order
	}
	if ids == nil {
		ids = make(map[string]struct{})
	}
	if _, exists := ids[id]; exists {
		return ids, order
	}
	ids[id] = struct{}{}
	order = append(order, id)
	if len(order) <= steeringIdentityLimit {
		return ids, order
	}
	oldest := order[0]
	copy(order, order[1:])
	order[len(order)-1] = ""
	order = order[:len(order)-1]
	delete(ids, oldest)
	return ids, order
}

func (e *Engine) rememberClaimedSteeringIDLocked(id string) {
	e.claimedSteeringIDs, e.claimedSteeringOrder = rememberBoundedSteeringID(
		e.claimedSteeringIDs, e.claimedSteeringOrder, id,
	)
}

func (e *Engine) rememberCommittedSteeringIDLocked(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	delete(e.claimedSteeringIDs, id)
	e.committedSteeringIDs, e.committedSteeringOrder = rememberBoundedSteeringID(
		e.committedSteeringIDs, e.committedSteeringOrder, id,
	)
}

func (e *Engine) markSteeringCommittedLocked(entries []queuedSteering) {
	if len(entries) == 0 {
		return
	}
	for i := range entries {
		entries[i].Status = core.SteeringCommitted
		e.rememberCommittedSteeringIDLocked(entries[i].ID)
	}
}

// drainSteering atomically commits all queued steering.
func (e *Engine) drainSteering() []queuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	return e.drainSteeringLocked()
}

func (e *Engine) drainSteeringForNextTurn(canAcceptMore bool) []queuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	out := e.drainSteeringLocked()
	if !canAcceptMore {
		e.steeringRunState = steeringRunNonConsuming
	}
	return out
}

func (e *Engine) drainSteeringLocked() []queuedSteering {
	if e.steeringTransition != nil {
		return nil
	}
	if len(e.pendingSteering) == 0 {
		return nil
	}
	out := make([]queuedSteering, len(e.pendingSteering))
	copy(out, e.pendingSteering)
	e.markSteeringCommittedLocked(out)
	e.pendingSteering = nil
	return out
}

// BeginSteeringRunForTest marks a steering run as active without streaming.
// It lets tests in other packages exercise provider-specific steering
// availability, which depends on the concrete provider type held by the engine.
func (e *Engine) BeginSteeringRunForTest(canConsume bool) { e.beginSteeringRun(canConsume) }

func (e *Engine) beginSteeringRun(canConsume bool) {
	e.callbackMu.Lock()
	if canConsume {
		e.steeringRunState = steeringRunAccepting
	} else {
		e.steeringRunState = steeringRunNonConsuming
	}
	e.callbackMu.Unlock()
}

func (e *Engine) markSteeringRunNonConsuming() {
	e.callbackMu.Lock()
	e.steeringRunState = steeringRunNonConsuming
	e.callbackMu.Unlock()
}

// drainBoundarySteering atomically either commits every pending steer for
// another provider turn or closes this run's final agentic boundary. Explicit
// error and cancellation exits are closed by runLoop's deferred teardown.
func (e *Engine) drainBoundarySteering(canContinue, canAcceptMore bool) []queuedSteering {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()

	if !canContinue || len(e.pendingSteering) == 0 {
		e.steeringRunState = steeringRunNonConsuming
		return nil
	}
	out := e.drainSteeringLocked()
	if !canAcceptMore {
		e.steeringRunState = steeringRunNonConsuming
	}
	return out
}

func (e *Engine) continueWithSteering(ctx context.Context, send corestream.Sender, req *core.Request, turnCallback TurnCompletedCallback, attempt int, finalMsg core.Message, canContinue, canAcceptMore bool) (bool, error) {
	steering := e.drainBoundarySteering(canContinue, canAcceptMore)
	if len(steering) == 0 {
		return false, nil
	}
	if len(finalMsg.Parts) > 0 {
		req.Messages = append(req.Messages, finalMsg)
	}
	steeringMsgs := make([]core.Message, 0, len(steering))
	for _, steering := range steering {
		steeringMsg := steering.Message
		steeringMsg.Role = core.RoleUser
		req.Messages = append(req.Messages, steeringMsg)
		steeringMsgs = append(steeringMsgs, steeringMsg)
	}
	if turnCallback != nil {
		cbCtx, cancel := callbackContext(ctx)
		_ = turnCallback(cbCtx, attempt, steeringMsgs, core.TurnMetrics{})
		cancel()
	}
	for _, steering := range steering {
		text := steering.DisplayText
		if text == "" {
			text = core.MessageText(steering.Message)
		}
		if err := send.Send(core.Event{Type: core.EventSteering, Text: text, SteeringID: steering.ID, Message: steering.Message, SteeringStatus: core.SteeringCommitted}); err != nil {
			return false, err
		}
	}
	return true, nil
}
