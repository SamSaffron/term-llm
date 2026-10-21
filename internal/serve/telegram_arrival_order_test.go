package serve

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

// Admission orders persistence, not provider dispatch. Hold the replacement's
// cancellation at the bot boundary until the photo stream is actually running;
// otherwise a busy scheduler can validly skip the photo's provider call entirely.
// Do not hold fakeBotSender.mu here: the photo still needs to send its placeholder.
type arrivalOrderBot struct {
	*fakeBotSender
	ctx           context.Context
	photoStarted  <-chan struct{}
	stopAttempted chan struct{}
}

func (b *arrivalOrderBot) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	if msg, ok := c.(tgbotapi.MessageConfig); ok && msg.Text == "↩️ Stopping current work and switching to your new request." {
		close(b.stopAttempted)
		select {
		case <-b.photoStarted:
		case <-b.ctx.Done():
			return tgbotapi.Message{}, b.ctx.Err()
		}
	}
	return b.fakeBotSender.Send(c)
}

func TestStreamReply_AdmissionCancellationBeforeProviderStartPreservesPhoto(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "early-cancel.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("replacement answer")
	mgr := &telegramSessionMgr{
		sessions: make(map[int64]*telegramSession),
		store:    store,
		settings: Settings{MaxTurns: 5, NewSession: func(context.Context) (*SessionRuntime, error) {
			return &SessionRuntime{Engine: engine.NewEngine(provider, engine.NewToolRegistry()), ProviderName: "mock", ModelName: "test"}, nil
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := mgr.getOrCreate(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	bot := &fakeBotSender{}
	photo := core.UserImageMessageWithPath("image/jpeg", "/9j/2wBDAAg=", "", "first photo")
	photoCtx, cancelPhoto := context.WithCancel(ctx)
	defer cancelPhoto()
	admitted := false
	err = mgr.streamReplyWithAdmission(photoCtx, bot, sess, 42, photo, func() {
		admitted = true
		// This is the same legal schedule as a later /stop winning the race
		// immediately after admission release, before Engine.Stream is called.
		cancelPhoto()
	})
	if !admitted {
		t.Fatal("photo admission was not released")
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled photo: %v", err)
	}
	if requests := provider.RecordedRequests(); len(requests) != 0 {
		t.Fatalf("cancelled photo reached provider: %d requests", len(requests))
	}
	if err := mgr.streamReply(ctx, bot, sess, 42, core.UserText("/stop")); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want only replacement", len(requests))
	}
	var users []core.Message
	for _, msg := range requests[0].Messages {
		if msg.Role == core.RoleUser {
			users = append(users, msg)
		}
	}
	// Telegram intentionally normalizes older images to a placeholder in the
	// in-memory provider history; the accepted photo must not disappear entirely.
	wantPhotoText := collectUserText(normalizeUserMessageForHistory(photo))
	if len(users) != 2 || collectUserText(users[0]) != wantPhotoText || collectUserText(users[1]) != "/stop" {
		t.Fatalf("replacement lost ordered photo history: %+v", users)
	}
	persisted, err := store.GetMessages(ctx, sess.meta.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var persistedUsers []session.Message
	for _, msg := range persisted {
		if msg.Role == core.RoleUser {
			persistedUsers = append(persistedUsers, msg)
		}
	}
	if len(persistedUsers) != 2 || persistedUsers[0].TextContent != "first photo" || !testMessageHasImage(core.Message{Parts: persistedUsers[0].Parts}) || persistedUsers[1].TextContent != "/stop" || persistedUsers[0].Sequence >= persistedUsers[1].Sequence {
		t.Fatalf("persisted users lost photo or arrival order: %+v", persistedUsers)
	}
}
