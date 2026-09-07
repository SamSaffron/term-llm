package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestServeRejectsNewUnsafeSessionIDs(t *testing.T) {
	for _, endpoint := range []struct {
		path   string
		body   string
		handle func(*serveServer, http.ResponseWriter, *http.Request)
	}{
		{"/v1/responses", `{"input":"hello"}`, (*serveServer).handleResponses},
		{"/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"}]}`, (*serveServer).handleChatCompletions},
		{"/v1/messages", `{"model":"mock","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, (*serveServer).handleAnthropicMessages},
	} {
		for _, header := range []string{"X-Term-LLM-Session-ID", "session_id"} {
			for _, id := range []string{"chat/session", `chat\session`, ".", "..", "session\x00id"} {
				t.Run(endpoint.path+"/"+header+"/"+id, func(t *testing.T) {
					created := 0
					manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
						created++
						return nil, fmt.Errorf("unexpected runtime creation")
					})
					defer manager.Close()
					srv := &serveServer{sessionMgr: manager}
					req := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set(header, id)
					rr := httptest.NewRecorder()
					endpoint.handle(srv, rr, req)
					if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "session ID") {
						t.Fatalf("status/body = %d %s, want invalid session ID", rr.Code, rr.Body.String())
					}
					if created != 0 {
						t.Fatalf("invalid ID created %d runtimes", created)
					}
				})
			}
		}
	}
}

func TestServeContinuesExistingUnsafeSessionID(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	legacy := &session.Session{ID: "chat/session", Provider: "mock", ProviderKey: "mock", Model: "mock-model"}
	if err := store.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("ok")
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), defaultModel: "mock-model"}, nil
	})
	defer manager.Close()
	srv := &serveServer{sessionMgr: manager, store: store}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-Session-ID", legacy.ID)
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d", len(provider.Requests))
	}
}

func TestHubLegacySessionNumberRoutes(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	legacy := &session.Session{ID: "chat/session", Provider: "mock", Model: "legacy-model"}
	if err := store.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	message := session.NewMessage(legacy.ID, llm.UserText("saved conversation"), -1)
	if err := store.AddMessage(context.Background(), legacy.ID, message); err != nil {
		t.Fatal(err)
	}
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		return nil, fmt.Errorf("read must not create a runtime")
	})
	defer manager.Close()
	srv := &serveServer{store: store, sessionMgr: manager}
	hub := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/chat")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			srv.handleSideQuestion(w, r)
		} else {
			srv.handleSessionByID(w, r)
		}
	})
	for _, path := range []string{
		fmt.Sprintf("/v1/sessions/%d/state", legacy.Number),
		fmt.Sprintf("/v1/sessions/%d/messages", legacy.Number),
		fmt.Sprintf("/api/sessions/%d/side-question", legacy.Number),
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			hub.handleNodeProxy(rr, httptest.NewRequest(http.MethodGet, "/node/alpha"+path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status/body = %d %s", rr.Code, rr.Body.String())
			}
			if strings.HasSuffix(path, "/state") && !strings.Contains(rr.Body.String(), "legacy-model") {
				t.Fatalf("wrong session state: %s", rr.Body.String())
			}
			if strings.HasSuffix(path, "/messages") && !strings.Contains(rr.Body.String(), "saved conversation") {
				t.Fatalf("saved messages missing: %s", rr.Body.String())
			}
		})
	}
	rr := httptest.NewRecorder()
	hub.handleNodeProxy(rr, httptest.NewRequest(http.MethodGet, "/node/alpha/v1/sessions/chat%2Fsession/state", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("encoded slash protection changed: %d", rr.Code)
	}
}
