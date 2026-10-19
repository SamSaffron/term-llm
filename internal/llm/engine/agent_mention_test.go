package engine

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"strings"
	"testing"
)

func TestPrepareProviderRequestConvertsOnlyTypedAgentMentionContext(t *testing.T) {
	const visible = "ask @agent:codebase"
	const hidden = "<term_llm_agent_mentions>delegate</term_llm_agent_mentions>"
	original := []core.Message{{Role: core.RoleUser, Parts: []core.Part{
		{Type: core.PartText, Text: visible},
		{Type: core.PartAgentMention, Text: hidden},
		{Type: core.PartSkillActivation, SkillActivation: &core.SkillActivationProvenance{Name: "test"}},
	}}}
	engine := NewEngine(mockprovider.NewMockProvider("mock"), nil)
	prepared := engine.prepareProviderRequest(core.Request{Messages: original})
	if len(prepared.Messages) != 1 || len(prepared.Messages[0].Parts) != 2 {
		t.Fatalf("prepared messages = %#v", prepared.Messages)
	}
	if prepared.Messages[0].Parts[0].Type != core.PartText || prepared.Messages[0].Parts[1].Type != core.PartText {
		t.Fatalf("provider part types = %#v", prepared.Messages[0].Parts)
	}
	if got := core.MessageText(prepared.Messages[0]); !strings.Contains(got, visible) || !strings.Contains(got, hidden) {
		t.Fatalf("provider text = %q", got)
	}
	if original[0].Parts[1].Type != core.PartAgentMention || original[0].Parts[2].Type != core.PartSkillActivation {
		t.Fatalf("provider preparation mutated stored structure: %#v", original)
	}
	if got := core.MessageText(original[0]); got != visible {
		t.Fatalf("human message text included typed provider context: %q", got)
	}
}

func TestPrepareProviderRequestStripsDiffCommentMetadata(t *testing.T) {
	original := []core.Message{{Role: core.RoleUser, Parts: []core.Part{
		{Type: core.PartDiffComment, DiffComment: &core.DiffComment{ID: "comment-1", Path: "main.go", Side: "new", Line: 3, FileChangeSeq: 7, Instruction: "rename this"}},
		{Type: core.PartText, Text: "provider-facing anchored instruction"},
	}}}
	prepared := NewEngine(mockprovider.NewMockProvider("mock"), nil).prepareProviderRequest(core.Request{Messages: original})
	if len(prepared.Messages) != 1 || len(prepared.Messages[0].Parts) != 1 || prepared.Messages[0].Parts[0].Type != core.PartText {
		t.Fatalf("prepared messages = %#v", prepared.Messages)
	}
	if original[0].Parts[0].Type != core.PartDiffComment || original[0].Parts[0].DiffComment == nil {
		t.Fatalf("provider preparation mutated stored metadata: %#v", original)
	}
}
