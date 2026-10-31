package session

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func skillActivationRow(text string, provenance core.SkillActivationProvenance) core.Message {
	return core.Message{Role: core.RoleDeveloper, Parts: []core.Part{
		{Type: core.PartSkillActivation, SkillActivation: &provenance},
		{Type: core.PartText, Text: text},
	}}
}

// A main-context /skill invocation stores its expanded instructions in a
// developer row just before the visible user row. Undo must remove (and redo
// restore) that row with the turn, or the instructions keep steering later
// turns and a re-sent invocation is duplicated.
func TestSQLiteUndoRedoMovesDirectSkillActivationWithItsTurn(t *testing.T) {
	store, _ := newUndoRedoSQLiteStore(t)
	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	base := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	add := func(msg core.Message) {
		base = base.Add(time.Second)
		addUndoRedoMessage(t, store, sess.ID, msg, base)
	}
	add(core.UserText("first"))
	add(core.AssistantText("first answer"))
	// An isolated run's result context precedes the next turn but is not part of it.
	add(skillActivationRow("isolated review result", core.SkillActivationProvenance{Name: "review", Execution: "isolated", RunID: "skill-1"}))
	add(skillActivationRow("explain instructions", core.SkillActivationProvenance{Name: "explain", Origin: "user", Execution: "main"}))
	add(core.UserText("/explain internal/config"))
	add(core.AssistantText("explained"))
	before := transcriptText(t, store, sess.ID)

	result := applyUndoRedo(t, store, sess.ID, false)
	if result.UserText != "/explain internal/config" {
		t.Fatalf("undo composer text = %q", result.UserText)
	}
	want := []string{"first", "first answer", "isolated review result"}
	if got := transcriptText(t, store, sess.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("transcript after undo = %q, want %q", got, want)
	}

	applyUndoRedo(t, store, sess.ID, true)
	if got := transcriptText(t, store, sess.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("transcript after redo = %q, want %q", got, before)
	}

	// Undoing a later ordinary turn leaves the earlier skill turn intact.
	add(core.UserText("thanks"))
	add(core.AssistantText("you're welcome"))
	applyUndoRedo(t, store, sess.ID, false)
	if got := transcriptText(t, store, sess.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("transcript after undoing an ordinary turn = %q, want %q", got, before)
	}
}
