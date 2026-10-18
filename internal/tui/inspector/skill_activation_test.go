package inspector

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestInspectorRendersSkillActivationProvenance(t *testing.T) {
	renderer := NewContentRenderer(100, ui.DefaultStyles(), nil, nil, "", "", nil, defaultInspectorReasoningConfig())
	message := session.Message{
		Role: core.RoleDeveloper,
		Parts: []core.Part{
			{Type: core.PartSkillActivation, SkillActivation: &core.SkillActivationProvenance{
				Name:           "review",
				Source:         "local",
				SourcePath:     "/repo/.skills/review",
				Origin:         "user",
				Execution:      "isolated",
				RawArguments:   "internal/config",
				Agent:          "reviewer",
				Model:          "fast",
				RunID:          "skill-1",
				ChildSessionID: "child-1",
				Status:         "complete",
			}},
			{Type: core.PartText, Text: "expanded body"},
		},
		TextContent: "expanded body",
	}

	got := renderer.renderTextContent(message)
	for _, want := range []string{"Skill activation: review", "local", "/repo/.skills/review", "isolated", "internal/config", "reviewer", "fast", "skill-1", "child-1", "complete", "expanded body"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderTextContent() missing %q:\n%s", want, got)
		}
	}
}

func TestInspectorRendersSkillRunEventMessages(t *testing.T) {
	renderer := NewContentRenderer(100, ui.DefaultStyles(), nil, nil, "", "", nil, defaultInspectorReasoningConfig())
	message := session.Message{
		Role:        core.RoleEvent,
		Parts:       []core.Part{{Type: core.PartText, Text: "↳ Skill /review · complete"}},
		TextContent: "↳ Skill /review · complete",
	}
	got, _, lines := renderer.renderMessageWithItems(message, "event-1", 0)
	if !strings.Contains(got, "Event") || !strings.Contains(got, "Skill /review") || lines == 0 {
		t.Fatalf("event render = %q lines=%d", got, lines)
	}
}

func defaultInspectorReasoningConfig() (cfg config.ReasoningConfig) {
	return config.DefaultReasoningConfig()
}
