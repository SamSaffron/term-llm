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
	script          func(call int, req Request) []Event
	calls           []Request
	capabilities    Capabilities
	hasCapabilities bool
}

func (p *fakeProvider) Name() string {
	return "fake"
}

func (p *fakeProvider) Credential() string {
	return "test"
}

func (p *fakeProvider) Capabilities() Capabilities {
	if p.hasCapabilities {
		return p.capabilities
	}
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

func (t *countingSearchTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	t.calls++
	return TextOutput(fmt.Sprintf("result %d", t.calls)), nil
}

func (t *countingSearchTool) Preview(args json.RawMessage) string {
	return ""
}

type countingTool struct {
	calls int
}

func (t *countingTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "count_tool",
		Description: "Counts executions",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *countingTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	t.calls++
	return TextOutput("ok"), nil
}

func (t *countingTool) Preview(args json.RawMessage) string {
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
	// EventToolCall events are now emitted for all tool calls to preserve interleaving order
	if toolEvents != 2 {
		t.Fatalf("expected 2 tool call events, got %d", toolEvents)
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

func TestEngineDedupesToolCallsByID(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages:   []Message{UserText("run tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

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

	if tool.calls != 1 {
		t.Fatalf("expected 1 tool execution, got %d", tool.calls)
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

func TestEngineForceExternalSearchDisablesNativeProviderSearch(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&countingSearchTool{})

	provider := &fakeProvider{
		hasCapabilities: true,
		capabilities: Capabilities{
			NativeWebSearch: true,
			NativeWebFetch:  false,
			ToolCalls:       true,
		},
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "ok"},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:            []Message{UserText("search this")},
		Search:              true,
		ForceExternalSearch: true,
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
	}

	if len(provider.calls) != 1 {
		t.Fatalf("expected 1 provider call, got %d", len(provider.calls))
	}

	firstReq := provider.calls[0]
	if firstReq.Search {
		t.Fatalf("expected provider request Search=false when force_external is true")
	}

	hasExternalSearchTool := false
	for _, spec := range firstReq.Tools {
		if spec.Name == WebSearchToolName {
			hasExternalSearchTool = true
			break
		}
	}
	if !hasExternalSearchTool {
		t.Fatalf("expected injected external web_search tool")
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

func (t *delayingTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
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

	return TextOutput("done"), nil
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

func (t *namedTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return TextOutput("ok"), nil
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

func TestBuildAssistantMessage_WithReasoning(t *testing.T) {
	// Test building assistant message with reasoning content
	toolCalls := []ToolCall{
		{
			ID:        "call-123",
			Name:      "list_files",
			Arguments: json.RawMessage(`{"path": "."}`),
		},
	}

	msg := buildAssistantMessage("Here are the files", toolCalls, "I should list the files in the current directory")

	if msg.Role != RoleAssistant {
		t.Errorf("expected role RoleAssistant, got %v", msg.Role)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(msg.Parts))
	}

	// First part should be text with reasoning
	textPart := msg.Parts[0]
	if textPart.Type != PartText {
		t.Errorf("expected first part type PartText, got %v", textPart.Type)
	}
	if textPart.Text != "Here are the files" {
		t.Errorf("expected text 'Here are the files', got %q", textPart.Text)
	}
	if textPart.ReasoningContent != "I should list the files in the current directory" {
		t.Errorf("expected reasoning content, got %q", textPart.ReasoningContent)
	}

	// Second part should be tool call
	toolPart := msg.Parts[1]
	if toolPart.Type != PartToolCall {
		t.Errorf("expected second part type PartToolCall, got %v", toolPart.Type)
	}
	if toolPart.ToolCall.ID != "call-123" {
		t.Errorf("expected tool call ID 'call-123', got %q", toolPart.ToolCall.ID)
	}
}

func TestBuildAssistantMessage_ReasoningOnlyCreatesTextPart(t *testing.T) {
	// Test that reasoning alone (without text) still creates a text part
	msg := buildAssistantMessage("", nil, "Some reasoning content")

	if len(msg.Parts) != 1 {
		t.Fatalf("expected 1 part for reasoning-only message, got %d", len(msg.Parts))
	}

	part := msg.Parts[0]
	if part.Type != PartText {
		t.Errorf("expected part type PartText, got %v", part.Type)
	}
	if part.Text != "" {
		t.Errorf("expected empty text, got %q", part.Text)
	}
	if part.ReasoningContent != "Some reasoning content" {
		t.Errorf("expected reasoning content, got %q", part.ReasoningContent)
	}
}

func TestEngineEmitsToolCallAndExecStartForEachTool(t *testing.T) {
	// This test verifies that the engine emits both EventToolCall (during streaming)
	// and EventToolExecStart (at execution time) for each tool, with matching IDs.
	// The UI layer should deduplicate these.
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-A", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-B", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	toolCallEvents := make(map[string]int) // ID -> count
	toolExecStartEvents := make(map[string]int)
	toolExecEndEvents := make(map[string]int)

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		switch event.Type {
		case EventToolCall:
			toolCallEvents[event.ToolCallID]++
		case EventToolExecStart:
			toolExecStartEvents[event.ToolCallID]++
		case EventToolExecEnd:
			toolExecEndEvents[event.ToolCallID]++
		}
	}

	// Should have 2 unique tools (call-A and call-B)
	if len(toolCallEvents) != 2 {
		t.Errorf("expected 2 unique EventToolCall IDs, got %d: %v", len(toolCallEvents), toolCallEvents)
	}
	if len(toolExecStartEvents) != 2 {
		t.Errorf("expected 2 unique EventToolExecStart IDs, got %d: %v", len(toolExecStartEvents), toolExecStartEvents)
	}

	// Each ID should appear exactly once in each event type
	for id, count := range toolCallEvents {
		if count != 1 {
			t.Errorf("EventToolCall ID %q appeared %d times, expected 1", id, count)
		}
	}
	for id, count := range toolExecStartEvents {
		if count != 1 {
			t.Errorf("EventToolExecStart ID %q appeared %d times, expected 1", id, count)
		}
	}

	// The IDs from EventToolCall should match those from EventToolExecStart
	for id := range toolCallEvents {
		if _, ok := toolExecStartEvents[id]; !ok {
			t.Errorf("EventToolCall ID %q has no matching EventToolExecStart", id)
		}
	}
}

// imageTool returns structured ToolOutput with Images and Diffs.
type imageTool struct{}

func (t *imageTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "image_tool",
		Description: "Returns images and diffs",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *imageTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return ToolOutput{
		Content: "Generated image",
		Images:  []string{"/tmp/test.png"},
		Diffs: []DiffData{
			{File: "test.go", Old: "old", New: "new", Line: 10},
		},
	}, nil
}

func (t *imageTool) Preview(args json.RawMessage) string {
	return "test.png"
}

func TestEngineToolOutputStructuredFields(t *testing.T) {
	// Verify that structured ToolOutput fields (Images, Diffs) propagate
	// through to EventToolExecEnd events and ToolResult messages.
	tool := &imageTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "img-1", Name: "image_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages: []Message{UserText("generate image")},
		Tools:    []ToolSpec{tool.Spec()},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var endEvents []Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventToolExecEnd {
			endEvents = append(endEvents, event)
		}
	}

	if len(endEvents) != 1 {
		t.Fatalf("expected 1 EventToolExecEnd, got %d", len(endEvents))
	}

	end := endEvents[0]
	if !end.ToolSuccess {
		t.Error("expected ToolSuccess=true")
	}
	if end.ToolOutput != "Generated image" {
		t.Errorf("expected ToolOutput 'Generated image', got %q", end.ToolOutput)
	}
	if len(end.ToolImages) != 1 || end.ToolImages[0] != "/tmp/test.png" {
		t.Errorf("expected ToolImages=[/tmp/test.png], got %v", end.ToolImages)
	}
	if len(end.ToolDiffs) != 1 {
		t.Fatalf("expected 1 ToolDiff, got %d", len(end.ToolDiffs))
	}
	d := end.ToolDiffs[0]
	if d.File != "test.go" || d.Old != "old" || d.New != "new" || d.Line != 10 {
		t.Errorf("unexpected diff data: %+v", d)
	}

	// Verify the provider received the tool result with structured fields
	// in the second call's messages
	if len(provider.calls) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(provider.calls))
	}
	msgs := provider.calls[1].Messages
	var toolResult *ToolResult
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult && part.ToolResult != nil && part.ToolResult.ID == "img-1" {
				toolResult = part.ToolResult
			}
		}
	}
	if toolResult == nil {
		t.Fatal("tool result not found in second provider call")
	}
	if toolResult.Content != "Generated image" {
		t.Errorf("expected tool result content 'Generated image', got %q", toolResult.Content)
	}
	if len(toolResult.Diffs) != 1 {
		t.Errorf("expected 1 diff in tool result, got %d", len(toolResult.Diffs))
	}
	if len(toolResult.Images) != 1 {
		t.Errorf("expected 1 image in tool result, got %d", len(toolResult.Images))
	}
}

// mockResettableProvider wraps MockProvider and tracks ResetConversation calls.
type mockResettableProvider struct {
	*MockProvider
	resetCalls int
}

func (m *mockResettableProvider) ResetConversation() {
	m.resetCalls++
}

func TestEngineResetConversation(t *testing.T) {
	provider := NewMockProvider("test")
	e := NewEngine(provider, nil)

	// Simulate some conversation state
	e.callbackMu.Lock()
	e.lastTotalTokens = 500
	e.lastMessageCount = 10
	e.systemPrompt = "You are a helpful assistant."
	e.contextNoticeEmitted = true
	e.callbackMu.Unlock()

	// Reset conversation
	e.ResetConversation()

	// Verify all engine state is cleared
	e.callbackMu.RLock()
	if e.lastTotalTokens != 0 {
		t.Errorf("expected lastTotalTokens=0, got %d", e.lastTotalTokens)
	}
	if e.lastMessageCount != 0 {
		t.Errorf("expected lastMessageCount=0, got %d", e.lastMessageCount)
	}
	if e.systemPrompt != "" {
		t.Errorf("expected systemPrompt=\"\", got %q", e.systemPrompt)
	}
	if e.contextNoticeEmitted {
		t.Error("expected contextNoticeEmitted=false")
	}
	e.callbackMu.RUnlock()
}

func TestEngineResetConversationCallsProvider(t *testing.T) {
	inner := NewMockProvider("test")
	provider := &mockResettableProvider{MockProvider: inner}
	e := NewEngine(provider, nil)

	e.ResetConversation()

	if provider.resetCalls != 1 {
		t.Errorf("expected provider ResetConversation called once, got %d", provider.resetCalls)
	}
}

func TestEngineResetConversationSkipsNonResettableProvider(t *testing.T) {
	// Regular MockProvider doesn't implement ResetConversation
	provider := NewMockProvider("test")
	e := NewEngine(provider, nil)

	// Should not panic
	e.ResetConversation()
}

func TestLastTotalTokensIncludesCachedTokens(t *testing.T) {
	// Provider returns a text response with usage that has cached tokens.
	// Uses a tool spec so the engine enters the agentic runLoop path
	// where lastTotalTokens is updated.
	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "hello"},
				{
					Type: EventUsage,
					Use: &Usage{
						InputTokens:       1000,  // new (uncached) input tokens
						OutputTokens:      500,   // output tokens
						CachedInputTokens: 50000, // tokens read from cache
						CacheWriteTokens:  200,   // tokens written to cache (subset of input, not additive)
					},
				},
				{Type: EventDone},
			}
		},
	}
	e := NewEngine(provider, nil)
	e.inputLimit = 200000 // enable token tracking

	// Provide a dummy tool spec so the engine enters the agentic loop
	dummyTool := &countingSearchTool{}
	e.RegisterTool(dummyTool)

	req := Request{
		Model:    "test-model",
		Messages: []Message{UserText("hi")},
		Tools:    []ToolSpec{dummyTool.Spec()},
	}

	stream, err := e.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error: %v", err)
		}
	}

	// lastTotalTokens should include cached + new input + output
	// = 50000 + 1000 + 500 = 51500
	// cache_write tokens are NOT additive (they're a subset of input tokens)
	got := e.LastTotalTokens()
	want := 51500
	if got != want {
		t.Errorf("LastTotalTokens() = %d, want %d (cached 50000 + input 1000 + output 500; cache_write excluded)", got, want)
	}
}

// --- Interjection tests ---

// TestEngineInterjection_Basic verifies that a user interjection queued during
// tool execution appears in the conversation as a user message after tool results.
func TestEngineInterjection_Basic(t *testing.T) {
	tool := &delayingTool{delay: 50 * time.Millisecond}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "delay_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages:   []Message{UserText("do something")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	// Queue the interjection while the stream is in progress
	engine.Interject("stop doing that")

	var text strings.Builder
	var gotInterjection bool
	var interjectionText string

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
			if event.Err != nil {
				t.Fatalf("event error: %v", event.Err)
			}
		case EventTextDelta:
			text.WriteString(event.Text)
		case EventInterjection:
			gotInterjection = true
			interjectionText = event.Text
		}
	}

	if !gotInterjection {
		t.Fatal("expected EventInterjection to be emitted")
	}
	if interjectionText != "stop doing that" {
		t.Fatalf("expected interjection text %q, got %q", "stop doing that", interjectionText)
	}
	if text.String() != "final answer" {
		t.Fatalf("expected final text %q, got %q", "final answer", text.String())
	}

	// Verify the LLM saw the interjection on the second call:
	// Messages should be: [user] + [assistant+tool_call] + [tool_result] + [user interjection]
	if len(provider.calls) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(provider.calls))
	}
	secondCall := provider.calls[1]
	lastMsg := secondCall.Messages[len(secondCall.Messages)-1]
	if lastMsg.Role != RoleUser {
		t.Fatalf("expected last message in second call to be user role, got %v", lastMsg.Role)
	}
	if len(lastMsg.Parts) == 0 || lastMsg.Parts[0].Text != "stop doing that" {
		t.Fatalf("expected last message text to be interjection, got %v", lastMsg.Parts)
	}
}

// TestEngineInterjection_NoToolCalls verifies that an interjection stays in the
// channel when the LLM returns no tool calls (text-only response).
func TestEngineInterjection_NoToolCalls(t *testing.T) {
	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "just text"},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, nil)
	req := Request{
		Messages: []Message{UserText("hello")},
	}

	// Queue interjection before streaming
	engine.Interject("change of plan")

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var gotInterjection bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventInterjection {
			gotInterjection = true
		}
	}

	// Interjection should NOT have been emitted (no tool execution path)
	if gotInterjection {
		t.Fatal("interjection should not be emitted when there are no tool calls")
	}

	// The interjection should still be pending in the channel
	residual := engine.DrainInterjection()
	if residual != "change of plan" {
		t.Fatalf("expected pending interjection %q, got %q", "change of plan", residual)
	}
}

// TestEngineInterjection_MultipleInterjections verifies that sending multiple
// interjections before a turn completes only keeps the latest one.
func TestEngineInterjection_MultipleInterjections(t *testing.T) {
	tool := &delayingTool{delay: 100 * time.Millisecond}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "delay_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages:   []Message{UserText("run tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	// Queue two interjections rapidly — only the latest should be kept
	engine.Interject("first attempt")
	engine.Interject("second attempt")

	var interjectionText string
	var interjectionCount int

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventInterjection {
			interjectionCount++
			interjectionText = event.Text
		}
		if event.Type == EventError && event.Err != nil {
			t.Fatalf("event error: %v", event.Err)
		}
	}

	if interjectionCount != 1 {
		t.Fatalf("expected exactly 1 interjection event, got %d", interjectionCount)
	}
	if interjectionText != "second attempt" {
		t.Fatalf("expected latest interjection %q, got %q", "second attempt", interjectionText)
	}
}

// TestEngineInterjection_DrainOnNoPending verifies that drainInterjection
// returns "" when nothing is queued and that DrainInterjection is safe
// to call before any Interject().
func TestEngineInterjection_DrainOnNoPending(t *testing.T) {
	engine := NewEngine(NewMockProvider("test"), nil)

	// Before any Interject: channel is nil, should return ""
	if text := engine.DrainInterjection(); text != "" {
		t.Fatalf("expected empty string, got %q", text)
	}

	// After Interject + Drain: channel exists but is empty, should return ""
	engine.Interject("test")
	_ = engine.DrainInterjection()
	if text := engine.DrainInterjection(); text != "" {
		t.Fatalf("expected empty string after drain, got %q", text)
	}
}

// TestEngineInterjection_TurnCallback verifies that the turn callback receives
// the interjected user message for session persistence.
func TestEngineInterjection_TurnCallback(t *testing.T) {
	tool := &delayingTool{delay: 50 * time.Millisecond}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "delay_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "ok"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)

	var callbackMsgs [][]Message
	var mu sync.Mutex
	engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, messages []Message, metrics TurnMetrics) error {
		mu.Lock()
		defer mu.Unlock()
		// Deep copy messages for test verification
		copied := make([]Message, len(messages))
		copy(copied, messages)
		callbackMsgs = append(callbackMsgs, copied)
		return nil
	})

	req := Request{
		Messages:   []Message{UserText("run tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	// Queue interjection
	engine.Interject("hey wait")

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

	mu.Lock()
	defer mu.Unlock()

	// We expect at least 2 callback invocations for the first turn:
	//   1. Tool results (from regular turn completion)
	//   2. Interjection user message
	// Plus potentially a 3rd for the final text-only response.
	foundInterjection := false
	for _, msgs := range callbackMsgs {
		for _, msg := range msgs {
			if msg.Role == RoleUser && len(msg.Parts) > 0 && msg.Parts[0].Text == "hey wait" {
				foundInterjection = true
			}
		}
	}
	if !foundInterjection {
		t.Fatal("expected turn callback to receive interjection user message")
	}
}

// TestEngineInterjection_EventEmitted verifies that EventInterjection events
// are properly emitted through the stream with the correct text.
func TestEngineInterjection_EventEmitted(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages:   []Message{UserText("run tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	engine.Interject("redirect please")

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var events []Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		events = append(events, event)
	}

	// Find the interjection event
	var found bool
	for _, ev := range events {
		if ev.Type == EventInterjection {
			found = true
			if ev.Text != "redirect please" {
				t.Fatalf("expected interjection text %q, got %q", "redirect please", ev.Text)
			}
		}
	}
	if !found {
		t.Fatal("EventInterjection not found in event stream")
	}
}
