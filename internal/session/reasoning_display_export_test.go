package session

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestExportsSelectOrderedReasoningBursts(t *testing.T) {
	messages := []Message{{Role: core.RoleAssistant, Parts: []core.Part{
		{Type: core.PartText, Text: "Opening answer.", ReasoningContent: "aggregate must stay hidden", ReasoningKind: core.ReasoningKindSummary, ReasoningReplayOnly: true},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "First burst body.", Kind: core.ReasoningKindSummary}, ReasoningContent: "host aggregate hidden", ReasoningKind: core.ReasoningKindSummary, ReasoningReplayOnly: true},
		{Type: core.PartText, Text: "Middle answer."},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{SummaryParts: []string{"Second burst body."}, Kind: core.ReasoningKindSummary}},
		{Type: core.PartText, Text: "Closing answer."},
	}}}
	exporters := map[string]func(ExportOptions) string{
		"markdown": func(opts ExportOptions) string { return ExportToMarkdown(&Session{ID: "ordered"}, messages, opts) },
		"html": func(opts ExportOptions) string {
			got, err := ExportToHTML(&Session{ID: "ordered"}, messages, opts)
			if err != nil {
				t.Fatal(err)
			}
			return got
		},
	}
	for name, export := range exporters {
		t.Run(name, func(t *testing.T) {
			got := export(ExportOptions{IncludeReasoningSummaries: true, IncludeRawReasoning: true})
			last := -1
			for _, want := range []string{"Opening answer.", "First burst body.", "Middle answer.", "Second burst body.", "Closing answer."} {
				idx := strings.Index(got, want)
				if idx <= last || strings.Count(got, want) != 1 {
					t.Fatalf("expected ordered unique %q in export:\n%s", want, got)
				}
				last = idx
			}
			if strings.Contains(got, "aggregate") {
				t.Fatalf("aggregate leaked into export:\n%s", got)
			}
			without := export(ExportOptions{})
			if strings.Contains(without, "burst body") || strings.Contains(without, "aggregate") {
				t.Fatalf("reasoning gates bypassed:\n%s", without)
			}
		})
	}
}

func TestRenderExportReasoningOrderedRawRequiresOptIn(t *testing.T) {
	part := core.Part{Type: core.PartText, ReasoningContent: "aggregate summary", ReasoningKind: core.ReasoningKindSummary,
		ReasoningDisplay: &core.ReasoningDisplay{Content: "raw burst", Kind: core.ReasoningKindRaw}}
	if got := RenderExportReasoning(part, ExportOptions{IncludeReasoningSummaries: true}); got != "" {
		t.Fatalf("raw burst must not use aggregate summary gate: %q", got)
	}
	got := RenderExportReasoning(part, ExportOptions{IncludeRawReasoning: true})
	if !strings.Contains(got, "raw burst") || strings.Contains(got, "aggregate") {
		t.Fatalf("wrong display selection: %q", got)
	}
}
