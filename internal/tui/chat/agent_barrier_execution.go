package chat

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tui/chat/agentbarrier"
)

// Renew through settlement, independently of human cancellation.
func (m *Model) agentBarrierSettlementContext(lease *turnLease, sessionID, runID string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(m.mainRunManager.ctx)
	lease.startRenewals(ctx, func() {
		cancel()
		m.mainRunManager.cancelRun(sessionID, runID, false)
	})
	return ctx, cancel
}

func (turn *agentBarrierTurn) offeredEvents() []session.AgentRun {
	if turn == nil {
		return nil
	}
	return turn.events
}

func (m *Model) persistAgentBarrierNotice(ctx context.Context, runID string, barrier *agentBarrierTurn, messages []core.Message, boundary *runboundary.Tracker) ([]core.Message, error) {
	messages, row, err := agentbarrier.PersistNotice(ctx, m.store, barrier.events, messages, boundary)
	if err == nil {
		m.setStreamingContextMessages(messages)
		if row != nil {
			_ = m.mainRunManager.DeliverUI(row.SessionID, agentBarrierNoticeMsg{runID: runID, message: *row})
		}
	}
	return messages, err
}

type agentBarrierNoticeMsg struct {
	runID   string
	message session.Message
}

func (m *Model) applyAgentBarrierNotice(notice agentBarrierNoticeMsg) {
	if notice.message.SessionID != m.SessionID() || (m.mainRunID != "" && m.mainRunID != notice.runID) {
		return
	}
	m.messagesMu.Lock()
	// A freshly attached view may already have loaded this persisted row.
	for _, row := range m.messages {
		if row.ID == notice.message.ID {
			m.messagesMu.Unlock()
			return
		}
	}
	m.messages = append(m.messages, notice.message)
	m.messagesMu.Unlock()
	m.invalidateHistoryCache()
}
