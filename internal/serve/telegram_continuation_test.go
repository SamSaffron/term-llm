package serve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestTelegramContinuationDoesNotCreateUserTurn(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("initial answer")
	h.Provider.AddTextResponse("continued answer")
	mgr := &telegramSessionMgr{sessions: make(map[int64]*telegramSession), store: store, settings: Settings{MaxTurns: 9, NewSession: func(context.Context) (*SessionRuntime, error) {
		return &SessionRuntime{Engine: h.Engine, ProviderName: "mock", ModelName: "mock"}, nil
	}}}
	sess, err := mgr.getOrCreate(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.closeAllSessions()
	bot := &fakeBotSender{nextID: 1}
	if err := mgr.streamReply(ctx, bot, sess, 42, llm.UserText("original task")); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, sess.meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	newMessages := bot.newMessages
	if err := mgr.streamReplyContinuation(ctx, bot, sess, 42, "Continue unfinished work; do not repeat completed effects.", 1, &telegramReplyCursor{MessageID: 1, Prefix: "initial answer\n"}); err != nil {
		t.Fatal(err)
	}
	if bot.newMessages != newMessages || bot.editIDs[len(bot.editIDs)-1] != 1 || bot.lastText() != "initial answer\ncontinued answer" {
		t.Fatal("continuation duplicated a message or lost its displayed prefix")
	}
	after, err := store.Get(ctx, sess.meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UserTurns != 1 || after.UserTurns != before.UserTurns || after.Summary != before.Summary {
		t.Fatalf("continuation changed user accounting: before=%+v after=%+v", before, after)
	}
	messages, err := store.GetMessages(ctx, sess.meta.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	users, developers := 0, 0
	var answers []string
	for _, msg := range messages {
		if msg.Role == llm.RoleAssistant {
			answers = append(answers, msg.TextContent)
		}
		if msg.Role == llm.RoleUser {
			users++
		}
		if msg.Role == llm.RoleDeveloper {
			developers++
		}
	}
	if len(answers) != 2 || answers[0] != "initial answer" || answers[1] != "continued answer" {
		t.Fatalf("display prefix duplicated into transcript: %q", answers)
	}
	if users != 1 || developers != 1 {
		t.Fatalf("persisted roles: users=%d developers=%d", users, developers)
	}
	historyDevelopers := 0
	for _, msg := range sess.history {
		if msg.Role == llm.RoleDeveloper {
			historyDevelopers++
		}
	}
	if historyDevelopers != 1 {
		t.Fatal("continuation was relabelled as user in live history")
	}
	if len(h.Provider.Requests) != 2 || h.Provider.Requests[1].MaxTurns != 1 {
		t.Fatal("remaining turn allowance was reset")
	}
	if err := mgr.streamReplyContinuation(ctx, bot, sess, 42, "exhausted", 0, nil); err == nil {
		t.Fatal("exhausted allowance started another model turn")
	}
	if len(h.Provider.Requests) != 2 {
		t.Fatal("exhausted continuation contacted provider")
	}
	mgr.store = &failingTelegramTurnStore{Store: store, failRole: llm.RoleDeveloper}
	if err := mgr.streamReplyContinuation(ctx, bot, sess, 42, "must commit before running", 1, nil); err == nil {
		t.Fatal("failed continuation persistence still started model work")
	}
	if len(h.Provider.Requests) != 2 {
		t.Fatal("uncommitted continuation contacted provider")
	}
}

type telegramContinuationRunner struct{ requests chan runpkg.Request }

func (r *telegramContinuationRunner) Run(ctx context.Context, req runpkg.Request, sink runpkg.EventSink) (runpkg.Result, error) {
	r.requests <- req
	req.OnEngineReady(req.Engine)
	defer req.OnEngineDone(req.Engine)
	message := llm.AssistantText("continued via shared runner")
	if err := req.OnResponseCompleted(ctx, 0, message, llm.TurnMetrics{}); err != nil {
		return runpkg.Result{}, err
	}
	sink.Event(llm.Event{Type: llm.EventTextDelta, Text: "continued via shared runner"})
	return runpkg.Result{}, nil
}

func TestTelegramContinuationSharedRunnerUsesExistingHistory(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := testutil.NewEngineHarness()
	runner := &telegramContinuationRunner{requests: make(chan runpkg.Request, 1)}
	mgr := &telegramSessionMgr{store: store, sessions: make(map[int64]*telegramSession), settings: Settings{MaxTurns: 50, Runner: runner, NewSession: func(context.Context) (*SessionRuntime, error) {
		return &SessionRuntime{Engine: h.Engine, ProviderName: "mock", ModelName: "mock"}, nil
	}}}
	sess, err := mgr.getOrCreate(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.closeAllSessions()
	sess.history = []llm.Message{llm.UserText("original task"), llm.AssistantText("completed partial work")}
	for _, message := range sess.history {
		if err := store.AddMessage(ctx, sess.meta.ID, session.NewMessage(sess.meta.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.IncrementUserTurns(ctx, sess.meta.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.streamReplyContinuation(ctx, &fakeBotSender{}, sess, 42, "Continue only unfinished work.", 2, nil); err != nil {
		t.Fatal(err)
	}
	req := <-runner.requests
	if req.MaxTurns != 2 || req.SessionID != sess.meta.ID || req.Engine != h.Engine || req.Persist || !req.DisableRuntimePersistence {
		t.Fatal("shared runner lost original runtime/session/budget ownership")
	}
	if len(req.Messages) != 3 || req.Messages[0].Role != llm.RoleUser || req.Messages[2].Role != llm.RoleDeveloper {
		t.Fatal("shared runner did not receive original history plus developer continuation")
	}
	meta, err := store.Get(ctx, sess.meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.UserTurns != 1 {
		t.Fatal("shared runner continuation counted as another user turn")
	}
}
