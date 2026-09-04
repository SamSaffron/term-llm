package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestHandleCreateWebSessionPersistsBlankWorkspaceSession(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	startupDir := t.TempDir()
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		runtime := &serveRuntime{provider: provider, providerKey: "mock", defaultModel: "default", store: store}
		runtime.Touch()
		return runtime, nil
	})
	defer manager.Close()
	server := &serveServer{
		cfg: serveServerConfig{ui: true}, store: store, sessionMgr: manager, startupDir: startupDir,
	}
	body, err := json.Marshal(createWebSessionRequest{
		Model: "chosen-model", ReasoningEffort: "medium", UseDefaultWorkspace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Term-LLM-UI-Version", "test")
	response := httptest.NewRecorder()

	server.handleSessions(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Session webSessionEntry `json:"session"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(context.Background(), payload.Session.ID)
	if err != nil || persisted == nil {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
	if persisted.MessageCount != 0 || persisted.Origin != session.OriginWeb || persisted.Model != "chosen-model" {
		t.Fatalf("blank session=%#v", persisted)
	}
	cwd, err := server.resolveShellCWD(context.Background(), persisted.ID)
	if err != nil || !sameServePath(cwd, startupDir) {
		t.Fatalf("shell cwd=%q err=%v, want %q", cwd, err, startupDir)
	}
}

func TestHandleCreateWebSessionRejectsNonUIRequest(t *testing.T) {
	server := &serveServer{store: &session.SQLiteStore{}, sessionMgr: newServeSessionManager(time.Minute, 1, nil)}
	defer server.sessionMgr.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader([]byte("{}")))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.handleSessions(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
