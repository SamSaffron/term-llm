package chat

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func (m *Model) ConversationID() string {
	if m == nil || m.sess == nil {
		return ""
	}
	return m.sess.ID
}

func (m *Model) StreamGeneration() uint64 {
	if m == nil {
		return 0
	}
	return m.routedGeneration.Load()
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
