package serve

import (
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestTelegramEventAccumulatorMatrixAndSnapshotIsolation(t *testing.T) {
	t.Parallel()
	a := newTelegramEventAccumulator(&telegramContinuation{Text: "prior", Images: []string{"old.png"}, Media: []core.MediaArtifact{{Name: "old"}}})
	events := []core.Event{
		{Type: core.EventTextDelta, Text: " text"},
		{Type: core.EventReasoningDelta, Text: "hidden"},
		{Type: core.EventToolExecStart, ToolCallID: "1", ToolName: "shell"},
		{Type: core.EventPhase, Text: "working"},
		{Type: core.EventRetry, RetryAttempt: 1, RetryMaxAttempts: 3},
		{Type: core.EventSteering, Text: "new direction"},
		{Type: core.EventToolCall}, {Type: core.EventUsage}, {Type: core.EventDone},
		{Type: core.EventToolExecEnd, ToolCallID: "1", ToolImages: []string{"new.png"}, ToolMedia: []core.MediaArtifact{{Name: "new"}}},
		{Type: core.EventError, Err: errors.New("boom")},
		{Type: core.EventType("unknown_test")},
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
	snapshot.OtherTypes[core.EventType("unknown_test")] = 99
	again := a.Snapshot()
	if again.Images[0] != "old.png" || again.Media[0].Name != "old" || again.OtherTypes[core.EventType("unknown_test")] != 1 {
		t.Fatal("snapshot exposed mutable accumulator state")
	}
}
