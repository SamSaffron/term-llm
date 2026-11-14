package inspector

import (
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/ui"
)

func TestInspectorRendersOrderedReasoningBursts(t *testing.T) {
	r := NewContentRenderer(100, ui.DefaultStyles(), nil, nil, "", "", nil, config.DefaultReasoningConfig())
	msg := session.Message{Role: core.RoleAssistant, Parts: []core.Part{
		{Type: core.PartText, Text: "Opening answer", ReasoningContent: "aggregate hidden", ReasoningKind: core.ReasoningKindSummary, ReasoningReplayOnly: true},
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "tool", Name: "ordered_tool", Arguments: []byte(`{}`)}},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "First burst body", Kind: core.ReasoningKindSummary, SummaryTitle: "First burst"}, ReasoningContent: "host aggregate hidden", ReasoningKind: core.ReasoningKindRaw, ReasoningReplayOnly: true},
		{Type: core.PartText, Text: "Middle answer"},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "Second burst body", Kind: core.ReasoningKindRaw, SummaryTitle: "Second burst"}},
		{Type: core.PartText, Text: "Closing answer"},
	}}
	got := ui.StripANSI(r.renderAssistantContent(msg))
	last := -1
	for _, want := range []string{"Opening answer", "ordered_tool", "Thought: First burst", "First burst body", "Middle answer", "Thought: Second burst", "Second burst body", "Closing answer"} {
		idx := strings.Index(got, want)
		if idx <= last || strings.Count(got, want) != 1 {
			t.Fatalf("expected ordered unique %q in inspector:\n%s", want, got)
		}
		last = idx
	}
	if strings.Contains(got, "aggregate") {
		t.Fatalf("aggregate replay leaked into inspector:\n%s", got)
	}
	if msg.Parts[2].ReasoningContent != "host aggregate hidden" {
		t.Fatal("inspector mutated aggregate replay metadata")
	}
}

func TestCompactionDebugSelectsReasoningDisplay(t *testing.T) {
	part := core.Part{Type: core.PartText, ReasoningContent: "aggregate", ReasoningReplayOnly: true,
		ReasoningDisplay: &core.ReasoningDisplay{SummaryParts: []string{"first burst", "second paragraph"}, Kind: core.ReasoningKindSummary}}
	if got := compactionDebugReasoningText(part); got != "first burst\n\nsecond paragraph" {
		t.Fatalf("wrong debug display: %q", got)
	}
	part.ReasoningDisplay = nil
	if got := compactionDebugReasoningText(part); got != "" {
		t.Fatalf("aggregate replay leaked into compaction debug: %q", got)
	}
}
