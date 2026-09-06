package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestTelegramPlatformHandoffBindsBotAndAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr := &telegramSessionMgr{store: store, sessions: make(map[int64]*telegramSession)}
	reopen := mgr.restartGate.Pause()
	defer reopen()
	input, err := json.Marshal(tgbotapi.Update{UpdateID: 105, Message: &tgbotapi.Message{MessageID: 8, Chat: &tgbotapi.Chat{ID: 42}, From: &tgbotapi.User{ID: 1}, Text: "not started"}})
	if err != nil {
		t.Fatal(err)
	}
	var update tgbotapi.Update
	if err := json.Unmarshal(input, &update); err != nil {
		t.Fatal(err)
	}
	finish, _, err := mgr.inbox.begin(update)
	if err != nil {
		t.Fatal(err)
	}
	mgr.inbox.pauseUnstarted()
	if (telegramInboxReceipt{inbox: &mgr.inbox, id: update.UpdateID}).start() {
		t.Fatal("paused input started")
	}
	finish()
	for _, reclaim := range []bool{false, true} {
		id := "replacement"
		instance := "new"
		if reclaim {
			id = "rollback"
			instance = "original"
		}
		if err := mgr.savePlatformHandoff(ctx, id, "service", "original", 7, 108); err != nil {
			t.Fatal(err)
		}
		if _, err := takeTelegramPlatformHandoff(ctx, store, id, "service", instance, 8, reclaim); err == nil {
			t.Fatal("wrong bot consumed handoff")
		}
		wrongInstance := "original"
		if reclaim {
			wrongInstance = "new"
		}
		if _, err := takeTelegramPlatformHandoff(ctx, store, id, "service", wrongInstance, 7, reclaim); err == nil {
			t.Fatal("wrong instance consumed handoff")
		}
		state, err := takeTelegramPlatformHandoff(ctx, store, id, "service", instance, 7, reclaim)
		if err != nil {
			t.Fatal(err)
		}
		if state.BotID != 7 || state.NextOffset != 108 || len(state.Pending) != 1 || string(state.Pending[0]) != string(input) {
			t.Fatal("handoff lost acknowledgement or admitted input")
		}
		receiver := &telegramReceiver{state: telegramPollState{NextOffset: 12}}
		target := &telegramSessionMgr{store: store}
		broken := *state
		broken.Conversations = []json.RawMessage{json.RawMessage(`{"session_id":"missing","chat_id":42}`)}
		if _, err := target.restorePlatformHandoff(ctx, receiver, &broken, 7); err == nil {
			t.Fatal("invalid chat bootstrap succeeded")
		}
		if receiver.state.NextOffset != 12 || len(target.sessions) != 0 {
			t.Fatal("failed bootstrap partly installed state")
		}
		inputs, err := target.restorePlatformHandoff(ctx, receiver, state, 7)
		if err != nil {
			t.Fatal(err)
		}
		if receiver.state.NextOffset != 108 || len(inputs) != 1 || inputs[0].UpdateID != 105 {
			t.Fatal("bootstrap lost cursor or pending input")
		}
		receiver.running = true
		if _, err := target.restorePlatformHandoff(ctx, receiver, state, 7); err == nil {
			t.Fatal("live receiver accepted restoration")
		}
		if _, err := takeTelegramPlatformHandoff(ctx, store, id, "service", instance, 7, reclaim); err == nil {
			t.Fatal("handoff consumed twice")
		}
	}
	state := telegramPlatformHandoff{Version: 1, BotID: 7, NextOffset: 108, Pending: []json.RawMessage{input, input}}
	if err := state.validate(7); err == nil {
		t.Fatal("duplicate pending update accepted")
	}
	state.Pending = state.Pending[:1]
	state.NextOffset = 105
	if err := state.validate(7); err == nil {
		t.Fatal("unacknowledged input accepted for replay")
	}
	state.NextOffset = 108
	state.Version = 2
	if err := state.validate(7); err == nil {
		t.Fatal("unknown checkpoint schema accepted")
	}
}
