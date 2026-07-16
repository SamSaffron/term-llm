package cmd

import (
	"context"
	"encoding/json"
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
	rt := &serveRuntime{
		provider: provider, providerKey: "mock", engine: llm.NewEngine(provider, nil),
		store: store, defaultModel: "model", platform: "web",
	}
	if _, err := rt.Run(ctx, true, false, []llm.Message{llm.UserText("local question")}, llm.Request{SessionID: side.ID, Model: "model"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("requests = %d", len(provider.Requests))
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
