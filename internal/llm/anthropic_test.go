package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestToolCallAccumulatorInputJSONDelta(t *testing.T) {
	acc := newToolCallAccumulator()
	start := ToolCall{
		ID:   "tool-1",
		Name: "edit",
	}
	acc.Start(0, start)

	acc.Append(0, `{"file_path":"main.go","old_string":"foo"`)
	acc.Append(0, `,"new_string":"bar"}`)

	final, ok := acc.Finish(0)
	if !ok {
		t.Fatalf("expected tool call")
	}

	var payload map[string]string
	if err := json.Unmarshal(final.Arguments, &payload); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}

	if payload["file_path"] != "main.go" {
		t.Fatalf("file_path=%q", payload["file_path"])
	}
	if payload["old_string"] != "foo" {
		t.Fatalf("old_string=%q", payload["old_string"])
	}
	if payload["new_string"] != "bar" {
		t.Fatalf("new_string=%q", payload["new_string"])
	}
}

func TestToolCallAccumulatorFallbackArgs(t *testing.T) {
	acc := newToolCallAccumulator()
	start := ToolCall{
		ID:        "tool-2",
		Name:      "edit",
		Arguments: json.RawMessage(`{"file_path":"main.go","old_string":"a","new_string":"b"}`),
	}
	acc.Start(1, start)

	final, ok := acc.Finish(1)
	if !ok {
		t.Fatalf("expected tool call")
	}

	var payload map[string]string
	if err := json.Unmarshal(final.Arguments, &payload); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}

	if payload["new_string"] != "b" {
		t.Fatalf("new_string=%q", payload["new_string"])
	}
}

func TestBuildAnthropicBlocks_AssistantReasoningReplay(t *testing.T) {
	parts := []Part{
		{
			Type:                      PartText,
			Text:                      "Final answer",
			ReasoningContent:          "I should inspect configuration first.",
			ReasoningEncryptedContent: "anthropic-signature-1",
		},
	}

	blocks := buildAnthropicBlocks(parts, true)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (thinking + text), got %d", len(blocks))
	}

	if got := blocks[0].GetSignature(); got == nil || *got != "anthropic-signature-1" {
		t.Fatalf("expected thinking signature anthropic-signature-1, got %v", got)
	}
	if got := blocks[0].GetThinking(); got == nil || *got != "I should inspect configuration first." {
		t.Fatalf("expected thinking text to round-trip, got %v", got)
	}
	if got := blocks[1].GetText(); got == nil || *got != "Final answer" {
		t.Fatalf("expected second block text Final answer, got %v", got)
	}
	if got := blocks[1].GetSignature(); got != nil {
		t.Fatalf("did not expect thinking signature in text block, got %v", got)
	}
}

func TestBuildAnthropicBlocks_UserDoesNotReplayReasoning(t *testing.T) {
	parts := []Part{
		{
			Type:                      PartText,
			Text:                      "User message",
			ReasoningContent:          "ignored",
			ReasoningEncryptedContent: "ignored-signature",
		},
	}

	blocks := buildAnthropicBlocks(parts, false)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 text block for non-assistant replay, got %d", len(blocks))
	}
	if got := blocks[0].GetText(); got == nil || *got != "User message" {
		t.Fatalf("expected user text block, got %v", got)
	}
	if got := blocks[0].GetSignature(); got != nil {
		t.Fatalf("did not expect thinking signature in non-assistant block, got %v", got)
	}
}

func TestBuildAnthropicBetaBlocks_AssistantReasoningReplay(t *testing.T) {
	parts := []Part{
		{
			Type:                      PartText,
			Text:                      "Search answer",
			ReasoningContent:          "I should verify sources.",
			ReasoningEncryptedContent: "anthropic-signature-beta-1",
		},
	}

	blocks := buildAnthropicBetaBlocks(parts, true)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 beta blocks (thinking + text), got %d", len(blocks))
	}

	if got := blocks[0].GetSignature(); got == nil || *got != "anthropic-signature-beta-1" {
		t.Fatalf("expected beta thinking signature anthropic-signature-beta-1, got %v", got)
	}
	if got := blocks[0].GetThinking(); got == nil || *got != "I should verify sources." {
		t.Fatalf("expected beta thinking text to round-trip, got %v", got)
	}
	if got := blocks[1].GetText(); got == nil || *got != "Search answer" {
		t.Fatalf("expected second beta block text Search answer, got %v", got)
	}
	if got := blocks[1].GetSignature(); got != nil {
		t.Fatalf("did not expect thinking signature in beta text block, got %v", got)
	}
}

func TestEmitReasoningDelta_ProducesReasoningEvent(t *testing.T) {
	events := make(chan Event, 1)

	emitReasoningDelta(events, "thinking chunk", "sig-123")

	select {
	case ev := <-events:
		if ev.Type != EventReasoningDelta {
			t.Fatalf("expected EventReasoningDelta, got %v", ev.Type)
		}
		if ev.Text != "thinking chunk" {
			t.Fatalf("expected reasoning text chunk, got %q", ev.Text)
		}
		if ev.ReasoningEncryptedContent != "sig-123" {
			t.Fatalf("expected reasoning signature sig-123, got %q", ev.ReasoningEncryptedContent)
		}
	default:
		t.Fatal("expected reasoning event")
	}
}

func TestAnthropicThinkingBlockHelpersExposeReplayFields(t *testing.T) {
	thinking := anthropic.NewThinkingBlock("sig-x", "reasoning text")
	if got := thinking.GetSignature(); got == nil || *got != "sig-x" {
		t.Fatalf("expected helper signature sig-x, got %v", got)
	}
	if got := thinking.GetThinking(); got == nil || *got != "reasoning text" {
		t.Fatalf("expected helper thinking text, got %v", got)
	}
}
