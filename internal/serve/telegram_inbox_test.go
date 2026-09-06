package serve

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/testutil"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestTelegramInboxRetainsInputAfterAdmissionRelease(t *testing.T) {
	mgr := &telegramSessionMgr{}
	msg := &tgbotapi.Message{MessageID: 7, Chat: &tgbotapi.Chat{ID: 42}, Text: "original"}
	finish, duplicate, err := mgr.inbox.begin(tgbotapi.Update{UpdateID: 12, Message: msg})
	if err != nil || duplicate {
		t.Fatalf("begin: duplicate=%v err=%v", duplicate, err)
	}
	admission := mgr.admitMessage(msg)
	admission.release() // Active stream now accepts steering; it is not finished.
	msg.Text = "mutated caller"
	raw := mgr.inbox.snapshot()
	if len(raw) != 1 {
		t.Fatal("admission release lost unfinished input")
	}
	var saved tgbotapi.Update
	if err := json.Unmarshal(raw[0], &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Message.Text != "original" {
		t.Fatal("input snapshot aliases caller")
	}
	raw[0][0] = '!'
	if !json.Valid(mgr.inbox.snapshot()[0]) {
		t.Fatal("snapshot aliases inbox")
	}
	if _, duplicate, err := mgr.inbox.begin(tgbotapi.Update{UpdateID: 12}); err != nil || !duplicate {
		t.Fatal("duplicate active input was admitted")
	}
	finish()
	// Finishing twice must not remove a subsequently admitted receipt.
	next, duplicate, err := mgr.inbox.begin(tgbotapi.Update{UpdateID: 12})
	if err != nil || duplicate {
		t.Fatal("completed receipt retained")
	}
	finish()
	if len(mgr.inbox.snapshot()) != 1 {
		t.Fatal("stale completion removed new receipt")
	}
	next()
	if len(mgr.inbox.snapshot()) != 0 {
		t.Fatal("finished update retained")
	}
}

func TestTelegramInboxSnapshotUsesUpdateOrder(t *testing.T) {
	var inbox telegramInbox
	for _, id := range []int{20, 3, 15} {
		finish, duplicate, err := inbox.begin(tgbotapi.Update{UpdateID: id})
		if err != nil || duplicate {
			t.Fatal(err)
		}
		defer finish()
	}
	for n, raw := range inbox.snapshot() {
		var update tgbotapi.Update
		if err := json.Unmarshal(raw, &update); err != nil {
			t.Fatal(err)
		}
		if update.UpdateID != []int{3, 15, 20}[n] {
			t.Fatal("snapshot reordered inputs")
		}
	}
}

func TestTelegramParkedHandlerHasNoEffectsAndReplaysOnceOnRollback(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("done")
	factories := 0
	mgr := &telegramSessionMgr{sessions: make(map[int64]*telegramSession), allowedUserIDs: map[int64]struct{}{1: {}}, idleTimeout: time.Hour, settings: Settings{MaxTurns: 5, NewSession: func(context.Context) (*SessionRuntime, error) {
		factories++
		return &SessionRuntime{Engine: h.Engine, Provider: h.Provider, ProviderName: "mock", ModelName: "mock"}, nil
	}}}
	defer mgr.closeAllSessions()
	update := tgbotapi.Update{UpdateID: 8, Message: &tgbotapi.Message{MessageID: 3, Chat: &tgbotapi.Chat{ID: 42}, From: &tgbotapi.User{ID: 1}, Text: "original queued input"}}
	finish, duplicate, err := mgr.inbox.begin(update)
	if err != nil || duplicate {
		t.Fatal("could not admit fixture")
	}
	mgr.inbox.pauseUnstarted()
	ctx := context.WithValue(context.Background(), telegramInboxContextKey{}, telegramInboxReceipt{inbox: &mgr.inbox, id: 8})
	bot := &fakeBotSender{}
	mgr.handleMessage(ctx, bot, update.Message)
	if _, err := mgr.inbox.resumeUnstarted(); err == nil {
		t.Fatal("rollback released an unfinished receipt")
	}
	finish()
	if factories != 0 || len(bot.allTexts()) != 0 || len(h.Provider.RecordedRequests()) != 0 {
		t.Fatal("parked handler performed side effects")
	}
	pending, err := mgr.inbox.parkedInputs()
	if err != nil || len(pending) != 1 {
		t.Fatal("parked input lost after handler return")
	}
	replay, err := mgr.inbox.resumeUnstarted()
	if err != nil || len(replay) != 1 {
		t.Fatal("rollback failed to transfer pending input")
	}
	if len(mgr.inbox.snapshot()) != 0 {
		t.Fatal("rollback retained duplicate receipt")
	}
	var recovered tgbotapi.Update
	if err := json.Unmarshal(replay[0], &recovered); err != nil {
		t.Fatal(err)
	}
	again, duplicate, err := mgr.inbox.begin(recovered)
	if err != nil || duplicate {
		t.Fatal("rollback input could not be readmitted")
	}
	mgr.handleMessage(ctx, bot, recovered.Message)
	again()
	if factories != 1 || len(h.Provider.RecordedRequests()) != 1 {
		t.Fatal("rollback input not executed exactly once")
	}
	if len(mgr.inbox.snapshot()) != 0 {
		t.Fatal("completed replay remained pending")
	}
}

func TestTelegramStartedReceiptCannotBecomeReplayInput(t *testing.T) {
	var inbox telegramInbox
	finish, _, err := inbox.begin(tgbotapi.Update{UpdateID: 1})
	if err != nil {
		t.Fatal(err)
	}
	receipt := telegramInboxReceipt{inbox: &inbox, id: 1}
	if !receipt.start() {
		t.Fatal("receipt not started")
	}
	inbox.pauseUnstarted()
	if !receipt.start() {
		t.Fatal("pause retroactively parked running work")
	}
	if _, err := inbox.parkedInputs(); err == nil {
		t.Fatal("running work classified as safe replay")
	}
	finish()
	pending, err := inbox.parkedInputs()
	if err != nil || len(pending) != 0 {
		t.Fatal("completed work retained for replay")
	}
}
