package llm

import (
	"strings"
	"testing"
)

func TestPrepareProviderRequestConvertsOnlyTypedAgentMentionContext(t *testing.T) {
	const visible = "ask @agent:codebase"
	const hidden = "<term_llm_agent_mentions>delegate</term_llm_agent_mentions>"
	original := []Message{{Role: RoleUser, Parts: []Part{
		{Type: PartText, Text: visible},
		{Type: PartAgentMention, Text: hidden},
		{Type: PartSkillActivation, SkillActivation: &SkillActivationProvenance{Name: "test"}},
	}}}
	engine := NewEngine(NewMockProvider("mock"), nil)
	prepared := engine.prepareProviderRequest(Request{Messages: original})
	if len(prepared.Messages) != 1 || len(prepared.Messages[0].Parts) != 2 {
		t.Fatalf("prepared messages = %#v", prepared.Messages)
	}
	if prepared.Messages[0].Parts[0].Type != PartText || prepared.Messages[0].Parts[1].Type != PartText {
		t.Fatalf("provider part types = %#v", prepared.Messages[0].Parts)
	}
	if got := MessageText(prepared.Messages[0]); !strings.Contains(got, visible) || !strings.Contains(got, hidden) {
		t.Fatalf("provider text = %q", got)
	}
	if original[0].Parts[1].Type != PartAgentMention || original[0].Parts[2].Type != PartSkillActivation {
		t.Fatalf("provider preparation mutated stored structure: %#v", original)
	}
	if got := MessageText(original[0]); got != visible {
		t.Fatalf("human message text included typed provider context: %q", got)
	}
}
