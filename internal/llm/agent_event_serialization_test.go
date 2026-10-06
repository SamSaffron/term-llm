package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// Internal lifecycle input uses a marked synthetic user turn, so fresh and
// resumed CLI serializers both deliver the event instead of the previous prompt.
func TestClaudeBinDeliversMarkedInternalLifecycleTurn(t *testing.T) {
	wake := GoalSteeringText(`Trusted internal lifecycle event: [{"agent_id":"c1","status":"completed"}]`)
	history := []Message{
		SystemText("system"),
		UserText("please build the release and tell me when done"),
		AssistantText("Spawned a builder; I will report back."),
	}
	all := append(append([]Message(nil), history...), wake)

	// Fresh provider (e.g. after a daemon restart): full transcript is rebuilt.
	p := &ClaudeBinProvider{}
	fresh := p.buildStreamJsonInput(all, "")
	t.Logf("fresh stream-json input: %s", fresh)
	if !strings.Contains(fresh, "lifecycle event") {
		t.Errorf("wake event dropped from claude-bin input; previous user prompt re-sent=%v", strings.Contains(fresh, "please build the release"))
	}

	// Resumed CLI session: only the tail after messagesSent is delivered.
	p2 := &ClaudeBinProvider{sessionID: "resumed", messagesSent: 2}
	tail := p2.messagesForClaudeTurn(Request{Messages: all})
	resumed := p2.buildStreamJsonInput(tail, "resumed")
	t.Logf("resumed tail roles=%v input=%q", agentEventRoles(tail), resumed)
	if strings.TrimSpace(resumed) == "" {
		t.Error("resumed claude-bin wake input is empty")
	}
}

func agentEventRoles(ms []Message) []Role {
	out := make([]Role, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Role)
	}
	return out
}

func TestMarkedLifecycleTurnSurvivesGeminiAndGrok(t *testing.T) {
	messages := []Message{UserText("original task"), AssistantText("child running"), GoalSteeringText("Trusted internal lifecycle event for child-identity")}
	_, contents := buildGeminiContents(messages)
	encoded, err := json.Marshal(contents)
	if err != nil || !strings.Contains(string(encoded), "child-identity") {
		t.Fatalf("Gemini dropped event: %s %v", encoded, err)
	}
	prompt, err := buildGrokACPPrompt(messages)
	if err != nil || !strings.Contains(string(prompt), "child-identity") {
		t.Fatalf("Grok dropped event: %s %v", prompt, err)
	}
}
