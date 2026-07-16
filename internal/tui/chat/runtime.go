package chat

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func (m *Model) ConversationID() string {
	if m == nil || m.sess == nil {
		return ""
	}
	return m.sess.ID
}

// SetRuntimeRoutingID installs the immutable address used by asynchronous
// runtime callbacks. Unlike the persisted session ID, this identity survives
// /clear and /new replacing the session owned by a running model.
func (m *Model) SetRuntimeRoutingID(id string) {
	if m == nil || strings.TrimSpace(id) == "" {
		return
	}
	if current, _ := m.runtimeRoutingID.Load().(string); current == "" {
		m.runtimeRoutingID.Store(strings.TrimSpace(id))
	}
}

func (m *Model) RuntimeRoutingID() string {
	if m == nil {
		return ""
	}
	if id, _ := m.runtimeRoutingID.Load().(string); id != "" {
		return id
	}
	return ""
}

func (m *Model) StreamGeneration() uint64 {
	if m == nil {
		return 0
	}
	return m.routedGeneration.Load()
}

// advanceRoutingGeneration invalidates commands and interactive events queued
// for the session being replaced by /clear or /new.
func (m *Model) advanceRoutingGeneration() {
	if m == nil {
		return
	}
	m.streamGeneration++
	m.routedGeneration.Store(m.streamGeneration)
}

func (m *Model) EnableConversationNavigation(enabled bool) {
	if m != nil {
		m.conversationNavigation = enabled
	}
}

// SetRuntimeCancel transfers ownership of the runtime root context to the model.
func (m *Model) SetRuntimeCancel(cancel context.CancelFunc) {
	if m != nil {
		m.runtimeCancel = cancel
	}
}

func (m *Model) SetParentRuntimeStatus(status string) {
	if m != nil {
		m.parentRuntimeStatus = strings.TrimSpace(status)
	}
}

func (m *Model) SetSideRuntimeStatus(status string) {
	if m != nil {
		m.sideRuntimeStatus = strings.TrimSpace(status)
	}
}

// QueueAutoSend delivers a one-shot prompt to an already initialized runtime.
// It must be called by the host update loop, which owns the model's UI state.
func (m *Model) QueueAutoSend(text string) tea.Cmd {
	if m == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	m.textarea.SetValue(strings.TrimSpace(text))
	m.updateTextareaHeight()
	return func() tea.Msg { return autoSendMsg{} }
}

// RuntimeStatus summarizes background progress without exposing or rendering
// the inactive model's viewport.
func (m *Model) RuntimeStatus() string {
	if m == nil {
		return "closed"
	}
	switch {
	case m.approvalModel != nil || m.approvalDoneCh != nil:
		return "needs approval"
	case m.askUserModel != nil || m.askUserDoneCh != nil:
		return "needs input"
	case m.handoverPreview != nil || m.pendingHandover != nil || m.handoverToolDoneCh != nil:
		return "needs input"
	case m.streaming || m.streamCancelFunc != nil:
		return "running"
	case m.err != nil:
		return "failed"
	case m.sess != nil && m.sess.Status == session.StatusError:
		return "failed"
	default:
		return "done"
	}
}

// Shutdown resolves all pending interaction callers before cancelling this
// model's root context. It intentionally touches no resources owned by another
// conversation.
func (m *Model) Shutdown() {
	if m == nil {
		return
	}
	_, _ = m.cancelActiveForInterrupt()
	if m.runtimeCancel != nil {
		m.runtimeCancel()
		m.runtimeCancel = nil
	}
	m.WaitStreamDone()
	if m.mcpManager != nil {
		m.mcpManager.StopAll()
	}
	if cleaner, ok := m.provider.(llm.ProviderCleaner); ok {
		cleaner.CleanupMCP()
	}
}
