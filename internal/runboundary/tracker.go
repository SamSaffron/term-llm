// Package runboundary tracks the provider-complete and durably branchable
// prefixes of one active model run.
package runboundary

import (
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

// Snapshot is an immutable view of one run boundary. Durable is deliberately
// separate from DurableAnchorID: row ID zero is root branching, not an
// unavailable active-run boundary.
type Snapshot struct {
	RunID           string
	TurnIndex       int
	Messages        []llm.Message
	DurableAnchorID int64
	Durable         bool
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
}

// New initializes a tracker. An initial durable row is published only when
// durable is true and durableAnchorID is positive.
func New(runID string, messages []llm.Message, durableAnchorID int64, durable bool) *Tracker {
	t := &Tracker{}
	t.Reset(runID, messages, durableAnchorID, durable)
	return t
}

// Reset starts ownership of a new run and invalidates callbacks carrying the
// previous run identity.
func (t *Tracker) Reset(runID string, messages []llm.Message, durableAnchorID int64, durable bool) {
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

// Commit records one provider-completed turn. Duplicate and out-of-order
// callbacks are rejected so completed progress is monotonic within the run.
func (t *Tracker) Commit(runID string, turnIndex int, messages []llm.Message) bool {
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
// that may have replaced the published row identity.
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
	return Snapshot{RunID: t.runID, TurnIndex: t.completedTurn, Messages: cloneMessages(t.completed), DurableAnchorID: t.durableAnchorID, Durable: t.durable}
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
