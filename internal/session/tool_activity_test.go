package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSQLitePersistsToolActivityPart(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "chatgpt", Model: "gpt-test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	activity := &llm.ToolActivity{
		ID:        "ws_1",
		Name:      llm.WebSearchToolName,
		Info:      "(discourse news)",
		Arguments: []byte(`{"query":"discourse news"}`),
		Status:    llm.ToolActivityCompleted,
	}
	if !transcriptRowHasDisplayBody(llm.RoleAssistant, []llm.Part{{Type: llm.PartToolActivity, ToolActivity: activity}}, nil) {
		t.Fatal("activity-only assistant row should be visible in transcript index")
	}
	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartText, Text: "answer"},
		{Type: llm.PartToolActivity, ToolActivity: activity},
	}}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	reloadedActivity := messages[0].Parts[1].ToolActivity
	if messages[0].Parts[1].Type != llm.PartToolActivity || reloadedActivity == nil || reloadedActivity.ID != "ws_1" || reloadedActivity.Info != "(discourse news)" || string(reloadedActivity.Arguments) != `{"query":"discourse news"}` || reloadedActivity.Status != llm.ToolActivityCompleted {
		t.Fatalf("reloaded activity = %#v", messages[0].Parts[1])
	}
}
