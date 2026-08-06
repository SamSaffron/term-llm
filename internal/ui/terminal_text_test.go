package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSanitizeTerminalText(t *testing.T) {
	input := "before\x1b[31mred\x1b[0m\x1b[2K\x1b[1Gafter" +
		"\x1b]2;forged title\x07" +
		"\x1b_Ga=d\x1b\\" +
		"\u009b2Jc1" +
		"\rnext\tcolumn\x07"
	got := terminaltext.Sanitize(input)
	want := "beforeredafterc1\nnext\tcolumn"
	if got != want {
		t.Fatalf("terminaltext.Sanitize() = %q, want %q", got, want)
	}
	if got := terminaltext.SanitizeSingleLine("one\rTWO\nthree\tfour"); got != "one TWO three four" {
		t.Fatalf("terminaltext.SanitizeSingleLine() = %q", got)
	}
	if got := terminaltext.EscapeControls("echo\x1b[2K\r\n"); got != `echo\x1b[2K\x0d\x0a` {
		t.Fatalf("terminaltext.EscapeControls() = %q", got)
	}
}

func TestRenderShellToolSegmentSanitizesProviderControls(t *testing.T) {
	seg := &Segment{
		Type:       SegmentTool,
		ToolName:   "shell\x1b]2;tool-title\x07",
		ToolInfo:   " (stderr \x1b[31mRED\x1b[0m \x1b[2K\x1b[1GOVERWRITE\rFAKE ROW)",
		ToolStatus: ToolSuccess,
	}
	rendered := RenderToolSegment(seg, -1, 100, false)
	for _, forbidden := range []string{"\x1b[31m", "\x1b[2K", "\x1b[1G", "tool-title"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rendered tool metadata retained terminal control %q: %q", forbidden, rendered)
		}
	}
	plain := StripANSI(rendered)
	if plain != "● shell  (stderr RED OVERWRITE FAKE ROW)" {
		t.Fatalf("rendered visible text = %q", plain)
	}
}

func TestHistoricalAndExpandedShellDetailsNeutralizeControls(t *testing.T) {
	args, err := json.Marshal(tools.ShellArgs{
		Command:     "printf \x1b[31mred\x1b[0m",
		Description: "describe\rFAKE ROW",
		Env:         tools.EnvMap{"EVIL": "\x1b]2;title\x07"},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := &llm.ToolCall{
		Name:      "shell",
		ToolInfo:  "preview\rFAKE ROW\x1b[31m",
		Arguments: args,
	}

	collapsed := StripANSI(RenderToolCallFromPartWithStatus(call, 120, false, ToolSuccess))
	if strings.Contains(collapsed, "\n") || collapsed != "● shell preview FAKE ROW" {
		t.Fatalf("collapsed historical shell = %q", collapsed)
	}

	expanded := StripANSI(RenderToolCallFromPartWithStatus(call, 120, true, ToolSuccess))
	for _, want := range []string{`describe FAKE ROW`, `printf \x1b[31mred\x1b[0m`, `EVIL=\x1b]2;title\x07`} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded historical shell missing %q: %q", want, expanded)
		}
	}
	if strings.ContainsRune(expanded, '\x1b') {
		t.Fatalf("expanded historical shell retained ESC: %q", expanded)
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
