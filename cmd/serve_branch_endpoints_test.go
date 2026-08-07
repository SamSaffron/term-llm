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

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestWebBranchTreePointsIncludeEveryVisibleUserMessage(t *testing.T) {
	message := func(id int64, sequence int, value llm.Message) session.Message {
		result := *session.NewMessage("source", value, sequence)
		result.ID = id
		return result
	}
	messages := []session.Message{
		message(10, 0, llm.UserText("first request")),
		message(20, 1, llm.AssistantText("first answer")),
		message(30, 2, llm.ToolResultMessage("call-1", "read_file", "done", nil)),
		message(40, 3, llm.UserText("second request")),
		message(50, 4, llm.UserText("consecutive correction")),
		message(60, 5, llm.AssistantText("second answer")),
		message(70, 6, llm.UserText("third request")),
		message(80, 7, llm.UserText("hidden compaction duplicate")),
	}
	messages[len(messages)-1].CompactionTail = true

	points := webBranchTreePoints(messages)
	if len(points) != 4 {
		t.Fatalf("branch points = %#v, want one for each of 4 visible user messages", points)
	}
	wantIDs := []int64{10, 40, 50, 70}
	wantAnchors := []int64{0, 20, 20, 60}
	for i, point := range points {
		if point.MessageID != wantIDs[i] || point.AnchorMessageID != wantAnchors[i] || point.Role != string(llm.RoleUser) || point.Prefill == "" {
			t.Fatalf("branch point %d = %#v, want message %d anchored at %d", i, point, wantIDs[i], wantAnchors[i])
		}
	}
}

func TestSessionBranchEndpointsCreateChildBeforePreparingContext(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sourceID = "immediate-branch-source"
	if err := store.Create(ctx, &session.Session{ID: sourceID, Name: "Parent chat", Provider: "mock", ProviderKey: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMessages(ctx, sourceID, []session.Message{
		*session.NewMessage(sourceID, llm.UserText("first"), -1),
		*session.NewMessage(sourceID, llm.AssistantText("first answer"), -1),
		*session.NewMessage(sourceID, llm.UserText("later finding"), -1),
	}); err != nil {
		t.Fatal(err)
	}
	messages, _ := store.GetMessages(ctx, sourceID, 0, 0)
	points := webBranchTreePoints(messages)
	if len(points) != 2 || points[0].Role != "user" || points[0].AnchorMessageID != 0 || points[0].LaterMessageCount != 2 ||
		points[1].Role != "user" || points[1].Prefill != "later finding" || points[1].AnchorMessageID != messages[1].ID || points[1].LaterMessageCount != 0 {
		t.Fatalf("web branch points = %#v", points)
	}
	state, _ := store.(session.TranscriptUndoRedoStore).TranscriptMutationState(ctx, sourceID)
	provider := llm.NewMockProvider("mock").AddTextResponse("- Keep the later finding.")
	srv := &serveServer{
		store:                    store,
		pathNotesProviderFactory: func(_, _ string) (llm.Provider, error) { return provider, nil },
	}
	treeReq := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sourceID+"/tree?include_branch_points=1", nil)
	treeRR := httptest.NewRecorder()
	srv.handleSessionByID(treeRR, treeReq)
	var treeResponse webBranchTreeResponse
	if treeRR.Code != http.StatusOK || json.Unmarshal(treeRR.Body.Bytes(), &treeResponse) != nil || len(treeResponse.BranchPoints) != 2 {
		t.Fatalf("tree branch points status/body = %d %s", treeRR.Code, treeRR.Body.String())
	}
	branchBody := fmt.Sprintf(`{"anchor_message_id":%d,"expected_rev":%d,"idempotency_key":"immediate-branch"}`, messages[1].ID, state.Rev)
	branchReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sourceID+"/branches", strings.NewReader(branchBody))
	branchReq.Header.Set("Content-Type", "application/json")
	branchRR := httptest.NewRecorder()
	srv.handleSessionByID(branchRR, branchReq)
	if branchRR.Code != http.StatusCreated {
		t.Fatalf("branch status/body = %d %s", branchRR.Code, branchRR.Body.String())
	}
	var created createSessionBranchResponse
	if err := json.Unmarshal(branchRR.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Session.ID == "" || created.Session.ID == sourceID || created.ParentSessionID != sourceID || created.ParentTitle != "Parent chat" || created.CopiedAnchorMessageID == 0 {
		t.Fatalf("created branch = %#v", created)
	}
	childMessages, _ := store.GetMessages(ctx, created.Session.ID, 0, 0)
	if len(childMessages) != 2 || len(provider.Requests) != 0 {
		t.Fatalf("child should exist before helper: messages=%#v requests=%d", childMessages, len(provider.Requests))
	}

	notesReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.Session.ID+"/path-notes", strings.NewReader(`{"mode":"notes"}`))
	notesReq.Header.Set("Content-Type", "application/json")
	notesRR := httptest.NewRecorder()
	srv.handleSessionByID(notesRR, notesReq)
	if notesRR.Code != http.StatusOK {
		t.Fatalf("notes status/body = %d %s", notesRR.Code, notesRR.Body.String())
	}
	childMessages, _ = store.GetMessages(ctx, created.Session.ID, 0, 0)
	if len(childMessages) != 3 || len(provider.Requests) != 1 {
		t.Fatalf("path notes not appended once: messages=%#v requests=%d", childMessages, len(provider.Requests))
	}
	provenance, ok := childMessages[2].PathNoteProvenance()
	if !ok || provenance.SourceSessionID != sourceID || provenance.AnchorMessageID != messages[1].ID {
		t.Fatalf("path note provenance = %#v", provenance)
	}

	replayReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.Session.ID+"/path-notes", strings.NewReader(`{"mode":"notes"}`))
	replayReq.Header.Set("Content-Type", "application/json")
	replayRR := httptest.NewRecorder()
	srv.handleSessionByID(replayRR, replayReq)
	if replayRR.Code != http.StatusOK || len(provider.Requests) != 1 {
		t.Fatalf("replay status/requests = %d/%d body=%s", replayRR.Code, len(provider.Requests), replayRR.Body.String())
	}

	tree, err := store.(session.ConversationBranchStore).GetBranchTree(ctx, created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	edge, ok := directBranchNode(tree, created.Session.ID)
	if !ok || edge.CopiedAnchorMessageID != created.CopiedAnchorMessageID {
		t.Fatalf("tree copied anchor = %#v", edge)
	}
}
