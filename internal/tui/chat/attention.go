package chat

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
)

const attentionPresenceLead = 60 * time.Second

// attentionFinalizedMsg carries the exact terminal watermark from a lease's
// worker to the visible model. Only Update may decide whether the user saw it.
type attentionFinalizedMsg struct {
	sessionID string
	state     session.AttentionState
	store     session.AttentionStore
}

type pendingAttentionAck struct {
	sessionID       string
	storeInstanceID string
	throughSeq      int64
	store           session.AttentionStore
}

func (m *Model) presenceTime() time.Time {
	if m.attentionNow != nil {
		return m.attentionNow()
	}
	return time.Now()
}

func (m *Model) attentionSessionID() string {
	// A transition hides the outgoing transcript before its replacement is ready.
	if m.sessionTransition != nil || m.quitting {
		return ""
	}
	return m.SessionID()
}

// transcriptHidden reports whether a full-screen view or transition covers the
// displayed conversation, so input there is not evidence of reading it.
func (m *Model) transcriptHidden() bool {
	return m.sessionTransition != nil || m.quitting || m.inspectorMode || m.worktreeBrowserMode || m.resumeBrowserMode
}

// attentionAckFailedMsg returns a watermark whose acknowledgement failed for a
// transient reason so the next presence signal can retry it.
type attentionAckFailedMsg struct {
	pending pendingAttentionAck
}

func (m *Model) dropHiddenAttention() {
	if m.pendingAttention != nil && m.pendingAttention.sessionID != m.attentionSessionID() {
		m.pendingAttention = nil
	}
}

// updateAttention runs before input routing, including embedded dialogs/views.
// The returned command is batched with whatever command the input produces.
func (m *Model) updateAttention(message tea.Msg) tea.Cmd {
	m.dropHiddenAttention()
	presence := false
	switch msg := message.(type) {
	case tea.FocusMsg:
		m.focusKnown, m.focused = true, true
		presence = true
	case tea.BlurMsg:
		m.focusKnown, m.focused = true, false
	case tea.KeyPressMsg:
		presence = true
	case tea.MouseMsg:
		presence = m.mouseMode && m.autoSendQueue == nil
	case attentionFinalizedMsg:
		return m.finalizedAttentionCmd(msg)
	case attentionAckFailedMsg:
		if m.pendingAttention == nil && msg.pending.sessionID == m.attentionSessionID() {
			m.pendingAttention = &msg.pending
		}
		return nil
	}
	if !presence {
		return nil
	}
	m.lastPresenceAt = m.presenceTime()
	if m.externalProcessActive || m.transcriptHidden() || (m.focusKnown && !m.focused) {
		return nil
	}
	return m.takeAttentionAckCmd()
}

func (m *Model) finalizedAttentionCmd(msg attentionFinalizedMsg) tea.Cmd {
	if !msg.state.Changed || msg.store == nil || msg.sessionID != m.attentionSessionID() {
		return nil
	}
	// Delivery from different workers can be reordered. Never replace a newer
	// pending watermark with an older one from the same store.
	if pending := m.pendingAttention; pending != nil && pending.storeInstanceID == msg.state.StoreInstanceID && pending.throughSeq >= msg.state.LatestAttentionSeq {
		return nil
	}
	m.pendingAttention = &pendingAttentionAck{
		sessionID:       msg.sessionID,
		storeInstanceID: msg.state.StoreInstanceID,
		throughSeq:      msg.state.LatestAttentionSeq,
		store:           msg.store,
	}
	if m.externalProcessActive || m.transcriptHidden() || (m.focusKnown && !m.focused) || m.lastPresenceAt.IsZero() || m.lastPresenceAt.Before(msg.state.TerminalAt.Add(-attentionPresenceLead)) {
		return nil
	}
	return m.takeAttentionAckCmd()
}

func (m *Model) takeAttentionAckCmd() tea.Cmd {
	pending := m.pendingAttention
	if pending == nil {
		return nil
	}
	m.pendingAttention = nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), turnLeaseReleaseTimeout)
		defer cancel()
		_, err := pending.store.MarkAttentionSeenFrom(ctx, pending.sessionID, pending.storeInstanceID, pending.throughSeq, session.AttentionSourceTUI)
		// A conflict or missing session means the marker is stale; only
		// transient failures are worth retrying on the next presence signal.
		if err != nil && !errors.Is(err, session.ErrAttentionConflict) && !errors.Is(err, session.ErrNotFound) {
			return attentionAckFailedMsg{pending: *pending}
		}
		return nil
	}
}
