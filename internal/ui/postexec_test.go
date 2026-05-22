package ui

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestBuildCommandHelpRequestUsesSystemInstructions(t *testing.T) {
	req := buildCommandHelpRequest("ls -la", "bash")
	if len(req.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("first message role = %q, want system", req.Messages[0].Role)
	}
	if req.Messages[1].Role != llm.RoleUser {
		t.Fatalf("second message role = %q, want user", req.Messages[1].Role)
	}
	system := req.Messages[0].Parts[0].Text
	if !strings.Contains(system, "friendly CLI tutor") || !strings.Contains(system, "bash commands") {
		t.Fatalf("system instructions missing tutor/shell context: %q", system)
	}
	user := req.Messages[1].Parts[0].Text
	if !strings.Contains(user, "ls -la") {
		t.Fatalf("user prompt missing command: %q", user)
	}
}

func TestHelpModelStreamingDoesNotPullScrolledViewportToBottom(t *testing.T) {
	m := newHelpModel(80, 6)
	model, _ := m.Update(contentMsg("```\n" + strings.Repeat("line\n", 40) + "```\n"))
	m = model.(helpModel)
	if !m.viewport.AtBottom() {
		t.Fatal("precondition: expected initial streamed content to follow to bottom")
	}

	m.viewport.ScrollUp(3)
	if m.viewport.AtBottom() {
		t.Fatal("precondition: expected viewport to be scrolled away from bottom")
	}
	yOffset := m.viewport.YOffset()

	model, _ = m.Update(contentMsg("```\n" + strings.Repeat("more\n", 20) + "```\n"))
	m = model.(helpModel)
	if m.viewport.AtBottom() {
		t.Fatal("streaming content pulled scrolled viewport back to bottom")
	}
	if got := m.viewport.YOffset(); got != yOffset {
		t.Fatalf("streaming content changed viewport offset: got %d, want %d", got, yOffset)
	}
}

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
