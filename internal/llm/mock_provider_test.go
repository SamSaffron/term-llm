package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestMockProvider_BasicInfo(t *testing.T) {
	p := NewMockProvider("test-mock")

	if got := p.Name(); got != "test-mock" {
		t.Errorf("Name() = %q, want %q", got, "test-mock")
	}

	if got := p.Credential(); got != "mock" {
		t.Errorf("Credential() = %q, want %q", got, "mock")
	}

	// Default capabilities should have ToolCalls enabled
	caps := p.Capabilities()
	if !caps.ToolCalls {
		t.Error("expected ToolCalls to be true by default")
	}
}

func TestMockProvider_WithCapabilities(t *testing.T) {
	p := NewMockProvider("test").WithCapabilities(Capabilities{
		NativeWebSearch: true,
		NativeWebFetch:  true,
		ToolCalls:       false,
	})

	caps := p.Capabilities()
	if !caps.NativeWebSearch {
		t.Error("expected NativeWebSearch to be true")
	}
	if !caps.NativeWebFetch {
		t.Error("expected NativeWebFetch to be true")
	}
	if caps.ToolCalls {
		t.Error("expected ToolCalls to be false")
	}
}

func TestMockProvider_StreamTextResponse(t *testing.T) {
	p := NewMockProvider("test")
	p.AddTextResponse("Hello, world!")

	ctx := context.Background()
	stream, err := p.Stream(ctx, Request{
		Messages: []Message{UserText("Hi")},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var text string
	var gotUsage bool

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}

		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventUsage:
			gotUsage = true
		}
	}

	if text != "Hello, world!" {
		t.Errorf("got text %q, want %q", text, "Hello, world!")
	}
	if !gotUsage {
		t.Error("expected usage event")
	}

	// Verify request was recorded
	if len(p.Requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(p.Requests))
	}
}

func TestMockProvider_StreamToolCall(t *testing.T) {
	p := NewMockProvider("test")
	p.AddToolCall("call_123", "read_file", map[string]string{"path": "main.go"})

	ctx := context.Background()
	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var toolCall *ToolCall

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}

		if event.Type == EventToolCall {
			toolCall = event.Tool
		}
	}

	if toolCall == nil {
		t.Fatal("expected tool call event")
	}
	if toolCall.ID != "call_123" {
		t.Errorf("tool call ID = %q, want %q", toolCall.ID, "call_123")
	}
	if toolCall.Name != "read_file" {
		t.Errorf("tool call Name = %q, want %q", toolCall.Name, "read_file")
	}

	var args map[string]string
	if err := json.Unmarshal(toolCall.Arguments, &args); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}
	if args["path"] != "main.go" {
		t.Errorf("args[path] = %q, want %q", args["path"], "main.go")
	}
}

func TestMockProvider_MultiTurn(t *testing.T) {
	p := NewMockProvider("test")
	p.AddToolCall("call_1", "read_file", map[string]string{"path": "main.go"})
	p.AddTextResponse("The file contains the main function.")

	ctx := context.Background()

	// First turn: tool call
	stream1, err := p.Stream(ctx, Request{Messages: []Message{UserText("What's in main.go?")}})
	if err != nil {
		t.Fatalf("Stream() turn 1 error = %v", err)
	}

	var gotToolCall bool
	for {
		event, err := stream1.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() turn 1 error = %v", err)
		}
		if event.Type == EventToolCall {
			gotToolCall = true
		}
	}
	stream1.Close()

	if !gotToolCall {
		t.Error("expected tool call in turn 1")
	}

	// Second turn: text response (after tool result)
	stream2, err := p.Stream(ctx, Request{Messages: []Message{
		UserText("What's in main.go?"),
		ToolResultMessage("call_1", "read_file", "package main\n\nfunc main() {}"),
	}})
	if err != nil {
		t.Fatalf("Stream() turn 2 error = %v", err)
	}

	var text string
	for {
		event, err := stream2.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() turn 2 error = %v", err)
		}
		if event.Type == EventTextDelta {
			text += event.Text
		}
	}
	stream2.Close()

	if text != "The file contains the main function." {
		t.Errorf("turn 2 text = %q, want %q", text, "The file contains the main function.")
	}

	// Verify both requests were recorded
	if len(p.Requests) != 2 {
		t.Errorf("expected 2 requests, got %d", len(p.Requests))
	}
}

func TestMockProvider_NoMoreTurns(t *testing.T) {
	p := NewMockProvider("test")
	p.AddTextResponse("Hello")

	ctx := context.Background()

	// First turn succeeds
	stream1, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() turn 1 error = %v", err)
	}
	for {
		_, err := stream1.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
	}
	stream1.Close()

	// Second turn should fail
	_, err = p.Stream(ctx, Request{})
	if err == nil {
		t.Error("expected error when no more turns configured")
	}
}

func TestMockProvider_Error(t *testing.T) {
	testErr := errors.New("test error")
	p := NewMockProvider("test")
	p.AddError(testErr)

	ctx := context.Background()
	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var gotError error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			gotError = err
			break
		}
		if event.Type == EventError {
			gotError = event.Err
		}
	}

	if gotError == nil {
		t.Error("expected error event")
	}
	if !errors.Is(gotError, testErr) {
		t.Errorf("got error %v, want %v", gotError, testErr)
	}
}

func TestMockProvider_Delay(t *testing.T) {
	p := NewMockProvider("test")
	p.AddTurn(MockTurn{
		Text:  "Delayed response",
		Delay: 50 * time.Millisecond,
	})

	ctx := context.Background()
	start := time.Now()

	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	// Consume all events
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
	}

	elapsed := time.Since(start)
	if elapsed < 50*time.Millisecond {
		t.Errorf("expected delay of at least 50ms, got %v", elapsed)
	}
}

func TestMockProvider_CancelDuringDelay(t *testing.T) {
	p := NewMockProvider("test")
	p.AddTurn(MockTurn{
		Text:  "Delayed response",
		Delay: 1 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())

	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	// Cancel immediately
	cancel()

	// Should return context error
	_, err = stream.Recv()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestMockProvider_Reset(t *testing.T) {
	p := NewMockProvider("test")
	p.AddTextResponse("Hello")

	ctx := context.Background()

	// First stream
	stream, _ := p.Stream(ctx, Request{})
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
	}
	stream.Close()

	if len(p.Requests) != 1 {
		t.Errorf("expected 1 request, got %d", len(p.Requests))
	}

	// Reset
	p.Reset()

	if len(p.Requests) != 0 {
		t.Errorf("expected 0 requests after reset, got %d", len(p.Requests))
	}
	if p.CurrentTurn() != 0 {
		t.Errorf("expected turn index 0 after reset, got %d", p.CurrentTurn())
	}

	// Should be able to stream again
	stream2, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() after reset error = %v", err)
	}
	for {
		_, err := stream2.Recv()
		if err == io.EOF {
			break
		}
	}
	stream2.Close()
}

func TestChunkText(t *testing.T) {
	tests := []struct {
		text      string
		chunkSize int
		wantLen   int
	}{
		{"", 10, 0},
		{"hello", 10, 1},
		{"hello world", 10, 2},
		{"hello world this is a longer text", 10, 4},
	}

	for _, tt := range tests {
		chunks := chunkText(tt.text, tt.chunkSize)
		if len(chunks) != tt.wantLen {
			t.Errorf("chunkText(%q, %d) = %d chunks, want %d", tt.text, tt.chunkSize, len(chunks), tt.wantLen)
		}

		// Verify reassembly
		var reassembled string
		for _, c := range chunks {
			reassembled += c
		}
		if reassembled != tt.text {
			t.Errorf("reassembled text = %q, want %q", reassembled, tt.text)
		}
	}
}
