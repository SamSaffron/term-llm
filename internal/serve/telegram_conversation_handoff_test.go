package serve

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTelegramConversationHandoffPreservesExactSession(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := testutil.NewEngineHarness()
	created, cleaned := 0, 0
	mgr := &telegramSessionMgr{store: store, settings: Settings{Agent: "test", TelegramCarryoverChars: 0, NewSession: func(context.Context) (*SessionRuntime, error) {
		created++
		return &SessionRuntime{Engine: h.Engine, ProviderName: "mock", ModelName: "mock", Cleanup: func() { cleaned++ }}, nil
	}}}
	sess, err := mgr.newSession(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	mgr.persistSession(ctx, sess)
	sess.history = []llm.Message{llm.SystemText("prior policy"), llm.UserImageMessageWithPath("image/png", "aW1hZ2U=", "/private/upload.png", "untruncated original input"), llm.AssistantText("complete answer")}
	sess.carryoverMessageCount = 2
	sess.carryoverContext = "pending carryover"
	sess.carryoverContextLabel = "custom label"
	sess.systemPromptPersisted = true
	sess.lastActivity = time.Date(2026, 9, 6, 1, 2, 3, 0, time.UTC)
	if _, err := mgr.snapshotConversation(ctx, 42, sess); err == nil {
		t.Fatal("snapshot allowed open admission")
	}
	reopen := mgr.restartGate.Pause()
	defer reopen()
	releaseChild := mgr.restartGate.TrackChild()
	if _, err := mgr.snapshotConversation(ctx, 42, sess); err == nil {
		t.Fatal("snapshot allowed unsettled descendant")
	}
	releaseChild()
	sess.runtimeStale.Store(true)
	if _, err := mgr.snapshotConversation(ctx, 42, sess); err == nil {
		t.Fatal("snapshot allowed quarantined runtime")
	}
	sess.runtimeStale.Store(false)
	raw, err := mgr.snapshotConversation(ctx, 42, sess)
	if err != nil {
		t.Fatal(err)
	}
	restored, chatID, err := mgr.restoreConversation(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.runtime.Cleanup()
	if chatID != 42 || restored.meta.ID != sess.meta.ID || !reflect.DeepEqual(restored.history, sess.history) || restored.carryoverMessageCount != 2 || !restored.systemPromptPersisted || restored.carryoverContext != sess.carryoverContext || restored.carryoverContextLabel != sess.carryoverContextLabel || !restored.lastActivity.Equal(sess.lastActivity) {
		t.Fatal("conversation restored lossily or with new identity")
	}
	var state telegramConversationState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.ChatID = 99
	wrong, _ := json.Marshal(state)
	if _, _, err := mgr.restoreConversation(ctx, wrong); err == nil {
		t.Fatal("wrong chat accepted")
	}
	if created != 2 {
		t.Fatal("invalid handoff created a runtime")
	}
	factory := mgr.settings.NewSession
	mgr.settings.NewSession = func(ctx context.Context) (*SessionRuntime, error) {
		runtime, err := factory(ctx)
		runtime.ModelName = "different-model"
		return runtime, err
	}
	if _, _, err := mgr.restoreConversation(ctx, raw); err == nil {
		t.Fatal("changed model accepted")
	}
	mgr.settings.NewSession = factory
	if cleaned != 1 || created != 3 {
		t.Fatal("rejected runtime was not cleaned exactly once")
	}
	if err := store.AddMessage(ctx, sess.meta.ID, &session.Message{SessionID: sess.meta.ID, Role: llm.RoleUser, Parts: llm.UserText("later change").Parts, CreatedAt: time.Now(), Sequence: -1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.restoreConversation(ctx, raw); !errors.Is(err, session.ErrExecHandoffConflict) {
		t.Fatalf("stale transcript accepted: %v", err)
	}
	if cleaned != 1 || created != 3 {
		t.Fatal("stale handoff created or cleaned runtime")
	}
}
