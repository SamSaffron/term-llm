package runboundary

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestNilTrackerSnapshotHasNoCompletedTurn(t *testing.T) {
	var tracker *Tracker
	if got := tracker.CompletedSnapshot(); got.TurnIndex != -1 || got.Durable || got.DurableAnchorID != 0 {
		t.Fatalf("nil tracker snapshot = %#v", got)
	}
}

func TestTrackerCompletedAndDurableBoundaries(t *testing.T) {
	base := []llm.Message{llm.SystemText("system"), llm.UserText("question")}
	tracker := New("run-1", base, 10, true)
	tracker.UpdateAssistant("run-1", llm.AssistantText("partial"))
	if got := tracker.CompletedSnapshot(); len(got.Messages) != len(base) || got.DurableAnchorID != 10 || !got.Durable {
		t.Fatalf("partial advanced boundary: %#v", got)
	}
	toolCall := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "read_file"}}}}
	toolResult := llm.ToolResultMessage("call-1", "read_file", "result", nil)
	if !tracker.Commit("run-1", 0, []llm.Message{toolCall, toolResult}) {
		t.Fatal("commit rejected")
	}
	if got := tracker.CompletedSnapshot(); len(got.Messages) != 4 || got.DurableAnchorID != 10 {
		t.Fatalf("completed boundary = %#v", got)
	}
	if !tracker.PublishDurable("run-1", 0, 12) {
		t.Fatal("durable publication rejected")
	}
	tracker.UpdateAssistant("run-1", llm.AssistantText("next partial"))
	got := tracker.CompletedSnapshot()
	if len(got.Messages) != 4 || got.DurableAnchorID != 12 || !got.Durable {
		t.Fatalf("next partial advanced boundary: %#v", got)
	}
}

func TestTrackerFailsClosedAndRejectsStaleUpdates(t *testing.T) {
	tracker := New("old", []llm.Message{llm.UserText("old")}, 1, true)
	tracker.Reset("new", []llm.Message{llm.UserText("new")}, 0, false)
	if tracker.Commit("old", 0, []llm.Message{llm.AssistantText("stale")}) || tracker.PublishDurable("old", 0, 99) {
		t.Fatal("accepted stale run update")
	}
	if got := tracker.CompletedSnapshot(); got.Durable || got.DurableAnchorID != 0 || len(got.Messages) != 1 {
		t.Fatalf("new boundary corrupted: %#v", got)
	}
	if !tracker.Commit("new", 0, []llm.Message{llm.AssistantText("done")}) {
		t.Fatal("new commit rejected")
	}
	if tracker.PublishDurable("new", -1, 2) || tracker.PublishDurable("new", 0, 0) {
		t.Fatal("accepted mismatched or root durable publication")
	}
	if !tracker.PublishDurable("new", 0, 2) || !tracker.InvalidateDurable("new") {
		t.Fatal("publish/invalidate failed")
	}
	if got := tracker.CompletedSnapshot(); got.Durable || got.DurableAnchorID != 0 {
		t.Fatalf("invalidation failed: %#v", got)
	}
}
