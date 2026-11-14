package chat

import (
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/ui"
)

func TestHistoryRendersOrderedReasoningBursts(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 100
	m.reasoningConfig = config.DefaultReasoningConfig()
	m.reasoningConfig.Display = config.ReasoningDisplayExpanded
	m.reasoningConfig.History = config.ReasoningHistoryExpanded
	m.chatRenderer.SetReasoningConfig(m.reasoningConfig)
	display := &core.ReasoningDisplay{Content: "first burst body", Kind: core.ReasoningKindSummary, SummaryTitle: "First burst"}
	m.messages = []session.Message{{Role: core.RoleAssistant, Parts: []core.Part{
		{Type: core.PartText, Text: "Opening answer", ReasoningContent: "aggregate hidden", ReasoningKind: core.ReasoningKindSummary, ReasoningReplayOnly: true},
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "tool", Name: "ordered_tool", Arguments: []byte(`{}`)}},
		{Type: core.PartText, ReasoningDisplay: display, ReasoningContent: "host aggregate hidden", ReasoningKind: core.ReasoningKindSummary, ReasoningReplayOnly: true},
		{Type: core.PartText, Text: "Middle answer"},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "second burst body", Kind: core.ReasoningKindSummary, SummaryTitle: "Second burst"}},
		{Type: core.PartText, Text: "Closing answer"},
	}}}
	got := ui.StripANSI(m.renderHistory())
	last := -1
	for _, want := range []string{"Opening answer", "ordered_tool", "First burst", "first burst body", "Middle answer", "Second burst", "second burst body", "Closing answer"} {
		idx := strings.Index(got, want)
		if idx <= last || strings.Count(got, want) != 1 {
			t.Fatalf("expected ordered unique %q in history:\n%s", want, got)
		}
		last = idx
	}
	if strings.Contains(got, "aggregate") || m.chatRenderer.ReasoningHeaderCount() != 2 {
		t.Fatalf("aggregate displayed or wrong burst count:\n%s", got)
	}
	if m.messages[0].Parts[2].ReasoningDisplay != display || m.messages[0].Parts[0].ReasoningContent != "aggregate hidden" {
		t.Fatal("rendering mutated persisted metadata")
	}
	m.viewAltScreenEnsureHistory()
	before := m.viewCache.historySignature
	display.Content = "updated first burst"
	m.forceHistoryRerender()
	m.viewAltScreenEnsureHistory()
	if m.viewCache.historySignature == before || !strings.Contains(ui.StripANSI(m.viewCache.historyContent), "updated first burst") {
		t.Fatalf("display change failed to invalidate history cache:\n%s", m.viewCache.historyContent)
	}
}

func TestReasoningSegmentSelectsDisplayNotReplay(t *testing.T) {
	part := core.Part{Type: core.PartText, ReasoningContent: "aggregate", ReasoningKind: core.ReasoningKindRaw, ReasoningReplayOnly: true,
		ReasoningDisplay: &core.ReasoningDisplay{Content: "burst", Kind: core.ReasoningKindSummary, SummaryTitle: "Burst title"}}
	got := reasoningSegmentFromPart(part)
	if got.Content != "burst" || got.Kind != string(core.ReasoningKindSummary) || got.Title != "Burst title" {
		t.Fatalf("wrong segment: %#v", got)
	}
	part.ReasoningDisplay = nil
	if got := reasoningSegmentFromPart(part); got.Content != "" {
		t.Fatalf("aggregate replay selected: %#v", got)
	}
}
