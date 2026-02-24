package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

// Runtime bundles the state needed to run an LLM session.
type Runtime struct {
	Engine       *llm.Engine
	ProviderName string
	ModelName    string
	ToolMgr      *tools.ToolManager
	MCPManager   *mcp.Manager
	Cleanup      func()
}

// RuntimeFactory creates a new runtime for a chat session.
type RuntimeFactory func(ctx context.Context) (*Runtime, error)

// RemoteSession tracks a remote WebSocket chat session.
type RemoteSession struct {
	ID           string
	Agent        string
	History      []session.Message
	EventBuf     []WireEvent
	NextSeq      int64
	LastActiveAt time.Time
	mu           sync.Mutex
	conn         *websocket.Conn
	cancelStream context.CancelFunc
	runtime      *Runtime

	pendingApprovals map[string]chan tools.ApprovalResult
	pendingAskUser   map[string]chan []tools.AskUserAnswer
}

// SessionManager manages active remote chat sessions.
type SessionManager struct {
	sessions       map[string]*RemoteSession
	mu             sync.RWMutex
	cfg            config.ChatServeConfig
	runtimeFactory RuntimeFactory
	systemPrompt   string
	search         bool
	forceSearch    bool
	maxTurns       int
	agentName      string
}

// NewSessionManager creates a session manager using the supplied configuration.
func NewSessionManager(cfg config.ChatServeConfig) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*RemoteSession),
		cfg:      cfg,
	}
}

// SetRuntimeFactory supplies the runtime factory used for new sessions.
func (m *SessionManager) SetRuntimeFactory(factory RuntimeFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runtimeFactory = factory
}

// SetDefaults configures defaults used when creating new sessions.
func (m *SessionManager) SetDefaults(systemPrompt string, search bool, forceSearch bool, maxTurns int, agentName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.systemPrompt = systemPrompt
	m.search = search
	m.forceSearch = forceSearch
	m.maxTurns = maxTurns
	m.agentName = agentName
}

// HTTPHandler returns an http.Handler for the chat endpoints.
func (m *SessionManager) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/sessions", m.auth(m.handleListSessions))
	mux.HandleFunc("/chat/sessions/new", m.auth(m.handleNewSession))
	mux.HandleFunc("/chat/sessions/", m.auth(m.handleResumeSession))
	return mux
}

// StartGC starts background GC for inactive sessions.
func (m *SessionManager) StartGC(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.gcSessions()
		case <-ctx.Done():
			return
		}
	}
}

func (m *SessionManager) gcSessions() {
	cutoff := time.Now().Add(-30 * time.Minute)
	var stale []*RemoteSession

	m.mu.Lock()
	for id, sess := range m.sessions {
		sess.mu.Lock()
		inactive := sess.LastActiveAt.Before(cutoff)
		streaming := sess.cancelStream != nil
		connected := sess.conn != nil
		sess.mu.Unlock()
		if inactive && !streaming && !connected {
			delete(m.sessions, id)
			stale = append(stale, sess)
		}
	}
	m.mu.Unlock()

	for _, sess := range stale {
		sess.mu.Lock()
		rt := sess.runtime
		sess.runtime = nil
		sess.mu.Unlock()
		if rt != nil && rt.Cleanup != nil {
			rt.Cleanup()
		}
	}
}

func (m *SessionManager) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]map[string]any, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sess.mu.Lock()
		item := map[string]any{
			"id":          sess.ID,
			"agent":       sess.Agent,
			"last_active": sess.LastActiveAt.Format(time.RFC3339Nano),
		}
		sess.mu.Unlock()
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
}

func (m *SessionManager) handleNewSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	conn, err := m.upgrade(w, r)
	if err != nil {
		return
	}

	sess, err := m.newSession(r.Context())
	if err != nil {
		_ = conn.Close()
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	m.attachConn(sess, conn)
	m.sendSessionReady(sess, nil)
	m.runSessionLoop(sess)
}

func (m *SessionManager) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/chat/sessions/")
	id = strings.Trim(id, "/")
	if id == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	m.mu.RLock()
	sess := m.sessions[id]
	m.mu.RUnlock()
	if sess == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	conn, err := m.upgrade(w, r)
	if err != nil {
		return
	}

	m.attachConn(sess, conn)

	since := int64(0)
	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if parsed, err := strconv.ParseInt(sinceStr, 10, 64); err == nil {
			since = parsed
		}
	}

	m.sendSessionReady(sess, func() *WireEvent {
		if since <= 0 {
			return nil
		}
		sess.mu.Lock()
		defer sess.mu.Unlock()
		var events []WireEvent
		for _, evt := range sess.EventBuf {
			if evt.Seq > since {
				events = append(events, evt)
			}
		}
		if len(events) == 0 {
			return nil
		}
		return &WireEvent{Seq: 0, Type: "catchup", Events: events}
	})

	m.runSessionLoop(sess)
}

func (m *SessionManager) runSessionLoop(sess *RemoteSession) {
	readCh := make(chan ClientEvent)
	go func() {
		defer close(readCh)
		for {
			var ev ClientEvent
			if err := sess.conn.ReadJSON(&ev); err != nil {
				return
			}
			readCh <- ev
		}
	}()

	for ev := range readCh {
		sess.mu.Lock()
		sess.LastActiveAt = time.Now()
		sess.mu.Unlock()

		switch ev.Type {
		case "message":
			if strings.TrimSpace(ev.Text) == "" {
				continue
			}
			go m.startStream(sess, ev.Text)
		case "interrupt":
			m.interruptStream(sess)
		case "reset":
			m.resetSession(sess)
		case "approval_response":
			m.handleApprovalResponse(sess, ev.RequestID, ev.Approved)
		case "ask_user_response":
			m.handleAskUserResponse(sess, ev.RequestID, ev.Responses)
		}
	}

	m.detachConn(sess)
}

func (m *SessionManager) interruptStream(sess *RemoteSession) {
	sess.mu.Lock()
	cancel := sess.cancelStream
	sess.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *SessionManager) resetSession(sess *RemoteSession) {
	m.interruptStream(sess)
	sess.mu.Lock()
	sess.History = nil
	sess.EventBuf = nil
	sess.NextSeq = 1
	sess.mu.Unlock()
}

func (m *SessionManager) startStream(sess *RemoteSession, text string) {
	sess.mu.Lock()
	if sess.cancelStream != nil {
		sess.mu.Unlock()
		m.writeError(sess, "stream already in progress")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sess.cancelStream = cancel
	sess.mu.Unlock()

	defer func() {
		sess.mu.Lock()
		sess.cancelStream = nil
		sess.mu.Unlock()
	}()

	runtime := m.ensureRuntime(ctx, sess)
	if runtime == nil || runtime.Engine == nil {
		m.writeError(sess, "failed to initialize runtime")
		return
	}

	userMsg := session.NewMessage(sess.ID, llm.UserText(text), -1)
	sess.mu.Lock()
	sess.History = append(sess.History, *userMsg)
	sess.mu.Unlock()

	runtime.Engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
		sessMsg := session.NewMessage(sess.ID, assistantMsg, -1)
		sess.mu.Lock()
		sess.History = append(sess.History, *sessMsg)
		sess.mu.Unlock()
		return nil
	})

	runtime.Engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
		if len(turnMessages) == 0 {
			return nil
		}
		sess.mu.Lock()
		for _, msg := range turnMessages {
			sess.History = append(sess.History, *session.NewMessage(sess.ID, msg, -1))
		}
		sess.mu.Unlock()
		return nil
	})

	messages := m.buildMessages(sess)
	req := llm.Request{
		SessionID:           sess.ID,
		Messages:            messages,
		Tools:               runtime.Engine.Tools().AllSpecs(),
		Search:              m.search,
		ForceExternalSearch: m.forceSearch,
		ParallelToolCalls:   true,
		MaxTurns:            m.maxTurns,
	}

	if runtime.ToolMgr != nil && runtime.ToolMgr.ApprovalMgr != nil {
		runtime.ToolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
			return m.requestApproval(ctx, sess, path, isWrite, isShell)
		}
	}

	tools.SetAskUserUIFunc(func(questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
		return m.requestAskUser(ctx, sess, questions)
	})
	defer tools.ClearAskUserUIFunc()

	adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
	stream, err := runtime.Engine.Stream(ctx, req)
	if err != nil {
		adapter.EmitErrorAndClose(err)
	} else {
		go func() {
			defer stream.Close()
			adapter.ProcessStream(ctx, stream)
		}()
	}

	doneSent := false
	for ev := range adapter.Events() {
		wire := ToWireEvent(0, ev)
		if wire.Type == "message_done" {
			if doneSent {
				continue
			}
			doneSent = true
		}
		if wire.Type == "" {
			continue
		}
		m.writeStreamEvent(sess, wire)
	}

	if !doneSent {
		m.writeStreamEvent(sess, WireEvent{Type: "message_done"})
	}
}

func (m *SessionManager) buildMessages(sess *RemoteSession) []llm.Message {
	var messages []llm.Message
	if strings.TrimSpace(m.systemPrompt) != "" {
		messages = append(messages, llm.SystemText(m.systemPrompt))
	}

	sess.mu.Lock()
	history := make([]session.Message, len(sess.History))
	copy(history, sess.History)
	sess.mu.Unlock()

	for _, msg := range history {
		messages = append(messages, msg.ToLLMMessage())
	}
	return messages
}

func (m *SessionManager) ensureRuntime(ctx context.Context, sess *RemoteSession) *Runtime {
	sess.mu.Lock()
	if sess.runtime != nil {
		rt := sess.runtime
		sess.mu.Unlock()
		return rt
	}
	factory := m.runtimeFactory
	sess.mu.Unlock()

	if factory == nil {
		return nil
	}

	runtime, err := factory(ctx)
	if err != nil {
		return nil
	}

	sess.mu.Lock()
	sess.runtime = runtime
	sess.mu.Unlock()
	return runtime
}

func (m *SessionManager) newSession(ctx context.Context) (*RemoteSession, error) {
	id := uuid.NewString()
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	sess := &RemoteSession{
		ID:               id,
		Agent:            m.agentName,
		NextSeq:          1,
		LastActiveAt:     time.Now(),
		pendingApprovals: make(map[string]chan tools.ApprovalResult),
		pendingAskUser:   make(map[string]chan []tools.AskUserAnswer),
	}

	runtime := m.ensureRuntime(ctx, sess)
	if runtime == nil {
		return nil, fmt.Errorf("runtime factory unavailable")
	}
	sess.runtime = runtime

	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	return sess, nil
}

func (m *SessionManager) attachConn(sess *RemoteSession, conn *websocket.Conn) {
	sess.mu.Lock()
	sess.conn = conn
	sess.LastActiveAt = time.Now()
	sess.mu.Unlock()
}

func (m *SessionManager) detachConn(sess *RemoteSession) {
	sess.mu.Lock()
	if sess.conn != nil {
		_ = sess.conn.Close()
	}
	sess.conn = nil
	sess.mu.Unlock()
}

func (m *SessionManager) sendSessionReady(sess *RemoteSession, catchup func() *WireEvent) {
	sess.mu.Lock()
	history := make([]HistoryItem, 0, len(sess.History))
	for _, msg := range sess.History {
		history = append(history, HistoryItem{Role: string(msg.Role), Text: msg.TextContent})
	}
	agent := sess.Agent
	sess.mu.Unlock()

	_ = writeEvent(sess.conn, WireEvent{Seq: 0, Type: "session_ready", SessionID: sess.ID, Agent: agent, History: history})
	if catchup != nil {
		if ev := catchup(); ev != nil {
			_ = writeEvent(sess.conn, *ev)
		}
	}
}

func (m *SessionManager) writeStreamEvent(sess *RemoteSession, ev WireEvent) {
	sess.mu.Lock()
	seq := sess.NextSeq
	sess.NextSeq++
	ev.Seq = seq
	sess.EventBuf = append(sess.EventBuf, ev)
	conn := sess.conn
	sess.mu.Unlock()

	if conn != nil {
		_ = writeEvent(conn, ev)
	}
}

func (m *SessionManager) writeError(sess *RemoteSession, message string) {
	m.writeStreamEvent(sess, WireEvent{Type: "error", Message: message})
}

func (m *SessionManager) requestApproval(ctx context.Context, sess *RemoteSession, path string, isWrite bool, isShell bool) (tools.ApprovalResult, error) {
	requestID := uuid.NewString()
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}

	respCh := make(chan tools.ApprovalResult, 1)
	sess.mu.Lock()
	sess.pendingApprovals[requestID] = respCh
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.pendingApprovals, requestID)
		sess.mu.Unlock()
	}()

	m.writeStreamEvent(sess, WireEvent{
		Type:        "approval_request",
		RequestID:   requestID,
		Description: path,
		IsWrite:     isWrite,
		IsShell:     isShell,
	})

	select {
	case result := <-respCh:
		return result, nil
	case <-ctx.Done():
		return tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}, ctx.Err()
	}
}

func (m *SessionManager) requestAskUser(ctx context.Context, sess *RemoteSession, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
	requestID := uuid.NewString()
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}

	respCh := make(chan []tools.AskUserAnswer, 1)
	sess.mu.Lock()
	sess.pendingAskUser[requestID] = respCh
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		delete(sess.pendingAskUser, requestID)
		sess.mu.Unlock()
	}()

	wireQuestions := make([]AskUserQuestion, 0, len(questions))
	for _, q := range questions {
		opts := make([]AskUserOption, 0, len(q.Options))
		for _, opt := range q.Options {
			opts = append(opts, AskUserOption{Label: opt.Label, Description: opt.Description})
		}
		wireQuestions = append(wireQuestions, AskUserQuestion{
			Header:      q.Header,
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		})
	}

	m.writeStreamEvent(sess, WireEvent{
		Type:      "ask_user_request",
		RequestID: requestID,
		Questions: wireQuestions,
	})

	select {
	case result := <-respCh:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *SessionManager) handleApprovalResponse(sess *RemoteSession, requestID string, approved bool) {
	if requestID == "" {
		return
	}

	sess.mu.Lock()
	ch := sess.pendingApprovals[requestID]
	delete(sess.pendingApprovals, requestID)
	sess.mu.Unlock()
	if ch == nil {
		return
	}

	result := tools.ApprovalResult{Choice: tools.ApprovalChoiceDeny}
	if approved {
		result.Choice = tools.ApprovalChoiceOnce
	}
	ch <- result
}

func (m *SessionManager) handleAskUserResponse(sess *RemoteSession, requestID string, responses []AskUserAnswer) {
	if requestID == "" {
		return
	}

	sess.mu.Lock()
	ch := sess.pendingAskUser[requestID]
	delete(sess.pendingAskUser, requestID)
	sess.mu.Unlock()
	if ch == nil {
		return
	}

	answers := make([]tools.AskUserAnswer, 0, len(responses))
	for _, resp := range responses {
		selected := ""
		if len(resp.Selected) > 0 {
			selected = resp.Selected[0]
		}
		answers = append(answers, tools.AskUserAnswer{
			QuestionIndex: resp.QuestionIndex,
			Selected:      selected,
			SelectedList:  resp.Selected,
			IsMultiSelect: len(resp.Selected) > 1,
		})
	}
	ch <- answers
}

func (m *SessionManager) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !m.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (m *SessionManager) authorized(r *http.Request) bool {
	token := strings.TrimSpace(m.cfg.Token)
	if token == "" {
		return true
	}
	value := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(value, prefix)) == token
}

func (m *SessionManager) upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	return upgrader.Upgrade(w, r, nil)
}

func writeEvent(conn *websocket.Conn, e WireEvent) error {
	if conn == nil {
		return nil
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
