package serve

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

type telegramCheckpointProvider struct {
	*llm.MockProvider
	coordinator *restart.Coordinator
	requested   atomic.Bool
}

func (p *telegramCheckpointProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	stream, err := p.MockProvider.Stream(ctx, req)
	if p.requested.CompareAndSwap(false, true) {
		p.coordinator.Request()
	}
	return stream, err
}

type telegramCheckpointTool struct{ calls atomic.Int32 }

func (*telegramCheckpointTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "checkpoint_effect", Schema: map[string]any{"type": "object"}}
}
func (*telegramCheckpointTool) Preview(json.RawMessage) string { return "effect" }
func (t *telegramCheckpointTool) Execute(context.Context, json.RawMessage) (llm.ToolOutput, error) {
	t.calls.Add(1)
	return llm.TextOutput("done"), nil
}

func TestTelegramContinuationPreservesMessageAndDoesNotDuplicateUserTurn(t *testing.T) {
	old := restart.Default
	c := &restart.Coordinator{}
	restart.Default = c
	defer func() { restart.Default = old }()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "telegram.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &telegramCheckpointProvider{MockProvider: llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Text: "Hello", Usage: llm.Usage{InputTokens: 11, OutputTokens: 7}, ToolCalls: []llm.ToolCall{{ID: "one", Name: "checkpoint_effect", Arguments: json.RawMessage(`{}`)}}}).AddTextResponse("finished"), coordinator: c}
	tool := &telegramCheckpointTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	mgr := &telegramSessionMgr{store: store, sessions: make(map[int64]*telegramSession), settings: Settings{MaxTurns: 5, Store: store, NewSession: func(context.Context) (*SessionRuntime, error) {
		return &SessionRuntime{Engine: llm.NewEngine(provider, registry), Provider: provider, ProviderName: "mock", ModelName: "fixture"}, nil
	}}}
	sess, err := mgr.getOrCreate(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	bot := &fakeBotSender{nextID: 1}
	ctx, release, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec failure") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := mgr.streamReply(ctx, bot, sess, 42, llm.UserText("work")); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	saved := mgr.pendingReload[42]
	delete(mgr.pendingReload, 42)
	mgr.mu.Unlock()
	if saved == nil || saved.Engine == nil || len(saved.Engine.Pending) != 1 || saved.MessageID != 1 || tool.calls.Load() != 0 {
		t.Fatalf("invalid suspended delivery: %+v tools=%d", saved, tool.calls.Load())
	}
	release()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitReady(waitCtx); err != nil {
		t.Fatal(err)
	}
	resumeCtx, releaseResume, err := c.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseResume()
	if err := mgr.streamReplyContinuation(resumeCtx, bot, sess, 42, llm.Message{}, nil, saved); err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 1 {
		t.Fatal("tool replayed", tool.calls.Load())
	}
	placeholders := 0
	for _, text := range bot.allTexts() {
		if text == "⏳" {
			placeholders++
		}
	}
	if placeholders != 1 || !strings.Contains(bot.lastText(), "Hello") || !strings.Contains(bot.lastText(), "finished") {
		t.Fatal("delivery was duplicated or lost", placeholders, bot.allTexts())
	}
	messages, err := store.GetMessages(context.Background(), sess.meta.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, msg := range messages {
		if msg.Role == llm.RoleUser {
			users++
		}
	}
	meta, err := store.Get(context.Background(), sess.meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if users != 1 || meta.UserTurns != 1 || meta.ToolCalls != 1 || meta.InputTokens < 11 {
		t.Fatalf("duplicated/lost persisted progress: users=%d meta=%+v", users, meta)
	}
}

func TestTelegramResumeErrorDoesNotExposeProviderDetails(t *testing.T) {
	const secret = "synthetic-provider-secret"
	mgr := &telegramSessionMgr{
		pendingReload: map[int64]*telegramContinuation{42: {Engine: &llm.Continuation{}}},
		sessions:      make(map[int64]*telegramSession),
		settings: Settings{NewSession: func(context.Context) (*SessionRuntime, error) {
			return nil, errors.New("provider URL contains " + secret)
		}},
	}
	bot := &fakeBotSender{}
	if err := mgr.resumeTelegramContinuations(context.Background(), bot); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for len(bot.allTexts()) == 0 {
		select {
		case <-deadline:
			t.Fatal("resume failure was not reported")
		case <-time.After(time.Millisecond):
		}
	}
	texts := bot.allTexts()
	if len(texts) != 1 || texts[0] != "The interrupted response could not resume after process replacement." {
		t.Fatalf("unexpected public resume error: %q", texts)
	}
}
