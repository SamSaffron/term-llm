package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

type bridgeBarrierTool struct {
	started chan string
	release chan struct{}
}

func (t *bridgeBarrierTool) Spec() ToolSpec {
	return ToolSpec{Name: "spawn_agent", Schema: map[string]any{"type": "object"}}
}
func (t *bridgeBarrierTool) Preview(json.RawMessage) string { return "" }
func (t *bridgeBarrierTool) Execute(_ context.Context, args json.RawMessage) (ToolOutput, error) {
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return ToolOutput{}, err
	}
	t.started <- payload.ID
	<-t.release
	return TextOutput("done:" + payload.ID), nil
}

type concurrentSyncBridgeProvider struct {
	mu       sync.Mutex
	turn     int
	results  []ToolExecutionResponse
	callIDs  []string
	finished chan struct{}
}

func (p *concurrentSyncBridgeProvider) Name() string       { return "sync-bridge" }
func (p *concurrentSyncBridgeProvider) Credential() string { return "test" }
func (p *concurrentSyncBridgeProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}
}
func (p *concurrentSyncBridgeProvider) Stream(ctx context.Context, _ Request) (Stream, error) {
	p.mu.Lock()
	turn := p.turn
	p.turn++
	p.mu.Unlock()
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		if turn > 0 {
			if err := send.Send(Event{Type: EventTextDelta, Text: "complete"}); err != nil {
				return err
			}
			return send.Send(Event{Type: EventDone})
		}
		responses := make([]chan ToolExecutionResponse, 2)
		for i := range responses {
			responses[i] = make(chan ToolExecutionResponse, 1)
			id := fmt.Sprintf("toolu_%d", i+1)
			args, _ := json.Marshal(map[string]string{"id": id})
			if err := send.Send(Event{
				Type: EventToolCall, ToolCallID: id, ToolName: "spawn_agent",
				Tool: &ToolCall{ID: id, Name: "spawn_agent", Arguments: args}, ToolResponse: responses[i],
			}); err != nil {
				return err
			}
		}
		for i, response := range responses {
			select {
			case result := <-response:
				p.mu.Lock()
				p.callIDs = append(p.callIDs, fmt.Sprintf("toolu_%d", i+1))
				p.results = append(p.results, result)
				p.mu.Unlock()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		close(p.finished)
		return send.Send(Event{Type: EventDone})
	}), nil
}

func TestEngineRunsConcurrentSyncBridgeCallsThroughSharedExecutor(t *testing.T) {
	tool := &bridgeBarrierTool{started: make(chan string, 2), release: make(chan struct{})}
	registry := NewToolRegistry()
	registry.Register(tool)
	provider := &concurrentSyncBridgeProvider{finished: make(chan struct{})}
	engine := NewEngine(provider, registry)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := engine.Stream(ctx, Request{
		Messages: []Message{UserText("run both")}, Tools: []ToolSpec{tool.Spec()},
		ParallelToolCalls: true, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	drained := make(chan error, 1)
	go func() {
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				drained <- nil
				return
			}
			if err != nil {
				drained <- err
				return
			}
		}
	}()

	started := map[string]bool{}
	for range 2 {
		select {
		case id := <-tool.started:
			started[id] = true
		case <-ctx.Done():
			t.Fatalf("only started %v: %v", started, ctx.Err())
		}
	}
	if !started["toolu_1"] || !started["toolu_2"] {
		t.Fatalf("started = %v", started)
	}
	close(tool.release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.results) != 2 || provider.results[0].Result.Content != "done:toolu_1" || provider.results[1].Result.Content != "done:toolu_2" {
		t.Fatalf("bridge results = %#v", provider.results)
	}
}
