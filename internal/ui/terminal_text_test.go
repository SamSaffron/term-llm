package ui

import (
	"strings"
	"testing"
)

func TestSanitizeTerminalText(t *testing.T) {
	input := "before\x1b[31mred\x1b[0m\x1b[2K\x1b[1Gafter" +
		"\x1b]2;forged title\x07" +
		"\x1b_Ga=d\x1b\\" +
		"\u009b2Jc1" +
		"\rnext\tcolumn\x07"
	got := sanitizeTerminalText(input)
	want := "beforeredafterc1\nnext\tcolumn"
	if got != want {
		t.Fatalf("sanitizeTerminalText() = %q, want %q", got, want)
	}
}

func TestRenderShellToolSegmentSanitizesProviderControls(t *testing.T) {
	seg := &Segment{
		Type:       SegmentTool,
		ToolName:   "shell\x1b]2;tool-title\x07",
		ToolInfo:   " (stderr \x1b[31mRED\x1b[0m \x1b[2K\x1b[1GOVERWRITE)",
		ToolStatus: ToolSuccess,
	}
	rendered := RenderToolSegment(seg, -1, 100, false)
	for _, forbidden := range []string{"\x1b[31m", "\x1b[2K", "\x1b[1G", "tool-title"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rendered tool metadata retained terminal control %q: %q", forbidden, rendered)
		}
	}
	plain := StripANSI(rendered)
	if plain != "● shell  (stderr RED OVERWRITE)" {
		t.Fatalf("rendered visible text = %q", plain)
	}
}

func TestBuildSubagentPreviewSanitizesNestedToolMetadata(t *testing.T) {
	preview := BuildSubagentPreview(&SubagentProgress{
		CompletedTools: []ToolSegment{{
			Name:    "shell\x1b[2J",
			Info:    "stderr \x1b[31mred\x1b[0m",
			Success: true,
		}},
	}, 5)
	if len(preview) != 1 {
		t.Fatalf("preview = %#v", preview)
	}
	for _, forbidden := range []string{"\x1b[2J", "\x1b[31m"} {
		if strings.Contains(preview[0], forbidden) {
			t.Fatalf("nested preview retained terminal control %q: %q", forbidden, preview[0])
		}
	}
	if plain := StripANSI(preview[0]); plain != "● shell stderr red" {
		t.Fatalf("nested preview visible text = %q", plain)
	}
}
