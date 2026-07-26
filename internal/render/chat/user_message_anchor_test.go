package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestRenderer_UserMessageAnchorsMatchRenderedBlocks(t *testing.T) {
	renderer := NewRenderer(40, 12)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)
	messages := []session.Message{
		{ID: 1, Role: llm.RoleUser, TextContent: "  first prompt\nwith more detail  ", Parts: []llm.Part{{Type: llm.PartText, Text: "  first prompt\nwith more detail  "}}},
		{ID: 2, Role: llm.RoleAssistant, TextContent: "first answer", Parts: []llm.Part{{Type: llm.PartText, Text: "first answer"}}},
		{ID: 3, Role: llm.RoleUser, TextContent: "second prompt", Parts: []llm.Part{{Type: llm.PartText, Text: "second prompt"}}},
	}

	output := renderer.Render(RenderState{
		Messages: messages,
		Viewport: ViewportState{Height: 12},
		Mode:     RenderModeAltScreen,
		Width:    40,
		Height:   12,
	})
	anchors := renderer.UserMessageAnchorsSnapshot()
	if len(anchors) != 2 {
		t.Fatalf("anchor count = %d, want 2: %#v", len(anchors), anchors)
	}
	if anchors[0].MessageID != 1 || anchors[0].Preview != "first prompt with more detail" {
		t.Fatalf("first anchor = %#v", anchors[0])
	}
	if anchors[1].MessageID != 3 || anchors[1].Preview != "second prompt" {
		t.Fatalf("second anchor = %#v", anchors[1])
	}
	lines := strings.Split(output, "\n")
	for _, anchor := range anchors {
		if anchor.StartLine < 0 || anchor.InteractiveEndLine <= anchor.StartLine || anchor.EndLine <= anchor.InteractiveEndLine || anchor.EndLine > len(lines) {
			t.Fatalf("invalid anchor bounds %#v for %d lines", anchor, len(lines))
		}
		if !strings.Contains(ui.StripANSI(lines[anchor.StartLine]), "❯") {
			t.Fatalf("anchor start %d is not a user prompt: %q", anchor.StartLine, lines[anchor.StartLine])
		}
	}
}

func TestRenderer_UserMessageAnchorsSkipInternalCompactionSummary(t *testing.T) {
	renderer := NewRenderer(40, 12)
	messages := []session.Message{
		{ID: 1, Role: llm.RoleUser, TextContent: "[Context Compaction]\nhidden summary"},
		{ID: 2, Role: llm.RoleUser, TextContent: "visible prompt", Parts: []llm.Part{{Type: llm.PartText, Text: "visible prompt"}}},
	}
	_ = renderer.Render(RenderState{Messages: messages, Mode: RenderModeAltScreen, Width: 40, Height: 12})

	anchors := renderer.UserMessageAnchorsSnapshot()
	if len(anchors) != 1 || anchors[0].MessageID != 2 {
		t.Fatalf("anchors = %#v, want only visible prompt", anchors)
	}
}
