package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func newSideTestStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestServeRuntimeSideUsesInheritedContextWithoutPersistingIt(t *testing.T) {
	ctx := context.Background()
	store := newSideTestStore(t)
	parent := &session.Session{ID: "parent", Provider: "mock", ProviderKey: "mock", Model: "model", Origin: session.OriginWeb}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, parent.ID, session.NewMessage(parent.ID, llm.UserText("inherited reference"), -1)); err != nil {
		t.Fatal(err)
	}
	side, err := store.ForkSide(ctx, parent.ID, session.OriginWeb)
	if err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("side answer")
	registry := llm.NewToolRegistry()
	registry.Register(&serveRuntimeTestTool{}) // unknown/custom mutation surface must fail closed
	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{
		provider: provider, providerKey: "mock", engine: engine,
		store: store, defaultModel: "model", platform: "web",
	}
	if _, err := rt.Run(ctx, true, false, []llm.Message{llm.UserText("local question")}, llm.Request{SessionID: side.ID, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("requests = %d", len(provider.Requests))
	}
	if _, ok := engine.Tools().Get("serve_runtime_test_tool"); ok {
		t.Fatal("unknown dynamic tool remained available in side runtime")
	}
	request := provider.Requests[0]
	joined := ""
	for _, msg := range request.Messages {
		joined += llm.MessageText(msg) + "\n"
	}
	for _, want := range []string{"reference-only", "inherited reference", "local question"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("request missing %q: %s", want, joined)
		}
	}
	persisted, err := store.GetMessages(ctx, side.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range persisted {
		if strings.Contains(msg.TextContent, "inherited reference") || strings.Contains(msg.TextContent, "reference-only") {
			t.Fatalf("inherited/policy content polluted local transcript: %#v", persisted)
		}
	}
}

func TestServeRuntimeSideCompactionConsumesInheritedContextOnce(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "serve-runtime-compact", Model: "compact-runtime", InputLimit: 1000}})
	defer llm.RegisterConfigLimits(nil)
	ctx := context.Background()
	store := newSideTestStore(t)
	parent := &session.Session{ID: "parent", Provider: "serve-runtime-compact", ProviderKey: "serve-runtime-compact", Model: "compact-runtime"}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, parent.ID, session.NewMessage(parent.ID, llm.UserText(strings.Repeat("inherited history ", 1000)), -1)); err != nil {
		t.Fatal(err)
	}
	side, err := store.ForkSide(ctx, parent.ID, session.OriginWeb)
	if err != nil {
		t.Fatal(err)
	}
	provider := &serveRuntimeCompactionProvider{}
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: engine, store: store, autoCompact: true, defaultModel: "compact-runtime"}
	rt.configureContextManagementForRequest(llm.Request{Model: "compact-runtime"})
	engine.SetContextEstimateBaseline(910, 4)
	if _, err := rt.Run(ctx, true, false, []llm.Message{llm.UserText("continue")}, llm.Request{SessionID: side.ID, Model: "compact-runtime", Tools: []llm.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}}}); err != nil {
		t.Fatal(err)
	}
	base, err := store.GetSideContext(ctx, side.ID)
	if err != nil || len(base) != 0 {
		t.Fatalf("persisted inherited context after compaction=%#v err=%v", base, err)
	}
	if len(rt.baseContext) != 0 {
		t.Fatalf("runtime inherited context after compaction=%#v", rt.baseContext)
	}
}

func TestServeRuntimeClosedSideRejectsRun(t *testing.T) {
	ctx := context.Background()
	store := newSideTestStore(t)
	parent := &session.Session{ID: "parent", Provider: "mock", Model: "model"}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	side, err := store.ForkSide(ctx, parent.ID, session.OriginWeb)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloseSide(ctx, side.ID); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("must not run")
	rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), store: store, defaultModel: "model"}
	_, err = rt.Run(ctx, true, false, []llm.Message{llm.UserText("hello")}, llm.Request{SessionID: side.ID})
	if err != session.ErrSideClosed {
		t.Fatalf("error = %v", err)
	}
	if len(provider.Requests) != 0 {
		t.Fatal("closed side reached provider")
	}
}

type failingSideContextStore struct {
	session.Store
	session.SideStore
	err error
}

func (s failingSideContextStore) GetSideContext(context.Context, string) ([]llm.Message, error) {
	return nil, s.err
}

func TestConfigureSideRuntimePreservesContextLoadError(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	parent := &session.Session{ID: "parent_error", Provider: "mock", Model: "model"}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	side, err := store.ForkSide(ctx, parent.ID, session.OriginWeb)
	if err != nil {
		t.Fatal(err)
	}
	contextErr := errors.New("context storage unavailable")
	rt := &serveRuntime{store: failingSideContextStore{Store: store, SideStore: store, err: contextErr}}
	if err := rt.configureSideRuntime(ctx, side); !errors.Is(err, contextErr) {
		t.Fatalf("configure error = %v, want wrapped context error", err)
	}
}

func TestServeSideCloseRejectsConcurrentRun(t *testing.T) {
	store := newSideTestStore(t)
	parent := &session.Session{ID: "parent", Provider: "mock", Model: "model"}
	if err := store.Create(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	side, err := store.ForkSide(context.Background(), parent.ID, session.OriginWeb)
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{}
	rt.mu.Lock() // models the run lock held from start through completion
	mgr := &serveSessionManager{sessions: map[string]*serveRuntime{side.ID: rt}}
	server := &serveServer{store: store, sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+side.ID+"/side/close", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	server.handleSessionByID(rec, req)
	rt.mu.Unlock()
	if rec.Code != http.StatusConflict {
		t.Fatalf("close while running status=%d body=%s", rec.Code, rec.Body.String())
	}
	persisted, _ := store.Get(context.Background(), side.ID)
	if persisted.SideState != session.SideOpen {
		t.Fatalf("close raced through active run: %s", persisted.SideState)
	}
}

func TestServeSideEndpointsCreateBackCloseAndReopen(t *testing.T) {
	store := newSideTestStore(t)
	parent := &session.Session{ID: "parent", Provider: "mock", Model: "model", Origin: session.OriginWeb}
	if err := store.Create(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	server := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/parent/side", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	server.handleSessionByID(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var side session.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &side); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/"+side.ID+"/relationship", nil)
	rec = httptest.NewRecorder()
	server.handleSessionByID(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"parent"`) {
		t.Fatalf("relationship status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+side.ID+"/side/close", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	server.handleSessionByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("close status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+side.ID+"/side/reopen", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	server.handleSessionByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reopen status=%d body=%s", rec.Code, rec.Body.String())
	}
}
