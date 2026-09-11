package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func (m *Model) handleCompactDone(msg compactDoneMsg) (tea.Model, tea.Cmd) {
	m.streaming = false
	m.phase = "Thinking"
	m.releaseStreamCancelFunc()
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
			return m.showFooterMuted("Compaction cancelled.")
		}
		return m.showFooterError(fmt.Sprintf("Compaction failed: %v", msg.err))
	}
	if msg.result == nil {
		return m.showFooterError("Compaction failed: no result returned.")
	}
	if m.engine != nil {
		sessionID := sessionIDOf(m.sess)
		var toolSpecs []llm.ToolSpec
		for _, specName := range m.localTools {
			if tool, ok := m.engine.Tools().Get(specName); ok {
				toolSpecs = append(toolSpecs, tool.Spec())
			}
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		err := m.engine.PrepareCompactionContext(restoreCtx, sessionID, toolSpecs, msg.result)
		cancel()
		if err != nil {
			slog.Warn("plan restoration after manual compaction failed; continuing without it", "error", err)
		}
	}
	m.messagesMu.Lock()
	full := append([]session.Message(nil), m.messages...)
	m.messagesMu.Unlock()
	var updated []session.Message
	var activeStart int
	var refreshed *session.Session
	if m.store != nil {
		var err error
		updated, activeStart, refreshed, err = session.ApplyCompaction(context.Background(), m.store, m.sess, full, msg.result)
		if err != nil {
			return m.showFooterError(fmt.Sprintf("Compaction finished, but saving failed: %v", err))
		}
	} else {
		updated, activeStart, refreshed, _ = session.ApplyCompaction(context.Background(), nil, m.sess, full, msg.result)
	}
	m.setStreamingContextMessages(msg.result.ActiveMessages())
	m.applyCompactionToUI(compactionAppliedMsg{
		sessionID:   sessionIDOf(m.sess),
		messages:    updated,
		activeStart: activeStart,
		refreshed:   refreshed,
		model:       msg.result.Model,
		usage:       msg.result.Usage,
	})
	m.resetPendingAssistantAfterCompaction()

	if m.engine != nil {
		m.engine.ResetConversation()
		m.engine.SetContextEstimateBaseline(0, 0)
	}
	return m.showFooterSuccess("Conversation compacted.")
}
