package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type serveShareGistMock struct {
	public      bool
	description string
	files       map[string]string
	err         error
}

func (m *serveShareGistMock) Create(description string, public bool, files map[string]string) (*gist.Gist, error) {
	m.description, m.public, m.files = description, public, files
	if m.err != nil {
		return nil, m.err
	}
	return &gist.Gist{ID: "abc123", URL: "https://gist.github.com/test/abc123", Public: public}, nil
}

func newServeShareFixture(t *testing.T) (*serveServer, int64) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	sess := &session.Session{
		ID: "share-session", Name: "Share test", Provider: "mock", Model: "model",
		Mode: session.ModeChat, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now,
		UserTurns: 4, LLMTurns: 4, ToolCalls: 8, InputTokens: 100, OutputTokens: 50,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	for _, message := range []llm.Message{
		llm.UserText("private prompt"),
		llm.AssistantText("shareable answer"),
		llm.UserText("later prompt"),
	} {
		if err := store.AddMessage(context.Background(), sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &serveServer{store: store}, messages[1].ID
}

func serveShareRequest(t *testing.T, server *serveServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/share-session/shares", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	server.handleSessionByID(rr, req)
	return rr
}

func TestCreateSessionShareResponseUsesTextOnlyExport(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	mock := &serveShareGistMock{}
	server.shareClientFactory = func() (serveGistCreator, error) { return mock, nil }
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"response","public":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !mock.public || !strings.Contains(mock.description, "assistant response") {
		t.Fatalf("gist request: public=%v description=%q", mock.public, mock.description)
	}
	for name, content := range mock.files {
		if !strings.Contains(content, "shareable answer") {
			t.Fatalf("%s omitted response", name)
		}
		if strings.Contains(content, "private prompt") || strings.Contains(content, "later prompt") {
			t.Fatalf("%s leaked prompt: %s", name, content)
		}
		if strings.Contains(content, "## Metrics") || strings.Contains(content, ">Activity<") {
			t.Fatalf("%s included whole-session metrics", name)
		}
	}
	if !strings.Contains(rr.Body.String(), `"preview_url":"https://gisthost.github.io/?abc123/index.html"`) {
		t.Fatalf("response=%s", rr.Body.String())
	}
}

func TestCreateSessionShareExplainsGitHubCLIDependency(t *testing.T) {
	server, anchor := newServeShareFixture(t)
	server.shareClientFactory = func() (serveGistCreator, error) {
		return nil, errors.New("gh CLI not found. Install from: https://cli.github.com")
	}
	rr := serveShareRequest(t, server, `{"anchor_message_id":`+formatInt64(anchor)+`,"scope":"conversation"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "requires the gh CLI") || !strings.Contains(rr.Body.String(), "cli.github.com") {
		t.Fatalf("dependency guidance missing: %s", rr.Body.String())
	}
}

func TestCreateSessionShareRejectsInvalidAnchorAndMethod(t *testing.T) {
	server, _ := newServeShareFixture(t)
	rr := serveShareRequest(t, server, `{"anchor_message_id":999999,"scope":"response"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("anchor status=%d body=%s", rr.Code, rr.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/share-session/shares", nil)
	rr = httptest.NewRecorder()
	server.handleSessionByID(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("method status=%d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
