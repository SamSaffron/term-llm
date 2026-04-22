package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestInfoModel_WindowResize_ReRendersMarkdown(t *testing.T) {
	content := strings.Repeat("word ", 60)
	m := newInfoModel(content, 80, 200)

	beforeLines := m.viewport.TotalLineCount()

	next, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 200})
	resized := next.(infoModel)

	afterLines := resized.viewport.TotalLineCount()
	if afterLines <= beforeLines {
		t.Fatalf("expected more wrapped lines after narrowing width: before=%d after=%d", beforeLines, afterLines)
	}
}

func TestRenderInfoMarkdown_NarrowWidth_DoesNotFallbackToRaw(t *testing.T) {
	input := "**bold**"
	got := renderInfoMarkdown(input, 1)

	if strings.TrimSpace(got) == strings.TrimSpace(input) {
		t.Fatalf("expected narrow-width markdown rendering, got raw fallback: %q", got)
	}
}

func TestRenderInfoMarkdown_ZeroWidth_DoesNotFallbackToRaw(t *testing.T) {
	input := "**bold**"
	got := renderInfoMarkdown(input, 0)

	if strings.TrimSpace(got) == strings.TrimSpace(input) {
		t.Fatalf("expected zero-width markdown rendering fallback clamp, got raw fallback: %q", got)
	}
}

func TestRenderInfoMarkdown_MatchesSharedRendererOptions(t *testing.T) {
	input := "# Heading\n\nBody"
	got := renderInfoMarkdown(input, 80)
	want := RenderMarkdownWithOptions(input, 80, MarkdownRenderOptions{
		WrapOffset:         2,
		NormalizeTabs:      false,
		NormalizeNewlines:  false,
		EnsureTrailingLine: true,
	})

	if got != want {
		t.Fatalf("info markdown render must match shared renderer\nwant:\n%q\n\ngot:\n%q", want, got)
	}
}
