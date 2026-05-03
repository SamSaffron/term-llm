package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/genai"
)

func TestBuildGeminiContents_DropsDanglingToolCalls(t *testing.T) {
	_, contents := buildGeminiContents([]Message{
		UserText("Run shell"),
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Working"},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "call-1",
						Name:      "shell",
						Arguments: []byte(`{"command":"sleep 10"}`),
					},
				},
			},
		},
		UserText("new request"),
	})

	if len(contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(contents))
	}

	assistant := contents[1]
	if assistant.Role != "model" {
		t.Fatalf("expected role model, got %q", assistant.Role)
	}

	var sawText bool
	for _, part := range assistant.Parts {
		if part.FunctionCall != nil {
			t.Fatalf("expected dangling functionCall to be removed, got %#v", part.FunctionCall)
		}
		if part.Text == "Working" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("expected assistant text to be preserved, got %#v", assistant.Parts)
	}
}

func TestEmitGeminiParts_StreamsTextAndToolCallsInOrder(t *testing.T) {
	thoughtSig := []byte{1, 2, 3}
	events := make(chan Event, 4)
	var lastThoughtSig []byte

	err := emitGeminiParts(eventSender{ctx: context.Background(), ch: events}, []*genai.Part{
		{Thought: true, ThoughtSignature: thoughtSig},
		{Text: "Working"},
		{Text: "..."},
		{FunctionCall: &genai.FunctionCall{
			ID:   "call_1",
			Name: "lookup",
			Args: map[string]any{"q": "weather"},
		}},
		{Text: "done"},
	}, &lastThoughtSig)
	if err != nil {
		t.Fatalf("emitGeminiParts() error = %v", err)
	}
	close(events)

	var got []Event
	for event := range events {
		got = append(got, event)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	if got[0].Type != EventTextDelta || got[0].Text != "Working..." {
		t.Fatalf("first event = %+v, want text delta %q", got[0], "Working...")
	}
	if got[1].Type != EventToolCall || got[1].Tool == nil {
		t.Fatalf("second event = %+v, want tool call", got[1])
	}
	if got[1].Tool.ID != "call_1" || got[1].Tool.Name != "lookup" || string(got[1].Tool.Arguments) != `{"q":"weather"}` {
		t.Fatalf("tool call = %+v", got[1].Tool)
	}
	if string(got[1].Tool.ThoughtSig) != string(thoughtSig) {
		t.Fatalf("tool thought signature = %v, want %v", got[1].Tool.ThoughtSig, thoughtSig)
	}
	if got[2].Type != EventTextDelta || got[2].Text != "done" {
		t.Fatalf("third event = %+v, want text delta %q", got[2], "done")
	}
	if string(lastThoughtSig) != string(thoughtSig) {
		t.Fatalf("lastThoughtSig = %v, want %v", lastThoughtSig, thoughtSig)
	}
}

func TestEmitGeminiUsage_DoesNotBlockAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := make(chan Event, 1)
	events <- Event{Type: EventTextDelta, Text: "buffer-full"}

	resp := &genai.GenerateContentResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     3,
			CandidatesTokenCount: 5,
			TotalTokenCount:      8,
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- emitGeminiUsage(eventSender{ctx: ctx, ch: events}, resp)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("emitGeminiUsage() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emitGeminiUsage blocked after context cancellation")
	}
}
