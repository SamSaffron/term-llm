package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestServeSideQuestionIsEphemeralAndToolless(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("private answer")
	rt := &serveRuntime{
		providerKey: "mock", defaultModel: "test-model",
		history: []llm.Message{llm.UserText("main question"), llm.AssistantText("main answer")},
	}
	rt.refreshSideQuestionSnapshot(rt.history)
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }

	events, err := rt.startSideQuestion(sideQuestionStart{Question: "clarify"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	view := rt.sideQuestion.view()
	if view.Running || len(view.History) != 1 || view.History[0].Response != "private answer" {
		t.Fatalf("view = %#v", view)
	}
	if len(rt.history) != 2 {
		t.Fatalf("side content entered main history: %#v", rt.history)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d", len(provider.Requests))
	}
	req := provider.Requests[0]
	if !req.Ephemeral || req.Search || len(req.Tools) != 0 || req.MaxTurns != 1 {
		t.Fatalf("unsafe side request: %#v", req)
	}
}

func TestServeSideFollowUpRefreshesMainContextAndKeepsPrivateHistoryChronological(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("first side answer").AddTextResponse("second side answer")
	rt := &serveRuntime{providerKey: "mock", defaultModel: "m"}
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	rt.refreshSideQuestionSnapshot([]llm.Message{llm.UserText("main one"), llm.AssistantText("main answer one")})

	events, err := rt.startSideQuestion(sideQuestionStart{Question: "first side question"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	rt.refreshSideQuestionSnapshot([]llm.Message{
		llm.UserText("main one"), llm.AssistantText("main answer one"),
		llm.UserText("main two"), llm.AssistantText("main answer two"),
	})
	events, err = rt.startSideQuestion(sideQuestionStart{Question: "second side question"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	if len(provider.Requests) != 2 {
		t.Fatalf("provider requests = %d", len(provider.Requests))
	}
	var joined strings.Builder
	for _, msg := range provider.Requests[1].Messages {
		for _, part := range msg.Parts {
			joined.WriteString(part.Text)
			joined.WriteByte('\n')
		}
	}
	text := joined.String()
	positions := []int{
		strings.Index(text, "main two"),
		strings.Index(text, "first side question"),
		strings.Index(text, "first side answer"),
		strings.LastIndex(text, "second side question"),
	}
	for i, position := range positions {
		if position < 0 || (i > 0 && position <= positions[i-1]) {
			t.Fatalf("follow-up context is missing or out of order: %q", text)
		}
	}
	if got := rt.sideQuestion.view().History; len(got) != 2 {
		t.Fatalf("private history len = %d, want 2", len(got))
	}
}

func TestServeSideQuestionEndpointsRecoverCancelAndClear(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("answer")
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{providerKey: "mock", defaultModel: "m"}
		rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
		return rt, nil
	})
	defer manager.Close()
	if _, err := manager.GetOrCreate(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	s := &serveServer{sessionMgr: manager}

	body, _ := json.Marshal(sideQuestionStart{Question: "question"})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/main/side-question", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleSideQuestion(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "done") {
		t.Fatalf("POST status/body = %d %q", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/sessions/main/side-question", nil)
	rr = httptest.NewRecorder()
	s.handleSideQuestion(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "answer") {
		t.Fatalf("GET status/body = %d %q", rr.Code, rr.Body.String())
	}

	for _, suffix := range []string{"active", "history", "history"} {
		req = httptest.NewRequest(http.MethodDelete, "/api/sessions/main/side-question/"+suffix, nil)
		rr = httptest.NewRecorder()
		s.handleSideQuestion(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s status = %d", suffix, rr.Code)
		}
	}
	rt, ok := manager.Get("main")
	if !ok {
		t.Fatal("runtime disappeared")
	}
	view := rt.sideQuestion.view()
	if len(view.History) != 0 || view.Question != "" || view.Response != "" || view.Error != "" {
		t.Fatalf("clear retained private side state: %#v", view)
	}
}

func TestServeSideQuestionRejectsToolAttemptFromHistory(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddToolCall("call", "shell", map[string]any{"command": "touch /tmp/no"})
	rt := &serveRuntime{providerKey: "mock", defaultModel: "m"}
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	events, err := rt.startSideQuestion(sideQuestionStart{Question: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	view := rt.sideQuestion.view()
	if !view.Synthetic || len(view.History) != 0 {
		t.Fatalf("tool attempt state = %#v", view)
	}
}

type blockingSideProvider struct{}

func (blockingSideProvider) Name() string                   { return "blocking" }
func (blockingSideProvider) Credential() string             { return "test" }
func (blockingSideProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (blockingSideProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	return &blockingSideStream{ctx: ctx}, nil
}

type blockingSideStream struct{ ctx context.Context }

func (s *blockingSideStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	return llm.Event{}, s.ctx.Err()
}
func (*blockingSideStream) Close() error { return nil }

func TestServeSideQuestionConcurrencyAndCancellationAreIndependent(t *testing.T) {
	provider := blockingSideProvider{}
	rt := &serveRuntime{providerKey: "blocking", defaultModel: "m"}
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	events, err := rt.startSideQuestion(sideQuestionStart{Question: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.startSideQuestion(sideQuestionStart{Question: "second"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second side start error = %v", err)
	}
	mainCtx, cancelMain := context.WithCancel(context.Background())
	cancelMain()
	if mainCtx.Err() == nil || !rt.sideQuestion.view().Running {
		t.Fatal("main cancellation affected side request")
	}
	rt.sideQuestion.cancelActive()
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("side cancellation did not stop provider")
	}
}

func TestServeSideQuestionHydratesPersistedHistoryOnFreshRuntime(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := &session.Session{
		ID: "persisted", Provider: "mock", ProviderKey: "mock", Model: "persisted-model",
		ReasoningEffort: "high", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := store.Create(ctx, meta); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []llm.Message{llm.UserText("persisted fact"), llm.AssistantText("persisted answer")} {
		if err := store.AddMessage(ctx, meta.ID, session.NewMessage(meta.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("side answer")
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{providerKey: "mock", defaultModel: "default", store: store}
		rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
		return rt, nil
	})
	defer manager.Close()
	s := &serveServer{sessionMgr: manager, store: store}

	body := bytes.NewBufferString(`{"question":"what was persisted?","model":"browser-override","reasoning_effort":"low"}`)
	rr := httptest.NewRecorder()
	s.handleSideQuestion(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/persisted/side-question", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status/body = %d %q", rr.Code, rr.Body.String())
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("requests = %d", len(provider.Requests))
	}
	req := provider.Requests[0]
	if req.Model != "persisted-model" || req.ReasoningEffort != "high" {
		t.Fatalf("runtime config = %q/%q, want persisted-model/high", req.Model, req.ReasoningEffort)
	}
	joined := ""
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			joined += part.Text + "\n"
		}
	}
	if !strings.Contains(joined, "persisted fact") || strings.Contains(joined, "browser-override") {
		t.Fatalf("request did not use persisted history/config: %q", joined)
	}
	stored, err := store.GetMessages(ctx, meta.ID, 0, 0)
	if err != nil || len(stored) != 2 {
		t.Fatalf("persisted transcript changed: len=%d err=%v", len(stored), err)
	}
}

func TestServeSideQuestionRejectsNonexistentSession(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	})
	defer manager.Close()
	s := &serveServer{sessionMgr: manager, store: store}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		path := "/api/sessions/missing/side-question"
		var body *bytes.Reader
		if method == http.MethodPost {
			body = bytes.NewReader([]byte(`{"question":"q"}`))
		} else {
			body = bytes.NewReader(nil)
		}
		if method == http.MethodDelete {
			path += "/history"
		}
		rr := httptest.NewRecorder()
		s.handleSideQuestion(rr, httptest.NewRequest(method, path, body))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", method, rr.Code)
		}
	}
}

type stubbornSideProvider struct {
	release chan struct{}
	mu      sync.Mutex
	starts  int
}

func (p *stubbornSideProvider) Name() string                   { return "stubborn" }
func (p *stubbornSideProvider) Credential() string             { return "test" }
func (p *stubbornSideProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p *stubbornSideProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	return &stubbornSideStream{release: p.release}, nil
}

func (p *stubbornSideProvider) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

type stubbornSideStream struct{ release <-chan struct{} }

func (s *stubbornSideStream) Recv() (llm.Event, error) {
	<-s.release
	return llm.Event{}, io.EOF
}
func (*stubbornSideStream) Close() error { return nil }

type burstSideProvider struct{}

func (burstSideProvider) Name() string                   { return "burst" }
func (burstSideProvider) Credential() string             { return "test" }
func (burstSideProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (burstSideProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return &burstSideStream{}, nil
}

type burstSideStream struct{ index int }

func (s *burstSideStream) Recv() (llm.Event, error) {
	if s.index == 100 {
		return llm.Event{}, io.EOF
	}
	s.index++
	return llm.Event{Type: llm.EventTextDelta, Text: "x"}, nil
}
func (*burstSideStream) Close() error { return nil }

func TestServeSideQuestionTerminalEventSurvivesBackpressure(t *testing.T) {
	rt := &serveRuntime{providerKey: "burst", defaultModel: "m"}
	rt.configureSideQuestionContext()
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return burstSideProvider{}, nil }
	events, err := rt.startSideQuestion(sideQuestionStart{Question: "question"})
	if err != nil {
		t.Fatal(err)
	}
	rt.sideQuestion.mu.Lock()
	done := rt.sideQuestion.done
	rt.sideQuestion.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("provider could not finish while event buffer was backpressured")
	}
	terminal := 0
	for event := range events {
		if event.Result != nil || event.Err != nil {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal events = %d, want 1", terminal)
	}
}

func TestServeSideQuestionCancelDoesNotOverlapStubbornRestart(t *testing.T) {
	provider := &stubbornSideProvider{release: make(chan struct{})}
	rt := &serveRuntime{providerKey: "stubborn", defaultModel: "m"}
	rt.configureSideQuestionContext()
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	events, err := rt.startSideQuestion(sideQuestionStart{Question: "first"})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rt.sideQuestion.cancelActive()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded cancellation took %v", elapsed)
	}
	if _, err := rt.startSideQuestion(sideQuestionStart{Question: "second"}); err == nil || !strings.Contains(err.Error(), "still stopping") {
		t.Fatalf("restart error = %v", err)
	}
	if provider.startCount() != 1 {
		t.Fatalf("overlapping starts = %d", provider.startCount())
	}
	close(provider.release)
	for range events {
	}
}
