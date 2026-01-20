package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type sliceStream struct {
	events []Event
	index  int
}

func (s *sliceStream) Recv() (Event, error) {
	if s.index >= len(s.events) {
		return Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *sliceStream) Close() error {
	return nil
}

type fakeProvider struct {
	script func(call int, req Request) []Event
	calls  []Request
}

func (p *fakeProvider) Name() string {
	return "fake"
}

func (p *fakeProvider) Credential() string {
	return "test"
}

func (p *fakeProvider) Capabilities() Capabilities {
	return Capabilities{
		NativeWebSearch: false,
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

func (p *fakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.calls = append(p.calls, req)
	call := len(p.calls) - 1
	events := p.script(call, req)
	return &sliceStream{events: events}, nil
}

type countingSearchTool struct {
	calls int
}

func (t *countingSearchTool) Spec() ToolSpec {
	return WebSearchToolSpec()
}

func (t *countingSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	t.calls++
	return fmt.Sprintf("result %d", t.calls), nil
}

func (t *countingSearchTool) Preview(args json.RawMessage) string {
	return ""
}

func TestEngineExternalSearchLoopsUntilNoToolCalls(t *testing.T) {
	tool := &countingSearchTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig"}`)}},
					{Type: EventDone},
				}
			case 1:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-2", Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig release"}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "final answer"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("latest release")},
		Search:   true,
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var toolEvents int
	var gotErr error

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		switch event.Type {
		case EventError:
			gotErr = event.Err
		case EventToolCall:
			toolEvents++
		case EventTextDelta:
			text.WriteString(event.Text)
		}
	}

	if gotErr != nil {
		t.Fatalf("unexpected stream error: %v", gotErr)
	}
	if toolEvents != 0 {
		t.Fatalf("unexpected tool call events: %d", toolEvents)
	}
	if text.String() != "final answer" {
		t.Fatalf("unexpected text: %q", text.String())
	}
	if tool.calls != 2 {
		t.Fatalf("expected 2 tool calls, got %d", tool.calls)
	}
	if len(provider.calls) != 3 {
		t.Fatalf("expected 3 provider calls, got %d", len(provider.calls))
	}

	last := provider.calls[len(provider.calls)-1]
	if countToolResults(last.Messages) != 2 {
		t.Fatalf("expected 2 tool results in final request")
	}
	if countToolCalls(last.Messages) != 2 {
		t.Fatalf("expected 2 tool calls in final request")
	}
}

func TestEngineExternalSearchStopsAfterMaxLoops(t *testing.T) {
	tool := &countingSearchTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventToolCall, Tool: &ToolCall{ID: fmt.Sprintf("call-%d", call), Name: WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig"}`)}},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages: []Message{UserText("latest release")},
		Search:   true,
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var gotErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventError && event.Err != nil {
			gotErr = event.Err
		}
	}

	if gotErr == nil || !strings.Contains(gotErr.Error(), "exceeded max turns") {
		t.Fatalf("expected max turns error, got %v", gotErr)
	}

	// Without pre-emptive search, the loop runs exactly defaultMaxTurns times
	expectedCalls := defaultMaxTurns
	if len(provider.calls) != expectedCalls {
		t.Fatalf("expected %d provider calls, got %d", expectedCalls, len(provider.calls))
	}

	last := provider.calls[len(provider.calls)-1]
	if !hasSystemText(last.Messages, stopSearchToolHint) {
		t.Fatalf("expected stop hint in final request")
	}
}

func countToolResults(messages []Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult {
				count++
			}
		}
	}
	return count
}

func countToolCalls(messages []Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolCall {
				count++
			}
		}
	}
	return count
}

func hasSystemText(messages []Message, text string) bool {
	for _, msg := range messages {
		if msg.Role != RoleSystem {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type == PartText && strings.Contains(part.Text, text) {
				return true
			}
		}
	}
	return false
}

// delayingTool simulates a slow tool that takes a specified duration
type delayingTool struct {
	delay        time.Duration
	calls        int32
	startTimes   []time.Time
	endTimes     []time.Time
	mu           sync.Mutex
	concurrentAt int32 // Peak concurrent executions
	current      int32 // Current concurrent executions
}

func (t *delayingTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "delay_tool",
		Description: "A tool that delays",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *delayingTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	t.mu.Lock()
	t.current++
	if t.current > t.concurrentAt {
		t.concurrentAt = t.current
	}
	t.startTimes = append(t.startTimes, time.Now())
	t.calls++
	t.mu.Unlock()

	time.Sleep(t.delay)

	t.mu.Lock()
	t.endTimes = append(t.endTimes, time.Now())
	t.current--
	t.mu.Unlock()

	return "done", nil
}

func (t *delayingTool) Preview(args json.RawMessage) string {
	return ""
}

func TestEngineParallelToolExecution(t *testing.T) {
	// Create a tool that takes 100ms to execute
	tool := &delayingTool{delay: 100 * time.Millisecond}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				// Request 3 parallel tool calls
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "delay_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-2", Name: "delay_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-3", Name: "delay_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "done"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:          []Message{UserText("run tools")},
		Tools:             []ToolSpec{tool.Spec()},
		ParallelToolCalls: true,
		ToolChoice:        ToolChoice{Mode: ToolChoiceAuto},
	}

	start := time.Now()
	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventError && event.Err != nil {
			t.Fatalf("event error: %v", event.Err)
		}
	}
	elapsed := time.Since(start)

	// Verify all 3 tools were called
	if tool.calls != 3 {
		t.Errorf("expected 3 tool calls, got %d", tool.calls)
	}

	// Verify parallel execution: if sequential, it would take ~300ms
	// If parallel, it should take ~100ms (with some overhead)
	// We check that the peak concurrency was > 1
	if tool.concurrentAt < 2 {
		t.Errorf("expected concurrent execution (peak concurrent: %d), tools may have run sequentially", tool.concurrentAt)
	}

	// Also verify total time - should be significantly less than 3x delay
	// With true parallelism: ~100ms + overhead
	// Sequential would be: ~300ms
	// Use a generous threshold (500ms) to avoid flaky tests on slow CI systems
	maxExpected := 500 * time.Millisecond
	if elapsed > maxExpected {
		t.Errorf("parallel execution took too long: %v (max expected: %v)", elapsed, maxExpected)
	}

	t.Logf("Parallel execution: peak concurrent=%d, elapsed=%v", tool.concurrentAt, elapsed)
}

// namedTool is a simple tool with a configurable name for testing
type namedTool struct {
	name string
}

func (t *namedTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "test tool"}
}

func (t *namedTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return "ok", nil
}

func (t *namedTool) Preview(args json.RawMessage) string {
	return ""
}

// TestEngineAllowedToolsEnforcement verifies that the engine blocks tools not in the allowed list.
func TestEngineAllowedToolsEnforcement(t *testing.T) {
	// Create tools with different names
	toolA := &namedTool{name: "tool_a"}
	toolB := &namedTool{name: "tool_b"}

	// Create engine with both tools
	registry := NewToolRegistry()
	registry.Register(toolA)
	registry.Register(toolB)

	provider := &fakeProvider{}
	engine := NewEngine(provider, registry)

	// Test 1: No filter - both tools should work
	if !engine.IsToolAllowed("tool_a") {
		t.Error("tool_a should be allowed with no filter")
	}
	if !engine.IsToolAllowed("tool_b") {
		t.Error("tool_b should be allowed with no filter")
	}

	// Test 2: Set filter - only tool_a allowed
	engine.SetAllowedTools([]string{"tool_a"})

	if !engine.IsToolAllowed("tool_a") {
		t.Error("tool_a should be allowed when in filter")
	}
	if engine.IsToolAllowed("tool_b") {
		t.Error("tool_b should NOT be allowed when not in filter")
	}

	// Test 3: Try to set a tool that doesn't exist - should be ignored
	engine.SetAllowedTools([]string{"nonexistent_tool", "tool_a"})
	if !engine.IsToolAllowed("tool_a") {
		t.Error("tool_a should still be allowed")
	}
	// nonexistent_tool isn't registered, so the filter should only contain tool_a

	// Test 4: Clear filter - all tools allowed again
	engine.ClearAllowedTools()

	if !engine.IsToolAllowed("tool_a") {
		t.Error("tool_a should be allowed after clearing filter")
	}
	if !engine.IsToolAllowed("tool_b") {
		t.Error("tool_b should be allowed after clearing filter")
	}

	// Test 5: Empty slice clears filter
	engine.SetAllowedTools([]string{"tool_a"})
	engine.SetAllowedTools([]string{})

	if !engine.IsToolAllowed("tool_a") {
		t.Error("tool_a should be allowed after setting empty filter")
	}
	if !engine.IsToolAllowed("tool_b") {
		t.Error("tool_b should be allowed after setting empty filter")
	}
}
