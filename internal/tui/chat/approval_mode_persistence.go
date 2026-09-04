package chat

import (
	"context"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func sessionApprovalModeFromTools(mode tools.ApprovalMode) session.SessionApprovalMode {
	switch mode {
	case tools.ModeAuto:
		return session.ApprovalModeAuto
	case tools.ModeYolo:
		return session.ApprovalModeYolo
	default:
		return session.ApprovalModePrompt
	}
}

func (m *Model) persistApprovalMode(mode tools.ApprovalMode) {
	if m == nil || m.store == nil || m.sess == nil {
		return
	}
	persistedMode := sessionApprovalModeFromTools(mode)
	if m.sess.ApprovalMode == persistedMode {
		return
	}
	m.sess.ApprovalMode = persistedMode
	_ = m.store.Update(context.Background(), m.sess)
}
