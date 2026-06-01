package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestServeRuntimeSessionNumberConcurrentAccess(t *testing.T) {
	rt := &serveRuntime{
		sessionMeta: &session.Session{ID: "sess", Number: 1},
	}

	const readers = 4
	const iterations = 1000

	start := make(chan struct{})
	var wg sync.WaitGroup

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				_ = rt.SessionNumber()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 1; i <= iterations; i++ {
			rt.mu.Lock()
			rt.sessionMeta = &session.Session{ID: "sess", Number: int64(i)}
			rt.mu.Unlock()
		}
	}()

	close(start)
	wg.Wait()

	if got := rt.SessionNumber(); got != iterations {
		t.Fatalf("SessionNumber() = %d, want %d", got, iterations)
	}
}

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

type serveRuntimeTestProvider struct {
	calls int
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
	switch p.calls % 2 {
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
	mu                 sync.Mutex
	sessions           map[string]*session.Session
	messages           map[string][]session.Message
	current            string
	replaceCalls       int
	replaceFailures    map[int]error
	addMessageCalls    int
	updateMessageCalls int
	updateFailures     map[int]error
	getMessagesCalls   int
	updateStatusCalls  int
	incrementCalls     int
	nextID             int64
}

var _ session.Store = (*serveRuntimeTestStore)(nil)

func newServeRuntimeTestStore() *serveRuntimeTestStore {
	return &serveRuntimeTestStore{
		sessions:        make(map[string]*session.Session),
		messages:        make(map[string][]session.Message),
		replaceFailures: make(map[int]error),
		updateFailures:  make(map[int]error),
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
	s.nextID++
	msg.ID = s.nextID
	copyMsg := *msg
	if copyMsg.Sequence < 0 {
		copyMsg.Sequence = len(s.messages[sessionID])
	}
	s.messages[sessionID] = append(s.messages[sessionID], copyMsg)
	s.addMessageCalls++
	return nil
}

func (s *serveRuntimeTestStore) UpdateMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateMessageCalls++
	if err := s.updateFailures[s.updateMessageCalls]; err != nil {
		return err
	}
	if msg == nil || msg.ID == 0 {
		return session.ErrNotFound
	}
	msgs := s.messages[sessionID]
	for i := range msgs {
		if msgs[i].ID == msg.ID {
			updated := *msg
			updated.Sequence = msgs[i].Sequence
			if updated.CreatedAt.IsZero() {
				updated.CreatedAt = msgs[i].CreatedAt
			}
			msgs[i] = updated
			s.messages[sessionID] = msgs
			return nil
		}
	}
	return session.ErrNotFound
}

func (s *serveRuntimeTestStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getMessagesCalls++
	msgs := s.messages[sessionID]
	out := make([]session.Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (s *serveRuntimeTestStore) GetMessagesFrom(ctx context.Context, sessionID string, fromSeq, limit int) ([]session.Message, error) {
	msgs, err := s.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	var filtered []session.Message
	for _, msg := range msgs {
		if msg.Sequence >= fromSeq {
			filtered = append(filtered, msg)
		}
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	out := make([]session.Message, len(filtered))
	copy(out, filtered)
	return out, nil
}

func (s *serveRuntimeTestStore) GetMessageByID(ctx context.Context, msgID int64) (*session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msgs := range s.messages {
		for i := range msgs {
			if msgs[i].ID == msgID {
				copyMsg := msgs[i]
				return &copyMsg, nil
			}
		}
	}
	return nil, session.ErrNotFound
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
	s.mu.Lock()
	defer s.mu.Unlock()
	start := len(s.messages[sessionID])
	out := append([]session.Message(nil), s.messages[sessionID]...)
	for i, msg := range messages {
		copyMsg := msg
		if copyMsg.ID == 0 {
			s.nextID++
			copyMsg.ID = s.nextID
		}
		copyMsg.SessionID = sessionID
		copyMsg.Sequence = start + i
		out = append(out, copyMsg)
	}
	s.messages[sessionID] = out
	if sess, ok := s.sessions[sessionID]; ok {
		sess.CompactionSeq = start
		sess.CompactionCount++
		sess.LastTotalTokens = 0
		sess.LastMessageCount = 0
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		sess.UserTurns++
	}
	s.incrementCalls++
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

func TestServeRuntimePersistSnapshotUpdatesUserTurns(t *testing.T) {
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-snapshot-turns", Status: session.StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rt := &serveRuntime{store: store, sessionMeta: sess}

	snapshot := []llm.Message{
		serveRuntimeTextMessage(llm.RoleUser, "first user"),
		serveRuntimeTextMessage(llm.RoleAssistant, "first assistant"),
		serveRuntimeTextMessage(llm.RoleUser, "second user"),
	}
	if !rt.persistSnapshot(context.Background(), sess.ID, snapshot) {
		t.Fatal("persistSnapshot returned false")
	}

	stored, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.UserTurns != 2 {
		t.Fatalf("stored UserTurns = %d, want 2", stored.UserTurns)
	}
	if rt.sessionMeta.UserTurns != 2 {
		t.Fatalf("runtime UserTurns = %d, want 2", rt.sessionMeta.UserTurns)
	}
	if stored.Summary != "first user" {
		t.Fatalf("summary = %q, want first user", stored.Summary)
	}
}

func TestServeRuntimeAppendMessagesIncrementsUserTurns(t *testing.T) {
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-append-turns", Status: session.StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rt := &serveRuntime{store: store, sessionMeta: sess}

	written := rt.appendMessages(context.Background(), sess.ID, []llm.Message{
		serveRuntimeTextMessage(llm.RoleUser, "first user"),
		serveRuntimeTextMessage(llm.RoleAssistant, "assistant"),
		serveRuntimeTextMessage(llm.RoleUser, "second user"),
	}, 3)
	if written != 3 {
		t.Fatalf("written = %d, want 3", written)
	}
	if store.incrementCalls != 2 {
		t.Fatalf("IncrementUserTurns calls = %d, want 2", store.incrementCalls)
	}
	stored, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.UserTurns != 2 {
		t.Fatalf("stored UserTurns = %d, want 2", stored.UserTurns)
	}
	if rt.sessionMeta.UserTurns != 2 {
		t.Fatalf("runtime UserTurns = %d, want 2", rt.sessionMeta.UserTurns)
	}
}

type serveRuntimeCompactionProvider struct {
	calls []llm.Request
}

func (p *serveRuntimeCompactionProvider) Name() string       { return "serve-runtime-compact" }
func (p *serveRuntimeCompactionProvider) Credential() string { return "test" }
func (p *serveRuntimeCompactionProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}
func (p *serveRuntimeCompactionProvider) ResetConversation() {}

func (p *serveRuntimeCompactionProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.calls = append(p.calls, req)
	last := ""
	if len(req.Messages) > 0 {
		for _, part := range req.Messages[len(req.Messages)-1].Parts {
			last += part.Text
		}
	}
	if strings.Contains(last, "Create a detailed summary of our conversation") {
		return &serveRuntimeTestStream{events: []llm.Event{{Type: llm.EventTextDelta, Text: "summary after runtime compaction"}}}, nil
	}
	return &serveRuntimeTestStream{events: []llm.Event{{Type: llm.EventTextDelta, Text: "final after compaction"}}}, nil
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

	if store.replaceCalls != 2 {
		t.Fatalf("ReplaceMessages call count = %d, want 2", store.replaceCalls)
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

func TestServeRuntimeSuccessfulRunSkipsFinalSnapshotRewrite(t *testing.T) {
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
		history: []llm.Message{
			serveRuntimeTextMessage(llm.RoleUser, "previous user"),
			serveRuntimeTextMessage(llm.RoleAssistant, "previous assistant"),
		},
	}

	result, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "current user")}, llm.Request{
		SessionID:   "sess-no-rewrite",
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

	if store.replaceCalls != 1 {
		t.Fatalf("ReplaceMessages call count = %d, want 1 initial snapshot only", store.replaceCalls)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-no-rewrite", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("stored message count = %d, want 6", len(msgs))
	}
	if msgs[5].Role != llm.RoleAssistant || msgs[5].TextContent != "done" {
		t.Fatalf("message[5] = %+v, want final assistant message", msgs[5])
	}
}

func TestServeRuntimeLaterTurnsStayAppendOnlyOnceHistoryPersisted(t *testing.T) {
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
		history: []llm.Message{
			serveRuntimeTextMessage(llm.RoleUser, "previous user"),
			serveRuntimeTextMessage(llm.RoleAssistant, "previous assistant"),
		},
	}

	for i, userText := range []string{"current user", "second user"} {
		result, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, userText)}, llm.Request{
			SessionID:   "sess-append-next-turn",
			Tools:       []llm.ToolSpec{tool.Spec()},
			ToolChoice:  llm.ToolChoice{Mode: llm.ToolChoiceAuto},
			MaxTurns:    4,
			Search:      false,
			Debug:       false,
			DebugRaw:    false,
			Temperature: 0,
		})
		if err != nil {
			t.Fatalf("Run(%d) error = %v", i+1, err)
		}
		if got := result.Text.String(); got != "done" {
			t.Fatalf("Run(%d) result text = %q, want %q", i+1, got, "done")
		}
	}

	if store.replaceCalls != 1 {
		t.Fatalf("ReplaceMessages call count = %d, want 1 across both turns", store.replaceCalls)
	}
	if !rt.historyPersisted {
		t.Fatal("runtime history should remain marked as fully persisted after second turn")
	}

	msgs, err := store.GetMessages(context.Background(), "sess-append-next-turn", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 10 {
		t.Fatalf("stored message count = %d, want 10", len(msgs))
	}
	if msgs[6].Role != llm.RoleUser || msgs[6].TextContent != "second user" {
		t.Fatalf("message[6] = %+v, want second user message", msgs[6])
	}
	if msgs[9].Role != llm.RoleAssistant || msgs[9].TextContent != "done" {
		t.Fatalf("message[9] = %+v, want final assistant message", msgs[9])
	}
}

func TestServeRuntimeAppendOnlyTurnFallsBackToSnapshotAfterAssistantUpdateFailure(t *testing.T) {
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-append-reconcile", Status: session.StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	seeded := []llm.Message{
		serveRuntimeTextMessage(llm.RoleUser, "previous user"),
		serveRuntimeTextMessage(llm.RoleAssistant, "previous assistant"),
	}
	seededMessages := make([]session.Message, 0, len(seeded))
	for _, msg := range seeded {
		seededMessages = append(seededMessages, *session.NewMessage(sess.ID, msg, -1))
	}
	if err := store.ReplaceMessages(context.Background(), sess.ID, seededMessages); err != nil {
		t.Fatalf("ReplaceMessages() error = %v", err)
	}
	store.replaceCalls = 0
	store.updateFailures[1] = errors.New("transient update failure")

	provider := &serveRuntimeTestProvider{}
	tool := &serveRuntimeTestTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	engine := llm.NewEngine(provider, registry)

	rt := &serveRuntime{
		provider:         provider,
		providerKey:      provider.Name(),
		engine:           engine,
		store:            store,
		sessionMeta:      sess,
		defaultModel:     "test-model",
		history:          seeded,
		historyPersisted: true,
	}

	result, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "current user")}, llm.Request{
		SessionID:   sess.ID,
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

	if store.replaceCalls != 1 {
		t.Fatalf("ReplaceMessages call count = %d, want 1 reconciliation snapshot only", store.replaceCalls)
	}
	if !rt.historyPersisted {
		t.Fatal("runtime history should be marked fully persisted after reconciliation snapshot")
	}

	msgs, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("stored message count = %d, want 6", len(msgs))
	}
	if msgs[3].Role != llm.RoleAssistant || len(msgs[3].Parts) == 0 || msgs[3].Parts[0].ToolCall == nil {
		t.Fatalf("message[3] = %+v, want assistant tool-call message", msgs[3])
	}
	if msgs[5].Role != llm.RoleAssistant || msgs[5].TextContent != "done" {
		t.Fatalf("message[5] = %+v, want final assistant message", msgs[5])
	}
}

func TestServeRuntimeFinalSnapshotReconcilesEarlierAssistantUpdateFailure(t *testing.T) {
	store := newServeRuntimeTestStore()
	store.updateFailures[1] = errors.New("transient update failure")
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
		SessionID:   "sess-reconcile-update-failure",
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

	if store.replaceCalls != 2 {
		t.Fatalf("ReplaceMessages call count = %d, want 2 including final reconciliation snapshot", store.replaceCalls)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-reconcile-update-failure", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("stored message count = %d, want 6", len(msgs))
	}
	if msgs[3].Role != llm.RoleAssistant || len(msgs[3].Parts) == 0 || msgs[3].Parts[0].ToolCall == nil {
		t.Fatalf("message[3] = %+v, want reconciled assistant tool-call message", msgs[3])
	}
	if msgs[5].Role != llm.RoleAssistant || msgs[5].TextContent != "done" {
		t.Fatalf("message[5] = %+v, want final assistant message", msgs[5])
	}
}

func TestServeRuntimeCompactionCallbackUpdatesActiveContext(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "serve-runtime-compact", Model: "compact-runtime", InputLimit: 100}})
	defer llm.RegisterConfigLimits(nil)

	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-runtime-compact", Status: session.StatusActive, CompactionSeq: -1}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	provider := &serveRuntimeCompactionProvider{}
	engine := llm.NewEngine(provider, nil)
	rt := &serveRuntime{
		provider:     provider,
		providerKey:  provider.Name(),
		engine:       engine,
		store:        store,
		sessionMeta:  sess,
		autoCompact:  true,
		defaultModel: "compact-runtime",
		systemPrompt: "system prompt",
		history: []llm.Message{
			serveRuntimeTextMessage(llm.RoleUser, strings.Repeat("stale pre-compaction history ", 60)),
			serveRuntimeTextMessage(llm.RoleAssistant, "old answer"),
		},
	}

	rt.configureContextManagementForRequest(llm.Request{Model: "compact-runtime"})
	if got := engine.InputLimit(); got != 100 {
		t.Fatalf("configured input limit = %d, want 100", got)
	}

	engine.SetContextEstimateBaseline(91, 4)

	_, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "continue task")}, llm.Request{
		SessionID: "sess-runtime-compact",
		Model:     "compact-runtime",
		Tools:     []llm.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(provider.calls) < 3 {
		t.Fatalf("provider calls = %d, want checkpoint + compaction + continuation", len(provider.calls))
	}
	firstLast := ""
	for _, part := range provider.calls[0].Messages[len(provider.calls[0].Messages)-1].Parts {
		firstLast += part.Text
	}
	if !strings.Contains(firstLast, "Context budget is getting tight") {
		t.Fatalf("first call did not request a soft checkpoint: %q", firstLast)
	}
	compactedHistoryText := ""
	for _, msg := range rt.history {
		for _, part := range msg.Parts {
			compactedHistoryText += part.Text + "\n"
		}
	}
	if !strings.Contains(compactedHistoryText, "Context Compaction") {
		t.Fatalf("runtime history was not replaced with compacted context: %#v", rt.history)
	}

	refreshed, err := store.Get(context.Background(), "sess-runtime-compact")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	active, err := session.LoadActiveMessages(context.Background(), store, refreshed)
	if err != nil {
		t.Fatalf("LoadActiveMessages() error = %v", err)
	}
	if len(active) == 0 {
		t.Fatal("active messages empty after compaction")
	}
	activeText := ""
	for _, msg := range active {
		activeText += msg.TextContent + "\n"
	}
	if !strings.Contains(activeText, "summary after runtime compaction") {
		t.Fatalf("active context missing compacted summary: %q", activeText)
	}
	if strings.Contains(activeText, "stale pre-compaction history") {
		t.Fatalf("active context resurrected stale pre-compaction history: %q", activeText)
	}
}

func TestServeRuntimeReplaceHistoryPreservesPersistedMessagesBeforeEarlyFailure(t *testing.T) {
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-replace", Status: session.StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	staleMessages := []session.Message{
		*session.NewMessage("sess-replace", serveRuntimeTextMessage(llm.RoleUser, "stale user"), -1),
		*session.NewMessage("sess-replace", serveRuntimeTextMessage(llm.RoleAssistant, "stale assistant"), -1),
	}
	if err := store.ReplaceMessages(context.Background(), "sess-replace", staleMessages); err != nil {
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
		history: []llm.Message{
			serveRuntimeTextMessage(llm.RoleUser, "stale user"),
			serveRuntimeTextMessage(llm.RoleAssistant, "stale assistant"),
		},
	}

	_, err := rt.Run(context.Background(), true, true, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "fresh user")}, llm.Request{
		SessionID: "sess-replace",
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Run() error = %v, want %v", err, providerErr)
	}

	if store.replaceCalls != 1 {
		t.Fatalf("ReplaceMessages call count = %d, want 1 seed call only", store.replaceCalls)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-replace", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("stored message count = %d, want 2 preserved messages", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].TextContent != "stale user" {
		t.Fatalf("message[0] = %+v, want preserved stale user", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].TextContent != "stale assistant" {
		t.Fatalf("message[1] = %+v, want preserved stale assistant", msgs[1])
	}
	if got := rt.history; len(got) != 2 {
		t.Fatalf("runtime history length = %d, want 2 restored messages", len(got))
	}
}

type serveRuntimeSnapshotErrProvider struct {
	err error
}

type serveRuntimeDisconnectDuringStreamStream struct {
	index int
}

func (s *serveRuntimeDisconnectDuringStreamStream) Recv() (llm.Event, error) {
	switch s.index {
	case 0:
		s.index++
		return llm.Event{Type: llm.EventTextDelta, Text: "partial text"}, nil
	case 1:
		s.index++
		time.Sleep(50 * time.Millisecond)
		return llm.Event{Type: llm.EventTextDelta, Text: " ignored"}, nil
	case 2:
		s.index++
		time.Sleep(50 * time.Millisecond)
		return llm.Event{}, io.EOF
	default:
		return llm.Event{}, io.EOF
	}
}

func (s *serveRuntimeDisconnectDuringStreamStream) Close() error {
	return nil
}

type serveRuntimeDisconnectDuringStreamProvider struct{}

func (p *serveRuntimeDisconnectDuringStreamProvider) Name() string {
	return "serve-runtime-disconnect-during-stream"
}

func (p *serveRuntimeDisconnectDuringStreamProvider) Credential() string { return "test" }

func (p *serveRuntimeDisconnectDuringStreamProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *serveRuntimeDisconnectDuringStreamProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return &serveRuntimeDisconnectDuringStreamStream{}, nil
}

func (p *serveRuntimeSnapshotErrProvider) Name() string { return "serve-runtime-snap-err" }

func (p *serveRuntimeSnapshotErrProvider) Credential() string { return "test" }

func (p *serveRuntimeSnapshotErrProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (p *serveRuntimeSnapshotErrProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return &serveRuntimeTestStream{events: []llm.Event{
		{Type: llm.EventTextDelta, Text: "partial text"},
		{Type: llm.EventToolCall, Tool: &llm.ToolCall{
			ID:        "call-mid-err",
			Name:      "serve_runtime_test_tool",
			Arguments: json.RawMessage(`{}`),
		}},
		{Type: llm.EventError, Err: p.err},
	}}, nil
}

func TestServeRuntimeSnapshotPersistsAssistantOnMidTurnError(t *testing.T) {
	store := newServeRuntimeTestStore()
	sess := &session.Session{ID: "sess-mid-err", Status: session.StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	providerErr := errors.New("mid-turn stream failure")
	provider := &serveRuntimeSnapshotErrProvider{err: providerErr}
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

	_, err := rt.Run(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "hello")}, llm.Request{
		SessionID:  "sess-mid-err",
		Tools:      []llm.ToolSpec{tool.Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		MaxTurns:   4,
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Run() error = %v, want %v", err, providerErr)
	}

	if store.addMessageCalls < 1 {
		t.Fatalf("addMessageCalls = %d, want >= 1 (AssistantSnapshotCallback must persist before mid-turn error)", store.addMessageCalls)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-mid-err", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	var assistant *session.Message
	for i := range msgs {
		if msgs[i].Role == llm.RoleAssistant {
			assistant = &msgs[i]
			break
		}
	}
	if assistant == nil {
		t.Fatalf("no assistant message found; messages=%+v", msgs)
	}

	var gotText, gotToolCallID string
	for _, p := range assistant.Parts {
		switch p.Type {
		case llm.PartText:
			gotText = p.Text
		case llm.PartToolCall:
			if p.ToolCall != nil {
				gotToolCallID = p.ToolCall.ID
			}
		}
	}
	if gotText != "partial text" {
		t.Fatalf("assistant text = %q, want %q", gotText, "partial text")
	}
	if gotToolCallID != "call-mid-err" {
		t.Fatalf("assistant tool call ID = %q, want %q", gotToolCallID, "call-mid-err")
	}
}

func TestServeRuntimePersistsPartialAssistantTextOnErrorBeforeCallbacks(t *testing.T) {
	store := newServeRuntimeTestStore()
	disconnectErr := errors.New("client disconnected")
	provider := &serveRuntimeDisconnectDuringStreamProvider{}
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

	textDeltas := 0
	_, err := rt.RunWithEvents(context.Background(), true, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "hello")}, llm.Request{
		SessionID:  "sess-partial-err",
		Tools:      []llm.ToolSpec{tool.Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		MaxTurns:   4,
	}, func(ev llm.Event) error {
		if ev.Type != llm.EventTextDelta {
			return nil
		}
		textDeltas++
		if textDeltas == 2 {
			return disconnectErr
		}
		return nil
	})
	if !errors.Is(err, disconnectErr) {
		t.Fatalf("RunWithEvents() error = %v, want %v", err, disconnectErr)
	}
	if store.addMessageCalls != 0 {
		t.Fatalf("addMessageCalls = %d, want 0 when no callbacks fire", store.addMessageCalls)
	}
	if store.updateMessageCalls != 0 {
		t.Fatalf("updateMessageCalls = %d, want 0 when no callbacks fire", store.updateMessageCalls)
	}

	msgs, err := store.GetMessages(context.Background(), "sess-partial-err", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages() error = %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("stored message count = %d, want 2", len(msgs))
	}
	if msgs[0].Role != llm.RoleUser || msgs[0].TextContent != "hello" {
		t.Fatalf("message[0] = %+v, want user hello", msgs[0])
	}
	if msgs[1].Role != llm.RoleAssistant || msgs[1].TextContent != "partial text" {
		t.Fatalf("message[1] = %+v, want partial assistant text", msgs[1])
	}
	if len(rt.history) != 2 {
		t.Fatalf("runtime history length = %d, want 2", len(rt.history))
	}
	if rt.history[1].Role != llm.RoleAssistant || len(rt.history[1].Parts) != 1 || rt.history[1].Parts[0].Type != llm.PartText || rt.history[1].Parts[0].Text != "partial text" {
		t.Fatalf("runtime history[1] = %+v, want partial assistant text", rt.history[1])
	}
}
