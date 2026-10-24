package clibin

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestClaudeUnacknowledgedFailureDiscardsNativeCursor(t *testing.T) {
	for _, laterTurn := range []bool{false, true} {
		name := "initial turn"
		if laterTurn {
			name = "later turn"
		}
		t.Run(name, func(t *testing.T) {
			h := newClaudeContinuationHarness(t, "native-session")
			messages := []core.Message{core.UserText("original task")}
			if laterTurn {
				h.turn(t, messages...)
				if h.provider.messagesSent == 0 {
					t.Fatal("successful turn did not establish native cursor")
				}
				messages = append(messages, core.AssistantText("earlier reply"), core.UserText("failed turn instruction"))
			}
			crash := errors.New("process exited with status 2 before terminal result")
			h.provider.commandRunner = func(_ context.Context, _ []string, _, _, _ string, _ bool, _ eventSender, _, _ bool) error {
				// A system message can establish a native ID before the process crashes.
				h.provider.sessionID = "native-session"
				return crash
			}
			stream, err := h.provider.Stream(context.Background(), core.Request{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			for {
				var event core.Event
				event, err = stream.Recv()
				if event.Err != nil {
					err = event.Err
				}
				if err != nil {
					break
				}
			}
			stream.Close()
			if err == io.EOF || !errors.Is(err, crash) {
				t.Fatalf("stream failure = %v", err)
			}
			if state, ok := h.provider.ExportProviderState(); ok || len(state) != 0 {
				t.Fatalf("untrusted state survived: %s", state)
			}
			if h.provider.messagesSent != 0 || h.provider.transcriptDigest != "" {
				t.Fatal("stale cursor survived native reset")
			}

			h.provider.commandRunner = func(_ context.Context, args []string, _, prompt, _ string, _ bool, send eventSender, _, _ bool) error {
				h.args, h.stdin = args, prompt
				return send.Send(core.Event{Type: core.EventTextDelta, Text: "recovered"})
			}
			messages = append(messages, core.UserText("continue"))
			h.turn(t, messages...)
			if h.resumed() {
				t.Fatalf("unacknowledged failure resumed native session: %v", h.args)
			}
			if laterTurn && !strings.Contains(h.stdin, "failed turn instruction") {
				t.Fatalf("fallback lost failed instruction: %s", h.stdin)
			}
		})
	}
}

func TestClaudeResumeBoundarySurvivesSystemPromptChange(t *testing.T) {
	transcript := []core.Message{core.SystemText("prompt v1"), core.UserText("hi"), core.AssistantText("hello")}
	changedPrompt := append([]core.Message{core.SystemText("prompt v2")}, transcript[1:]...)
	rewritten := []core.Message{core.SystemText("prompt v1"), core.UserText("bye"), core.AssistantText("hello")}

	for name, digest := range map[string]string{
		"current": claudeTranscriptDigest(transcript, 3),
		"legacy":  claudeDigest(transcript, 3, true),
	} {
		p := &ClaudeBinProvider{sessionID: "s", messagesSent: 3, transcriptDigest: digest}
		if !p.resumeBoundaryValid(transcript) {
			t.Fatalf("%s digest rejected its own transcript", name)
		}
		if name == "current" && !p.resumeBoundaryValid(changedPrompt) {
			t.Fatal("system prompt change must not invalidate the resume boundary")
		}
		if p.resumeBoundaryValid(rewritten) {
			t.Fatalf("%s digest accepted a rewritten user turn", name)
		}
	}
}

// A resumed native session that the API rejects as too long can never be
// resumed successfully: Claude Code re-sends its whole context with every later
// turn, however short. Keeping its cursor wedged a web session permanently
// ("hi" -> "Prompt is too long"), and Claude Code cannot natively compact a
// single oversized exchange. The cursor must be discarded instead.
func TestClaudePromptTooLongDiscardsNativeSession(t *testing.T) {
	h := newClaudeContinuationHarness(t, "native-session")
	messages := []core.Message{core.UserText("original task")}
	h.turn(t, messages...)
	if _, ok := h.provider.ExportProviderState(); !ok {
		t.Fatal("successful turn did not establish native state")
	}

	tooLong := errors.New("claude API error: Prompt is too long · the request is ~1129868 tokens (limit 1000000)")
	attempts := 0
	h.provider.commandRunner = func(_ context.Context, _ []string, _, _, _ string, _ bool, _ eventSender, _, _ bool) error {
		attempts++
		h.provider.sessionID = "native-session"
		return tooLong
	}
	messages = append(messages, core.AssistantText("earlier reply"), core.UserText("hi"))
	stream, err := h.provider.Stream(context.Background(), core.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	for {
		var event core.Event
		event, err = stream.Recv()
		if event.Err != nil {
			err = event.Err
		}
		if err != nil {
			break
		}
	}
	stream.Close()
	if !errors.Is(err, tooLong) {
		t.Fatalf("stream failure = %v", err)
	}
	if attempts == 0 {
		t.Fatal("command never ran")
	}
	if state, ok := h.provider.ExportProviderState(); ok || len(state) != 0 {
		t.Fatalf("oversized native session would be resumed: %s", state)
	}

	h.provider.commandRunner = func(_ context.Context, args []string, _, prompt, _ string, _ bool, send eventSender, _, _ bool) error {
		h.args, h.stdin = args, prompt
		h.provider.sessionID = "fresh-session"
		return send.Send(core.Event{Type: core.EventTextDelta, Text: "recovered"})
	}
	messages = append(messages, core.UserText("oh hello"))
	h.turn(t, messages...)
	if h.resumed() {
		t.Fatalf("next turn resumed the oversized session: %v", h.args)
	}
	if text := h.inputText(t); !strings.Contains(text, "original task") || !strings.Contains(text, "oh hello") {
		t.Fatalf("fresh turn did not replay the durable transcript: %s", text)
	}
}
