package serve

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func telegramStreamContext(ctx context.Context, sess *telegramSession, chatID int64) (context.Context, string) {
	if sess.meta == nil || sess.meta.ID == "" {
		return ctx, ""
	}
	return tools.ContextWithQueueAgentOrigin(ctx, tools.QueueAgentOriginContext{
		Origin: tools.QueueAgentOriginTelegram, SessionID: sess.meta.ID, TelegramChatID: chatID,
	}), sess.meta.ID
}

func initialTelegramActiveHistory(sess *telegramSession, user llm.Message, resume *telegramContinuation) []llm.Message {
	if sess.activeHistory == nil {
		return nil
	}
	history := append([]llm.Message{}, sess.activeHistory...)
	if resume == nil {
		history = append(history, normalizeUserMessageForHistory(user))
	}
	return history
}

func (m *telegramSessionMgr) persistTurnStart(ctx context.Context, sess *telegramSession, user llm.Message, userText string, resume *telegramContinuation) (degraded, includeSystemPromptOnReconcile bool) {
	includeSystemPromptOnReconcile = sess.systemPromptPersisted
	if resume != nil || m.store == nil || sess.meta == nil {
		return false, includeSystemPromptOnReconcile
	}
	if m.settings.SystemPrompt != "" && !sess.systemPromptPersisted {
		includeSystemPromptOnReconcile = true
		system := &session.Message{SessionID: sess.meta.ID, Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: m.settings.SystemPrompt}}, TextContent: m.settings.SystemPrompt, CreatedAt: time.Now(), Sequence: -1}
		if m.runStoreOp(ctx, sess.meta.ID, "AddMessage(system)", func(storeCtx context.Context) error {
			return m.store.AddMessage(storeCtx, sess.meta.ID, system)
		}) {
			sess.systemPromptPersisted = true
		} else {
			degraded = true
		}
	}
	if start, ok := llm.ConversationStartFrom(sess.history); ok && !sess.conversationStartPersisted {
		startMsg := session.NewMessage(sess.meta.ID, start, -1)
		if m.runStoreOp(ctx, sess.meta.ID, "AddMessage(conversation_start)", func(storeCtx context.Context) error {
			return m.store.AddMessage(storeCtx, sess.meta.ID, startMsg)
		}) {
			sess.conversationStartPersisted = true
		} else {
			degraded = true
		}
	}
	storeUser := &session.Message{SessionID: sess.meta.ID, Role: llm.RoleUser, Parts: user.Parts, TextContent: userText, CreatedAt: time.Now(), Sequence: -1}
	if !m.runStoreOp(ctx, sess.meta.ID, "AddMessage(user)", func(storeCtx context.Context) error {
		return m.store.AddMessage(storeCtx, sess.meta.ID, storeUser)
	}) {
		degraded = true
	}
	m.runStoreOp(ctx, sess.meta.ID, "IncrementUserTurns", func(storeCtx context.Context) error {
		return m.store.IncrementUserTurns(storeCtx, sess.meta.ID)
	})
	if sess.meta.Summary == "" {
		sess.meta.Summary = session.TruncateSummary(userText)
		m.runStoreOp(ctx, sess.meta.ID, "Update(summary)", func(storeCtx context.Context) error {
			return m.store.Update(storeCtx, sess.meta)
		})
	}
	m.runStoreOp(ctx, sess.meta.ID, "SetCurrent", func(storeCtx context.Context) error {
		return m.store.SetCurrent(storeCtx, sess.meta.ID)
	})
	m.runStoreOp(ctx, sess.meta.ID, "UpdateStatus(active)", func(storeCtx context.Context) error {
		return m.store.UpdateStatus(storeCtx, sess.meta.ID, session.StatusActive)
	})
	return degraded, includeSystemPromptOnReconcile
}

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
