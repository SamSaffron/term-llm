package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/glamour"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"
)

func TestWordwrapHyphenLineLen(t *testing.T) {
	// Regression: breakpoint characters (hyphens) were not counted in lineLen,
	// causing cascading short orphan lines. Also verify no line exceeds the limit.
	tests := []struct {
		name  string
		text  string
		limit int
	}{
		{
			"hyphenated_words",
			"aaa bbb ccc ddd eee fff ggg hhh iii jjj kkk lll mmm nnn ooo ppp-qqq rrr sss ttt uuu vvv www xxxx YYYY and more text after.",
			100,
		},
		{
			"multiple_hyphens",
			"anthropic-beta setup-token oauth-2025-04-20 some more words to fill the line up to the limit here.",
			80,
		},
		{
			"small_limit",
			"aaa-bbb cccc dddd eeee ffff gggg hhhh.",
			20,
		},
		{
			"no_hyphens_control",
			"aaa bbb ccc ddd eee fff ggg hhh iii jjj kkk lll mmm nnn ooo pppXqqq rrr sss ttt uuu vvv www xxxx YYYY and more text after.",
			100,
		},
		{
			"boundary_exact_limit",
			strings.Repeat("a", 10) + "-b",
			10,
		},
		{
			"boundary_limit_minus_one",
			strings.Repeat("a", 9) + "-b",
			10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := wordwrap.String(tc.text, tc.limit)
			lines := strings.Split(result, "\n")
			for i, line := range lines {
				w := xansi.StringWidth(line)
				if w > tc.limit {
					t.Errorf("line %d exceeds limit (width %d, limit %d): %q", i, w, tc.limit, line)
				}
				if i < len(lines)-1 && w > 0 && w < tc.limit/3 {
					t.Errorf("line %d is an orphan (width %d, limit %d): %q", i, w, tc.limit, line)
				}
			}
		})
	}
}

func TestGlamourWrapNoOrphans(t *testing.T) {
	width := 100
	style := GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		markdown string
	}{
		{
			"hyphenated_text",
			"aaa bbb ccc ddd eee fff ggg hhh iii jjj kkk lll mmm nnn ooo ppp-qqq rrr sss ttt uuu vvv www xxxx YYYY and more text after the wrapped word.",
		},
		{
			"inline_code_with_hyphens",
			"The simplest path is to install `anthropic-beta` and then use `setup-token` from `oauth-2025-04-20` to configure your environment properly.",
		},
		{
			"no_hyphens_control",
			"aaa bbb ccc ddd eee fff ggg hhh iii jjj kkk lll mmm nnn ooo ppp qqq rrr sss ttt uuu vvv www xxxx YYYY and more text after the wrapped word.",
		},
		{
			"boundary_hyphen_at_limit",
			strings.Repeat("a", 99) + "-b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := renderer.Render(tc.markdown)
			if err != nil {
				t.Fatal(err)
			}
			rendered = strings.TrimSpace(rendered)
			lines := strings.Split(rendered, "\n")
			for i, line := range lines {
				stripped := StripANSI(line)
				text := strings.TrimRight(stripped, " ")
				tw := xansi.StringWidth(text)
				if tw > width {
					t.Errorf("line %d exceeds width (visible width %d, limit %d): %q", i, tw, width, text)
				}
				if i < len(lines)-1 && tw > 0 && tw < width/3 {
					t.Errorf("line %d is an orphan (visible width %d, limit %d): %q", i, tw, width, text)
				}
			}
		})
	}
}
