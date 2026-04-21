package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type serveRuntimeTestStream struct {
	events []llm.Event
	index  int
}

func (s *serveRuntimeTestStream) Recv() (llm.Event, error) {
	if s.index >= len(s.events) {
		return llm.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *serveRuntimeTestStream) Close() error {
	return nil
}

type serveRuntimeErrorAfterEventsStream struct {
	events []llm.Event
	index  int
	err    error
}

func (s *serveRuntimeErrorAfterEventsStream) Recv() (llm.Event, error) {
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, nil
	}
	return llm.Event{}, s.err
}

func (s *serveRuntimeErrorAfterEventsStream) Close() error {
	return nil
}

type serveRuntimeTestProvider struct {
	calls int
}

type serveRuntimeToolCallThenErrorProvider struct {
	err error
}

func (p *serveRuntimeToolCallThenErrorProvider) Name() string {
	return "serve-runtime-toolcall-error"
}

func (p *serveRuntimeToolCallThenErrorProvider) Credential() string {
	return "test"
}

func (p *serveRuntimeToolCallThenErrorProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *serveRuntimeToolCallThenErrorProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return &serveRuntimeErrorAfterEventsStream{
		events: []llm.Event{{
			Type: llm.EventToolCall,
			Tool: &llm.ToolCall{ID: "call-err", Name: "serve_runtime_test_tool", Arguments: json.RawMessage(`{"value":1}`)},
		}},
		err: p.err,
	}, nil
}

func (p *serveRuntimeTestProvider) Name() string {
	return "serve-runtime-test"
}

func (p *serveRuntimeTestProvider) Credential() string {
	return "test"
}

func (p *serveRuntimeTestProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *serveRuntimeTestProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	var events []llm.Event
	switch p.calls {
	case 0:
		events = []llm.Event{
			{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "call-1", Name: "serve_runtime_test_tool", Arguments: json.RawMessage(`{}`)}},
			{Type: llm.EventDone},
		}
	case 1:
		events = []llm.Event{
			{Type: llm.EventTextDelta, Text: "done"},
			{Type: llm.EventDone},
		}
	default:
		return nil, errors.New("unexpected provider call")
	}
	p.calls++
	return &serveRuntimeTestStream{events: events}, nil
}

type serveRuntimeTestTool struct{}

func (t *serveRuntimeTestTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "serve_runtime_test_tool", Description: "test tool", Schema: map[string]interface{}{}}
}

func (t *serveRuntimeTestTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("tool ok"), nil
}

func (t *serveRuntimeTestTool) Preview(args json.RawMessage) string {
	return ""
}

type serveRuntimeTestStore struct {
	mu                sync.Mutex
	sessions          map[string]*session.Session
	messages          map[string][]session.Message
	current           string
	replaceCalls      int
	replaceFailures   map[int]error
	addMessageCalls   int
	updateStatusCalls int
}

var _ session.Store = (*serveRuntimeTestStore)(nil)

func newServeRuntimeTestStore() *serveRuntimeTestStore {
	return &serveRuntimeTestStore{
		sessions:        make(map[string]*session.Session),
		messages:        make(map[string][]session.Message),
		replaceFailures: make(map[int]error),
	}
}

func (s *serveRuntimeTestStore) Create(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[sess.ID]; exists {
		return errors.New("session exists")
	}
	copySess := *sess
	s.sessions[sess.ID] = &copySess
	return nil
}

func (s *serveRuntimeTestStore) Get(ctx context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	copySess := *sess
	return &copySess, nil
}

func (s *serveRuntimeTestStore) GetByNumber(ctx context.Context, number int64) (*session.Session, error) {
	return nil, nil
}

func (s *serveRuntimeTestStore) GetByPrefix(ctx context.Context, prefix string) (*session.Session, error) {
	return nil, nil
}

func (s *serveRuntimeTestStore) Update(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copySess := *sess
	s.sessions[sess.ID] = &copySess
	return nil
}

func (s *serveRuntimeTestStore) MarkTitleSkipped(ctx context.Context, id string, t time.Time) error {
	return nil
}

func (s *serveRuntimeTestStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	delete(s.messages, id)
	if s.current == id {
		s.current = ""
	}
	return nil
}

func (s *serveRuntimeTestStore) List(ctx context.Context, opts session.ListOptions) ([]session.SessionSummary, error) {
	return nil, nil
}

func (s *serveRuntimeTestStore) Search(ctx context.Context, query string, limit int) ([]session.SearchResult, error) {
	return nil, nil
}

func (s *serveRuntimeTestStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copyMsg := *msg
	if copyMsg.Sequence < 0 {
		copyMsg.Sequence = len(s.messages[sessionID])
	}
	s.messages[sessionID] = append(s.messages[sessionID], copyMsg)
	s.addMessageCalls++
	return nil
}

func (s *serveRuntimeTestStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messages[sessionID]
	out := make([]session.Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (s *serveRuntimeTestStore) GetMessagesFrom(ctx context.Context, sessionID string, fromSeq int) ([]session.Message, error) {
	msgs, err := s.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	if fromSeq <= 0 || fromSeq >= len(msgs) {
		return msgs, nil
	}
	out := make([]session.Message, len(msgs[fromSeq:]))
	copy(out, msgs[fromSeq:])
	return out, nil
}

func (s *serveRuntimeTestStore) ReplaceMessages(ctx context.Context, sessionID string, messages []session.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaceCalls++
	if err := s.replaceFailures[s.replaceCalls]; err != nil {
		return err
	}
	out := make([]session.Message, len(messages))
	for i, msg := range messages {
		copyMsg := msg
		if copyMsg.Sequence < 0 {
			copyMsg.Sequence = i
		}
		out[i] = copyMsg
	}
	s.messages[sessionID] = out
	return nil
}

func (s *serveRuntimeTestStore) CompactMessages(ctx context.Context, sessionID string, messages []session.Message) error {
	return nil
}

func (s *serveRuntimeTestStore) UpdateMetrics(ctx context.Context, id string, llmTurns, toolCalls, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int) error {
	return nil
}

func (s *serveRuntimeTestStore) UpdateContextEstimate(ctx context.Context, id string, lastTotalTokens, lastMessageCount int) error {
	return nil
}

func (s *serveRuntimeTestStore) UpdateStatus(ctx context.Context, id string, status session.SessionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.Status = status
	}
	s.updateStatusCalls++
	return nil
}

func (s *serveRuntimeTestStore) IncrementUserTurns(ctx context.Context, id string) error {
	return nil
}

func (s *serveRuntimeTestStore) SetCurrent(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = sessionID
	return nil
}

func (s *serveRuntimeTestStore) GetCurrent(ctx context.Context) (*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == "" {
		return nil, nil
	}
	sess, ok := s.sessions[s.current]
	if !ok {
		return nil, nil
	}
	copySess := *sess
	return &copySess, nil
}

func (s *serveRuntimeTestStore) ClearCurrent(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = ""
	return nil
}

func (s *serveRuntimeTestStore) SavePushSubscription(ctx context.Context, sub *session.PushSubscription) error {
	return nil
}

func (s *serveRuntimeTestStore) DeletePushSubscription(ctx context.Context, endpoint string) error {
	return nil
}

func (s *serveRuntimeTestStore) ListPushSubscriptions(ctx context.Context) ([]session.PushSubscription, error) {
	return nil, nil
}

func (s *serveRuntimeTestStore) Close() error {
	return nil
}

func serveRuntimeTextMessage(role llm.Role, text string) llm.Message {
	return llm.Message{
		Role: role,
		Parts: []llm.Part{{
			Type: llm.PartText,
			Text: text,
		}},
	}
}

type serveRuntimeBlockingStream struct {
	ctx context.Context
}

func (s *serveRuntimeBlockingStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	return llm.Event{}, s.ctx.Err()
}

func (s *serveRuntimeBlockingStream) Close() error {
	return nil
}

type serveRuntimeBlockingProvider struct {
	startOnce     sync.Once
	streamStarted chan struct{}
}

func (p *serveRuntimeBlockingProvider) Name() string {
	return "serve-runtime-blocking"
}

func (p *serveRuntimeBlockingProvider) Credential() string {
	return "test"
}

func (p *serveRuntimeBlockingProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}

func (p *serveRuntimeBlockingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.startOnce.Do(func() {
		close(p.streamStarted)
	})
	return &serveRuntimeBlockingStream{ctx: ctx}, nil
}

type serveRuntimeErrorProvider struct {
	err error
}

func (p *serveRuntimeErrorProvider) Name() string {
	return "serve-runtime-error"
}

func (p *serveRuntimeErrorProvider) Credential() string {
	return "test"
}

func (p *serveRuntimeErrorProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{}
}

func (p *serveRuntimeErrorProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return nil, p.err
}

func TestServeRuntimeCloseCancelsActiveRun(t *testing.T) {
	provider := &serveRuntimeBlockingProvider{streamStarted: make(chan struct{})}
	engine := llm.NewEngine(provider, llm.NewToolRegistry())
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		defaultModel: "test-model",
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), false, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "hello")}, llm.Request{
			SessionID: "sess-close",
		})
		runErrCh <- err
	}()

	select {
	case <-provider.streamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start streaming")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		rt.interruptMu.Lock()
		active := rt.activeInterrupt
		rt.interruptMu.Unlock()
		if active != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not publish interrupt state")
		}
		time.Sleep(10 * time.Millisecond)
	}

	closeDone := make(chan struct{})
	go func() {
		rt.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after canceling active run")
	}

	select {
	case err := <-runErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not exit after Close")
	}
}

func TestServeRuntimeRetriesInitialSnapshotBeforeAppending(t *testing.T) {
	store := newServeRuntimeTestStore()
	store.replaceFailures[1] = errors.New("initial snapshot failed")
	store.replaceFailures[3] = errors.New("final snapshot failed")

	provider := &serveRuntimeTestProvider{}
	tool := &serveRuntimeTestTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		store:        store,
		defaultModel: "test-model",
		history: []llm.Message{
			serveRuntimeTextMessage(llm.RoleUser, "previous user"),
			serveRuntimeTextMessage(llm.RoleAssistant, "previous assistant"),
		},
	}

	result, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "current user")}, llm.Request{
		SessionID:   "sess-1",
		Tools:       []llm.ToolSpec{tool.Spec()},
		ToolChoice:  llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		MaxTurns:    4,
		Search:      false,
		Debug:       false,
		DebugRaw:    false,
		Temperature: 0,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := result.Text.String(); got != "done" {
		t.Fatalf("result text = %q, want %q", got, "done")
	}

	if store.replaceCalls != 3 {
		t.Fatalf("ReplaceMessages call count = %d, want 3", store.replaceCalls)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-1", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("stored message count = %d, want 6", len(msgs))
	}

	if msgs[0].Role != llm.RoleUser || msgs[0].TextContent != "previous user" {
		t.Fatalf("message[0] = %+v, want previous user message", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].TextContent != "previous assistant" {
		t.Fatalf("message[1] = %+v, want previous assistant message", msgs[1])
	}
	if msgs[2].Role != llm.RoleUser || msgs[2].TextContent != "current user" {
		t.Fatalf("message[2] = %+v, want current user message", msgs[2])
	}
	if msgs[3].Role != llm.RoleAssistant || len(msgs[3].Parts) != 1 || msgs[3].Parts[0].Type != llm.PartToolCall {
		t.Fatalf("message[3] = %+v, want assistant tool call message", msgs[3])
	}
	if msgs[4].Role != llm.RoleTool || len(msgs[4].Parts) != 1 || msgs[4].Parts[0].Type != llm.PartToolResult {
		t.Fatalf("message[4] = %+v, want tool result message", msgs[4])
	}
	if msgs[5].Role != llm.RoleAssistant || msgs[5].TextContent != "done" {
		t.Fatalf("message[5] = %+v, want final assistant message", msgs[5])
	}
}

func TestServeRuntimeReplaceHistoryClearsPersistedMessagesBeforeEarlyFailure(t *testing.T) {
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-replace", Status: session.StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.ReplaceMessages(context.Background(), "sess-replace", []session.Message{
		*session.NewMessage("sess-replace", serveRuntimeTextMessage(llm.RoleUser, "stale user"), -1),
		*session.NewMessage("sess-replace", serveRuntimeTextMessage(llm.RoleAssistant, "stale assistant"), -1),
	}); err != nil {
		t.Fatalf("ReplaceMessages() error = %v", err)
	}

	providerErr := errors.New("provider startup failed")
	provider := &serveRuntimeErrorProvider{err: providerErr}
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		store:        store,
		defaultModel: "test-model",
	}

	_, err := rt.Run(context.Background(), true, true, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "fresh user")}, llm.Request{
		SessionID: "sess-replace",
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Run() error = %v, want %v", err, providerErr)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-replace", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("stored message count = %d, want 0", len(msgs))
	}
	if got := rt.history; len(got) != 0 {
		t.Fatalf("runtime history length = %d, want 0", len(got))
	}
}

func TestServeRuntimeSnapshotsStreamedToolCallWithoutDuplicatingAssistant(t *testing.T) {
	store := newServeRuntimeTestStore()
	provider := &serveRuntimeTestProvider{}
	tool := &serveRuntimeTestTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		store:        store,
		defaultModel: "test-model",
	}

	result, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "current user")}, llm.Request{
		SessionID:   "sess-streaming-toolcall",
		Tools:       []llm.ToolSpec{tool.Spec()},
		ToolChoice:  llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		MaxTurns:    4,
		Search:      false,
		Debug:       false,
		DebugRaw:    false,
		Temperature: 0,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := result.Text.String(); got != "done" {
		t.Fatalf("result text = %q, want %q", got, "done")
	}

	msgs, err := store.GetMessages(context.Background(), "sess-streaming-toolcall", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("stored message count = %d, want 4", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].TextContent != "current user" {
		t.Fatalf("message[0] = %+v, want current user message", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].Parts) != 1 || msgs[1].Parts[0].Type != llm.PartToolCall {
		t.Fatalf("message[1] = %+v, want assistant tool call message", msgs[1])
	}
	if msgs[2].Role != llm.RoleTool || len(msgs[2].Parts) != 1 || msgs[2].Parts[0].Type != llm.PartToolResult {
		t.Fatalf("message[2] = %+v, want tool result message", msgs[2])
	}
	if msgs[3].Role != llm.RoleAssistant || msgs[3].TextContent != "done" {
		t.Fatalf("message[3] = %+v, want final assistant message", msgs[3])
	}
}

func TestServeRuntimePersistsStreamedToolCallBeforeCallbacks(t *testing.T) {
	store := newServeRuntimeTestStore()
	providerErr := errors.New("provider stream failed after tool call")
	provider := &serveRuntimeToolCallThenErrorProvider{err: providerErr}
	engine := llm.NewEngine(provider, llm.NewToolRegistry())

	rt := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		store:        store,
		defaultModel: "test-model",
	}

	_, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "current user")}, llm.Request{
		SessionID: "sess-toolcall-error",
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Run() error = %v, want %v", err, providerErr)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-toolcall-error", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("stored message count = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].TextContent != "current user" {
		t.Fatalf("message[0] = %+v, want current user message", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || len(msgs[1].Parts) != 1 || msgs[1].Parts[0].Type != llm.PartToolCall {
		t.Fatalf("message[1] = %+v, want assistant tool call message", msgs[1])
	}
	if msgs[1].Parts[0].ToolCall == nil || msgs[1].Parts[0].ToolCall.ID != "call-err" {
		t.Fatalf("message[1] tool call = %+v, want call-err", msgs[1].Parts[0].ToolCall)
	}
	if len(rt.history) != 2 {
		t.Fatalf("runtime history length = %d, want 2", len(rt.history))
	}
}
