package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

func TestSideQuestionDoesNotMasqueradeAsMainRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &serveRuntime{}
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.running = true
	rt.sideQuestion.cancel = cancel
	rt.sideQuestion.done = make(chan struct{})
	rt.sideQuestion.mu.Unlock()
	t.Cleanup(cancel)

	if rt.hasActiveRun() {
		t.Fatal("side-only request reported an active main run")
	}
	if !rt.hasActiveActivity() {
		t.Fatal("side-only request was not protected as active runtime activity")
	}
	if ctx.Err() != nil {
		t.Fatal("test side context unexpectedly cancelled")
	}
}

func TestReplaceHistoryFailureRestoresSideQuestionState(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddError(errors.New("boom"))
	rt := &serveRuntime{
		providerKey: "mock", defaultModel: "m", provider: provider,
		engine: llm.NewEngine(provider, nil), history: []llm.Message{llm.UserText("old main")},
	}
	rt.refreshSideQuestionSnapshot(rt.history)
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.history = []sidequestion.Entry{{Question: "old side", Response: "old answer"}}
	rt.sideQuestion.mu.Unlock()

	_, err := rt.Run(context.Background(), true, true, []llm.Message{llm.UserText("replacement")}, llm.Request{Model: "m"})
	if err == nil {
		t.Fatal("replace-history run unexpectedly succeeded")
	}
	if len(rt.history) != 1 || llm.MessageText(rt.history[0]) != "old main" {
		t.Fatalf("main history was not restored: %#v", rt.history)
	}
	view := rt.sideQuestion.view()
	if len(view.History) != 1 || view.History[0].Question != "old side" {
		t.Fatalf("side history was not restored: %#v", view)
	}
	rt.sideQuestion.mu.Lock()
	snapshot := sidequestion.CloneMessages(rt.sideQuestion.mainSnapshot)
	rt.sideQuestion.mu.Unlock()
	if len(snapshot) != 1 || llm.MessageText(snapshot[0]) != "old main" {
		t.Fatalf("side snapshot was not restored: %#v", snapshot)
	}
}

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
	if view.Running || len(view.History) != 1 || view.History[0].Response != "private answer" || view.Question != "" || view.Response != "" {
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

func TestServeSideQuestionPreservesMainPrefixAndDeduplicatesRuntimeContext(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTextResponse("answer")
	rt := &serveRuntime{providerKey: "mock", defaultModel: "m", systemPrompt: "system", platform: "web"}
	rt.platformMessages.Web = "platform"
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	mainPrefix := []llm.Message{
		llm.SystemText("system"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "platform"}}},
		llm.UserText("main question"),
	}
	rt.refreshSideQuestionSnapshot(mainPrefix)

	events, err := rt.startSideQuestion(sideQuestionStart{Question: "side question"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	got := provider.Requests[0].Messages
	if len(got) < len(mainPrefix) {
		t.Fatalf("request too short: %#v", got)
	}
	for i, want := range mainPrefix {
		if got[i].Role != want.Role || llm.MessageText(got[i]) != llm.MessageText(want) {
			t.Fatalf("shared prefix message %d = %#v, want %#v", i, got[i], want)
		}
	}
	for _, text := range []string{"system", "platform"} {
		count := 0
		for _, msg := range got {
			if llm.MessageText(msg) == text {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("context %q appeared %d times", text, count)
		}
	}
}

func TestAppendMissingSideContextRebuildsCanonicalPrefix(t *testing.T) {
	contextMessages := []llm.Message{
		llm.SystemText("system"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "platform"}}},
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "cwd"}}},
	}
	snapshot := []llm.Message{
		llm.SystemText("system"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "platform"}}},
		llm.UserText("main question"), llm.AssistantText("main answer"),
	}
	got := appendMissingSideContext(snapshot, contextMessages)
	want := []string{"system", "platform", "cwd", "main question", "main answer"}
	if len(got) != len(want) {
		t.Fatalf("messages = %#v", got)
	}
	for i, text := range want {
		if llm.MessageText(got[i]) != text {
			t.Fatalf("message %d = %#v, want %q", i, got[i], text)
		}
	}
}

func TestServeSideQuestionClearHistoryInvalidatesActiveGeneration(t *testing.T) {
	provider := &stubbornSideProvider{release: make(chan struct{})}
	rt := &serveRuntime{providerKey: "stubborn", defaultModel: "m"}
	rt.configureSideQuestionContext()
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
	events, err := rt.startSideQuestion(sideQuestionStart{Question: "question"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for provider.startCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	rt.sideQuestion.clearHistory()
	close(provider.release)
	for range events {
	}
	view := rt.sideQuestion.view()
	if view.Running || len(view.History) != 0 || view.Question != "" || view.Response != "" {
		t.Fatalf("cleared side state was resurrected: %#v", view)
	}
}

func TestServeSideUsageSurvivesPrivateHistoryClear(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddTurn(llm.MockTurn{Text: "answer", Usage: llm.Usage{InputTokens: 11, OutputTokens: 3}})
	rt := &serveRuntime{providerKey: "mock", defaultModel: "m"}
	rt.configureSideQuestionContext()
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }

	events, err := rt.startSideQuestion(sideQuestionStart{Question: "question"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	rt.sideQuestion.clearHistory()
	view := rt.sideQuestion.view()
	if view.Requests != 1 || view.TotalUsage.InputTokens != 11 || view.TotalUsage.OutputTokens != 3 {
		t.Fatalf("side accounting was cleared with private history: %#v", view)
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
	if generation := rr.Header().Get("x-side-generation"); generation == "" || generation == "0" {
		t.Fatalf("POST generation header = %q", generation)
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

type pauseSecondTurnProvider struct {
	base          *llm.MockProvider
	mu            sync.Mutex
	calls         int
	secondStarted chan struct{}
	release       chan struct{}
}

func (p *pauseSecondTurnProvider) Name() string       { return p.base.Name() }
func (p *pauseSecondTurnProvider) Credential() string { return p.base.Credential() }
func (p *pauseSecondTurnProvider) Capabilities() llm.Capabilities {
	return p.base.Capabilities()
}
func (p *pauseSecondTurnProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 2 {
		close(p.secondStarted)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.base.Stream(ctx, req)
}

func TestServeSideQuestionForksAtCurrentCompletedToolBoundary(t *testing.T) {
	mainBase := llm.NewMockProvider("main").
		WithCapabilities(llm.Capabilities{ToolCalls: true}).
		AddToolCall("call-1", "echo", map[string]any{"input": "live tool context"}).
		AddTextResponse("main done")
	mainProvider := &pauseSecondTurnProvider{
		base: mainBase, secondStarted: make(chan struct{}), release: make(chan struct{}),
	}
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	sideProvider := llm.NewMockProvider("side").AddTextResponse("side answer")
	rt := &serveRuntime{
		providerKey: "main", defaultModel: "m", provider: mainProvider,
		engine: llm.NewEngine(mainProvider, registry),
	}
	rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return sideProvider, nil }

	mainDone := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), true, false, []llm.Message{llm.UserText("active main question")}, llm.Request{
			Model: "m", MaxTurns: 5, Tools: []llm.ToolSpec{(&echoTool{}).Spec()},
		})
		mainDone <- err
	}()
	select {
	case <-mainProvider.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("main run did not reach its second provider turn")
	}

	events, err := rt.startSideQuestion(sideQuestionStart{Question: "what did the tool show?"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(sideProvider.Requests) != 1 {
		t.Fatalf("side requests = %d", len(sideProvider.Requests))
	}
	request := sideProvider.Requests[0]
	joined := ""
	hasToolCall, hasToolResult := false, false
	for _, msg := range request.Messages {
		joined += llm.MessageText(msg) + "\n"
		for _, part := range msg.Parts {
			hasToolCall = hasToolCall || part.ToolCall != nil
			hasToolResult = hasToolResult || part.ToolResult != nil
		}
	}
	if !strings.Contains(joined, "active main question") || !hasToolCall || !hasToolResult {
		t.Fatalf("side request missed live completed tool boundary: %#v", request.Messages)
	}

	close(mainProvider.release)
	if err := <-mainDone; err != nil {
		t.Fatalf("main run failed: %v", err)
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

func TestServeSideQuestionGetDoesNotHydrateFreshRuntime(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := &session.Session{ID: "persisted-get", Provider: "mock", ProviderKey: "mock", Model: "m", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(ctx, meta); err != nil {
		t.Fatal(err)
	}
	created := 0
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		created++
		return &serveRuntime{}, nil
	})
	defer manager.Close()
	s := &serveServer{sessionMgr: manager, store: store}

	rr := httptest.NewRecorder()
	s.handleSideQuestion(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/persisted-get/side-question", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status/body = %d %q", rr.Code, rr.Body.String())
	}
	if created != 0 {
		t.Fatalf("GET created %d runtimes, want none", created)
	}
	if _, ok := manager.Get("persisted-get"); ok {
		t.Fatal("GET installed a runtime for an unused side question")
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

func TestServeSideQuestionColdHydrationUsesActiveCompactionBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta := &session.Session{ID: "compacted-side", Provider: "mock", ProviderKey: "mock", Model: "m", CreatedAt: time.Now(), UpdatedAt: time.Now(), CompactionSeq: -1}
	if err := store.Create(ctx, meta); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []llm.Message{llm.UserText("obsolete fact"), llm.AssistantText("obsolete answer")} {
		if err := store.AddMessage(ctx, meta.ID, session.NewMessage(meta.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CompactMessages(ctx, meta.ID, []session.Message{
		*session.NewMessage(meta.ID, llm.UserText("active summary"), -1),
		*session.NewMessage(meta.ID, llm.AssistantText("summary ack"), -1),
	}); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("answer")
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{providerKey: "mock", defaultModel: "m", store: store}
		rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
		return rt, nil
	})
	defer manager.Close()
	s := &serveServer{sessionMgr: manager, store: store}
	body := bytes.NewBufferString(`{"question":"what is active?"}`)
	rr := httptest.NewRecorder()
	s.handleSideQuestion(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/compacted-side/side-question", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST status/body = %d %q", rr.Code, rr.Body.String())
	}
	joined := messageTextForTest(provider.Requests[0].Messages)
	if strings.Contains(joined, "obsolete fact") || !strings.Contains(joined, "active summary") {
		t.Fatalf("side request ignored compaction boundary: %q", joined)
	}
}

func messageTextForTest(messages []llm.Message) string {
	var text strings.Builder
	for _, msg := range messages {
		text.WriteString(llm.MessageText(msg))
		text.WriteByte('\n')
	}
	return text.String()
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

type disconnectSideProvider struct{}

func (disconnectSideProvider) Name() string                   { return "disconnect" }
func (disconnectSideProvider) Credential() string             { return "test" }
func (disconnectSideProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (disconnectSideProvider) Stream(ctx context.Context, _ llm.Request) (llm.Stream, error) {
	return &disconnectSideStream{ctx: ctx}, nil
}

type disconnectSideStream struct {
	ctx  context.Context
	sent bool
}

func (s *disconnectSideStream) Recv() (llm.Event, error) {
	if !s.sent {
		s.sent = true
		return llm.Event{Type: llm.EventTextDelta, Text: "partial"}, nil
	}
	<-s.ctx.Done()
	return llm.Event{}, s.ctx.Err()
}
func (*disconnectSideStream) Close() error { return nil }

type failingSideResponseWriter struct{ header http.Header }

func (w *failingSideResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*failingSideResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}
func (*failingSideResponseWriter) WriteHeader(int) {}
func (*failingSideResponseWriter) Flush()          {}

func TestServeSideQuestionDisconnectCancelsProvider(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 4, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{providerKey: "disconnect", defaultModel: "m"}
		rt.configureSideQuestionContext()
		rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return disconnectSideProvider{}, nil }
		return rt, nil
	})
	defer manager.Close()
	rt, err := manager.GetOrCreate(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"question":"question"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/sessions/main/side-question", body)
	writer := &failingSideResponseWriter{}
	(&serveServer{sessionMgr: manager}).handleSideQuestion(writer, request)

	deadline := time.Now().Add(time.Second)
	for rt.hasActiveSideQuestion() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rt.hasActiveSideQuestion() {
		t.Fatal("side provider remained active after SSE disconnect")
	}
	rt.sideQuestion.mu.Lock()
	done := rt.sideQuestion.done
	rt.sideQuestion.mu.Unlock()
	if !waitForSideQuestion(done, time.Second) {
		t.Fatal("side provider did not finish after disconnect cancellation")
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

type burstSideProvider struct {
	deltas int
}

func (p burstSideProvider) Name() string                   { return "burst" }
func (p burstSideProvider) Credential() string             { return "test" }
func (p burstSideProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }
func (p burstSideProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	deltas := p.deltas
	if deltas == 0 {
		deltas = 100
	}
	return &burstSideStream{remaining: deltas}, nil
}

type burstSideStream struct{ remaining int }

func (s *burstSideStream) Recv() (llm.Event, error) {
	if s.remaining == 0 {
		return llm.Event{}, io.EOF
	}
	s.remaining--
	return llm.Event{Type: llm.EventTextDelta, Text: "x"}, nil
}
func (*burstSideStream) Close() error { return nil }

func BenchmarkStartSideQuestionTextDeltas(b *testing.B) {
	const deltas = 10_000
	provider := burstSideProvider{deltas: deltas}
	b.ReportAllocs()
	b.SetBytes(deltas)
	for b.Loop() {
		rt := &serveRuntime{providerKey: "burst", defaultModel: "m"}
		rt.configureSideQuestionContext()
		rt.sideProviderFactory = func(_, _ string) (llm.Provider, error) { return provider, nil }
		events, err := rt.startSideQuestion(sideQuestionStart{Question: "question"})
		if err != nil {
			b.Fatal(err)
		}
		for range events {
		}
	}
}

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
	deadline := time.Now().Add(time.Second)
	for provider.startCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if provider.startCount() != 1 {
		t.Fatal("provider did not start")
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
