package session

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestDisplayTextSurvivesPersistenceAndReconciliation(t *testing.T) {
	store, sess := newTranscriptTestStore(t)
	input := llm.UserText("<realtime_delegation>provider-only context</realtime_delegation>")
	input.DisplayText = "list the files"
	stored := NewMessage(sess.ID, input, -1)
	if len(input.Parts) != 1 {
		t.Fatal("NewMessage mutated source parts")
	}
	if err := store.AddMessage(context.Background(), sess.ID, stored); err != nil {
		t.Fatal(err)
	}
	messages, err := store.GetMessages(context.Background(), sess.ID, 0, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("messages=%v, err=%v", messages, err)
	}
	restored := messages[0].ToLLMMessage()
	if restored.DisplayText != input.DisplayText || messages[0].TextContent != input.DisplayText || messages[0].DisplayText() != input.DisplayText {
		t.Fatalf("display lost: %+v / %+v", messages[0], restored)
	}
	if len(restored.Parts) != 1 || llm.MessageText(restored) != llm.MessageText(input) {
		t.Fatalf("provider context changed: %+v", restored)
	}
	reconciled := NewMessage(sess.ID, restored, -1)
	if reconciled.TextContent != input.DisplayText || reconciled.DisplayText() != input.DisplayText {
		t.Fatalf("reconciliation lost display: %+v", reconciled)
	}
	plain := NewMessage(sess.ID, llm.UserText("<realtime_delegation>typed literally</realtime_delegation>"), -1)
	if plain.DisplayText() != "" {
		t.Fatal("display inferred from prompt text")
	}
}
