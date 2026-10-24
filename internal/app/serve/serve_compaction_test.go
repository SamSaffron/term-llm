package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	calls    int
	imported string
	err      error
}

func (p *nativeCompactProvider) ImportProviderState(state []byte) error {
	p.imported = string(state)
	return nil
}

func (p *nativeCompactProvider) NativeCompact(context.Context, core.NativeCompactRequest) (*core.NativeCompactResult, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
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

// Native compaction runs against the provider's own session, which may be the
// thing that is broken: Claude Code cannot compact a single oversized exchange.
// The transcript compaction does not depend on that session and must still run.
func TestServeRuntimeCompactSessionFallsBackWhenNativeCompactionFails(t *testing.T) {
	provider := &nativeCompactProvider{
		MockProvider: mockprovider.NewMockProvider("mock").AddTurn(mockprovider.MockTurn{Text: "Continue from the second answer."}),
		err:          errors.New("claude-bin native compact: Prompt is too long; a single-exchange conversation cannot be compacted"),
	}
	rt, history := newServeCompactionTestRuntime(t, provider)
	rt.beginCompactionActivity()
	prepared, err := rt.prepareCompaction(context.Background(), rt.sessionMeta.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.execute(context.Background())
	rt.finishCompactionActivityWithRelease(err, prepared.release)
	if err != nil || provider.calls != 1 {
		t.Fatalf("execute err = %v; native calls = %d", err, provider.calls)
	}
	if result == nil || result.OriginalCount != len(history) {
		t.Fatalf("transcript compaction did not run: %+v", result)
	}
	if rt.sessionMeta.CompactionCount != 1 {
		t.Fatalf("compaction count = %d, want 1", rt.sessionMeta.CompactionCount)
	}
}

func TestServeRuntimeCompactSessionReturnsNativeCancellation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		cancelParent bool
	}{
		{"parent cancelled", context.Canceled, true},
		// A provider's own child context can expire while the caller's is live.
		{"provider deadline", fmt.Errorf("claude-bin native compact: %w", context.DeadlineExceeded), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &nativeCompactProvider{MockProvider: mockprovider.NewMockProvider("mock"), err: tc.err}
			rt, history := newServeCompactionTestRuntime(t, provider)
			prepared, err := rt.prepareCompaction(context.Background(), rt.sessionMeta.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelParent {
				cancel()
			}
			result, err := prepared.execute(ctx)
			prepared.release()
			if !errors.Is(err, tc.err) && !errors.Is(err, context.Canceled) || result != nil {
				t.Fatalf("execute = %+v, %v; want %v", result, err, tc.err)
			}
			if len(rt.history) != len(history) || rt.sessionMeta.CompactionCount != 0 {
				t.Fatal("cancelled compaction fell back and rewrote the transcript")
			}
		})
	}
}

func TestServeRuntimeCompactSessionRestoresProviderStateBeforeAnyTurn(t *testing.T) {
	// A runtime rehydrated after a restart has run no turn yet, so the provider
	// has not imported its persisted resume state; native compaction needs it.
	ctx := context.Background()
	provider := &nativeCompactProvider{MockProvider: mockprovider.NewMockProvider("mock")}
	rt, _ := newServeCompactionTestRuntime(t, provider)
	if err := rt.store.(session.ProviderStateStore).SaveProviderState(ctx, rt.sessionMeta.ID, "mock", []byte(`{"session_id":"claude-1"}`)); err != nil {
		t.Fatal(err)
	}
	prepared, err := rt.prepareCompaction(ctx, rt.sessionMeta.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.release()
	if provider.imported != `{"session_id":"claude-1"}` {
		t.Fatalf("provider state not restored before compaction: %q", provider.imported)
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

// A compacted session's runtime history holds only the active tail. A later
// turn that falls back to snapshot persistence must replace only that tail:
// a plain replacement deleted every archived row and the compaction boundary.
// Unanswered user turns in the compacted tail carry no client_message_id (the
// archived originals keep theirs), so the next turn trims them as retries and
// always takes the snapshot path. That is how a stuck, compacted web session
// lost its whole history on the first post after the rescue.
func TestServeRunAfterCompactionPreservesArchivedTranscript(t *testing.T) {
	ctx := context.Background()
	provider := mockprovider.NewMockProvider("mock").
		AddTurn(mockprovider.MockTurn{Text: "Continue from the second answer."}).
		AddTextResponse("fresh answer")
	rt, _ := newServeCompactionTestRuntime(t, provider)
	store, sess := rt.store, rt.sessionMeta
	for _, text := range []string{"hi", "oh hello"} {
		unanswered := core.UserText(text)
		unanswered.ClientMessageID = "client-" + strings.ReplaceAll(text, " ", "-")
		rt.history = append(rt.history, unanswered)
	}
	seeded := make([]session.Message, 0, len(rt.history))
	for i, message := range rt.history {
		seeded = append(seeded, *session.NewMessage(sess.ID, message, i/2))
	}
	if err := store.ReplaceMessages(ctx, sess.ID, seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.compactSession(ctx, sess.ID); err != nil {
		t.Fatalf("compactSession: %v", err)
	}
	compacted, err := store.Get(ctx, sess.ID)
	if err != nil || !session.HasCompactionBoundary(compacted) {
		t.Fatalf("no compaction boundary after compaction: %+v, %v", compacted, err)
	}
	before, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Rehydrate as a fresh runtime would after the idle TTL.
	rt.history, rt.historyPersisted, rt.sessionMeta = nil, false, compacted
	if !rt.restorePersistedHistory(ctx, compacted) {
		t.Fatal("restore failed")
	}
	input := core.UserText("next question")
	input.ClientMessageID = "client-next"
	if _, err := rt.Run(ctx, true, false, []core.Message{input}, core.Request{SessionID: sess.ID}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	after, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CompactionSeq != compacted.CompactionSeq || after.CompactionCount != compacted.CompactionCount {
		t.Fatalf("compaction boundary changed from %d/%d to %d/%d", compacted.CompactionSeq, compacted.CompactionCount, after.CompactionSeq, after.CompactionCount)
	}
	stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < compacted.CompactionSeq; i++ {
		if i >= len(stored) || stored[i].ID != before[i].ID || stored[i].TextContent != before[i].TextContent {
			t.Fatalf("archived row %d was rewritten or deleted: %d rows before, %d after", i, len(before), len(stored))
		}
	}
	var sawNext, sawAnswer bool
	for _, msg := range stored[compacted.CompactionSeq:] {
		sawNext = sawNext || msg.TextContent == "next question"
		sawAnswer = sawAnswer || strings.Contains(msg.TextContent, "fresh answer")
	}
	if !sawNext || !sawAnswer {
		t.Fatalf("new turn missing from active tail: %+v", stored[compacted.CompactionSeq:])
	}
}

// An explicit fresh-history run (Responses API without previous_response_id)
// owns the whole conversation: it resets the transcript, boundary included.
func TestServeReplaceHistoryRunResetsCompactedTranscript(t *testing.T) {
	ctx := context.Background()
	provider := mockprovider.NewMockProvider("mock").
		AddTurn(mockprovider.MockTurn{Text: "Continue from the second answer."}).
		AddTextResponse("fresh answer")
	rt, _ := newServeCompactionTestRuntime(t, provider)
	store, sess := rt.store, rt.sessionMeta
	if _, err := rt.compactSession(ctx, sess.ID); err != nil {
		t.Fatalf("compactSession: %v", err)
	}
	if compacted, err := store.Get(ctx, sess.ID); err != nil || !session.HasCompactionBoundary(compacted) {
		t.Fatalf("no compaction boundary: %+v, %v", compacted, err)
	}

	if _, err := rt.Run(ctx, true, true, []core.Message{core.UserText("brand new conversation")}, core.Request{SessionID: sess.ID}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	after, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.HasCompactionBoundary(after) {
		t.Fatalf("boundary survived a whole-history replacement: %d/%d", after.CompactionSeq, after.CompactionCount)
	}
	stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range stored {
		if strings.Contains(msg.TextContent, "first question") || strings.Contains(msg.TextContent, "Continue from the second answer") {
			t.Fatalf("old transcript survived reset: %+v", stored)
		}
	}
	if len(stored) == 0 || !strings.Contains(stored[len(stored)-1].TextContent, "fresh answer") {
		t.Fatalf("new conversation not persisted: %+v", stored)
	}
}
