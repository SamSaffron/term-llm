package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

func TestStreamCommandHelpStopsOnContextCancel(t *testing.T) {
	provider := &blockingHelpProvider{started: make(chan context.Context, 1)}
	engine := llm.NewEngine(provider, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sent := make(chan tea.Msg, 1)
	streamDone := streamCommandHelp(ctx, engine, buildCommandHelpRequest("ls -la", "bash"), func(msg tea.Msg) {
		select {
		case sent <- msg:
		default:
		}
	})

	var providerCtx context.Context
	select {
	case providerCtx = <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider stream did not start")
	}

	cancel()

	select {
	case <-providerCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("provider context was not cancelled")
	}

	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("help stream goroutine did not exit after cancellation")
	}

	select {
	case msg := <-sent:
		t.Fatalf("unexpected message after cancellation: %T", msg)
	default:
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

type blockingHelpProvider struct {
	started chan context.Context
}

func (p *blockingHelpProvider) Name() string { return "blocking-help" }

func (p *blockingHelpProvider) Credential() string { return "test" }

func (p *blockingHelpProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (p *blockingHelpProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	select {
	case p.started <- ctx:
	default:
	}
	return blockingHelpStream{ctx: ctx}, nil
}

type blockingHelpStream struct {
	ctx context.Context
}

func (s blockingHelpStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	return llm.Event{}, s.ctx.Err()
}

func (s blockingHelpStream) Close() error { return nil }
