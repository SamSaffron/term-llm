package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func newTitleRefineTestStore(t *testing.T, id string) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Create(context.Background(), &session.Session{
		ID: id, Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginWeb, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func addTitleRefineMessage(t *testing.T, store session.Store, sessionID string, message llm.Message) {
	t.Helper()
	if err := store.AddMessage(context.Background(), sessionID, session.NewMessage(sessionID, message, -1)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionTitleRefineIncludesLatestMessageAfterPrefixLimit(t *testing.T) {
	const sessionID = "title-refine-head-tail"
	store := newTitleRefineTestStore(t, sessionID)
	addTitleRefineMessage(t, store, sessionID, llm.UserText("Two things in this screenshot are annoying"))
	for i := 0; i < sessionTitleMessagePageSize-1; i++ {
		addTitleRefineMessage(t, store, sessionID, llm.ToolResultMessage(
			fmt.Sprintf("call-%d", i),
			"inspect",
			"intermediate tool output",
			nil,
		))
	}
	const finalAnswer = "Corrected false active-run detection and removed the merge confirmation gate."
	addTitleRefineMessage(t, store, sessionID, llm.AssistantText(finalAnswer))

	provider := llm.NewMockProvider("title").AddTextResponse(
		`{"short_title":"Fix Worktree Merge Warnings","long_title":"Correct false worktree detection and remove merge confirmation","confidence":0.95}`,
	)
	server := &serveServer{
		cfgRef: &config.Config{},
		store:  store,
		titleProviderFactory: func(*config.Config) (llm.Provider, error) {
			return provider, nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/title/refine", strings.NewReader(`{"preview":true}`))
	response := httptest.NewRecorder()

	server.handleSessionTitleRefine(response, req, sessionID)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	requests := provider.RecordedRequests()
	if len(requests) != 1 || len(requests[0].Messages) < 2 {
		t.Fatalf("recorded requests = %#v", requests)
	}
	prompt := llm.MessageText(requests[0].Messages[1])
	if !strings.Contains(prompt, finalAnswer) {
		t.Fatalf("title prompt omitted latest assistant answer: %s", prompt)
	}
}

func TestSessionTitleRefineReturnsExpectedAbstention(t *testing.T) {
	const sessionID = "title-refine-abstention"
	store := newTitleRefineTestStore(t, sessionID)
	addTitleRefineMessage(t, store, sessionID, llm.UserText("please fix it"))
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Name = "Existing title"
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	provider := llm.NewMockProvider("title").AddTextResponse(
		`{"short_title":null,"long_title":null,"confidence":0}`,
	)
	server := &serveServer{
		cfgRef: &config.Config{},
		store:  store,
		titleProviderFactory: func(*config.Config) (llm.Provider, error) {
			return provider, nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/title/refine", strings.NewReader(`{"preview":true}`))
	response := httptest.NewRecorder()

	server.handleSessionTitleRefine(response, req, sessionID)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["refinement_status"] != "abstained" {
		t.Fatalf("refinement_status = %#v, want abstained", body["refinement_status"])
	}
	if body["short_title"] != "Existing title" {
		t.Fatalf("short_title = %#v, want existing title", body["short_title"])
	}
}
