package clibin

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"io"
	"strings"
	"testing"
)

// claudeContinuationHarness drives ClaudeBinProvider.Stream turn by turn while
// recording the argv and stdin handed to the CLI, so resume behaviour can be
// asserted against the exact payload Claude Code would receive.
type claudeContinuationHarness struct {
	provider *ClaudeBinProvider
	args     []string
	stdin    string
}

func newClaudeContinuationHarness(t *testing.T, sessionID string) *claudeContinuationHarness {
	t.Helper()
	h := &claudeContinuationHarness{provider: NewClaudeBinProvider("sonnet", nil)}
	h.provider.commandRunner = func(_ context.Context, args []string, _, prompt, _ string, _ bool, send eventSender, ephemeral, _ bool) error {
		h.args = append([]string(nil), args...)
		h.stdin = prompt
		if !ephemeral {
			h.provider.sessionID = sessionID
		}
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "ok"})
	}
	return h
}

func (h *claudeContinuationHarness) turn(t *testing.T, messages ...core.Message) {
	t.Helper()
	h.args = nil
	h.stdin = ""
	stream, err := h.provider.Stream(context.Background(), core.Request{Messages: messages})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}
}

func (h *claudeContinuationHarness) resumed() bool {
	for _, arg := range h.args {
		if arg == "--resume" {
			return true
		}
	}
	return false
}

func (h *claudeContinuationHarness) inputText(t *testing.T) string {
	t.Helper()
	messages := parseSDKUserMessages(t, h.stdin)
	var parts []string
	for _, message := range messages {
		for _, block := range message.Message.Content {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func assistantToolCall(id, name, args string) core.Message {
	return core.Message{
		Role: core.RoleAssistant,
		Parts: []core.Part{{
			Type:     core.PartToolCall,
			ToolCall: &core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)},
		}},
	}
}

// A resumed Claude Code session already holds every assistant reply it wrote and
// every tool result term-llm's MCP bridge returned to it. Re-serialising either
// into stdin duplicates the conversation: the previous assistant turn came back
// as a <conversation_history> echo on every single turn, and a turn that used a
// tool replayed the whole transcript on top of the live session.
func TestClaudeBinResumeSendsOnlyNewUserTurns(t *testing.T) {
	h := newClaudeContinuationHarness(t, "session-1")

	system := core.SystemText("be helpful")
	turnOne := core.UserText("turn one")
	h.turn(t, system, turnOne)
	if h.resumed() {
		t.Fatalf("first turn used --resume: %q", strings.Join(h.args, " "))
	}
	if got, want := h.inputText(t), "turn one"; got != want {
		t.Fatalf("first turn stdin text = %q, want %q", got, want)
	}

	replyOne := core.AssistantText("answer one")
	turnTwo := core.UserText("turn two")
	h.turn(t, system, turnOne, replyOne, turnTwo)
	if !h.resumed() {
		t.Fatalf("second turn did not resume: %q", strings.Join(h.args, " "))
	}
	if got, want := h.inputText(t), "turn two"; got != want {
		t.Fatalf("second turn stdin text = %q, want only the new user turn %q", got, want)
	}

	toolCall := assistantToolCall("call-1", "glob", `{"pattern":"*.md"}`)
	toolResult := core.ToolResultMessage("call-1", "glob", "README.md", nil)
	replyTwo := core.AssistantText("answer two")
	turnThree := core.UserText("turn three")
	h.turn(t, system, turnOne, replyOne, turnTwo, toolCall, toolResult, replyTwo, turnThree)
	if !h.resumed() {
		t.Fatalf("third turn did not resume: %q", strings.Join(h.args, " "))
	}
	if got, want := h.inputText(t), "turn three"; got != want {
		t.Fatalf("third turn stdin text = %q, want only the new user turn %q", got, want)
	}
	for _, banned := range []string{"<conversation_history>", "Assistant called tool:", "Tool result (", "answer one", "answer two"} {
		if strings.Contains(h.stdin, banned) {
			t.Fatalf("third turn stdin replayed %q into the resumed session:\n%s", banned, h.stdin)
		}
	}
}

// Developer turns are term-llm content that Claude Code has never seen, so they
// must survive the resume filter that drops assistant and tool messages.
func TestClaudeBinResumeKeepsDeveloperTurns(t *testing.T) {
	h := newClaudeContinuationHarness(t, "session-dev")

	system := core.SystemText("be helpful")
	turnOne := core.UserText("turn one")
	h.turn(t, system, turnOne)

	developer := core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "follow the style guide"}}}
	turnTwo := core.UserText("turn two")
	h.turn(t, system, turnOne, core.AssistantText("answer one"), developer, turnTwo)

	if !strings.Contains(h.stdin, "follow the style guide") {
		t.Fatalf("resume stdin dropped the developer turn:\n%s", h.stdin)
	}
	if strings.Contains(h.stdin, "answer one") {
		t.Fatalf("resume stdin echoed the assistant reply:\n%s", h.stdin)
	}
}

// Undo, branching and history rewrites replace earlier turns without shortening
// the transcript. Resuming on that boundary would silently drop or duplicate
// conversation, so a prefix that no longer matches must start a fresh session.
func TestClaudeBinResumeResetsWhenHistoryRewritten(t *testing.T) {
	h := newClaudeContinuationHarness(t, "session-2")

	system := core.SystemText("be helpful")
	h.turn(t, system, core.UserText("turn one"))
	if h.provider.transcriptDigest == "" {
		t.Fatal("completed turn recorded no transcript digest")
	}

	rewritten := core.UserText("turn one, rewritten")
	h.turn(t, system, rewritten, core.AssistantText("answer one"), core.UserText("turn two"))

	if h.resumed() {
		t.Fatalf("resumed a diverged transcript: %q", strings.Join(h.args, " "))
	}
	if !strings.Contains(h.stdin, "turn one, rewritten") {
		t.Fatalf("fresh session did not replay the rewritten history:\n%s", h.stdin)
	}
}

// Legacy persisted state predates transcript fingerprinting. It is unverifiable
// rather than proven stale, so it must keep resuming instead of forcing a replay.
func TestClaudeBinImportedStateWithoutDigestStillResumes(t *testing.T) {
	h := newClaudeContinuationHarness(t, "session-3")
	if err := h.provider.ImportProviderState([]byte(`{"session_id":"restored","messages_sent":2}`)); err != nil {
		t.Fatalf("ImportProviderState: %v", err)
	}

	h.turn(t, core.SystemText("be helpful"), core.UserText("turn one"), core.AssistantText("answer one"), core.UserText("turn two"))

	if !h.resumed() {
		t.Fatalf("legacy state did not resume: %q", strings.Join(h.args, " "))
	}
	if got, want := h.inputText(t), "turn two"; got != want {
		t.Fatalf("legacy resume stdin text = %q, want %q", got, want)
	}
	if h.provider.transcriptDigest == "" {
		t.Fatal("completed turn did not start fingerprinting the transcript")
	}
}

// Claude Code owns the tool loop for a turn, so its budget has to cover every
// tool round-trip. Capping it at one turn made Claude Code abandon the turn
// mid-tool-call and record a synthetic continuation exchange in the session.
func TestClaudeBinMaxTurnsCoversInlineToolLoop(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	tools := []core.ToolSpec{{Name: "glob", Description: "find files"}}

	tests := []struct {
		name string
		req  core.Request
		want string
	}{
		{name: "no tools cannot loop", req: core.Request{}, want: "--max-turns 1"},
		{name: "explicit budget", req: core.Request{Tools: tools, MaxTurns: 30}, want: "--max-turns 30"},
		{name: "default budget", req: core.Request{Tools: tools}, want: "--max-turns 200"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := p.buildArgs(context.Background(), tc.req, eventSender{})
			if joined := strings.Join(args, " "); !strings.Contains(joined, tc.want) {
				t.Fatalf("buildArgs() = %q, want %q", joined, tc.want)
			}
		})
	}
}

// Claude Code now ends the agentic loop itself, so its budget exhaustion has to
// reach the user the same way the engine's own budget did. The turn must still
// complete: the streamed work is real, and failing here would strand the resume
// boundary behind messages Claude Code has already accepted.
func TestClaudeBinReportsMaxTurnsWithoutFailingTheTurn(t *testing.T) {
	p := NewClaudeBinProvider("sonnet", nil)
	var (
		lastUsage       *core.Usage
		sawTextDelta    bool
		fallbackText    string
		handledTerminal bool
	)
	events := make(chan core.Event, 8)
	send := eventSender{ctx: context.Background(), ch: events}

	line := `{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["Reached maximum number of turns (2)"],"usage":{"input_tokens":10,"output_tokens":5}}`
	if err := p.handleClaudeLine(context.Background(), line, false, send, &lastUsage, &sawTextDelta, &fallbackText, &handledTerminal, false); err != nil {
		t.Fatalf("handleClaudeLine returned an error for a bounded loop: %v", err)
	}
	close(events)

	if !handledTerminal {
		t.Fatal("max-turns result was not treated as a handled terminal result; the CLI exit would surface as a failure")
	}
	var phases []string
	for event := range events {
		if event.Type == core.EventPhase {
			phases = append(phases, event.Text)
		}
	}
	if len(phases) != 1 || !strings.Contains(phases[0], "out of turns") {
		t.Fatalf("phases = %q, want an out-of-turns warning", phases)
	}
	if lastUsage == nil {
		t.Fatal("max-turns result dropped usage accounting")
	}
}

func TestClaudeBinRunsToolLoopInline(t *testing.T) {
	caps := NewClaudeBinProvider("sonnet", nil).Capabilities()
	if !caps.InlineToolLoop {
		t.Fatal("InlineToolLoop = false; re-entering the provider per tool call respawns the CLI mid-turn")
	}
	if !caps.OrderedInlineToolEvents {
		t.Fatal("OrderedInlineToolEvents = false; inline text/tool ordering would be rebuilt out of order")
	}
}
