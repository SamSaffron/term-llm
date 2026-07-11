package ui

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSubagentGuardianBeforeToolStartIsBuffered(t *testing.T) {
	tracker := NewToolTracker()
	subagents := NewSubagentTracker()
	tracker.HandleToolStart("spawn", "spawn_agent", "worker", nil)
	event := tools.GuardianEvent{ToolCallID: "child-shell", Message: "guardian: approved early", Outcome: tools.GuardianApproved}
	HandleSubagentProgress(tracker, subagents, "spawn", tools.SubagentEvent{Type: tools.SubagentEventGuardian, ToolCallID: "child-shell", Guardian: &event})
	HandleSubagentProgress(tracker, subagents, "spawn", tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "child-shell", ToolName: "shell", ToolInfo: "echo early"})

	preview := strings.Join(FindSegmentByCallID(tracker, "spawn").SubagentPreview, "\n")
	if !strings.Contains(preview, "echo early\n  Guardian: approved early") {
		t.Fatalf("early nested guardian was lost: %q", preview)
	}
}

func TestSubagentGuardianMatchesRepeatedToolsByCallID(t *testing.T) {
	tracker := NewToolTracker()
	subagents := NewSubagentTracker()
	tracker.HandleToolStart("spawn", "spawn_agent", "worker", nil)
	HandleSubagentProgress(tracker, subagents, "spawn", tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "shell-1", ToolName: "shell", ToolInfo: "echo one"})
	HandleSubagentProgress(tracker, subagents, "spawn", tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "shell-2", ToolName: "shell", ToolInfo: "echo two"})
	second := tools.GuardianEvent{ToolCallID: "shell-2", Message: "guardian: denied: second", Outcome: tools.GuardianDenied}
	first := tools.GuardianEvent{ToolCallID: "shell-1", Message: "guardian: approved first", Outcome: tools.GuardianApproved}
	HandleSubagentProgress(tracker, subagents, "spawn", tools.SubagentEvent{Type: tools.SubagentEventGuardian, ToolCallID: "shell-2", Guardian: &second})
	HandleSubagentProgress(tracker, subagents, "spawn", tools.SubagentEvent{Type: tools.SubagentEventGuardian, ToolCallID: "shell-1", Guardian: &first})

	preview := strings.Join(FindSegmentByCallID(tracker, "spawn").SubagentPreview, "\n")
	if !strings.Contains(preview, "echo one\n  Guardian: approved first") || !strings.Contains(preview, "echo two\n  Guardian: denied: second") {
		t.Fatalf("nested guardian decisions misplaced: %q", preview)
	}
}
