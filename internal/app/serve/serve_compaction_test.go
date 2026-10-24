package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

type serveCompactionContextTool struct{}

func (*serveCompactionContextTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "test_context", Schema: map[string]any{"type": "object"}}
}
func (*serveCompactionContextTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("ok"), nil
}
func (*serveCompactionContextTool) Preview(json.RawMessage) string { return "" }
func (*serveCompactionContextTool) PrepareRequestContext(_ context.Context, _ string, messages []core.Message) ([]core.Message, error) {
	return messages, nil
}
func (*serveCompactionContextTool) PrepareCompactionContext(_ context.Context, _ string, result *engine.CompactionResult) error {
	result.EphemeralMessages = append(result.EphemeralMessages, core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "ephemeral plan"}}})
	return nil
}

func newServeCompactionTestRuntime(t *testing.T, provider core.Provider) (*serveRuntime, []core.Message) {
	t.Helper()
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sess := &session.Session{
		ID: "compact-session", Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginWeb, Status: session.StatusActive,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	history := []core.Message{
		core.UserText("first question"), core.AssistantText("first answer"),
		core.UserText("second question"), core.AssistantText("second answer"),
	}
	stored := make([]session.Message, 0, len(history))
	for i, message := range history {
		stored = append(stored, *session.NewMessage(sess.ID, message, i/2))
	}
	if err := store.ReplaceMessages(ctx, sess.ID, stored); err != nil {
		t.Fatal(err)
	}
	engine := engine.NewEngine(provider, nil)
	engine.RegisterTool(&serveCompactionContextTool{})
	return &serveRuntime{
		provider: provider, providerKey: "mock", defaultModel: "mock-model",
		engine: engine, store: store, sessionMeta: sess,
		history: history, historyPersisted: true,
	}, history
}

func TestServeRuntimeCompactSessionPersistsAndReplacesActiveHistory(t *testing.T) {
	ctx := context.Background()
	provider := mockprovider.NewMockProvider("mock").AddTurn(mockprovider.MockTurn{
		Text:  "Continue from the second answer.",
		Usage: core.Usage{InputTokens: 20, OutputTokens: 5},
	})
	rt, history := newServeCompactionTestRuntime(t, provider)
	store, sess := rt.store, rt.sessionMeta

	result, err := rt.compactSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("compactSession: %v", err)
	}
	if result == nil || result.OriginalCount != len(history) {
		t.Fatalf("result = %+v, want original count %d", result, len(history))
	}
	if len(result.EphemeralMessages) != 1 {
		t.Fatalf("ephemeral messages = %#v", result.EphemeralMessages)
	}
	for _, message := range rt.history {
		if message.Role == core.RoleDeveloper || strings.Contains(core.MessageText(message), "ephemeral plan") {
			t.Fatalf("request-only compaction context leaked into runtime history: %#v", rt.history)
		}
	}
	if len(rt.history) == 0 || !rt.historyPersisted {
		t.Fatalf("runtime history was not replaced: persisted=%v history=%+v", rt.historyPersisted, rt.history)
	}
	active, err := session.LoadActiveMessages(ctx, store, rt.sessionMeta)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != len(rt.history) {
		t.Fatalf("persisted active messages = %d, runtime history = %d", len(active), len(rt.history))
	}
	if rt.sessionMeta.CompactionCount != 1 {
		t.Fatalf("compaction count = %d, want 1", rt.sessionMeta.CompactionCount)
	}
	if rt.cumulativeUsage.InputTokens != 20 || rt.cumulativeUsage.OutputTokens != 5 {
		t.Fatalf("cumulative usage = %+v", rt.cumulativeUsage)
	}
}

type nativeCompactProvider struct {
	*mockprovider.MockProvider
	calls int
}

func (p *nativeCompactProvider) NativeCompact(context.Context, core.NativeCompactRequest) (*core.NativeCompactResult, error) {
	p.calls++
	return &core.NativeCompactResult{PreTokens: 900, PostTokens: 300}, nil
}

func TestServeRuntimeCompactSessionPrefersNativeCompaction(t *testing.T) {
	provider := &nativeCompactProvider{MockProvider: mockprovider.NewMockProvider("mock")}
	rt, history := newServeCompactionTestRuntime(t, provider)
	rt.beginCompactionActivity()
	prepared, err := rt.prepareCompaction(context.Background(), rt.sessionMeta.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.execute(context.Background())
	rt.finishCompactionActivityWithRelease(err, prepared.release)
	if err != nil || result != nil || provider.calls != 1 {
		t.Fatalf("execute = %+v, %v; native calls = %d", result, err, provider.calls)
	}
	if len(rt.history) != len(history) || rt.sessionMeta.CompactionCount != 0 {
		t.Fatalf("native compaction rewrote the transcript: history=%d count=%d", len(rt.history), rt.sessionMeta.CompactionCount)
	}
	if got := rt.activitySnapshot(); got.Status != "completed" || got.Message != "Context compacted natively (900 → 300 tokens)." {
		t.Fatalf("activity = %+v", got)
	}
}

func TestServeRuntimeCompactSessionRejectsBusyRuntime(t *testing.T) {
	rt := &serveRuntime{}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, err := rt.compactSession(context.Background(), "session"); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("compactSession error = %v, want errServeSessionBusy", err)
	}
}

func TestServeSessionRuntimeCompactEndpoint(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess := &session.Session{
		ID: "web-compact", Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginWeb, Status: session.StatusActive,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, message := range []core.Message{core.UserText("question"), core.AssistantText("answer")} {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("Continue from the answer.")
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{
			provider: provider, providerKey: "mock", defaultModel: "mock-model",
			engine: engine.NewEngine(provider, nil), store: store,
		}, nil
	})
	t.Cleanup(manager.Close)
	server := &serveServer{sessionMgr: manager, store: store}

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/web-compact/runtime/compact", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.handleSessionByID(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status/body = %d %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"activity":`) {
		t.Fatalf("response = %q", rr.Body.String())
	}
	if activity := waitForServeCompaction(t, server, sess.ID); activity.Status != "completed" {
		t.Fatalf("compaction failed: %+v", activity)
	}
	stored, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CompactionCount != 1 {
		t.Fatalf("compaction count = %d, want 1", stored.CompactionCount)
	}
}
