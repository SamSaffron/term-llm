package engine

import (
	"context"
	"encoding/json"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"io"
	"testing"
)

type fastBenchmarkTool struct{}

func (t *fastBenchmarkTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "fast_benchmark_tool",
		Description: "fast no-op benchmark tool",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *fastBenchmarkTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("ok"), nil
}

func (t *fastBenchmarkTool) Preview(args json.RawMessage) string {
	return ""
}

func BenchmarkExecuteSingleToolCallFast(b *testing.B) {
	registry := NewToolRegistry()
	registry.Register(&fastBenchmarkTool{})
	engine := NewEngine(&fakeProvider{}, registry)
	call := core.ToolCall{
		ID:        "call-bench",
		Name:      "fast_benchmark_tool",
		Arguments: json.RawMessage(`{}`),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan core.Event, 1024)
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
	send := corestream.NewSender(ctx, events)

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
	events []core.Event
	idx    int
}

func (s *benchmarkEventStream) Recv() (core.Event, error) {
	if s.idx >= len(s.events) {
		return core.Event{}, io.EOF
	}
	event := s.events[s.idx]
	s.idx++
	return event, nil
}

func (s *benchmarkEventStream) Close() error { return nil }

func BenchmarkLoggingStreamTextDeltas(b *testing.B) {
	const textEvents = 2048
	events := make([]core.Event, 0, textEvents+1)
	for i := 0; i < textEvents; i++ {
		events = append(events, core.Event{Type: core.EventTextDelta, Text: "x"})
	}
	events = append(events, core.Event{Type: core.EventDone})

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

type scratchpadBenchmarkProvider struct {
	events    []core.Event
	toolCalls bool
}

func (p *scratchpadBenchmarkProvider) Name() string       { return "scratchpad-benchmark" }
func (p *scratchpadBenchmarkProvider) Credential() string { return "test" }
func (p *scratchpadBenchmarkProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: p.toolCalls}
}
func (p *scratchpadBenchmarkProvider) Stream(context.Context, core.Request) (core.Stream, error) {
	return &benchmarkEventStream{events: p.events}, nil
}

func BenchmarkProviderStreamScratchpad(b *testing.B) {
	const deltaCount = 4096
	events := make([]core.Event, 0, deltaCount+1)
	for i := 0; i < deltaCount; i++ {
		if i%2 == 0 {
			events = append(events, core.Event{Type: core.EventTextDelta, Text: "x"})
		} else {
			events = append(events, core.Event{Type: core.EventReasoningDelta, Text: "r", ReasoningKind: core.ReasoningKindRaw})
		}
	}
	events = append(events, core.Event{Type: core.EventDone})

	b.Run("Simple", func(b *testing.B) {
		provider := &scratchpadBenchmarkProvider{events: events}
		engine := NewEngine(provider, nil)
		req := core.Request{Messages: []core.Message{core.UserText("benchmark")}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := engine.runSimpleScratchpad(context.Background(), req, corestream.Sender{}); err != nil {
				b.Fatalf("runSimpleScratchpad returned error: %v", err)
			}
		}
	})

	b.Run("Agentic", func(b *testing.B) {
		provider := &scratchpadBenchmarkProvider{events: events, toolCalls: true}
		engine := NewEngine(provider, NewToolRegistry())
		req := core.Request{
			Messages: []core.Message{core.UserText("benchmark")},
			Tools:    []core.ToolSpec{{Name: "benchmark_tool", Schema: map[string]any{"type": "object"}}},
			MaxTurns: 1,
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := engine.runLoop(context.Background(), req, corestream.Sender{}); err != nil {
				b.Fatalf("runLoop returned error: %v", err)
			}
		}
	})
}
