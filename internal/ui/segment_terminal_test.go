package ui

import (
	"strings"
	"testing"
)

func TestRenderSegmentsSanitizesUnrenderedStreamingText(t *testing.T) {
	segment := &Segment{Type: SegmentText, Text: "before\x1b[2Jafter\u009b2K\nnext\x07"}
	rendered := RenderSegmentsWithLeading(nil, []*Segment{segment}, 80, 0, nil, false, false)

	if strings.Contains(rendered, "\x1b[2J") || strings.Contains(rendered, "\x1b[2K") {
		t.Fatalf("rendered output retained source terminal controls: %q", rendered)
	}
	if !strings.Contains(rendered, "beforeafter\nnext") {
		t.Fatalf("rendered text = %q", rendered)
	}
}

func TestRenderAskUserResultSanitizesTerminalControls(t *testing.T) {
	rendered := renderAskUserResult("Choice: before\x1b[2Jafter\nspoofed\x07", 80)
	if strings.Contains(rendered, "\x1b[2J") || strings.ContainsRune(rendered, '\x07') {
		t.Fatalf("ask_user result retained terminal controls: %q", rendered)
	}
	if !strings.Contains(rendered, "beforeafter spoofed") {
		t.Fatalf("ask_user result = %q", rendered)
	}
}
