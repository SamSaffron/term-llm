package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSpawnRunSinkRoutesCorrelatedGuardianEvent(t *testing.T) {
	var outerID string
	var got tools.SubagentEvent
	sink := &spawnRunSink{callID: "spawn-call", cb: func(callID string, event tools.SubagentEvent) {
		if event.Type == tools.SubagentEventGuardian {
			outerID, got = callID, event
		}
	}}
	event := tools.GuardianEvent{ToolCallID: "child-shell", Command: "echo child", Message: "guardian: approved", Outcome: tools.GuardianApproved}
	sink.GuardianEvent(event)

	if outerID != "spawn-call" || got.ToolCallID != "child-shell" || got.Guardian == nil || got.Guardian.Command != "echo child" {
		t.Fatalf("guardian event was not correlated: outer=%q event=%#v", outerID, got)
	}
}
