package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestHandleSessionChildrenReturnsBoundedAuthoritativeProjection(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent := &session.Session{ID: "parent", Provider: "debug", Model: "fast", Mode: session.ModeChat}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &session.Session{
		ID: "child", Provider: "debug", Model: "fast", Mode: session.ModeChat,
		ParentID: parent.ID, IsSubagent: true, Agent: "reviewer", Status: session.StatusComplete,
	}
	if err := store.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	if storedChild, getErr := store.Get(ctx, child.ID); getErr != nil || storedChild.ParentID != parent.ID {
		t.Fatalf("stored child = %#v, err = %v", storedChild, getErr)
	}
	if err := store.AddMessage(ctx, child.ID, &session.Message{
		Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "Review the concurrency boundary"}},
		TextContent: "Review the concurrency boundary",
	}); err != nil {
		t.Fatal(err)
	}
	spawnCall := session.NewMessage(parent.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "spawn-1", Name: tools.SpawnAgentToolName},
	}}}, -1)
	if err := store.AddMessage(ctx, parent.ID, spawnCall); err != nil {
		t.Fatal(err)
	}
	spawnResult, _ := json.Marshal(tools.SpawnAgentResult{AgentName: "reviewer", SessionID: child.ID})
	if err := store.AddMessage(ctx, parent.ID, session.NewMessage(
		parent.ID,
		llm.ToolResultMessage("spawn-1", tools.SpawnAgentToolName, string(spawnResult), nil),
		-1,
	)); err != nil {
		t.Fatal(err)
	}
	parentMessages, err := store.GetMessages(ctx, parent.ID, 1, 0)
	if err != nil || len(parentMessages) != 1 {
		t.Fatalf("parent messages = %#v, err = %v", parentMessages, err)
	}
	spawnItemID := parentMessages[0].ID
	unrelated := &session.Session{ID: "unrelated", Provider: "debug", Model: "fast", Mode: session.ModeChat}
	if err := store.Create(ctx, unrelated); err != nil {
		t.Fatal(err)
	}

	listed, listErr := store.List(ctx, session.ListOptions{ParentID: parent.ID, Limit: maxChildRunProjection, SortByActivity: true})
	if listErr != nil || len(listed) != 1 {
		t.Fatalf("listed children = %#v, err = %v", listed, listErr)
	}

	runs := newServeResponseRunManager()
	defer runs.Close()
	srv := &serveServer{store: store, responseRuns: runs}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/children", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionChildren(rr, req, parent.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		ParentSessionID string               `json:"parent_session_id"`
		Revision        int64                `json:"revision"`
		Children        []childRunProjection `json:"children"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ParentSessionID != parent.ID || payload.Revision <= 0 || len(payload.Children) != 1 {
		t.Fatalf("projection = %#v", payload)
	}
	got := payload.Children[0]
	if got.SessionID != child.ID || got.ParentSessionID != parent.ID || got.ParentSpawnItemID != spawnItemID || got.ParentSpawnCallID != "spawn-1" || got.TaskSummary != "Review the concurrency boundary" || got.StartedAt <= 0 || got.EndedAt <= 0 {
		t.Fatalf("child projection = %#v", got)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	conditional := httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/children", nil)
	conditional.Header.Set("If-None-Match", etag)
	conditionalResult := httptest.NewRecorder()
	srv.handleSessionChildren(conditionalResult, conditional, parent.ID)
	if conditionalResult.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", conditionalResult.Code)
	}
}
