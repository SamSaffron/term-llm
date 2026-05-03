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

func TestGeminiStreamEmitState_StreamsToolTurnsIncrementally(t *testing.T) {
	ctx := context.Background()
	events := make(chan Event, 8)
	sender := eventSender{ctx: ctx, ch: events}
	thoughtSig := []byte("thought-sig")

	var state geminiStreamEmitState
	if err := state.emit(sender, &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{Parts: []*genai.Part{
				{Text: "Thinking out loud..."},
				{Thought: true, ThoughtSignature: thoughtSig},
			}},
		}},
	}); err != nil {
		t.Fatalf("emit first chunk: %v", err)
	}

	toolChunk := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{
			Content: &genai.Content{Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   "call-1",
					Name: "lookup_weather",
					Args: map[string]any{"city": "Sydney"},
				},
			}}},
		}},
	}
	if err := state.emit(sender, toolChunk); err != nil {
		t.Fatalf("emit tool chunk: %v", err)
	}
	if err := state.emit(sender, toolChunk); err != nil {
		t.Fatalf("emit duplicate tool chunk: %v", err)
	}
	close(events)

	var got []Event
	for event := range events {
		got = append(got, event)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d: %#v", len(got), got)
	}
	if got[0].Type != EventTextDelta || got[0].Text != "Thinking out loud..." {
		t.Fatalf("first event = %#v, want text delta", got[0])
	}
	if got[1].Type != EventToolCall {
		t.Fatalf("second event = %#v, want tool call", got[1])
	}
	if got[1].Tool == nil {
		t.Fatal("expected tool call payload")
	}
	if got[1].Tool.Name != "lookup_weather" {
		t.Fatalf("tool name = %q, want lookup_weather", got[1].Tool.Name)
	}
	if string(got[1].Tool.Arguments) != `{"city":"Sydney"}` {
		t.Fatalf("tool args = %s, want %s", got[1].Tool.Arguments, `{"city":"Sydney"}`)
	}
	if string(got[1].Tool.ThoughtSig) != string(thoughtSig) {
		t.Fatalf("tool thought signature = %q, want %q", got[1].Tool.ThoughtSig, thoughtSig)
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
