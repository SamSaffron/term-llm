package serve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTelegramRestoredSteeringIsCommittedBeforeLastModelTurn(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("done")
	mgr := &telegramSessionMgr{store: store, sessions: make(map[int64]*telegramSession), settings: Settings{MaxTurns: 9, NewSession: func(context.Context) (*SessionRuntime, error) {
		return &SessionRuntime{Engine: h.Engine, Provider: h.Provider, ProviderName: "mock", ModelName: "mock"}, nil
	}}}
	sess, err := mgr.getOrCreate(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.closeAllSessions()
	note := llm.UserText("already committed note")
	note.ClientMessageID = "one"
	sess.history = []llm.Message{llm.UserText("original task"), note}
	if !mgr.reconcileTelegramTranscript(ctx, sess, sess.history, false, "fixture") {
		t.Fatal("persist fixture")
	}
	sess.restoredExecution = &telegramExecutionState{RemainingTurns: 1, Reply: &telegramReplyCursor{MessageID: 1}, Steering: []llm.QueuedSteering{{ID: "one", Message: note}, {ID: "two", Message: llm.UserText("pending instruction")}}}
	original := sess.restoredExecution.Steering[0]
	sess.restoredExecution.Steering[0].Message = llm.UserText("conflicting intent")
	if err := mgr.streamReplyContinuation(ctx, &fakeBotSender{}, sess, 42, "continue", 1, sess.restoredExecution.Reply); err == nil {
		t.Fatal("conflicting committed identity accepted")
	}
	sess.restoredExecution.Steering[0] = original
	mgr.store = &telegramFailedSealStore{Store: store, TranscriptIndexer: store.(session.TranscriptIndexer)}
	if err := mgr.streamReplyContinuation(ctx, &fakeBotSender{}, sess, 42, "continue", 1, sess.restoredExecution.Reply); err == nil {
		t.Fatal("failed steering commit started continuation")
	}
	if len(sess.history) != 2 || len(sess.restoredExecution.Steering) != 2 || len(h.Provider.RecordedRequests()) != 0 {
		t.Fatal("failed commit consumed input or executed model")
	}
	mgr.store = store
	if err := mgr.streamReplyContinuation(ctx, &fakeBotSender{}, sess, 42, "continue", 1, sess.restoredExecution.Reply); err != nil {
		t.Fatal(err)
	}
	requests := h.Provider.RecordedRequests()
	if len(requests) != 1 || requests[0].MaxTurns != 1 {
		t.Fatal("last-turn recovery changed budget")
	}
	counts := map[string]int{}
	for _, msg := range requests[0].Messages {
		if msg.ClientMessageID != "" {
			counts[msg.ClientMessageID]++
		}
	}
	if counts["one"] != 1 || counts["two"] != 1 {
		t.Fatalf("first model request lost or duplicated steering: %v", counts)
	}
	messages, err := store.GetMessages(ctx, sess.meta.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	counts = map[string]int{}
	for _, msg := range messages {
		if msg.ClientMessageID != "" {
			counts[msg.ClientMessageID]++
		}
	}
	if counts["one"] != 1 || counts["two"] != 1 || len(sess.restoredExecution.Steering) != 0 {
		t.Fatal("steering persistence/ownership did not settle exactly once")
	}
	h.Provider.AddTextResponse("new work")
	if err := mgr.streamReply(ctx, &fakeBotSender{}, sess, 42, llm.UserText("replacement task")); err != nil {
		t.Fatal(err)
	}
	if sess.restoredExecution != nil {
		t.Fatal("newer user turn retained recovered continuation")
	}
}
