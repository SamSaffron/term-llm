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
		done <- emitGeminiUsage(ctx, events, resp)
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
