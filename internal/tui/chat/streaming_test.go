package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

type interjectionTestTool struct{}

type updateMessageFailStore struct {
	*mockStore
	err error
}

func (s *updateMessageFailStore) UpdateMessage(context.Context, string, *session.Message) error {
	return s.err
}

func (s *updateMessageFailStore) GetMessages(ctx context.Context, sessionID string, limit, offset int) ([]session.Message, error) {
	msgs, err := s.mockStore.GetMessages(ctx, sessionID, limit, offset)
	if err != nil {
		return nil, err
	}
	return append([]session.Message(nil), msgs...), nil
}

func TestStatusLineContextEstimateUsesInProgressStreamingSnapshot(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 120
	m.providerName = "openai"
	m.modelName = "gpt-5"
	m.engine.ConfigureContextManagement(m.provider, m.providerName, m.modelName, false)

	baseMessages := []llm.Message{
		llm.UserText("hello"),
		llm.AssistantText("hi"),
	}
	m.engine.SetContextEstimateBaseline(1000, len(baseMessages))
	m.messages = []session.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}, TextContent: "hello"},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "hi"}}, TextContent: "hi"},
	}

	baseline := m.engine.EstimateTokens(m.buildMessagesForContextEstimate())
	if baseline != 1000 {
		t.Fatalf("baseline estimate = %d, want 1000", baseline)
	}

	largeToolResult := llm.ToolResultMessage("call-1", "read_file", strings.Repeat("tool output ", 1200), nil)
	m.streaming = true
	m.setStreamingContextMessages(append(baseMessages, largeToolResult))

	inProgress := m.engine.EstimateTokens(m.buildMessagesForContextEstimate())
	if inProgress <= baseline {
		t.Fatalf("in-progress estimate = %d, want > baseline %d", inProgress, baseline)
	}

	status := ui.StripANSI(m.renderStatusLine())
	wantUsage := "~" + llm.FormatTokenCount(inProgress) + "/" + llm.FormatTokenCount(m.engine.InputLimit())
	if !strings.Contains(status, wantUsage) {
		t.Fatalf("status line %q does not contain updated usage %q", status, wantUsage)
	}
}

func TestEstimateContextTokensCached_InvalidatesOnStreamingSnapshotChanges(t *testing.T) {
	m := newTestChatModel(false)
	m.engine.SetContextTracking(200_000)
	m.streaming = true
	m.setStreamingContextMessages([]llm.Message{llm.UserText("hello")})

	baseline := m.estimateContextTokensCached()
	if baseline <= 0 {
		t.Fatalf("baseline cached estimate = %d, want > 0", baseline)
	}
	if !m.contextEstimateCachedValid {
		t.Fatal("expected streaming estimate to populate cache")
	}
	version := m.contextEstimateVersion

	m.updateStreamingContextAssistant(llm.AssistantText(strings.Repeat("expanding snapshot ", 1500)))
	if m.contextEstimateCachedValid {
		t.Fatal("expected streaming snapshot update to invalidate cached estimate")
	}
	if m.contextEstimateVersion <= version {
		t.Fatalf("context estimate version = %d, want > %d after streaming snapshot update", m.contextEstimateVersion, version)
	}

	updated := m.estimateContextTokensCached()
	if updated <= baseline {
		t.Fatalf("updated cached estimate = %d, want > baseline %d", updated, baseline)
	}
	if !m.contextEstimateCachedValid {
		t.Fatal("expected updated streaming estimate to repopulate cache")
	}
}

func TestEstimateContextTokensCached_InvalidatesOnHistoryChanges(t *testing.T) {
	m := newTestChatModel(false)
	m.engine.SetContextTracking(200_000)
	m.messages = []session.Message{{
		Role:        llm.RoleUser,
		TextContent: "hello",
		Parts:       []llm.Part{{Type: llm.PartText, Text: "hello"}},
	}}
	m.invalidateHistoryCache()

	baseline := m.estimateContextTokensCached()
	if baseline <= 0 {
		t.Fatalf("baseline cached estimate = %d, want > 0", baseline)
	}
	if !m.contextEstimateCachedValid {
		t.Fatal("expected idle estimate to populate cache")
	}
	version := m.contextEstimateVersion

	bigger := strings.Repeat("history growth ", 1200)
	m.messages = append(m.messages, session.Message{
		Role:        llm.RoleAssistant,
		TextContent: bigger,
		Parts:       []llm.Part{{Type: llm.PartText, Text: bigger}},
	})
	m.invalidateHistoryCache()
	if m.contextEstimateCachedValid {
		t.Fatal("expected history invalidation to clear cached estimate")
	}
	if m.contextEstimateVersion <= version {
		t.Fatalf("context estimate version = %d, want > %d after history change", m.contextEstimateVersion, version)
	}

	updated := m.estimateContextTokensCached()
	if updated <= baseline {
		t.Fatalf("updated cached estimate = %d, want > baseline %d", updated, baseline)
	}
	if !m.contextEstimateCachedValid {
		t.Fatal("expected updated idle estimate to repopulate cache")
	}
}

func TestStreamingContextCallbacksUpdateEstimateSnapshotWithoutMutatingMessages(t *testing.T) {
	m := newTestChatModel(false)
	m.messages = []session.Message{{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "base"}}, TextContent: "base"}}
	baseMessages := []llm.Message{llm.UserText("base")}
	m.streaming = true
	m.setStreamingContextMessages(baseMessages)

	m.updateStreamingContextAssistant(llm.AssistantText("I'll inspect that."))
	m.updateStreamingContextAssistant(llm.AssistantText("I'll inspect that now."))
	m.appendStreamingContextTurnMessages([]llm.Message{
		llm.AssistantText("I'll inspect that now."),
		llm.ToolResultMessage("call-1", "read_file", "file contents", nil),
	})

	got := m.buildMessagesForContextEstimate()
	if len(got) != 3 {
		t.Fatalf("context estimate message count = %d, want 3", len(got))
	}
	if got[1].Role != llm.RoleAssistant || got[2].Role != llm.RoleTool {
		t.Fatalf("context estimate roles = %v, %v; want assistant, tool", got[1].Role, got[2].Role)
	}
	if len(m.messages) != 1 {
		t.Fatalf("m.messages was mutated; len = %d, want 1", len(m.messages))
	}
}

func TestModelSwapPhaseEventUpdatesStreamingStatus(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"

	updated, _ := m.Update(streamEventMsg{event: ui.PhaseEvent("Switching model: old → new; trying existing context…")})
	got := updated.(*Model)
	if got.phase != "Switching model: old → new; trying existing context…" {
		t.Fatalf("phase = %q, want model-swap progress", got.phase)
	}
	got.width = 120
	status := ui.StripANSI(got.renderStatusLine())
	if !strings.Contains(status, "Switching model") {
		t.Fatalf("rendered streaming status %q does not include model-swap phase", status)
	}
}

func (t *interjectionTestTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "noop_tool",
		Description: "does nothing",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *interjectionTestTool) Execute(_ context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("ok"), nil
}

func (t *interjectionTestTool) Preview(_ json.RawMessage) string { return "" }

// TestInterjectionDuringToolTurnDoesNotDoublePersist verifies that when a user
// interjects mid-turn, the interjection is persisted exactly once. The engine
// fires turnCallback with the interjection AND a separate EventInterjection
// event; the TUI's turn callback must skip RoleUser messages so the
// ui.StreamEventInterjection handler (simulated here) is the sole owner of
// interjection persistence. Covers both sync-tool/MCP and async-tool paths
// since both paths emit interjections via the same two mechanisms.
func TestInterjectionDuringToolTurnDoesNotDoublePersist(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call-1", "noop_tool", map[string]any{}).
		AddTextResponse("done")

	tool := &interjectionTestTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	engine := llm.NewEngine(provider, registry)

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "interject-dedup", CreatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	m := newTestChatModel(false)
	m.engine = engine
	m.store = store
	m.sess = sess

	m.setupStreamPersistenceCallbacks(time.Now())
	t.Cleanup(m.clearStreamCallbacks)

	engine.Interject("reconsider this")

	stream, err := engine.Stream(context.Background(), llm.Request{
		Messages:   []llm.Message{llm.UserText("run tool")},
		Tools:      []llm.ToolSpec{tool.Spec()},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		MaxTurns:   3,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	sawInterjection := false
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == llm.EventInterjection {
			sawInterjection = true
			userMsg := &session.Message{
				SessionID:   sess.ID,
				Role:        llm.RoleUser,
				Parts:       []llm.Part{{Type: llm.PartText, Text: ev.Text}},
				TextContent: ev.Text,
				CreatedAt:   time.Now(),
				Sequence:    -1,
			}
			if err := store.AddMessage(context.Background(), sess.ID, userMsg); err != nil {
				t.Fatalf("UI handler AddMessage: %v", err)
			}
		}
	}
	if !sawInterjection {
		t.Fatal("expected EventInterjection to fire")
	}

	time.Sleep(50 * time.Millisecond) // allow any lingering callback goroutines to settle

	msgs, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	userRows := 0
	var userTexts []string
	for _, msg := range msgs {
		if msg.Role == llm.RoleUser {
			userRows++
			userTexts = append(userTexts, msg.TextContent)
		}
	}
	if userRows != 1 {
		t.Fatalf("user row count = %d, want 1 (interjection must not double-persist); texts: %v", userRows, userTexts)
	}
	if userTexts[0] != "reconsider this" {
		t.Fatalf("persisted user text = %q, want %q", userTexts[0], "reconsider this")
	}
}

func TestListenForStreamEventsSync_CancelledClosedStreamUsesInterruptedCleanup(t *testing.T) {
	store := &mockStore{}
	sess := &session.Session{ID: "stream-cancel-closed", CreatedAt: time.Now()}
	userMsg := session.NewMessage(sess.ID, llm.UserText("hello"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.messages = []session.Message{*userMsg}
	m.streaming = true
	m.setStreamCancelRequested(true)
	m.streamStartTime = time.Now().Add(-2 * time.Second)
	m.pendingAssistantSnapshot = llm.AssistantText("partial answer")
	m.pendingAssistantSnapshotSet = true
	m.width = 80

	closed := make(chan ui.StreamEvent)
	close(closed)
	m.streamCoalescer = &streamEventCoalescer{ch: closed}

	msg := m.listenForStreamEventsSync()
	ev, ok := msg.(streamEventMsg)
	if !ok {
		t.Fatalf("listenForStreamEventsSync returned %T, want streamEventMsg", msg)
	}
	if ev.event.Type != ui.StreamEventError || !errors.Is(ev.event.Err, context.Canceled) {
		t.Fatalf("listenForStreamEventsSync event = %#v, want canceled error", ev.event)
	}

	_, _ = m.Update(msg)

	if len(m.messages) != 2 {
		t.Fatalf("in-memory message count = %d, want 2", len(m.messages))
	}
	if got := m.messages[1].TextContent; got != "partial answer" {
		t.Fatalf("in-memory assistant text = %q, want %q", got, "partial answer")
	}
	if len(store.statusUpdates) == 0 {
		t.Fatal("expected interrupted status update")
	}
	last := store.statusUpdates[len(store.statusUpdates)-1]
	if last.status != session.StatusInterrupted {
		t.Fatalf("final status = %q, want %q", last.status, session.StatusInterrupted)
	}
	if m.isStreamCancelRequested() {
		t.Fatal("expected cancellation flag to clear after interrupted cleanup")
	}
}

func TestUpdate_StreamErrorSalvagesPartialAssistantReply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "stream-error-salvage", CreatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	userMsg := session.NewMessage(sess.ID, llm.UserText("hello"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.messages = []session.Message{*userMsg}
	m.streaming = true
	m.streamStartTime = time.Now().Add(-2 * time.Second)
	m.width = 80
	m.currentResponse.WriteString("partial answer")
	m.tracker.AddTextSegment("partial answer", m.width)

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled)})

	if len(m.messages) != 2 {
		t.Fatalf("in-memory message count = %d, want 2", len(m.messages))
	}
	if got := m.messages[1].TextContent; got != "partial answer" {
		t.Fatalf("in-memory assistant text = %q, want %q", got, "partial answer")
	}

	persisted, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted message count = %d, want 2", len(persisted))
	}
	if persisted[1].Role != llm.RoleAssistant || persisted[1].TextContent != "partial answer" {
		t.Fatalf("persisted assistant = (%s, %q), want (%s, %q)", persisted[1].Role, persisted[1].TextContent, llm.RoleAssistant, "partial answer")
	}
	if m.messages[1].ID != persisted[1].ID {
		t.Fatalf("in-memory assistant ID = %d, want persisted ID %d", m.messages[1].ID, persisted[1].ID)
	}
}

func TestUpdate_StreamErrorUpdatesPendingAssistantFallbackRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "stream-error-pending", CreatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	userMsg := session.NewMessage(sess.ID, llm.UserText("hello"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	pendingMsg := session.NewMessage(sess.ID, llm.AssistantText("draft"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, pendingMsg); err != nil {
		t.Fatalf("AddMessage(pending): %v", err)
	}
	persisted, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages(before): %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("precondition persisted message count = %d, want 2", len(persisted))
	}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.messages = []session.Message{persisted[0]}
	m.pendingAssistantMsgID = persisted[1].ID
	m.streaming = true
	m.streamStartTime = time.Now().Add(-2 * time.Second)
	m.width = 80
	m.currentResponse.WriteString("updated partial")
	m.tracker.AddTextSegment("updated partial", m.width)

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})

	if len(m.messages) != 2 {
		t.Fatalf("in-memory message count = %d, want 2", len(m.messages))
	}
	if got := m.messages[1].TextContent; got != "updated partial" {
		t.Fatalf("in-memory assistant text = %q, want %q", got, "updated partial")
	}

	persisted, err = store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages(after): %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted message count = %d, want 2 (update existing pending row, not duplicate)", len(persisted))
	}
	if persisted[1].TextContent != "updated partial" {
		t.Fatalf("persisted assistant text = %q, want %q", persisted[1].TextContent, "updated partial")
	}
	if m.messages[1].ID != persisted[1].ID {
		t.Fatalf("in-memory assistant ID = %d, want pending row ID %d", m.messages[1].ID, persisted[1].ID)
	}
}

func TestUpdate_StreamErrorUsesCurrentTurnSnapshotAfterToolTurn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "stream-error-current-turn", CreatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	userMsg := session.NewMessage(sess.ID, llm.UserText("hello"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	completedAssistant := session.NewMessage(sess.ID, llm.AssistantText("first turn"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, completedAssistant); err != nil {
		t.Fatalf("AddMessage(completed assistant): %v", err)
	}
	pendingMsg := session.NewMessage(sess.ID, llm.AssistantText("stale second"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, pendingMsg); err != nil {
		t.Fatalf("AddMessage(pending): %v", err)
	}

	persisted, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages(before): %v", err)
	}
	if len(persisted) != 3 {
		t.Fatalf("precondition persisted message count = %d, want 3", len(persisted))
	}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.messages = []session.Message{persisted[0], persisted[1]}
	m.pendingAssistantMsgID = persisted[2].ID
	m.pendingAssistantSnapshot = llm.AssistantText("second turn partial")
	m.pendingAssistantSnapshotSet = true
	m.completedAssistantTurns = 1
	m.streaming = true
	m.streamStartTime = time.Now().Add(-2 * time.Second)
	m.width = 80
	// currentResponse is cumulative across the whole stream. Salvage must not use
	// it to overwrite the per-turn pending row after a completed tool turn.
	m.currentResponse.WriteString("first turnsecond turn partial")
	m.tracker.AddTextSegment("first turnsecond turn partial", m.width)

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})

	persisted, err = store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages(after): %v", err)
	}
	if len(persisted) != 3 {
		t.Fatalf("persisted message count = %d, want 3", len(persisted))
	}
	if got := persisted[1].TextContent; got != "first turn" {
		t.Fatalf("completed assistant text = %q, want %q", got, "first turn")
	}
	if got := persisted[2].TextContent; got != "second turn partial" {
		t.Fatalf("pending assistant text = %q, want current-turn snapshot", got)
	}
	if strings.Contains(persisted[2].TextContent, persisted[1].TextContent) {
		t.Fatalf("pending assistant duplicated completed turn: %q", persisted[2].TextContent)
	}
	if len(m.messages) != 3 {
		t.Fatalf("in-memory message count = %d, want 3", len(m.messages))
	}
	if m.messages[2].ID != persisted[2].ID {
		t.Fatalf("in-memory assistant ID = %d, want pending row ID %d", m.messages[2].ID, persisted[2].ID)
	}
}

func TestUpdate_StreamErrorReloadsPersistedToolTurnBeforeNextPrompt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: "stream-error-reload-tool-turn", CreatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	userMsg := session.NewMessage(sess.ID, llm.UserText("hello"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	completedAssistant := session.NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartText, Text: "first turn before tool"},
		{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)}},
	}}, -1)
	if err := store.AddMessage(context.Background(), sess.ID, completedAssistant); err != nil {
		t.Fatalf("AddMessage(completed assistant): %v", err)
	}
	toolResult := session.NewMessage(sess.ID, llm.ToolResultMessage("call-1", "read_file", "tool output", nil), -1)
	if err := store.AddMessage(context.Background(), sess.ID, toolResult); err != nil {
		t.Fatalf("AddMessage(tool result): %v", err)
	}
	pendingMsg := session.NewMessage(sess.ID, llm.AssistantText("stale second"), -1)
	if err := store.AddMessage(context.Background(), sess.ID, pendingMsg); err != nil {
		t.Fatalf("AddMessage(pending): %v", err)
	}

	persisted, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages(before): %v", err)
	}
	if len(persisted) != 4 {
		t.Fatalf("precondition persisted message count = %d, want 4", len(persisted))
	}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	// During a live stream the callbacks persist turn rows, but m.messages is not
	// normally reloaded until terminal stream handling. Simulate Ctrl-C before Done.
	m.messages = []session.Message{persisted[0]}
	m.pendingAssistantMsgID = persisted[3].ID
	m.pendingAssistantSnapshot = llm.AssistantText("second turn partial")
	m.pendingAssistantSnapshotSet = true
	m.completedAssistantTurns = 1
	m.streaming = true
	m.streamStartTime = time.Now().Add(-2 * time.Second)
	m.width = 80

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled)})

	if len(m.messages) != 4 {
		t.Fatalf("in-memory message count after interrupt = %d, want all 4 persisted rows; messages=%#v", len(m.messages), m.messages)
	}
	if got := m.messages[1].TextContent; got != "first turn before tool" {
		t.Fatalf("message[1] text = %q, want completed assistant", got)
	}
	if m.messages[2].Role != llm.RoleTool {
		t.Fatalf("message[2] role = %s, want tool result", m.messages[2].Role)
	}
	if got := m.messages[3].TextContent; got != "second turn partial" {
		t.Fatalf("message[3] text = %q, want interrupted pending assistant", got)
	}

	llmMessages := m.buildMessagesForStream()
	if len(llmMessages) != 4 {
		t.Fatalf("next stream context message count = %d, want 4", len(llmMessages))
	}
	if llmMessages[2].Role != llm.RoleTool {
		t.Fatalf("next stream context message[2] role = %s, want tool result", llmMessages[2].Role)
	}
}

func TestUpdate_StreamErrorPreservesUnpersistedSalvageWhenStoreUpdateFails(t *testing.T) {
	sess := &session.Session{ID: "stream-error-preserve-unpersisted", CreatedAt: time.Now()}
	userMsg := session.NewMessage(sess.ID, llm.UserText("hello"), 0)
	userMsg.ID = 1
	completedAssistant := session.NewMessage(sess.ID, llm.AssistantText("first turn"), 1)
	completedAssistant.ID = 2
	toolResult := session.NewMessage(sess.ID, llm.ToolResultMessage("call-1", "read_file", "tool output", nil), 2)
	toolResult.ID = 3
	stalePending := session.NewMessage(sess.ID, llm.AssistantText("stale second"), 3)
	stalePending.ID = 4

	baseStore := &mockStore{
		sessions: map[string]*session.Session{sess.ID: sess},
		messages: map[string][]session.Message{
			sess.ID: {*userMsg, *completedAssistant, *toolResult, *stalePending},
		},
	}
	store := &updateMessageFailStore{mockStore: baseStore, err: errors.New("database busy")}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.messages = []session.Message{*userMsg}
	m.pendingAssistantMsgID = stalePending.ID
	m.pendingAssistantSnapshot = llm.AssistantText("second turn partial")
	m.pendingAssistantSnapshotSet = true
	m.completedAssistantTurns = 1
	m.streaming = true
	m.streamStartTime = time.Now().Add(-2 * time.Second)
	m.width = 80

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled)})

	if len(m.messages) != 4 {
		t.Fatalf("in-memory message count after interrupt = %d, want 4; messages=%#v", len(m.messages), m.messages)
	}
	if got := m.messages[3].TextContent; got != "second turn partial" {
		t.Fatalf("interrupted assistant text = %q, want salvaged partial despite store update failure", got)
	}
	if got := m.messages[3].ID; got != stalePending.ID {
		t.Fatalf("interrupted assistant ID = %d, want stale pending row ID %d replaced in memory", got, stalePending.ID)
	}
	if got := store.messages[sess.ID][3].TextContent; got != "stale second" {
		t.Fatalf("store text = %q, want failed update to leave stale persisted row", got)
	}
}

func TestStartStreamRecoversFromStreamClosePanic(t *testing.T) {
	provider := panicCloseProvider{}
	m := newTestChatModel(false)
	m.provider = provider
	m.engine = llm.NewEngine(provider, nil)
	m.providerName = provider.Name()
	m.modelName = "panic-close-model"
	m.sess = &session.Session{ID: "stream-close-panic-test"}

	msg := m.startStream("hello")()
	if ev, ok := msg.(streamEventMsg); !ok || ev.event.Type != ui.StreamEventError {
		t.Fatalf("startStream returned %#v, want error stream event", msg)
	}

	m.WaitStreamDone()
}

type panicCloseProvider struct{}

func (p panicCloseProvider) Name() string { return "panic-close" }

func (p panicCloseProvider) Credential() string { return "test" }

func (p panicCloseProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (p panicCloseProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return panicCloseStream{}, nil
}

type panicCloseStream struct{}

func (s panicCloseStream) Recv() (llm.Event, error) { return llm.Event{}, io.EOF }

func (s panicCloseStream) Close() error { panic("close exploded") }

func TestStreamEventCoalescerMergesTextAndPreservesOrder(t *testing.T) {
	ch := make(chan ui.StreamEvent, 8)
	ch <- ui.TextEvent("a")
	ch <- ui.TextEvent("b")
	ch <- ui.TextEvent("c")
	ch <- ui.ToolStartEvent("id1", "shell", "", nil)
	ch <- ui.TextEvent("d")
	co := &streamEventCoalescer{ch: ch}

	ev, ok := co.next()
	if !ok || ev.Type != ui.StreamEventText || ev.Text != "abc" {
		t.Fatalf("expected merged text event %q, got ok=%v type=%v text=%q", "abc", ok, ev.Type, ev.Text)
	}

	ev, ok = co.next()
	if !ok || ev.Type != ui.StreamEventToolStart || ev.ToolCallID != "id1" {
		t.Fatalf("expected pending tool start event, got ok=%v type=%v", ok, ev.Type)
	}

	ev, ok = co.next()
	if !ok || ev.Type != ui.StreamEventText || ev.Text != "d" {
		t.Fatalf("expected trailing text event %q, got ok=%v type=%v text=%q", "d", ok, ev.Type, ev.Text)
	}

	close(ch)
	if _, ok = co.next(); ok {
		t.Fatal("expected closed channel to report ok=false")
	}
}

func TestStreamEventCoalescerDeliversMergedTextOnClose(t *testing.T) {
	ch := make(chan ui.StreamEvent, 4)
	ch <- ui.TextEvent("x")
	ch <- ui.TextEvent("y")
	close(ch)
	co := &streamEventCoalescer{ch: ch}

	ev, ok := co.next()
	if !ok || ev.Text != "xy" {
		t.Fatalf("expected merged %q before closure, got ok=%v text=%q", "xy", ok, ev.Text)
	}
	if _, ok = co.next(); ok {
		t.Fatal("expected closure on subsequent read")
	}
}
