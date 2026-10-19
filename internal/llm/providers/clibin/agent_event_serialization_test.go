package clibin

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"strings"
	"testing"
)

// Internal lifecycle input uses a marked synthetic user turn, so fresh and
// resumed CLI serializers both deliver the event instead of the previous prompt.
func TestClaudeBinDeliversMarkedInternalLifecycleTurn(t *testing.T) {
	wake := core.GoalSteeringText(`Trusted internal lifecycle event: [{"agent_id":"c1","status":"completed"}]`)
	history := []core.Message{
		core.SystemText("system"),
		core.UserText("please build the release and tell me when done"),
		core.AssistantText("Spawned a builder; I will report back."),
	}
	all := append(append([]core.Message(nil), history...), wake)

	// Fresh provider (e.g. after a daemon restart): full transcript is rebuilt.
	p := &ClaudeBinProvider{}
	fresh := p.buildStreamJsonInput(all, "")
	t.Logf("fresh stream-json input: %s", fresh)
	if !strings.Contains(fresh, "lifecycle event") {
		t.Errorf("wake event dropped from claude-bin input; previous user prompt re-sent=%v", strings.Contains(fresh, "please build the release"))
	}

	// Resumed CLI session: only the tail after messagesSent is delivered.
	p2 := &ClaudeBinProvider{sessionID: "resumed", messagesSent: 2}
	tail := p2.messagesForClaudeTurn(core.Request{Messages: all})
	resumed := p2.buildStreamJsonInput(tail, "resumed")
	t.Logf("resumed tail roles=%v input=%q", agentEventRoles(tail), resumed)
	if strings.TrimSpace(resumed) == "" {
		t.Error("resumed claude-bin wake input is empty")
	}
}

func agentEventRoles(ms []core.Message) []core.Role {
	out := make([]core.Role, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Role)
	}
	return out
}

func TestMarkedLifecycleTurnSurvivesGrok(t *testing.T) {
	messages := []core.Message{core.UserText("original task"), core.AssistantText("child running"), core.GoalSteeringText("Trusted internal lifecycle event for child-identity")}
	prompt, err := buildGrokACPPrompt(messages)
	if err != nil || !strings.Contains(string(prompt), "child-identity") {
		t.Fatalf("Grok dropped event: %s %v", prompt, err)
	}
}
