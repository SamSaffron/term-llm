package serve

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTelegramStreamControlStopRevokesRestart(t *testing.T) {
	for _, stopFirst := range []bool{false, true} {
		c := newTelegramStreamControl(context.Background())
		if stopFirst {
			c.stop()
		}
		c.interruptForRestart()
		if !stopFirst && !c.restarting() {
			t.Fatal("restart cause not retained")
		}
		c.stop()
		if c.restarting() {
			t.Fatal("Stop failed to revoke restart intent")
		}
	}
	c := newTelegramStreamControl(context.Background())
	c.interruptForRestart()
	c.close() // Normal stream cleanup must not masquerade as a user Stop.
	if !c.restarting() {
		t.Fatal("cleanup revoked internal interruption")
	}
	closeTelegramSession(&telegramSession{restartControl: c})
	if c.restarting() {
		t.Fatal("reset/retirement retained restart permission")
	}
	parent, cancel := context.WithCancel(context.Background())
	c = newTelegramStreamControl(parent)
	c.interruptForRestart()
	cancel()
	if c.restarting() {
		t.Fatal("platform shutdown retained restart permission")
	}
}

func TestTelegramStreamControlConcurrentStopWins(t *testing.T) {
	for n := 0; n < 100; n++ {
		c := newTelegramStreamControl(context.Background())
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); c.interruptForRestart() }()
		go func() { defer wg.Done(); c.stop() }()
		wg.Wait()
		if c.restarting() {
			t.Fatal("concurrent restart overrode Stop")
		}
	}
}

func TestTelegramStreamUsesRestartCauseWithoutRevokingOnCleanup(t *testing.T) {
	h := testutil.NewEngineHarness()
	started := make(chan struct{})
	h.Registry.Register(&testutil.MockTool{SpecData: llm.ToolSpec{Name: "wait", Schema: map[string]interface{}{"type": "object"}}, ExecuteFn: func(ctx context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
		close(started)
		<-ctx.Done()
		return llm.TextOutput("cancelled"), ctx.Err()
	}})
	h.Provider.AddToolCall("wait-1", "wait", map[string]any{})
	mgr, sess := newTestMgrAndSession(h)
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mgr.store = store
	runtime := sess.runtime
	runtime.Provider = h.Provider
	mgr.settings.NewSession = func(context.Context) (*SessionRuntime, error) { return runtime, nil }
	sess, err = mgr.getOrCreate(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	bot := &fakeBotSender{nextID: 1}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- mgr.streamReply(ctx, bot, sess, 42, llm.UserText("work")) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	sess.cancelMu.Lock()
	control := sess.restartControl
	sess.cancelMu.Unlock()
	if control == nil {
		t.Fatal("stream control was not installed")
	}
	_, status := h.Engine.QueueSteeringWithStatus(llm.QueuedSteering{Message: llm.UserText("pending note"), DisplayText: "pending note"})
	if status != llm.SteeringQueueQueued {
		t.Fatalf("queue steering: %v", status)
	}
	owner := llm.SteeringTransition{OperationID: "restart-test", Fence: 1}
	if _, err := control.freezeForRestart(owner); err != nil {
		t.Fatal(err)
	}
	defer h.Engine.ReleaseSteeringFreeze(owner, false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not settle")
	}
	if control.remainingTurns() != 4 {
		t.Fatalf("remaining turns=%d, want 4", control.remainingTurns())
	}
	if !control.restarting() {
		t.Fatal("stream cleanup revoked restart intent")
	}
	if !strings.Contains(bot.lastText(), "restarting") {
		t.Fatalf("restart presented as user interruption: %q", bot.lastText())
	}
	cursor := control.replyCursor()
	if cursor == nil || cursor.MessageID != 1 {
		t.Fatal("settled internal interruption lost reply cursor")
	}
	cursor.MessageID = 99
	if control.replyCursor().MessageID != 1 {
		t.Fatal("cursor aliases stored state")
	}
	reopen := mgr.restartGate.Pause()
	defer reopen()
	deadline := time.Now().Add(time.Second)
	for !mgr.restartGate.Drained() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	raw, err := mgr.snapshotConversation(ctx, 42, sess)
	if err != nil {
		t.Fatal(err)
	}
	handoffs, ok := session.AsCommandHandoffStore(store)
	if !ok {
		t.Fatal("missing durable handoff store")
	}
	if err := handoffs.SaveCommandHandoff(ctx, session.CommandHandoff{ID: "telegram-state-test", Service: "telegram-test", SourceInstance: "old", SessionID: sess.meta.ID, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	handoff, err := handoffs.ConsumeCommandHandoff(ctx, "telegram-state-test", "telegram-test", "new")
	if err != nil {
		t.Fatal(err)
	}
	mgr.settings.NewSession = func(context.Context) (*SessionRuntime, error) {
		next := testutil.NewEngineHarness()
		return &SessionRuntime{Engine: next.Engine, ProviderName: "mock", ModelName: "test"}, nil
	}
	restored, chatID, err := mgr.restoreConversation(ctx, handoff.Payload)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTelegramSession(restored)
	state := restored.restoredExecution
	if chatID != 42 || state == nil || state.RemainingTurns != 4 || state.Reply.MessageID != 1 || len(state.Steering) != 1 || state.Steering[0].DisplayText != "pending note" {
		t.Fatal("SQLite handoff lost interrupted execution state")
	}
	if len(restored.history) == 0 || collectUserText(restored.history[0]) != "work" {
		t.Fatal("checkpoint lost original input")
	}
	if _, err := handoffs.ConsumeCommandHandoff(ctx, "telegram-state-test", "telegram-test", "another"); err == nil {
		t.Fatal("handoff consumed twice")
	}
	if err := mgr.savePlatformHandoff(ctx, "platform-roundtrip", "service", "old", 7, 108); err != nil {
		t.Fatal(err)
	}
	platform, err := takeTelegramPlatformHandoff(ctx, store, "platform-roundtrip", "service", "new", 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if platform.NextOffset != 108 || len(platform.Conversations) != 1 {
		t.Fatal("platform envelope lost chat or offset")
	}
	var conversation telegramConversationState
	if err := json.Unmarshal(platform.Conversations[0], &conversation); err != nil {
		t.Fatal(err)
	}
	if conversation.Execution == nil || conversation.Execution.RemainingTurns != 4 || len(conversation.Execution.Steering) != 1 {
		t.Fatal("platform envelope lost execution state")
	}
	mgr.store = &telegramFailedSealStore{Store: store, TranscriptIndexer: store.(session.TranscriptIndexer)}
	if _, err := mgr.snapshotConversation(ctx, 42, sess); err == nil {
		t.Fatal("failed transcript sealing still produced checkpoint")
	}
	mgr.store = store
	closeTelegramSession(sess)
	if control.replyCursor() != nil {
		t.Fatal("Stop did not revoke reply cursor")
	}
	if control.restarting() {
		t.Fatal("session retirement failed to revoke continuation")
	}
}

func TestTelegramCannotFreezeBeforeDurableBoundary(t *testing.T) {
	h := testutil.NewEngineHarness()
	c := newTelegramStreamControl(context.Background())
	defer c.close()
	c.bind(h.Engine, 5)
	if _, err := c.freezeForRestart(llm.SteeringTransition{OperationID: "early", Fence: 1}); err == nil {
		t.Fatal("engine binding falsely implied durable readiness")
	}
	if c.ctx.Err() != nil {
		t.Fatal("failed readiness check cancelled live work")
	}
}

func TestTelegramPendingMediaSurvivesCursorCopyWithoutDuplicateReferences(t *testing.T) {
	a := llm.MediaArtifact{Reference: "IMAGE-A", StoredPath: "/durable/a.png", MediaType: "image/png"}
	b := llm.MediaArtifact{Reference: "video-b", StoredPath: "/durable/b.mp4", MediaType: "video/mp4"}
	duplicate := a
	duplicate.Reference = "image-a"
	merged := mergeTelegramPendingMedia([]llm.MediaArtifact{a}, []llm.MediaArtifact{duplicate, b})
	if len(merged) != 2 || merged[0] != a || merged[1] != b {
		t.Fatal("pending media was duplicated or reordered")
	}
	c := newTelegramStreamControl(context.Background())
	defer c.close()
	c.interruptForRestart()
	c.reply.Store(&telegramReplyCursor{MessageID: 1, Images: []string{"legacy.png"}, Media: []llm.MediaArtifact{a, b}, PendingMedia: merged})
	copy := c.replyCursor()
	copy.Images[0] = "changed"
	copy.Media[0].StoredPath = "changed"
	copy.PendingMedia[0].Reference = "changed"
	original := c.replyCursor()
	if original.Images[0] != "legacy.png" || original.Media[0] != a || original.PendingMedia[0] != a {
		t.Fatal("cursor copy aliases media state")
	}
}

type telegramFailedSealStore struct {
	session.Store
	session.TranscriptIndexer
}

func (s *telegramFailedSealStore) ReplaceMessages(context.Context, string, []session.Message) error {
	return errors.New("injected checkpoint write failure")
}
