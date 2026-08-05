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

func TestResponsesBranchCreatesChildAndContinuesWithoutChangingSource(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sourceID = "serve-branch-source"
	if err := store.Create(ctx, &session.Session{ID: sourceID, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMessages(ctx, sourceID, []session.Message{
		*session.NewMessage(sourceID, llm.UserText("first"), -1),
		*session.NewMessage(sourceID, llm.AssistantText("first answer"), -1),
		*session.NewMessage(sourceID, llm.UserText("second"), -1),
		*session.NewMessage(sourceID, llm.AssistantText("second answer"), -1),
	}); err != nil {
		t.Fatal(err)
	}
	seed, _ := store.GetMessages(ctx, sourceID, 0, 0)
	anchorID := durableResponseIDForMessageID(seed[1].ID)
	mutationStore := store.(session.TranscriptUndoRedoStore)
	state, err := mutationStore.TranscriptMutationState(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("alternate answer")
	manager := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{provider: provider, engine: engine, store: store, defaultModel: "mock-model"}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()
	srv := &serveServer{sessionMgr: manager, store: store}
	body := fmt.Sprintf(`{"input":"alternate question","stream":true,"previous_response_id":%q,"branch":true,"expected_rev":%d,"idempotency_key":"branch-request"}`, anchorID, state.Rev)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", sourceID)
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rr.Code, rr.Body.String())
	}
	childID := strings.TrimSpace(rr.Header().Get("x-session-id"))
	if childID == "" || childID == sourceID {
		t.Fatalf("x-session-id = %q", childID)
	}
	sourceMessages, _ := store.GetMessages(ctx, sourceID, 0, 0)
	if len(sourceMessages) != 4 || sourceMessages[3].TextContent != "second answer" {
		t.Fatalf("source changed: %#v", sourceMessages)
	}
	childMessages, _ := store.GetMessages(ctx, childID, 0, 0)
	if len(childMessages) != 4 {
		t.Fatalf("child messages = %#v", childMessages)
	}
	if got, want := strings.TrimSpace(rr.Header().Get("x-branch-anchor-id")), durableResponseIDForMessageID(childMessages[1].ID); got != want {
		t.Fatalf("x-branch-anchor-id = %q, want copied child anchor %q", got, want)
	}
	want := []string{"first", "first answer", "alternate question", "alternate answer"}
	for i := range want {
		if childMessages[i].TextContent != want[i] {
			t.Fatalf("child message %d = %q, want %q", i, childMessages[i].TextContent, want[i])
		}
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}
	providerTexts := make([]string, 0, len(provider.Requests[0].Messages))
	for _, message := range provider.Requests[0].Messages {
		if text := strings.TrimSpace(llm.MessageText(message)); text != "" {
			providerTexts = append(providerTexts, text)
		}
	}
	if got := strings.Join(providerTexts, "|"); got != "first|first answer|alternate question" {
		t.Fatalf("provider copied prefix/input = %q", got)
	}

	// Retrying the same streamed branch request must resolve to the same child
	// and replay the existing response run instead of calling the provider again.
	retryReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	retryReq.Header.Set("Content-Type", "application/json")
	retryReq.Header.Set("session_id", sourceID)
	retryRR := httptest.NewRecorder()
	srv.handleResponses(retryRR, retryReq)
	if retryRR.Code != http.StatusOK {
		t.Fatalf("retry status/body = %d %s", retryRR.Code, retryRR.Body.String())
	}
	if retryChildID := strings.TrimSpace(retryRR.Header().Get("x-session-id")); retryChildID != childID {
		t.Fatalf("retry x-session-id = %q, want %q", retryChildID, childID)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests after retry = %d, want 1", len(provider.Requests))
	}
	retryMessages, _ := store.GetMessages(ctx, childID, 0, 0)
	if len(retryMessages) != len(childMessages) {
		t.Fatalf("child messages after retry = %d, want %d", len(retryMessages), len(childMessages))
	}

	treeReq := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+childID+"/tree", nil)
	treeRR := httptest.NewRecorder()
	srv.handleSessionByID(treeRR, treeReq)
	if treeRR.Code != http.StatusOK {
		t.Fatalf("tree status/body = %d %s", treeRR.Code, treeRR.Body.String())
	}
	var tree session.BranchTree
	if err := json.Unmarshal(treeRR.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	if tree.RootSessionID != sourceID || tree.ActiveSessionID != childID || len(tree.Nodes) != 2 || tree.PathCount != 2 {
		t.Fatalf("tree = %#v", tree)
	}
}

func TestResponsesBranchKeepsStaleAndHeaderConflictSemantics(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sourceID = "serve-branch-conflict"
	if err := store.Create(ctx, &session.Session{ID: sourceID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMessages(ctx, sourceID, []session.Message{
		*session.NewMessage(sourceID, llm.UserText("one"), -1),
		*session.NewMessage(sourceID, llm.AssistantText("one answer"), -1),
		*session.NewMessage(sourceID, llm.UserText("two"), -1),
	}); err != nil {
		t.Fatal(err)
	}
	messages, _ := store.GetMessages(ctx, sourceID, 0, 0)
	staleID := durableResponseIDForMessageID(messages[0].ID)
	srv := &serveServer{store: store}

	request := func(body, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("session_id", header)
		}
		rr := httptest.NewRecorder()
		srv.handleResponses(rr, req)
		return rr
	}
	withoutBranch := request(fmt.Sprintf(`{"input":"next","previous_response_id":%q}`, staleID), sourceID)
	if withoutBranch.Code != http.StatusConflict || !strings.Contains(withoutBranch.Body.String(), "stale") {
		t.Fatalf("ordinary stale response = %d %s", withoutBranch.Code, withoutBranch.Body.String())
	}
	withWrongHeader := request(fmt.Sprintf(`{"input":"next","previous_response_id":%q,"branch":true}`, staleID), "other")
	if withWrongHeader.Code != http.StatusConflict || !strings.Contains(withWrongHeader.Body.String(), "conflicts") {
		t.Fatalf("branch header conflict = %d %s", withWrongHeader.Code, withWrongHeader.Body.String())
	}
}

func TestResponsesBranchSupportsEmptyAnchorAndRequiresAnchorField(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sourceID = "serve-empty-branch-source"
	if err := store.Create(ctx, &session.Session{ID: sourceID, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sourceID, session.NewMessage(sourceID, llm.UserText("source stays here"), -1)); err != nil {
		t.Fatal(err)
	}

	provider := llm.NewMockProvider("mock").AddTextResponse("fresh answer")
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		runtime := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), store: store, defaultModel: "mock-model"}
		runtime.Touch()
		return runtime, nil
	})
	defer manager.Close()
	srv := &serveServer{sessionMgr: manager, store: store}

	missing := httptest.NewRecorder()
	missingReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"fresh question","stream":true,"branch":true}`))
	missingReq.Header.Set("Content-Type", "application/json")
	missingReq.Header.Set("session_id", sourceID)
	srv.handleResponses(missing, missingReq)
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "requires previous_response_id") {
		t.Fatalf("missing anchor status/body = %d %s", missing.Code, missing.Body.String())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"fresh question","stream":true,"previous_response_id":"resp_msg_0","branch":true,"idempotency_key":"empty-branch"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", sourceID)
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty branch status/body = %d %s", rr.Code, rr.Body.String())
	}
	childID := strings.TrimSpace(rr.Header().Get("x-session-id"))
	if childID == "" || childID == sourceID {
		t.Fatalf("empty branch child ID = %q", childID)
	}
	if got := rr.Header().Get("x-branch-anchor-id"); got != "" {
		t.Fatalf("empty branch copied anchor header = %q, want empty", got)
	}
	childMessages, err := store.GetMessages(ctx, childID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(childMessages) != 2 || childMessages[0].TextContent != "fresh question" || childMessages[1].TextContent != "fresh answer" {
		t.Fatalf("empty branch messages = %#v", childMessages)
	}
	sourceMessages, _ := store.GetMessages(ctx, sourceID, 0, 0)
	if len(sourceMessages) != 1 || sourceMessages[0].TextContent != "source stays here" {
		t.Fatalf("empty branch changed source = %#v", sourceMessages)
	}
}

func TestResponsesBranchRejectsActiveSourceBeforeCopy(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sourceID = "serve-active-branch-source"
	if err := store.Create(ctx, &session.Session{ID: sourceID, Provider: "mock", Model: "mock", Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sourceID, session.NewMessage(sourceID, llm.AssistantText("durable answer"), -1)); err != nil {
		t.Fatal(err)
	}
	messages, _ := store.GetMessages(ctx, sourceID, 0, 0)

	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	runtime := &serveRuntime{}
	active := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	close(active.done)
	runtime.setActiveInterrupt(active)
	defer runtime.clearActiveInterrupt(active)
	putTestSession(manager, sourceID, runtime)
	srv := &serveServer{sessionMgr: manager, store: store}

	rr := httptest.NewRecorder()
	body := fmt.Sprintf(`{"input":"alternate","stream":true,"previous_response_id":%q,"branch":true}`, durableResponseIDForMessageID(messages[0].ID))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", sourceID)
	srv.handleResponses(rr, req)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "source work is active") {
		t.Fatalf("active source status/body = %d %s", rr.Code, rr.Body.String())
	}
	tree, err := store.(session.ConversationBranchStore).GetBranchTree(ctx, sourceID)
	if err != nil || len(tree.Nodes) != 1 {
		t.Fatalf("active source created a branch: %#v, %v", tree, err)
	}
}

func TestBranchStreamingModeIsFirstPartyOnly(t *testing.T) {
	if _, ok := parseDurableResponseMessageID("resp_msg_0"); ok {
		t.Fatal("ordinary continuation parser accepted branch-only empty anchor")
	}
	if id, ok := parseBranchDurableResponseMessageID("resp_msg_0"); !ok || id != 0 {
		t.Fatalf("branch empty anchor parse = %d/%v", id, ok)
	}
	external := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	external.Header.Set("Idempotency-Key", "header-key")
	if branchUsesFirstPartyUIStream(external, true) {
		t.Fatal("external branch request selected first-party UI streaming")
	}
	if got := responseRunIdempotencyKey(external, responsesCreateRequest{IdempotencyKey: "unrelated-body-key"}); got != "header-key" {
		t.Fatalf("non-branch run idempotency key = %q, want header-key", got)
	}
	if got := responseRunIdempotencyKey(external, responsesCreateRequest{Branch: true, IdempotencyKey: "branch-body-key"}); got != "branch-body-key" {
		t.Fatalf("branch run idempotency key = %q, want branch-body-key", got)
	}
	firstParty := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	firstParty.Header.Set("X-Term-LLM-UI-Version", "test")
	if !branchUsesFirstPartyUIStream(firstParty, true) {
		t.Fatal("first-party branch request did not select UI streaming")
	}
	if branchUsesFirstPartyUIStream(firstParty, false) {
		t.Fatal("ordinary first-party request was mislabeled as a branch stream")
	}
}
