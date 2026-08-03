package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type undoRedoResetProvider struct {
	*llm.MockProvider
	resets int
}

func (p *undoRedoResetProvider) ResetConversation() { p.resets++ }

func newServeUndoRedoTest(t *testing.T) (*serveServer, *session.SQLiteStore, *serveRuntime, *undoRedoResetProvider, string) {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sessionID := session.NewID()
	if err := store.Create(context.Background(), &session.Session{ID: sessionID, Provider: "mock", ProviderKey: "mock", Model: "mock", Mode: session.ModeChat}); err != nil {
		t.Fatal(err)
	}
	provider := &undoRedoResetProvider{MockProvider: llm.NewMockProvider("mock")}
	rt := &serveRuntime{
		provider:         provider,
		providerKey:      "mock",
		engine:           llm.NewEngine(provider, nil),
		store:            store,
		historyPersisted: true,
	}
	rt.Touch()
	mgr := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) { return rt, nil })
	t.Cleanup(mgr.Close)
	if _, err := mgr.GetOrCreate(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store, sessionMgr: mgr, responseRuns: newServeResponseRunManager()}
	t.Cleanup(func() { srv.responseRuns.Close() })
	return srv, store, rt, provider, sessionID
}

func addServeUndoRedoMessage(t *testing.T, store *session.SQLiteStore, sessionID string, msg llm.Message) session.Message {
	t.Helper()
	stored := session.NewMessage(sessionID, msg, -1)
	if err := store.AddMessage(context.Background(), sessionID, stored); err != nil {
		t.Fatal(err)
	}
	return *stored
}

func requestServeUndoRedo(t *testing.T, srv *serveServer, sessionID, operation string, state session.TranscriptMutationState) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(sessionUndoRedoRequest{ExpectedRev: state.Rev, ExpectedHeadID: state.HeadID})
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/runtime/"+operation, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	return rr
}

func TestServeUndoRedoShrinksRestoresAndResetsRuntime(t *testing.T) {
	srv, store, rt, provider, sessionID := newServeUndoRedoTest(t)
	first := addServeUndoRedoMessage(t, store, sessionID, llm.UserText("first"))
	second := addServeUndoRedoMessage(t, store, sessionID, llm.AssistantText("answer"))
	third := addServeUndoRedoMessage(t, store, sessionID, llm.UserText("restore me"))
	fourth := addServeUndoRedoMessage(t, store, sessionID, llm.AssistantText("latest answer"))
	rt.history = []llm.Message{first.ToLLMMessage(), second.ToLLMMessage(), third.ToLLMMessage(), fourth.ToLLMMessage()}
	if err := store.SaveProviderState(context.Background(), sessionID, "mock", []byte(`{"continuation":true}`)); err != nil {
		t.Fatal(err)
	}
	state, err := store.TranscriptMutationState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rr := requestServeUndoRedo(t, srv, sessionID, "undo", state)
	if rr.Code != http.StatusOK {
		t.Fatalf("undo status/body = %d %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Rev      int64  `json:"rev"`
		HeadID   int64  `json:"head_id"`
		UserText string `json:"user_text"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.UserText != "restore me" || len(rt.history) != 2 {
		t.Fatalf("undo payload/history = %#v len=%d", payload, len(rt.history))
	}
	if provider.resets != 1 || !rt.historyPersisted {
		t.Fatalf("runtime reset count=%d persisted=%v", provider.resets, rt.historyPersisted)
	}
	providerState, err := store.LoadProviderState(context.Background(), sessionID, "mock")
	if err != nil || len(providerState) != 0 {
		t.Fatalf("provider state = %q err=%v", providerState, err)
	}
	messages, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil || len(messages) != 2 {
		t.Fatalf("messages after undo len=%d err=%v", len(messages), err)
	}

	rr = requestServeUndoRedo(t, srv, sessionID, "redo", session.TranscriptMutationState{Rev: payload.Rev, HeadID: payload.HeadID})
	if rr.Code != http.StatusOK {
		t.Fatalf("redo status/body = %d %s", rr.Code, rr.Body.String())
	}
	messages, err = store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil || len(messages) != 4 {
		t.Fatalf("messages after redo len=%d err=%v", len(messages), err)
	}
	if messages[2].ID != third.ID || messages[3].ID != fourth.ID || len(rt.history) != 4 {
		t.Fatalf("redo did not restore exact suffix/runtime: %#v history=%d", messages, len(rt.history))
	}
	if provider.resets != 2 {
		t.Fatalf("provider reset count after redo = %d", provider.resets)
	}
}

func TestServeUndoRedoSequentialCommandsRestoreTurnsInLIFOOrder(t *testing.T) {
	srv, store, rt, provider, sessionID := newServeUndoRedoTest(t)
	turnB := addServeUndoRedoMessage(t, store, sessionID, llm.UserText("turn B"))
	answerB := addServeUndoRedoMessage(t, store, sessionID, llm.AssistantText("answer B"))
	turnA := addServeUndoRedoMessage(t, store, sessionID, llm.UserText("turn A"))
	answerA := addServeUndoRedoMessage(t, store, sessionID, llm.AssistantText("answer A"))
	rt.history = []llm.Message{turnB.ToLLMMessage(), answerB.ToLLMMessage(), turnA.ToLLMMessage(), answerA.ToLLMMessage()}
	state, err := store.TranscriptMutationState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	type mutationPayload struct {
		Rev      int64  `json:"rev"`
		HeadID   int64  `json:"head_id"`
		UserText string `json:"user_text"`
	}
	mutate := func(operation string) mutationPayload {
		t.Helper()
		rr := requestServeUndoRedo(t, srv, sessionID, operation, state)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status/body = %d %s", operation, rr.Code, rr.Body.String())
		}
		var payload mutationPayload
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		state = session.TranscriptMutationState{Rev: payload.Rev, HeadID: payload.HeadID}
		return payload
	}

	if payload := mutate("undo"); payload.UserText != "turn A" || len(rt.history) != 2 {
		t.Fatalf("first undo payload=%+v history=%d", payload, len(rt.history))
	}
	if payload := mutate("undo"); payload.UserText != "turn B" || len(rt.history) != 0 {
		t.Fatalf("second undo payload=%+v history=%d", payload, len(rt.history))
	}
	mutate("redo")
	messages, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil || len(messages) != 2 || messages[0].ID != turnB.ID || messages[1].ID != answerB.ID || len(rt.history) != 2 {
		t.Fatalf("first redo messages=%#v history=%d err=%v", messages, len(rt.history), err)
	}
	mutate("redo")
	messages, err = store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil || len(messages) != 4 || messages[2].ID != turnA.ID || messages[3].ID != answerA.ID || len(rt.history) != 4 {
		t.Fatalf("second redo messages=%#v history=%d err=%v", messages, len(rt.history), err)
	}
	if provider.resets != 4 {
		t.Fatalf("provider resets=%d, want one per sequential mutation", provider.resets)
	}
}

func TestServeUndoRejectsActiveWorkAndStaleClient(t *testing.T) {
	srv, store, _, _, sessionID := newServeUndoRedoTest(t)
	addServeUndoRedoMessage(t, store, sessionID, llm.UserText("prompt"))
	state, err := store.TranscriptMutationState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	srv.responseRuns.setActiveRun(sessionID, "resp_active")
	rr := requestServeUndoRedo(t, srv, sessionID, "undo", state)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "work is active") {
		t.Fatalf("active status/body = %d %s", rr.Code, rr.Body.String())
	}
	srv.responseRuns.clearActiveRun(sessionID, "resp_active")

	stale := state
	stale.HeadID++
	rr = requestServeUndoRedo(t, srv, sessionID, "undo", stale)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "transcript changed") {
		t.Fatalf("stale status/body = %d %s", rr.Code, rr.Body.String())
	}
	messages, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("stale request mutated transcript len=%d err=%v", len(messages), err)
	}
}

func TestServeUndoWorksWithoutCachedRuntimeOrRuntimeFactory(t *testing.T) {
	srv, store, _, _, sessionID := newServeUndoRedoTest(t)
	addServeUndoRedoMessage(t, store, sessionID, llm.UserText("storage only"))
	state, err := store.TranscriptMutationState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	srv.sessionMgr.mu.Lock()
	delete(srv.sessionMgr.sessions, sessionID)
	srv.sessionMgr.factory = func(context.Context) (*serveRuntime, error) {
		factoryCalls++
		return nil, context.Canceled
	}
	srv.sessionMgr.mu.Unlock()

	rr := requestServeUndoRedo(t, srv, sessionID, "undo", state)
	if rr.Code != http.StatusOK {
		t.Fatalf("storage-only undo status/body = %d %s", rr.Code, rr.Body.String())
	}
	if factoryCalls != 0 {
		t.Fatalf("runtime factory called %d times, want 0", factoryCalls)
	}
	messages, err := store.GetMessages(context.Background(), sessionID, 0, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("storage-only undo messages=%d err=%v", len(messages), err)
	}
}

func TestServeUndoReportsAttachmentsOmitted(t *testing.T) {
	srv, store, _, _, sessionID := newServeUndoRedoTest(t)
	message := llm.UserText("describe this")
	message.Parts = append(message.Parts, llm.Part{Type: llm.PartFile, Text: "file contents"})
	addServeUndoRedoMessage(t, store, sessionID, message)
	state, err := store.TranscriptMutationState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rr := requestServeUndoRedo(t, srv, sessionID, "undo", state)
	if rr.Code != http.StatusOK {
		t.Fatalf("undo status/body = %d %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		AttachmentsOmitted bool   `json:"attachments_omitted"`
		UserText           string `json:"user_text"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.AttachmentsOmitted || payload.UserText != "describe this" {
		t.Fatalf("payload = %s, want text-only composer value with attachment warning flag", rr.Body.String())
	}
}
