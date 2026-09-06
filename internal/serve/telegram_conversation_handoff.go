package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

// telegramConversationState preserves the live conversation, including history
// carried from earlier sessions that is deliberately absent from this session's
// transcript. This is not the lossy history used for ordinary /reset carryover.
// Active execution/delivery state must be sealed separately before replacement.
type telegramConversationState struct {
	Execution             *telegramExecutionState `json:"execution,omitempty"`
	ChatID                int64                   `json:"chat_id"`
	SessionID             string                  `json:"session_id"`
	Revision              int64                   `json:"revision"`
	History               []llm.Message           `json:"history"`
	SystemPromptPersisted bool                    `json:"system_prompt_persisted"`
	CarryoverContext      string                  `json:"carryover_context,omitempty"`
	CarryoverContextLabel string                  `json:"carryover_context_label,omitempty"`
	CarryoverMessageCount int                     `json:"carryover_message_count"`
	LastActivity          time.Time               `json:"last_activity"`
}

func (m *telegramSessionMgr) snapshotConversation(ctx context.Context, chatID int64, sess *telegramSession) (json.RawMessage, error) {
	if !m.restartGate.Drained() {
		return nil, fmt.Errorf("Telegram work has not settled")
	}
	if sess == nil || !sess.mu.TryLock() {
		return nil, fmt.Errorf("Telegram conversation is busy")
	}
	defer sess.mu.Unlock()
	if sess.meta == nil || sess.runtimeStale.Load() {
		return nil, fmt.Errorf("Telegram conversation cannot be checkpointed")
	}
	index, ok := m.store.(session.TranscriptIndexer)
	if !ok {
		return nil, fmt.Errorf("Telegram store does not support transcript fencing")
	}
	sess.cancelMu.Lock()
	control := sess.restartControl
	sess.cancelMu.Unlock()
	execution := sess.restoredExecution
	if control != nil {
		var err error
		execution, err = control.executionState()
		if err != nil {
			return nil, err
		}
	}
	if execution != nil && !m.reconcileTelegramTranscript(ctx, sess, sess.history, sess.systemPromptPersisted, "ReplaceMessages(restart_seal)") {
		return nil, fmt.Errorf("Telegram interrupted transcript could not be sealed")
	}
	revision, err := index.TranscriptRev(ctx, sess.meta.ID)
	if err != nil {
		return nil, err
	}
	sess.activityMu.Lock()
	lastActivity := sess.lastActivity
	sess.activityMu.Unlock()
	return json.Marshal(telegramConversationState{Execution: execution, ChatID: chatID, SessionID: sess.meta.ID, Revision: revision, History: sess.history, SystemPromptPersisted: sess.systemPromptPersisted, CarryoverContext: sess.carryoverContext, CarryoverContextLabel: sess.carryoverContextLabel, CarryoverMessageCount: sess.carryoverMessageCount, LastActivity: lastActivity})
}

// restoreConversation constructs, but does not publish, an exact session. The
// caller must first authenticate/consume the durable one-shot process handoff,
// and keep platform admission closed until all conversations have been restored.
func (m *telegramSessionMgr) restoreConversation(ctx context.Context, raw json.RawMessage) (*telegramSession, int64, error) {
	var state telegramConversationState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, 0, err
	}
	if state.SessionID == "" || state.CarryoverMessageCount < 0 || state.CarryoverMessageCount > len(state.History) {
		return nil, 0, fmt.Errorf("invalid Telegram conversation checkpoint")
	}
	if state.Execution != nil && (state.Execution.RemainingTurns < 0 || state.Execution.Reply == nil || state.Execution.Reply.MessageID <= 0) {
		return nil, 0, fmt.Errorf("invalid Telegram execution checkpoint")
	}
	index, ok := m.store.(session.TranscriptIndexer)
	if !ok {
		return nil, 0, fmt.Errorf("Telegram store does not support transcript fencing")
	}
	revision, err := index.TranscriptRev(ctx, state.SessionID)
	if err != nil {
		return nil, 0, err
	}
	if revision != state.Revision {
		return nil, 0, session.ErrExecHandoffConflict
	}
	meta, err := m.store.Get(ctx, state.SessionID)
	if err != nil {
		return nil, 0, err
	}
	if meta.Origin != session.OriginTelegram || meta.Name != fmt.Sprintf("telegram:%d", state.ChatID) || meta.Agent != m.settings.Agent {
		return nil, 0, fmt.Errorf("Telegram checkpoint session identity mismatch")
	}
	if m.settings.NewSession == nil {
		return nil, 0, fmt.Errorf("Telegram runtime factory is not configured")
	}
	runtime, err := m.settings.NewSession(ctx)
	if err != nil {
		return nil, 0, err
	}
	if runtime == nil {
		return nil, 0, fmt.Errorf("Telegram runtime factory returned nil")
	}
	identity := func(value string) string {
		if value = strings.TrimSpace(value); value == "" {
			return "unknown"
		}
		return value
	}
	if runtime.Engine == nil || identity(runtime.ProviderName) != meta.Provider || identity(runtime.ModelName) != meta.Model {
		if runtime.Cleanup != nil {
			runtime.Cleanup()
		}
		return nil, 0, fmt.Errorf("Telegram checkpoint runtime does not match saved provider/model")
	}
	runtime.Engine.SetContextEstimateBaseline(meta.LastTotalTokens, meta.LastMessageCount)
	return &telegramSession{restoredExecution: state.Execution, runtime: runtime, meta: meta, history: state.History, systemPromptPersisted: state.SystemPromptPersisted, carryoverContext: state.CarryoverContext, carryoverContextLabel: state.CarryoverContextLabel, carryoverMessageCount: state.CarryoverMessageCount, lastActivity: state.LastActivity}, state.ChatID, nil
}
