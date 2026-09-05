package llm

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

func boundaryStreamError(stream Stream) error {
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if event.Type == EventError {
			return event.Err
		}
	}
}

func TestModelBoundaryAfterToolPersistence(t *testing.T) {
	provider := NewMockProvider("boundary").
		AddToolCall("before", "count_tool", map[string]any{}).
		AddToolCall("after", "count_tool", map[string]any{}).
		AddTextResponse("finished")
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)
	var persisted atomic.Int64
	engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []Message, _ TurnMetrics) error {
		for _, message := range messages {
			for _, part := range message.Parts {
				if part.ToolResult != nil {
					persisted.Add(1)
				}
			}
		}
		return nil
	})
	parked := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boundary := func(ctx context.Context) error {
		if persisted.Load() != 1 {
			return nil
		}
		close(parked)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	stream, err := engine.Stream(ctx, Request{ModelBoundary: boundary, Messages: []Message{UserText("work")}, Tools: []ToolSpec{tool.Spec()}, MaxTurns: 5})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- boundaryStreamError(stream) }()
	select {
	case <-parked:
	case err := <-done:
		t.Fatalf("ended before parking: %v", err)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Errorf("executions while parked = %d, want 1", got)
	}
	if got := len(provider.RecordedRequests()); got != 1 {
		t.Errorf("provider requests while parked = %d, want 1", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := tool.calls.Load(); got != 2 {
		t.Fatalf("executions after release = %d, want 2", got)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	for _, request := range requests {
		users := 0
		for _, message := range request.Messages {
			if message.Role == RoleUser {
				users++
			}
		}
		if users != 1 {
			t.Errorf("boundary injected user input: %d user messages", users)
		}
	}
}

func TestModelBoundaryFailsClosedOnPersistenceError(t *testing.T) {
	provider := NewMockProvider("boundary").AddToolCall("before", "count_tool", map[string]any{}).AddTextResponse("must not run")
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)
	want := errors.New("checkpoint unavailable")
	engine.SetTurnCompletedCallback(func(context.Context, int, []Message, TurnMetrics) error { return want })
	var boundaries atomic.Int64
	ctx := context.Background()
	boundary := func(context.Context) error { boundaries.Add(1); return nil }
	stream, err := engine.Stream(ctx, Request{ModelBoundary: boundary, Messages: []Message{UserText("work")}, Tools: []ToolSpec{tool.Spec()}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := boundaryStreamError(stream); !errors.Is(err, want) {
		t.Fatalf("error = %v, want persistence failure", err)
	}
	if got := boundaries.Load(); got != 1 {
		t.Fatalf("boundary callbacks = %d, want initial boundary only", got)
	}
	if got := len(provider.RecordedRequests()); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
}

func TestModelBoundaryCancellation(t *testing.T) {
	provider := NewMockProvider("boundary").AddTextResponse("must not run")
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	boundary := func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	stream, err := engine.Stream(ctx, Request{ModelBoundary: boundary, Messages: []Message{UserText("work")}, Tools: []ToolSpec{tool.Spec()}})
	if err != nil {
		t.Fatal(err)
	}
	_ = boundaryStreamError(stream) // Cancellation may close the event stream directly.
	if got := len(provider.RecordedRequests()); got != 0 {
		t.Fatalf("provider requests = %d after cancellation, want 0", got)
	}
}

func TestModelBoundaryRejectsInlineTools(t *testing.T) {
	provider := NewMockProvider("native").WithCapabilities(Capabilities{ToolCalls: true, InlineToolLoop: true}).AddTextResponse("must not run")
	engine := NewEngine(provider, NewToolRegistry())
	var boundaries atomic.Int64
	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("work")},
		ModelBoundary: func(context.Context) error {
			boundaries.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := boundaryStreamError(stream); err == nil {
		t.Fatal("inline tool-loop provider admitted a cooperative boundary")
	}
	if boundaries.Load() != 0 || len(provider.RecordedRequests()) != 0 {
		t.Fatal("unsupported provider reached boundary or model")
	}
}

func TestModelBoundaryTextOnly(t *testing.T) {
	provider := NewMockProvider("text").WithCapabilities(Capabilities{}).AddTextResponse("finished")
	engine := NewEngine(provider, NewToolRegistry())
	var boundaries atomic.Int64
	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("work")},
		ModelBoundary: func(context.Context) error {
			boundaries.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := boundaryStreamError(stream); err != nil {
		t.Fatal(err)
	}
	if boundaries.Load() != 1 {
		t.Fatal("completed text response must not create a continuation boundary")
	}
	requests := provider.RecordedRequests()
	if len(requests) != 1 || requests[0].ModelBoundary != nil {
		t.Fatal("internal boundary leaked into provider request")
	}
}
