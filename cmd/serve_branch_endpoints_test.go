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

func TestActiveWebBranchSafetyAcceptsPublishedCompletedToolBoundary(t *testing.T) {
	const responseID = "resp-moving-boundary"
	messages := []session.Message{
		{ID: 1, Sequence: 0, Role: llm.RoleUser},
		{ID: 2, Sequence: 1, Role: llm.RoleAssistant, ResponseID: responseID},
		{ID: 3, Sequence: 2, Role: llm.RoleTool, ResponseID: responseID},
		{ID: 4, Sequence: 3, Role: llm.RoleAssistant, ResponseID: responseID},
	}
	if status := activeWebBranchAnchorSafety(messages, responseID, 3, 3); status != activeWebBranchAnchorSafe {
		t.Fatalf("published tool boundary status = %v, want safe", status)
	}
	if status := activeWebBranchAnchorSafety(messages, responseID, 3, 4); status != activeWebBranchAnchorUnstable {
		t.Fatalf("partial row status = %v, want unstable", status)
	}
	messages = append(messages, session.Message{ID: 5, Sequence: 4, Role: llm.RoleSystem})
	if status := activeWebBranchAnchorSafety(messages, responseID, 3, 5); status != activeWebBranchAnchorInvalid {
		t.Fatalf("system row status = %v, want invalid", status)
	}
	pruned := pruneActiveWebBranchOutput(messages, responseID, 3)
	if len(pruned) != 3 || pruned[len(pruned)-1].ID != 3 {
		t.Fatalf("active prefix = %#v", pruned)
	}
}

func TestWebBranchTreePointsPruneActiveResponseOutputAndUnsafeInterjections(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const (
		sourceID   = "active-tree-source"
		responseID = "resp-active-tree"
	)
	fallbackMessages := []session.Message{
		{ID: 1, Role: llm.RoleAssistant},
		{ID: 2, Role: llm.RoleUser},
		{ID: 3, Role: llm.RoleUser},
		{ID: 4, Role: llm.RoleAssistant, ResponseID: responseID},
	}
	if got := activeWebBranchAnchorRowID(fallbackMessages, responseID, 0); got != -1 {
		t.Fatalf("missing published active anchor = %d, want fail-closed sentinel -1", got)
	}
	if got := activeWebBranchAnchorRowID([]session.Message{{ID: 1, Role: llm.RoleAssistant, ResponseID: responseID}}, responseID, 0); got != -1 {
		t.Fatalf("unbounded active anchor = %d, want fail-closed sentinel -1", got)
	}
	if points := webBranchTreePointsForActiveRun(fallbackMessages, 999); len(points) != 0 {
		t.Fatalf("missing active anchor exposed branch points: %#v", points)
	}
	if err := store.Create(ctx, &session.Session{ID: sourceID, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	partialAssistant := *session.NewMessage(sourceID, llm.AssistantText("unfinished answer"), -1)
	partialAssistant.ResponseID = responseID
	partialTool := *session.NewMessage(sourceID, llm.ToolResultMessage("call-active", "read_file", "unfinished tool output", nil), -1)
	partialTool.ResponseID = responseID
	if err := store.ReplaceMessages(ctx, sourceID, []session.Message{
		*session.NewMessage(sourceID, llm.UserText("completed request"), -1),
		*session.NewMessage(sourceID, llm.AssistantText("completed answer"), -1),
		*session.NewMessage(sourceID, llm.UserText("active request"), -1),
		partialAssistant,
		partialTool,
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := store.GetMessages(ctx, sourceID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	manager := newServeResponseRunManager()
	t.Cleanup(manager.Close)
	run := newResponseRun(responseID, sourceID, "", "mock-model", time.Now().Unix(), nil)
	run.anchorRowID = messages[2].ID
	run.anchorAvailable = true
	if err := manager.create(run); err != nil {
		t.Fatal(err)
	}
	manager.setActiveRun(sourceID, responseID)
	srv := &serveServer{store: store, responseRuns: manager}

	loadPoints := func() []webBranchTreePoint {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sourceID+"/tree?include_branch_points=1", nil)
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("tree status/body = %d %s", rr.Code, rr.Body.String())
		}
		var response webBranchTreeResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.BranchPoints
	}

	points := loadPoints()
	if len(points) != 2 || points[1].MessageID != messages[2].ID || points[1].AnchorMessageID != messages[1].ID || points[1].Prefill != "active request" || points[1].LaterMessageCount != 0 {
		t.Fatalf("active branch points = %#v, want active user anchored at completed answer with no partial suffix", points)
	}

	if err := store.AddMessage(ctx, sourceID, session.NewMessage(sourceID, llm.UserText("mid-run interjection"), -1)); err != nil {
		t.Fatal(err)
	}
	points = loadPoints()
	if len(points) != 2 || points[1].MessageID != messages[2].ID || points[1].LaterMessageCount != 0 {
		t.Fatalf("interjection affected the safe active branch snapshot: %#v", points)
	}
}

func TestSessionBranchEndpointAllowsActiveSourceAtStableAnchor(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const (
		sourceID   = "active-branch-source"
		responseID = "resp-active-branch"
	)
	if err := store.Create(ctx, &session.Session{ID: sourceID, Name: "Active parent", Provider: "mock", ProviderKey: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	partial := *session.NewMessage(sourceID, llm.AssistantText("still streaming"), -1)
	partial.ResponseID = responseID
	if err := store.ReplaceMessages(ctx, sourceID, []session.Message{
		*session.NewMessage(sourceID, llm.UserText("completed request"), -1),
		*session.NewMessage(sourceID, llm.AssistantText("stable answer"), -1),
		*session.NewMessage(sourceID, llm.UserText("active request"), -1),
		partial,
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := store.GetMessages(ctx, sourceID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	manager := newServeResponseRunManager()
	t.Cleanup(manager.Close)
	run := newResponseRun(responseID, sourceID, "", "mock-model", time.Now().Unix(), nil)
	run.anchorRowID = messages[2].ID
	run.anchorAvailable = true
	if err := manager.create(run); err != nil {
		t.Fatal(err)
	}
	manager.setActiveRun(sourceID, responseID)
	srv := &serveServer{store: store, responseRuns: manager}

	// expected_rev is intentionally stale. Active output may advance the full
	// transcript head, but it cannot mutate the completed anchor copied below.
	body := fmt.Sprintf(`{"anchor_message_id":%d,"expected_rev":0,"idempotency_key":"active-stable-anchor"}`, messages[1].ID)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sourceID+"/branches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("active branch status/body = %d %s", rr.Code, rr.Body.String())
	}
	var created createSessionBranchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	childMessages, err := store.GetMessages(ctx, created.Session.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(childMessages) != 2 || childMessages[1].TextContent != "stable answer" {
		t.Fatalf("active branch copied messages = %#v, want completed prefix only", childMessages)
	}
	if manager.activeRunID(sourceID) != responseID {
		t.Fatalf("branch detached active response ownership: %q", manager.activeRunID(sourceID))
	}

	body = fmt.Sprintf(`{"anchor_message_id":%d,"idempotency_key":"active-partial-anchor"}`, messages[3].ID)
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sourceID+"/branches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not stable") {
		t.Fatalf("partial active anchor status/body = %d %s", rr.Code, rr.Body.String())
	}

	interjection := session.NewMessage(sourceID, llm.UserText("mid-run interjection"), -1)
	if err := store.AddMessage(ctx, sourceID, interjection); err != nil {
		t.Fatal(err)
	}
	body = fmt.Sprintf(`{"anchor_message_id":%d,"idempotency_key":"active-interjection-anchor"}`, interjection.ID)
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sourceID+"/branches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not stable") {
		t.Fatalf("interjection anchor status/body = %d %s", rr.Code, rr.Body.String())
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

	branchReplayReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sourceID+"/branches", strings.NewReader(branchBody))
	branchReplayReq.Header.Set("Content-Type", "application/json")
	branchReplayRR := httptest.NewRecorder()
	srv.handleSessionByID(branchReplayRR, branchReplayReq)
	var replayed createSessionBranchResponse
	if branchReplayRR.Code != http.StatusOK || json.Unmarshal(branchReplayRR.Body.Bytes(), &replayed) != nil || !replayed.Reused || replayed.ForkAfterMessageID != messages[1].ID {
		t.Fatalf("branch replay status/body = %d %s", branchReplayRR.Code, branchReplayRR.Body.String())
	}
	mismatchBody := fmt.Sprintf(`{"anchor_message_id":%d,"idempotency_key":"immediate-branch"}`, messages[0].ID)
	mismatchReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sourceID+"/branches", strings.NewReader(mismatchBody))
	mismatchReq.Header.Set("Content-Type", "application/json")
	mismatchRR := httptest.NewRecorder()
	srv.handleSessionByID(mismatchRR, mismatchReq)
	if mismatchRR.Code != http.StatusConflict || !strings.Contains(mismatchRR.Body.String(), "different branch point") {
		t.Fatalf("mismatched replay status/body = %d %s", mismatchRR.Code, mismatchRR.Body.String())
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
