package llm

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

type fastBenchmarkTool struct{}

func (t *fastBenchmarkTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "fast_benchmark_tool",
		Description: "fast no-op benchmark tool",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *fastBenchmarkTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return TextOutput("ok"), nil
}

func (t *fastBenchmarkTool) Preview(args json.RawMessage) string {
	return ""
}

func BenchmarkExecuteSingleToolCallFast(b *testing.B) {
	registry := NewToolRegistry()
	registry.Register(&fastBenchmarkTool{})
	engine := NewEngine(&fakeProvider{}, registry)
	call := ToolCall{
		ID:        "call-bench",
		Name:      "fast_benchmark_tool",
		Arguments: json.RawMessage(`{}`),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 1024)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case <-events:
			case <-ctx.Done():
				return
			}
		}
	}()
	send := eventSender{ctx: ctx, ch: events}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msgs, err := engine.executeSingleToolCall(ctx, call, send, false, false)
		if err != nil {
			b.Fatalf("executeSingleToolCall returned error: %v", err)
		}
		if len(msgs) != 1 {
			b.Fatalf("executeSingleToolCall returned %d messages, want 1", len(msgs))
		}
	}
	b.StopTimer()
	cancel()
	<-drained
}

type benchmarkEventStream struct {
	events []Event
	idx    int
}

func (s *benchmarkEventStream) Recv() (Event, error) {
	if s.idx >= len(s.events) {
		return Event{}, io.EOF
	}
	event := s.events[s.idx]
	s.idx++
	return event, nil
}

func (s *benchmarkEventStream) Close() error { return nil }

func BenchmarkLoggingStreamTextDeltas(b *testing.B) {
	const textEvents = 2048
	events := make([]Event, 0, textEvents+1)
	for i := 0; i < textEvents; i++ {
		events = append(events, Event{Type: EventTextDelta, Text: "x"})
	}
	events = append(events, Event{Type: EventDone})

	b.SetBytes(textEvents)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		stream := &loggingStream{inner: &benchmarkEventStream{events: events}}
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatalf("Recv returned error: %v", err)
			}
		}
	}
}
