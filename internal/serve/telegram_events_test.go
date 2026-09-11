package serve

import (
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestTelegramEventAccumulatorMatrixAndSnapshotIsolation(t *testing.T) {
	t.Parallel()
	a := newTelegramEventAccumulator(&telegramContinuation{Text: "prior", Images: []string{"old.png"}, Media: []llm.MediaArtifact{{Name: "old"}}})
	events := []llm.Event{
		{Type: llm.EventTextDelta, Text: " text"},
		{Type: llm.EventReasoningDelta, Text: "hidden"},
		{Type: llm.EventToolExecStart, ToolCallID: "1", ToolName: "shell"},
		{Type: llm.EventPhase, Text: "working"},
		{Type: llm.EventRetry, RetryAttempt: 1, RetryMaxAttempts: 3},
		{Type: llm.EventSteering, Text: "new direction"},
		{Type: llm.EventToolCall}, {Type: llm.EventUsage}, {Type: llm.EventDone},
		{Type: llm.EventToolExecEnd, ToolCallID: "1", ToolImages: []string{"new.png"}, ToolMedia: []llm.MediaArtifact{{Name: "new"}}},
		{Type: llm.EventError, Err: errors.New("boom")},
		{Type: llm.EventType("unknown_test")},
	}
	for _, event := range events {
		a.Apply(event)
	}
	snapshot := a.Snapshot()
	if snapshot.Text != "prior text" || !snapshot.ToolsRan || snapshot.ToolDisplay != "" {
		t.Fatalf("unexpected presentation snapshot: %#v", snapshot)
	}
	if snapshot.TextDeltas != 1 || snapshot.ReasoningDeltas != 1 || snapshot.ToolStarts != 1 || snapshot.ToolEnds != 1 || snapshot.ToolCalls != 1 || snapshot.UsageEvents != 1 || snapshot.DoneEvents != 1 || snapshot.RetryEvents != 1 || snapshot.ErrorEvents != 1 || snapshot.OtherEvents != 1 {
		t.Fatalf("unexpected counters: %#v", snapshot)
	}
	snapshot.Images[0] = "mutated"
	snapshot.Media[0].Name = "mutated"
	snapshot.OtherTypes[llm.EventType("unknown_test")] = 99
	again := a.Snapshot()
	if again.Images[0] != "old.png" || again.Media[0].Name != "old" || again.OtherTypes[llm.EventType("unknown_test")] != 1 {
		t.Fatal("snapshot exposed mutable accumulator state")
	}
}
