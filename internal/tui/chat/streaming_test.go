package chat

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type interjectionTestTool struct{}

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
