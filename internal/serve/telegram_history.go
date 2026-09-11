package serve

import (
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// prepareRequestHistory builds provider context while keeping the durable full
// transcript on sess.history. It intentionally consumes one-shot carryover and
// establishes time grounding before a continuation overrides provider messages.
func (m *telegramSessionMgr) prepareRequestHistory(sess *telegramSession, user llm.Message, resume *telegramContinuation, now time.Time) []llm.Message {
	if m.settings.TimeGrounding {
		sess.history = llm.BeginConversation(sess.history, now)
		if sess.activeHistory != nil {
			sess.activeHistory = llm.InsertConversationStart(sess.activeHistory, sess.history)
		}
	}
	history := sess.history
	if sess.activeHistory != nil {
		history = sess.activeHistory
	}
	messages := make([]llm.Message, 0, len(history)+3)
	if m.settings.SystemPrompt != "" && !containsSystemMsg(history) {
		messages = append(messages, llm.SystemText(m.settings.SystemPrompt))
	}
	if sess.carryoverContext != "" {
		label := sess.carryoverContextLabel
		if label == "" {
			label = "Context from previous session (tail):"
		}
		messages = append(messages, llm.SystemText(label+"\n"+sess.carryoverContext))
		sess.carryoverContext, sess.carryoverContextLabel = "", ""
	}
	if text := m.settings.PlatformMessages.For("telegram"); text != "" {
		messages = append(messages, llm.PlatformContextMessage(text))
	}
	messages = append(messages, history...)
	messages = append(messages, user)
	if resume != nil {
		messages = resume.Engine.Request.Messages
	}
	return messages
}
