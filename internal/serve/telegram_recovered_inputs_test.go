package serve

import (
	"context"
	"errors"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTelegramRecoveredAdmissionRetainsOnlyUnacceptedSuffix(t *testing.T) {
	ctx, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("first answer")
	h.Provider.AddTextResponse("second answer")
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	mgr := &telegramSessionMgr{sessions: make(map[int64]*telegramSession), messageSlots: make(chan struct{}, 1), allowedUserIDs: map[int64]struct{}{1: {}}, idleTimeout: time.Hour, settings: Settings{MaxTurns: 5, NewSession: func(ctx context.Context) (*SessionRuntime, error) {
		started <- ctx
		<-release
		return &SessionRuntime{Engine: h.Engine, Provider: h.Provider, ProviderName: "mock", ModelName: "mock"}, nil
	}}}
	defer func() {
		if !released {
			close(release)
			released = true
		}
		cancelLife()
		mgr.closeAllSessions()
	}()
	updates := []tgbotapi.Update{}
	for id, text := range []string{"first input", "second input"} {
		updates = append(updates, tgbotapi.Update{UpdateID: id + 10, Message: &tgbotapi.Message{MessageID: id + 1, Chat: &tgbotapi.Chat{ID: 42}, From: &tgbotapi.User{ID: 1}, Text: text}})
	}
	bot := &fakeBotSender{}
	admissionCtx, cancelAdmission := context.WithCancel(ctx)
	defer cancelAdmission()
	done := make(chan error, 1)
	go func() { done <- mgr.admitRecoveredInputs(ctx, admissionCtx, bot, &updates) }()
	var handlerCtx context.Context
	select {
	case handlerCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("first recovered input not accepted")
	}
	cancelAdmission()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("admission error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked admission did not cancel")
	}
	if len(updates) != 1 || updates[0].UpdateID != 11 {
		t.Fatal("retry queue retained accepted prefix or lost suffix")
	}
	if handlerCtx.Err() != nil {
		t.Fatal("admission cancellation cancelled already accepted handler")
	}
	close(release)
	released = true
	waitDrained := func() {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for !mgr.restartGate.Drained() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if !mgr.restartGate.Drained() {
			t.Fatal("accepted handlers did not settle")
		}
	}
	reopen := mgr.restartGate.Pause()
	waitDrained()
	reopen()
	if err := mgr.admitRecoveredInputs(ctx, ctx, bot, &updates); err != nil {
		t.Fatal(err)
	}
	reopen = mgr.restartGate.Pause()
	defer reopen()
	waitDrained()
	if len(updates) != 0 || len(h.Provider.RecordedRequests()) != 2 {
		t.Fatal("retry did not execute each input exactly once")
	}
	if len(mgr.inbox.snapshot()) != 0 {
		t.Fatal("completed recovered receipts retained")
	}
}

func TestTelegramRecoveredAdmissionStopsWithPlatformLifetime(t *testing.T) {
	mgr := &telegramSessionMgr{messageSlots: make(chan struct{}, 1)}
	mgr.messageSlots <- struct{}{}
	life, cancel := context.WithCancel(context.Background())
	pending := []tgbotapi.Update{{UpdateID: 1, Message: &tgbotapi.Message{}}}
	result := make(chan error, 1)
	go func() { result <- mgr.admitRecoveredInputs(life, context.Background(), &fakeBotSender{}, &pending) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("platform cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("admission outlived cancelled platform")
	}
	if len(pending) != 1 || len(mgr.inbox.snapshot()) != 0 {
		t.Fatal("shutdown transferred or lost unadmitted input")
	}
}
