package chat

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
)

func (m *Model) handleTitleFallbackTick(msg titleFallbackTickMsg) (tea.Model, tea.Cmd) {
	if m.sess != nil && msg.sessionID == m.sess.ID {
		if cmd := m.maybeGenerateSessionTitleCmd(); cmd != nil {
			return m, cmd
		}
	}
	return m, nil
}

func (m *Model) handleTitleGenerated(msg titleGeneratedMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID == m.titleGenerationSessionID {
		m.titleGenerationInFlight = false
	}
	if msg.sessionID == "" || m.sess == nil || msg.sessionID != m.sess.ID {
		return m, nil
	}
	if msg.err != nil {
		if msg.force {
			return m.showFooterError(fmt.Sprintf("Title generation failed: %v", msg.err))
		}
		return m, nil
	}
	if msg.force {
		if msg.manualEditVersion != m.titleManualEditVersion {
			return m, nil
		}
		if msg.clearManualName {
			m.sess.Name = ""
		} else if strings.TrimSpace(m.sess.Name) != "" || m.sess.TitleSource == session.TitleSourceUser {
			return m, nil
		}
		m.sess.GeneratedShortTitle = msg.candidate.ShortTitle
		m.sess.GeneratedLongTitle = msg.candidate.LongTitle
		m.sess.TitleSource = session.TitleSourceGenerated
		m.sess.TitleGeneratedAt = msg.generatedAt
		m.sess.TitleBasisMsgSeq = msg.basisMsgSeq
		if m.store != nil {
			var err error
			if msg.clearManualName {
				err = m.store.Update(context.Background(), m.sess)
			} else {
				err = session.UpdateGeneratedTitle(context.Background(), m.store, m.sess, msg.candidate.ShortTitle, msg.candidate.LongTitle, msg.generatedAt, msg.basisMsgSeq)
			}
			if err != nil {
				return m.showFooterError(fmt.Sprintf("Failed to update title: %v", err))
			}
		}
		updated, footerCmd := m.showFooterSuccess(fmt.Sprintf("Updated title: %s", msg.candidate.ShortTitle))
		return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
	}
	if strings.TrimSpace(m.sess.GeneratedShortTitle) == "" {
		m.sess.GeneratedShortTitle = msg.candidate.ShortTitle
		m.sess.GeneratedLongTitle = msg.candidate.LongTitle
		m.sess.TitleSource = session.TitleSourceGenerated
		m.sess.TitleGeneratedAt = msg.generatedAt
		m.sess.TitleBasisMsgSeq = msg.basisMsgSeq
	}
	return m, m.terminalTitleCmd()
}
