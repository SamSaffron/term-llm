// Package runboundary tracks the provider-complete and durably branchable
// prefixes of one active model run.
package runboundary

import (
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ProviderContext is the provider-side state captured with one boundary. It is
// one value with one owner rather than independent fields, because eligibility
// to branch from a boundary depends on provider and model identity as much as on
// the exported state itself: new identity must never end up attached to old
// state.
//
// State is opaque transport state from llm.ProviderStateExporter. The invariant
// that makes it safe is that it is never *ahead* of the messages captured with
// it; being behind is harmless, because the consumer re-delivers from the
// recorded offset.
type ProviderContext struct {
	State       []byte `json:"state,omitempty"`
	WorkingDir  string `json:"working_dir,omitempty"`
	ProviderKey string `json:"provider_key,omitempty"`
	Model       string `json:"model,omitempty"`
}

// Available reports whether the captured context can be used to branch.
func (c ProviderContext) Available() bool {
	return len(c.State) > 0
}

// MatchesProviderModel reports whether a prospective helper request targets the
// same provider and model the captured state belongs to.
func (c ProviderContext) MatchesProviderModel(providerKey, model string) bool {
	return c.ProviderKey == providerKey && c.Model == model
}

func (c ProviderContext) clone() ProviderContext {
	c.State = append([]byte(nil), c.State...)
	return c
}

// Snapshot is an immutable view of one run boundary. Durable is deliberately
// separate from DurableAnchorID: row ID zero is root branching, not an
// unavailable active-run boundary.
type Snapshot struct {
	RunID           string
	TurnIndex       int
	Messages        []llm.Message
	DurableAnchorID int64
	Durable         bool
	Provider        ProviderContext
}

// Tracker owns one run's live provider context and completed boundary.
type Tracker struct {
	mu sync.RWMutex

	runID            string
	live             []llm.Message
	pendingAssistant bool
	completed        []llm.Message
	completedTurn    int
	durableAnchorID  int64
	durable          bool
	provider         ProviderContext
}

// New initializes a tracker. An initial durable row is published only when
// durable is true and durableAnchorID is positive. provider is the state
// captured before the run's first provider turn, so the first turn of a run can
// still branch from the previous run's session.
func New(runID string, messages []llm.Message, durableAnchorID int64, durable bool, provider ProviderContext) *Tracker {
	t := &Tracker{}
	t.Reset(runID, messages, durableAnchorID, durable, provider)
	return t
}

// Reset starts ownership of a new run and invalidates callbacks carrying the
// previous run identity.
func (t *Tracker) Reset(runID string, messages []llm.Message, durableAnchorID int64, durable bool, provider ProviderContext) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.runID = runID
	t.live = cloneMessages(messages)
	t.pendingAssistant = false
	t.completed = cloneMessages(messages)
	t.completedTurn = -1
	t.durableAnchorID = durableAnchorID
	t.durable = durable && durableAnchorID > 0
	t.provider = provider.clone()
	t.mu.Unlock()
}

// RunID returns the currently owned run identity.
func (t *Tracker) RunID() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.runID
}

// UpdateAssistant replaces the pending assistant in live context. It never
// advances the completed or durable boundary.
func (t *Tracker) UpdateAssistant(runID string, assistant llm.Message) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if runID != t.runID {
		return false
	}
	if t.pendingAssistant && len(t.live) > 0 {
		t.live[len(t.live)-1] = cloneMessage(assistant)
	} else {
		t.live = append(t.live, cloneMessage(assistant))
		t.pendingAssistant = true
	}
	return true
}

// Commit records one provider-completed turn together with the provider state
// captured at the same instant. Duplicate and out-of-order callbacks are
// rejected so completed progress is monotonic within the run.
//
// Capturing both here is what keeps state from being published ahead of the
// messages it belongs to: a rejected or out-of-order commit leaves the previous
// pair intact rather than pairing new state with old messages.
func (t *Tracker) Commit(runID string, turnIndex int, messages []llm.Message, provider ProviderContext) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if runID != t.runID || turnIndex <= t.completedTurn {
		return false
	}
	appendStart := 0
	if len(messages) > 0 && messages[0].Role == llm.RoleAssistant {
		if t.pendingAssistant && len(t.live) > 0 {
			t.live[len(t.live)-1] = cloneMessage(messages[0])
		} else {
			t.live = append(t.live, cloneMessage(messages[0]))
		}
		appendStart = 1
	}
	for _, message := range messages[appendStart:] {
		t.live = append(t.live, cloneMessage(message))
	}
	t.pendingAssistant = false
	t.completed = cloneMessages(t.live)
	t.completedTurn = turnIndex
	// A turn completion that carries no exported state (engine steering injection
	// and error recovery both publish messages without a provider advance) keeps
	// the previous pair. That state is behind the new messages, which is the safe
	// direction: the consumer re-delivers from the recorded offset.
	if provider.Available() {
		t.provider = provider.clone()
	}
	return true
}

// PublishDurable associates a successfully persisted row with the matching
// completed turn. A partial or stale persistence completion cannot advance it.
func (t *Tracker) PublishDurable(runID string, turnIndex int, anchorID int64) bool {
	if t == nil || anchorID <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if runID != t.runID || turnIndex != t.completedTurn {
		return false
	}
	t.durableAnchorID = anchorID
	t.durable = true
	return true
}

// SetInitialDurable publishes the persisted input boundary before turn zero.
func (t *Tracker) SetInitialDurable(runID string, anchorID int64) bool {
	if t == nil || anchorID <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if runID != t.runID || t.completedTurn >= 0 {
		return false
	}
	t.durableAnchorID, t.durable = anchorID, true
	return true
}

// InvalidateDurable fails active branching closed after a persistence operation
// that may have replaced the published row identity. It deliberately leaves the
// captured provider context alone: whether that state can still be branched from
// is decided by the provider seam against the request, not by row durability.
func (t *Tracker) InvalidateDurable(runID string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if runID != t.runID {
		return false
	}
	t.durableAnchorID, t.durable = 0, false
	return true
}

// LiveSnapshot returns context suitable for estimation, including a pending
// assistant when one exists.
func (t *Tracker) LiveSnapshot() []llm.Message {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneMessages(t.live)
}

// CompletedSnapshot returns provider-complete context for side questions.
func (t *Tracker) CompletedSnapshot() Snapshot {
	if t == nil {
		return Snapshot{TurnIndex: -1}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return Snapshot{
		RunID: t.runID, TurnIndex: t.completedTurn, Messages: cloneMessages(t.completed),
		DurableAnchorID: t.durableAnchorID, Durable: t.durable, Provider: t.provider.clone(),
	}
}

func cloneMessages(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, len(messages))
	for i := range messages {
		out[i] = cloneMessage(messages[i])
	}
	return out
}

func cloneMessage(message llm.Message) llm.Message {
	copy := message
	copy.Parts = append([]llm.Part(nil), message.Parts...)
	return copy
}
