package ui

import (
	"strings"
	"testing"
)

func TestPostexecRenderMarkdown_NarrowWidth_DoesNotFallbackToRaw(t *testing.T) {
	input := "**bold**"
	got := renderMarkdown(input, 1)

	if strings.TrimSpace(got) == strings.TrimSpace(input) {
		t.Fatalf("expected narrow-width markdown rendering, got raw fallback: %q", got)
	}
}

func TestPostexecRenderMarkdown_TabsMatchSharedRenderer(t *testing.T) {
	input := "```\na\tb\n```"
	got := renderMarkdown(input, 80)
	want := RenderMarkdownWithOptions(input, 80, MarkdownRenderOptions{
		WrapOffset:         1,
		NormalizeTabs:      true,
		NormalizeNewlines:  false,
		EnsureTrailingLine: true,
	})

	if got != want {
		t.Fatalf("postexec markdown render must match shared renderer\nwant:\n%q\n\ngot:\n%q", want, got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("postexec render must preserve trailing newline, got %q", got)
	}
}
