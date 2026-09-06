package serve

import (
	"context"
	"encoding/json"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/session"
)

// telegramPlatformHandoff pairs the acknowledgement boundary with exact chats
// and admitted inputs that have NOT committed a model turn. Committed inputs
// belong to conversation execution state, never Pending: replaying them would
// duplicate a user turn or completed effects. The intake owner must classify
// and settle handlers before assembling this envelope.
type telegramPlatformHandoff struct {
	Version       int               `json:"version"`
	BotID         int64             `json:"bot_id"`
	NextOffset    int               `json:"next_offset"`
	Conversations []json.RawMessage `json:"conversations"`
	Pending       []json.RawMessage `json:"pending,omitempty"`
}

func (s *telegramPlatformHandoff) validate(botID int64) error {
	if s.Version != 1 || botID <= 0 || s.BotID != botID || s.NextOffset < 0 {
		return fmt.Errorf("invalid Telegram platform checkpoint identity or version")
	}
	previous := -1
	for _, raw := range s.Pending {
		var update tgbotapi.Update
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		if update.UpdateID < 0 || update.UpdateID <= previous || update.UpdateID >= s.NextOffset || update.Message == nil || update.Message.Chat == nil || update.Message.From == nil {
			return fmt.Errorf("invalid Telegram pending input or acknowledgement boundary")
		}
		previous = update.UpdateID
	}
	return nil
}

func telegramHandoffService(service string, botID int64) string {
	// Bind bot identity in the store's lookup fence, not just in payload validation
	// after consumption. A changed bot must not consume the correct bot's intent.
	return fmt.Sprintf("%s:telegram:%d", service, botID)
}

func (m *telegramSessionMgr) savePlatformHandoff(ctx context.Context, id, service, instance string, botID int64, offset int) error {
	handoffs, ok := session.AsCommandHandoffStore(m.store)
	if !ok {
		return fmt.Errorf("Telegram restart requires durable handoff storage")
	}
	pending, err := m.inbox.parkedInputs()
	if err != nil {
		return err
	}
	state := telegramPlatformHandoff{Version: 1, BotID: botID, NextOffset: offset, Pending: pending}
	if err := state.validate(botID); err != nil {
		return err
	}
	conversations, err := m.snapshotConversations(ctx)
	if err != nil {
		return err
	}
	state.Conversations = conversations
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return handoffs.SaveCommandHandoff(ctx, session.CommandHandoff{ID: id, Service: telegramHandoffService(service, botID), SourceInstance: instance, Payload: raw})
}

// takeTelegramPlatformHandoff authenticates a one-shot replacement/reclaim. It
// deliberately does not start polling, publish chats or dispatch pending input.
// The supervisor must retain the returned state across initialization retries.
func takeTelegramPlatformHandoff(ctx context.Context, store session.Store, id, service, instance string, botID int64, reclaim bool) (*telegramPlatformHandoff, error) {
	handoffs, ok := session.AsCommandHandoffStore(store)
	if !ok {
		return nil, fmt.Errorf("Telegram restart requires durable handoff storage")
	}
	var handoff session.CommandHandoff
	var err error
	if reclaim {
		handoff, err = handoffs.ReclaimCommandHandoff(ctx, id, telegramHandoffService(service, botID), instance)
	} else {
		handoff, err = handoffs.ConsumeCommandHandoff(ctx, id, telegramHandoffService(service, botID), instance)
	}
	if err != nil {
		return nil, err
	}
	var state telegramPlatformHandoff
	if err := json.Unmarshal(handoff.Payload, &state); err != nil {
		return nil, err
	}
	if err := state.validate(botID); err != nil {
		return nil, err
	}
	return &state, nil
}

// restorePlatformHandoff installs chats and acknowledgement state together,
// before polling can start. Pending inputs are returned in update order for the
// intake owner to admit before requesting newer updates; they are not executed
// here. Failed initialization leaves the receiver offset and chat map unchanged.
func (m *telegramSessionMgr) restorePlatformHandoff(ctx context.Context, receiver *telegramReceiver, state *telegramPlatformHandoff, botID int64) ([]tgbotapi.Update, error) {
	if state == nil || receiver == nil {
		return nil, fmt.Errorf("missing Telegram restart state or receiver")
	}
	if err := state.validate(botID); err != nil {
		return nil, err
	}
	pending := make([]tgbotapi.Update, 0, len(state.Pending))
	for _, raw := range state.Pending {
		var update tgbotapi.Update
		if err := json.Unmarshal(raw, &update); err != nil {
			return nil, err
		}
		pending = append(pending, update)
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if receiver.running {
		return nil, fmt.Errorf("Telegram receiver already running during recovery")
	}
	if err := m.restoreConversations(ctx, state.Conversations); err != nil {
		return nil, err
	}
	receiver.state.NextOffset = state.NextOffset
	return pending, nil
}
