package chat

import (
	"context"
	"encoding/json"
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
