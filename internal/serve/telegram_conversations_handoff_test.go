package serve

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTelegramConversationsHandoffPublishesAllOrNone(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, cleaned := 0, 0
	settings := Settings{NewSession: func(context.Context) (*SessionRuntime, error) {
		created++
		h := testutil.NewEngineHarness()
		return &SessionRuntime{Engine: h.Engine, ProviderName: "mock", ModelName: "mock", Cleanup: func() { cleaned++ }}, nil
	}}
	source := &telegramSessionMgr{store: store, settings: settings, sessions: make(map[int64]*telegramSession)}
	for _, chat := range []int64{20, 10} {
		sess, err := source.newSession(ctx, chat)
		if err != nil {
			t.Fatal(err)
		}
		source.persistSession(ctx, sess)
		source.sessions[chat] = sess
	}
	defer source.closeAllSessions()
	reopen := source.restartGate.Pause()
	defer reopen()
	states, err := source.snapshotConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first telegramConversationState
	if err := json.Unmarshal(states[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.ChatID != 10 {
		t.Fatal("nondeterministic snapshot order")
	}
	target := &telegramSessionMgr{store: store, settings: settings}
	if err := target.restoreConversations(ctx, []json.RawMessage{states[0], states[0]}); err == nil {
		t.Fatal("duplicate chat accepted")
	}
	if created != 2 {
		t.Fatal("duplicate identities allocated runtimes")
	}
	var bad telegramConversationState
	if err := json.Unmarshal(states[1], &bad); err != nil {
		t.Fatal(err)
	}
	bad.Revision++
	raw, _ := json.Marshal(bad)
	if err := target.restoreConversations(ctx, []json.RawMessage{states[0], raw}); err == nil {
		t.Fatal("stale later chat accepted")
	}
	if len(target.sessions) != 0 || created != 3 || cleaned != 1 {
		t.Fatalf("partial recovery leaked: published=%d created=%d cleaned=%d", len(target.sessions), created, cleaned)
	}
	if err := target.restoreConversations(ctx, states); err != nil {
		t.Fatal(err)
	}
	defer target.closeAllSessions()
	for id, old := range source.sessions {
		if target.sessions[id] == nil || target.sessions[id].meta.ID != old.meta.ID {
			t.Fatal("original chat identity lost")
		}
	}
	if err := target.restoreConversations(ctx, states); err == nil {
		t.Fatal("recovery overwrote live chats")
	}
	if created != 5 || cleaned != 1 {
		t.Fatal("duplicate publication changed runtimes")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cancelled := &telegramSessionMgr{store: store, settings: settings}
	cancelled.settings.NewSession = func(ctx context.Context) (*SessionRuntime, error) {
		runtime, err := settings.NewSession(ctx)
		cancel()
		return runtime, err
	}
	if err := cancelled.restoreConversations(cancelCtx, states); err == nil {
		t.Fatal("cancelled bootstrap published chats")
	}
	if len(cancelled.sessions) != 0 || created != 6 || cleaned != 2 {
		t.Fatal("cancelled bootstrap leaked runtime or published a partial map")
	}
	raced := &telegramSessionMgr{store: store, settings: settings}
	factories := 0
	raced.settings.NewSession = func(ctx context.Context) (*SessionRuntime, error) {
		runtime, err := settings.NewSession(ctx)
		factories++
		if factories == 2 {
			if err := store.AddMessage(ctx, first.SessionID, session.NewMessage(first.SessionID, llm.UserText("changed during runtime construction"), -1)); err != nil {
				t.Fatal(err)
			}
		}
		return runtime, err
	}
	if err := raced.restoreConversations(ctx, states); err == nil {
		t.Fatal("earlier transcript changed during recovery but was published")
	}
	if len(raced.sessions) != 0 || created != 8 || cleaned != 4 {
		t.Fatal("late revision conflict leaked recovered runtimes")
	}
}
