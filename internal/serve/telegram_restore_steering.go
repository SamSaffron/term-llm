package serve

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// prepareRestoredSteering runs under sess.mu, before building the continuation
// request. Pending notes must be visible on its FIRST model turn, including when
// only one turn remains; requeueing them into the engine would defer them until
// a later tool boundary and can lose them entirely at the turn limit.
func (m *telegramSessionMgr) prepareRestoredSteering(ctx context.Context, sess *telegramSession) error {
	state := sess.restoredExecution
	if state == nil || len(state.Steering) == 0 {
		return nil
	}
	history := append([]llm.Message(nil), sess.history...)
	seen := make(map[string]llm.Message)
	for _, msg := range history {
		if id := strings.TrimSpace(msg.ClientMessageID); id != "" {
			seen[id] = msg
		}
	}
	pending := make(map[string]bool)
	for _, entry := range state.Steering {
		id := strings.TrimSpace(entry.ID)
		if id == "" || pending[id] {
			return fmt.Errorf("invalid restored Telegram steering identity")
		}
		pending[id] = true
		msg := entry.Message
		msg.Role = llm.RoleUser
		if msg.ClientMessageID != "" && msg.ClientMessageID != id {
			return fmt.Errorf("conflicting restored Telegram steering identity")
		}
		msg.ClientMessageID = id
		if prior, exists := seen[id]; exists {
			if prior.Role != msg.Role || !reflect.DeepEqual(prior.Parts, msg.Parts) {
				return fmt.Errorf("restored Telegram steering conflicts with committed intent")
			}
			continue
		}
		history = append(history, msg)
		seen[id] = msg
	}
	if !m.reconcileTelegramTranscript(ctx, sess, history, sess.systemPromptPersisted, "ReplaceMessages(restart_steering)") {
		return fmt.Errorf("restored Telegram steering could not be committed")
	}
	sess.history = history
	state.Steering = nil
	return nil
}
