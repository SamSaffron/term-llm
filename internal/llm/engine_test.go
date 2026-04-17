package llm

import (
	"context"
	"encoding/json"
	"errors"
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

type errAfterEventsStream struct {
	events []Event
	err    error
	index  int
}

func (s *errAfterEventsStream) Recv() (Event, error) {
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return Event{}, err
	}
	return Event{}, io.EOF
}

func (s *errAfterEventsStream) Close() error {
	return nil
}

type concurrentCloseStream struct {
	mu          sync.Mutex
	calls       int
	recvBlocked chan struct{}
	releaseEOF  chan struct{}
	closeOnce   sync.Once
}

func newConcurrentCloseStream() *concurrentCloseStream {
	return &concurrentCloseStream{
		recvBlocked: make(chan struct{}),
		releaseEOF:  make(chan struct{}),
	}
}

func (s *concurrentCloseStream) Recv() (Event, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()

	switch call {
	case 1:
		return Event{Type: EventTextDelta, Text: "hello"}, nil
	case 2:
		close(s.recvBlocked)
		<-s.releaseEOF
		return Event{}, io.EOF
	default:
		return Event{}, io.EOF
	}
}

func (s *concurrentCloseStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.releaseEOF)
	})
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

type streamProvider struct {
	stream          Stream
	capabilities    Capabilities
	hasCapabilities bool
}

func (p *streamProvider) Name() string {
	return "fake"
}

func (p *streamProvider) Credential() string {
	return "test"
}

func (p *streamProvider) Capabilities() Capabilities {
	if p.hasCapabilities {
		return p.capabilities
	}
	return Capabilities{ToolCalls: true}
}

func (p *streamProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return p.stream, nil
}

type signalRecvStream struct {
	recvCalled chan struct{}
	event      Event
	sent       bool
}

func (s *signalRecvStream) Recv() (Event, error) {
	if s.sent {
		return Event{}, io.EOF
	}
	s.sent = true
	close(s.recvCalled)
	return s.event, nil
}

func (s *signalRecvStream) Close() error {
	return nil
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

type signalTool struct {
	started chan struct{}
}

func (t *signalTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "signal_tool",
		Description: "Signals when execution starts",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *signalTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	return TextOutput("ok"), nil
}

func (t *signalTool) Preview(args json.RawMessage) string {
	return ""
}

type timeoutTool struct {
	calls int
}

func (t *timeoutTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "timeout_tool",
		Description: "Returns a timeout marker output",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *timeoutTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	t.calls++
	return ToolOutput{Content: "[Command timed out]\n\nexit_code: 0", TimedOut: true}, nil
}

func (t *timeoutTool) Preview(args json.RawMessage) string {
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

func TestEngineToolTimeoutOutputMarksToolEndNonSuccessButContinuesLoop(t *testing.T) {
	tool := &timeoutTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "timeout_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "continued after timeout"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:   []Message{UserText("run timeout tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var gotToolEnd bool
	var toolSuccess bool
	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		switch event.Type {
		case EventToolExecEnd:
			if event.ToolCallID == "call-1" {
				gotToolEnd = true
				toolSuccess = event.ToolSuccess
			}
		case EventTextDelta:
			text.WriteString(event.Text)
		case EventError:
			if event.Err != nil {
				t.Fatalf("unexpected event error: %v", event.Err)
			}
		}
	}

	if !gotToolEnd {
		t.Fatal("expected tool end event for timeout_tool")
	}
	if toolSuccess {
		t.Fatal("expected ToolSuccess=false for timeout output")
	}
	if text.String() != "continued after timeout" {
		t.Fatalf("expected loop to continue and produce final text, got %q", text.String())
	}
	if tool.calls != 1 {
		t.Fatalf("expected tool to run once, got %d", tool.calls)
	}
}

// contentWithTimeoutStringTool returns output that contains the timeout message
// in its content but does NOT set TimedOut. Simulates e.g. read_file on shell.go.
type contentWithTimeoutStringTool struct{}

func (t *contentWithTimeoutStringTool) Spec() ToolSpec {
	return ToolSpec{Name: "content_tool", Schema: map[string]any{"type": "object"}}
}
func (t *contentWithTimeoutStringTool) Execute(_ context.Context, _ json.RawMessage) (ToolOutput, error) {
	return TextOutput("here is some source code: [Command timed out] in a string literal"), nil
}
func (t *contentWithTimeoutStringTool) Preview(_ json.RawMessage) string { return "" }

func TestEngineToolWithTimeoutStringInContentIsNotMarkedFailed(t *testing.T) {
	tool := &contentWithTimeoutStringTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "content_tool", Arguments: json.RawMessage(`{}`)}},
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
		Messages:   []Message{UserText("run content tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}
	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var toolSuccess bool
	var gotToolEnd bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventToolExecEnd && event.ToolCallID == "call-1" {
			gotToolEnd = true
			toolSuccess = event.ToolSuccess
		}
	}
	if !gotToolEnd {
		t.Fatal("expected tool end event")
	}
	if !toolSuccess {
		t.Error("expected ToolSuccess=true: content containing timeout string should not be treated as timed out")
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

func TestEngineDisableExternalWebFetchWithNativeSearch(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&countingSearchTool{})
	registry.Register(NewReadURLTool())

	provider := &fakeProvider{
		hasCapabilities: true,
		capabilities: Capabilities{
			NativeWebSearch: true,
			NativeWebFetch:  false,
			ToolCalls:       true,
		},
		script: func(call int, req Request) []Event {
			return []Event{{Type: EventDone}}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:                []Message{UserText("search this")},
		Search:                  true,
		DisableExternalWebFetch: true,
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
	if !firstReq.Search {
		t.Fatalf("expected provider request Search=true with native search")
	}
	if hasToolNamed(firstReq.Tools, ReadURLToolName) {
		t.Fatalf("did not expect injected %s tool", ReadURLToolName)
	}
	if hasToolNamed(firstReq.Tools, WebSearchToolName) {
		t.Fatalf("did not expect injected %s tool", WebSearchToolName)
	}
}

func TestEngineDisableExternalWebFetchStillAllowsExternalSearch(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&countingSearchTool{})
	registry.Register(NewReadURLTool())

	provider := &fakeProvider{
		hasCapabilities: true,
		capabilities: Capabilities{
			NativeWebSearch: true,
			NativeWebFetch:  false,
			ToolCalls:       true,
		},
		script: func(call int, req Request) []Event {
			return []Event{{Type: EventDone}}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:                []Message{UserText("search this")},
		Search:                  true,
		ForceExternalSearch:     true,
		DisableExternalWebFetch: true,
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
	if !hasToolNamed(firstReq.Tools, WebSearchToolName) {
		t.Fatalf("expected injected %s tool", WebSearchToolName)
	}
	if hasToolNamed(firstReq.Tools, ReadURLToolName) {
		t.Fatalf("did not expect injected %s tool", ReadURLToolName)
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

type cancellableDelayTool struct {
	delay time.Duration
}

func (t *cancellableDelayTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "cancel_delay_tool",
		Description: "A tool that waits or exits on cancellation",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *cancellableDelayTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	timer := time.NewTimer(t.delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ToolOutput{}, ctx.Err()
	case <-timer.C:
		return TextOutput("done"), nil
	}
}

func (t *cancellableDelayTool) Preview(args json.RawMessage) string {
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

type finishingNamedTool struct {
	name string
}

func (t *finishingNamedTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: "finishing test tool"}
}

func (t *finishingNamedTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return TextOutput("done"), nil
}

func (t *finishingNamedTool) Preview(args json.RawMessage) string {
	return ""
}

func (t *finishingNamedTool) IsFinishingTool() bool {
	return true
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

func TestEngineStopsAfterAsyncFinishingTool(t *testing.T) {
	tool := &finishingNamedTool{name: "finish_now"}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "finish_now", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				t.Fatalf("provider should not be called after finishing tool (call=%d)", call)
				return nil
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:   []Message{UserText("finish")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
		MaxTurns:   4,
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
		t.Fatalf("provider calls = %d, want %d", len(provider.calls), 1)
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

func TestEnginePersistsToolInfoInAssistantMessage(t *testing.T) {
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

	var persisted Message
	engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg Message, metrics TurnMetrics) error {
		persisted = assistantMsg
		return nil
	})

	req := Request{
		Messages: []Message{UserText("generate image")},
		Tools:    []ToolSpec{tool.Spec()},
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

	if len(persisted.Parts) != 1 {
		t.Fatalf("expected persisted assistant message to contain 1 part, got %d", len(persisted.Parts))
	}
	if persisted.Parts[0].Type != PartToolCall || persisted.Parts[0].ToolCall == nil {
		t.Fatalf("expected persisted assistant message to contain a tool call part, got %+v", persisted.Parts[0])
	}
	if got := persisted.Parts[0].ToolCall.ToolInfo; got != "(test.png)" {
		t.Fatalf("persisted tool info = %q, want %q", got, "(test.png)")
	}

	if len(provider.calls) < 2 {
		t.Fatalf("provider calls = %d, want at least 2", len(provider.calls))
	}
	found := false
	for _, msg := range provider.calls[1].Messages {
		if msg.Role != RoleAssistant {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type == PartToolCall && part.ToolCall != nil && part.ToolCall.ID == "img-1" {
				found = true
				if got := part.ToolCall.ToolInfo; got != "(test.png)" {
					t.Fatalf("tool info in next request = %q, want %q", got, "(test.png)")
				}
			}
		}
	}
	if !found {
		t.Fatal("expected assistant tool call with persisted tool info in next request")
	}
}

func TestRunLoopPersistsPartialAssistantMessageOnStreamRecvError(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &streamProvider{
		stream: &errAfterEventsStream{
			events: []Event{{Type: EventTextDelta, Text: "partial"}},
			err:    errors.New("stream recv failed"),
		},
	}

	engine := NewEngine(provider, registry)

	var (
		callbackCount int
		persisted     []Message
	)
	engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, messages []Message, metrics TurnMetrics) error {
		callbackCount++
		persisted = append([]Message(nil), messages...)
		return nil
	})

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var streamErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventError && event.Err != nil {
			streamErr = event.Err
			break
		}
	}

	if streamErr == nil || !strings.Contains(streamErr.Error(), "stream recv failed") {
		t.Fatalf("expected stream error to contain recv failure, got %v", streamErr)
	}
	if callbackCount != 1 {
		t.Fatalf("turn callback count = %d, want 1", callbackCount)
	}
	if len(persisted) != 1 || persisted[0].Role != RoleAssistant {
		t.Fatalf("persisted messages = %#v, want single assistant message", persisted)
	}
	if len(persisted[0].Parts) != 1 || persisted[0].Parts[0].Type != PartText {
		t.Fatalf("persisted parts = %#v, want single text part", persisted[0].Parts)
	}
	if persisted[0].Parts[0].Text != "partial" {
		t.Fatalf("persisted text = %q, want %q", persisted[0].Parts[0].Text, "partial")
	}
}

func TestRunLoopPersistsPartialAssistantMessageOnEventErrorAfterToolCall(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "partial"},
				{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
				{Type: EventError, Err: errors.New("provider stream failed")},
			}
		},
	}

	engine := NewEngine(provider, registry)

	var (
		responseCount int
		turnCount     int
		persisted     Message
	)
	engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg Message, metrics TurnMetrics) error {
		responseCount++
		persisted = assistantMsg
		return nil
	})
	engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, messages []Message, metrics TurnMetrics) error {
		turnCount++
		return nil
	})

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("test")},
		Tools:    []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var streamErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventError && event.Err != nil {
			streamErr = event.Err
			break
		}
	}

	if streamErr == nil || !strings.Contains(streamErr.Error(), "provider stream failed") {
		t.Fatalf("expected stream error to contain provider failure, got %v", streamErr)
	}
	if responseCount != 1 {
		t.Fatalf("response callback count = %d, want 1", responseCount)
	}
	if turnCount != 0 {
		t.Fatalf("turn callback count = %d, want 0", turnCount)
	}
	if len(persisted.Parts) != 2 {
		t.Fatalf("persisted part count = %d, want 2", len(persisted.Parts))
	}
	if persisted.Parts[0].Type != PartText || persisted.Parts[0].Text != "partial" {
		t.Fatalf("persisted text part = %#v, want partial text", persisted.Parts[0])
	}
	if persisted.Parts[1].Type != PartToolCall || persisted.Parts[1].ToolCall == nil {
		t.Fatalf("persisted tool part = %#v, want tool call", persisted.Parts[1])
	}
	if persisted.Parts[1].ToolCall.ID != "call-1" {
		t.Fatalf("persisted tool call ID = %q, want %q", persisted.Parts[1].ToolCall.ID, "call-1")
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

func TestRunLoopDoesNotDeadlockOnBlockedToolCallForwardingWhenCancelled(t *testing.T) {
	stream := &signalRecvStream{
		recvCalled: make(chan struct{}),
		event: Event{
			Type: EventToolCall,
			Tool: &ToolCall{
				Name:      "count_tool",
				Arguments: json.RawMessage(`{"input":"test"}`),
			},
		},
	}
	engine := NewEngine(&streamProvider{stream: stream}, NewToolRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan Event, 1)
	events <- Event{Type: EventPhase, Text: "buffer-full"}

	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.runLoop(ctx, Request{
			Messages: []Message{UserText("test")},
		}, eventSender{ctx: ctx, ch: events})
	}()

	select {
	case <-stream.recvCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tool call event")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLoop remained blocked after cancellation")
	}
}

func TestRunLoopDoesNotDeadlockOnBlockedToolExecStartWhenCancelled(t *testing.T) {
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
				{Type: EventDone},
			}
		},
	}

	engine := NewEngine(provider, registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan Event, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- engine.runLoop(ctx, Request{
			Messages: []Message{UserText("test")},
			Tools:    []ToolSpec{tool.Spec()},
		}, eventSender{ctx: ctx, ch: events})
	}()

	deadline := time.After(5 * time.Second)
	for len(events) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for tool call event")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLoop remained blocked after cancellation")
	}

	if tool.calls != 0 {
		t.Fatalf("expected tool not to execute once cancellation unblocked the start event, got %d calls", tool.calls)
	}
}

func TestExecuteSingleToolCallDoesNotDeadlockOnBlockedToolExecEndWhenCancelled(t *testing.T) {
	tool := &signalTool{started: make(chan struct{})}
	registry := NewToolRegistry()
	registry.Register(tool)

	engine := NewEngine(&fakeProvider{}, registry)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := make(chan Event, 1)
	events <- Event{Type: EventToolCall, ToolCallID: "buffer-full"}

	type result struct {
		msgs []Message
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		msgs, err := engine.executeSingleToolCall(ctx, ToolCall{
			ID:        "call-1",
			Name:      "signal_tool",
			Arguments: json.RawMessage(`{}`),
		}, eventSender{ctx: ctx, ch: events}, false, false)
		resultCh <- result{msgs: msgs, err: err}
	}()

	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	cancel()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("expected nil error, got %v", res.err)
		}
		if len(res.msgs) != 1 {
			t.Fatalf("expected 1 tool result message, got %d", len(res.msgs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executeSingleToolCall remained blocked after cancellation")
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
	e.contextNoticeEmitted.Store(true)
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
	if e.contextNoticeEmitted.Load() {
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

func TestEngineResetConversationCallsWrappedProvider(t *testing.T) {
	inner := &mockResettableProvider{MockProvider: NewMockProvider("test")}
	provider := WrapWithRetry(inner, DefaultRetryConfig())
	e := NewEngine(provider, nil)

	e.ResetConversation()

	if inner.resetCalls != 1 {
		t.Errorf("expected wrapped provider ResetConversation called once, got %d", inner.resetCalls)
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

// TestLastTotalTokensNoDoubleCounting verifies that high cache hit rates do not
// inflate lastTotalTokens by double-counting cached tokens.
//
// This is the bug that caused spurious compaction: for OpenAI-family providers,
// prompt_tokens INCLUDES cached tokens. Before the fix, providers set
// InputTokens=prompt_tokens (inclusive) and CachedInputTokens=cached_tokens, so
// the engine computed lastTotalTokens = prompt + cached + output, double-counting.
//
// After the fix, providers set InputTokens=(prompt-cached) so both fields are
// additive: lastTotalTokens = (prompt-cached) + cached + output = prompt + output.
// This matches the actual context size and prevents false compaction triggers.
func TestLastTotalTokensNoDoubleCounting(t *testing.T) {
	// Simulate a high-cache-hit turn: 127K total input, 127K cached (warm session).
	// Old (buggy) OpenAI provider would have set InputTokens=127431, CachedInputTokens=126976.
	// New (fixed) provider sets InputTokens=455, CachedInputTokens=126976.
	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			return []Event{
				{Type: EventTextDelta, Text: "hello"},
				{
					Type: EventUsage,
					Use: &Usage{
						InputTokens:       455,    // 127431 - 126976 (non-cached portion only)
						OutputTokens:      206,    // completion tokens
						CachedInputTokens: 126976, // from cache (additive, not a subset)
					},
				},
				{Type: EventDone},
			}
		},
	}
	e := NewEngine(provider, nil)
	e.inputLimit = 922_000 // gpt-5.4 input limit

	dummyTool := &countingSearchTool{}
	e.RegisterTool(dummyTool)

	stream, err := e.Stream(context.Background(), Request{
		Model:    "gpt-5.4-medium",
		Messages: []Message{UserText("hi")},
		Tools:    []ToolSpec{dummyTool.Spec()},
	})
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

	// lastTotalTokens should be 455 + 126976 + 206 = 127637 (≈ 127K)
	// NOT 127431 + 126976 + 206 = 254613 (the old double-counted value).
	// 127637 is well below the 830K threshold (922K * 0.9), so compaction
	// should NOT be triggered next turn.
	got := e.LastTotalTokens()
	wantApprox := 127637 // 455 + 126976 + 206
	if got != wantApprox {
		t.Errorf("LastTotalTokens() = %d, want %d (no double-counting of cached tokens)", got, wantApprox)
	}

	// Confirm the estimate is far below the compaction threshold
	threshold := int(float64(922_000) * 0.90)
	if got >= threshold {
		t.Errorf("LastTotalTokens() %d >= threshold %d — would falsely trigger compaction!", got, threshold)
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

func TestCallbackStream_CloseWhileDrainingFiresCallbackOnce(t *testing.T) {
	inner := newConcurrentCloseStream()

	var (
		mu       sync.Mutex
		calls    int
		messages []Message
	)

	stream := wrapCallbackStream(context.Background(), inner, func(ctx context.Context, turnIndex int, got []Message, metrics TurnMetrics) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		messages = append([]Message(nil), got...)
		return nil
	})

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("first recv error: %v", err)
	}
	if event.Type != EventTextDelta || event.Text != "hello" {
		t.Fatalf("first recv = %#v, want hello text delta", event)
	}

	recvDone := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		recvDone <- err
	}()

	select {
	case <-inner.recvBlocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recv to block")
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}

	select {
	case err := <-recvDone:
		if err != io.EOF {
			t.Fatalf("second recv error = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second recv to finish")
	}

	mu.Lock()
	defer mu.Unlock()

	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 1 || messages[0].Parts[0].Text != "hello" {
		t.Fatalf("callback messages = %#v, want single assistant hello message", messages)
	}
}

// TestEngineInterject_ConcurrentCallsDoNotBlock verifies that concurrent
// Interject calls remain non-blocking even when several goroutines race to
// replace the single pending interjection.
func TestEngineInterject_ConcurrentCallsDoNotBlock(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		engine := NewEngine(NewMockProvider("test"), nil)

		const goroutines = 32
		start := make(chan struct{})

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func(i int) {
				defer wg.Done()
				<-start
				engine.Interject(fmt.Sprintf("msg-%d", i))
			}(i)
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		close(start)

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("concurrent Interject calls blocked on attempt %d", attempt)
		}

		if text := engine.DrainInterjection(); text == "" {
			t.Fatalf("expected an interjection to remain queued on attempt %d", attempt)
		}
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

func TestEngineTurnCallbackContextSurvivesCancellation(t *testing.T) {
	tool := &cancellableDelayTool{delay: 200 * time.Millisecond}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := NewMockProvider("test").
		AddToolCall("call-1", "cancel_delay_tool", map[string]any{}).
		AddTextResponse("done")

	engine := NewEngine(provider, registry)

	var mu sync.Mutex
	var callbackCtxErr error
	var sawToolResult bool
	callbackDone := make(chan struct{}, 1)

	engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, messages []Message, metrics TurnMetrics) error {
		mu.Lock()
		defer mu.Unlock()

		if callbackCtxErr == nil {
			callbackCtxErr = ctx.Err()
		}

		for _, msg := range messages {
			for _, part := range msg.Parts {
				if part.Type == PartToolResult && part.ToolResult != nil && part.ToolResult.ID == "call-1" {
					sawToolResult = true
					select {
					case callbackDone <- struct{}{}:
					default:
					}
				}
			}
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := Request{
		Messages:   []Message{UserText("run tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	cancelIssued := false
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if !errors.Is(recvErr, context.Canceled) {
				t.Fatalf("recv error: %v", recvErr)
			}
			break
		}
		if event.Type == EventToolExecStart && !cancelIssued {
			cancelIssued = true
			cancel()
		}
	}

	if !cancelIssued {
		t.Fatal("expected tool execution to start before cancellation")
	}

	select {
	case <-callbackDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn callback with tool result")
	}

	mu.Lock()
	defer mu.Unlock()

	if !sawToolResult {
		t.Fatal("expected turn callback to include tool result after cancellation")
	}
	if callbackCtxErr != nil {
		t.Fatalf("expected callback context to be usable after cancellation, got: %v", callbackCtxErr)
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

// panickingTool always panics when executed.
type panickingTool struct{}

func (t *panickingTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "panic_tool",
		Description: "Always panics",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *panickingTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	panic("unexpected nil pointer")
}

func (t *panickingTool) Preview(args json.RawMessage) string {
	return ""
}

func TestEnginePanickingToolSingleCall(t *testing.T) {
	tool := &panickingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "panic_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "recovered"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:   []Message{UserText("run panicking tool")},
		Tools:      []ToolSpec{tool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventTextDelta {
			text.WriteString(event.Text)
		}
	}

	if text.String() != "recovered" {
		t.Fatalf("expected 'recovered', got %q", text.String())
	}

	// Verify the provider received the panic error as a tool result
	if len(provider.calls) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(provider.calls))
	}
	lastReq := provider.calls[1]
	for _, msg := range lastReq.Messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult && part.ToolResult != nil && part.ToolResult.IsError {
				if !strings.Contains(part.ToolResult.Content, "tool panicked") {
					t.Fatalf("expected panic error message, got: %s", part.ToolResult.Content)
				}
				return
			}
		}
	}
	t.Fatal("expected a tool error result containing 'tool panicked'")
}

func TestEnginePanickingToolParallelCalls(t *testing.T) {
	panicTool := &panickingTool{}
	countTool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(panicTool)
	registry.Register(countTool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-1", Name: "panic_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventToolCall, Tool: &ToolCall{ID: "call-2", Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					{Type: EventDone},
				}
			default:
				return []Event{
					{Type: EventTextDelta, Text: "both done"},
					{Type: EventDone},
				}
			}
		},
	}

	engine := NewEngine(provider, registry)
	req := Request{
		Messages:   []Message{UserText("run both tools")},
		Tools:      []ToolSpec{panicTool.Spec(), countTool.Spec()},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventTextDelta {
			text.WriteString(event.Text)
		}
	}

	if text.String() != "both done" {
		t.Fatalf("expected 'both done', got %q", text.String())
	}

	// The counting tool should still have executed
	if countTool.calls != 1 {
		t.Fatalf("expected counting tool to execute once, got %d", countTool.calls)
	}

	// Verify the provider received both tool results (one error, one success)
	if len(provider.calls) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(provider.calls))
	}
	lastReq := provider.calls[1]
	var panicResult, okResult bool
	for _, msg := range lastReq.Messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult && part.ToolResult != nil {
				if part.ToolResult.IsError && strings.Contains(part.ToolResult.Content, "tool panicked") {
					panicResult = true
				}
				if !part.ToolResult.IsError && part.ToolResult.Content == "ok" {
					okResult = true
				}
			}
		}
	}
	if !panicResult {
		t.Fatal("expected a tool error result containing 'tool panicked'")
	}
	if !okResult {
		t.Fatal("expected a successful tool result from count_tool")
	}
}

func TestEngineNormalizesToolCallID(t *testing.T) {
	// When a provider sets ToolCallID on the event but leaves Tool.ID empty,
	// the engine should copy ToolCallID into Tool.ID so downstream consumers
	// (serve handlers, API responses) get the correct ID.
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			switch call {
			case 0:
				return []Event{
					// Simulate a provider that sets ToolCallID but not Tool.ID
					{Type: EventToolCall, ToolCallID: "call-1", Tool: &ToolCall{Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
					// Also test the synthetic fallback: both ToolCallID and Tool.ID empty
					{Type: EventToolCall, Tool: &ToolCall{Name: "count_tool", Arguments: json.RawMessage(`{}`)}},
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

	var toolCallEvents []Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv error: %v", err)
		}
		if event.Type == EventToolCall && event.Tool != nil {
			toolCallEvents = append(toolCallEvents, event)
		}
	}

	if len(toolCallEvents) < 2 {
		t.Fatalf("expected at least 2 EventToolCall events, got %d", len(toolCallEvents))
	}

	// First tool call: ToolCallID was "call-1", Tool.ID should be normalized to "call-1"
	ev0 := toolCallEvents[0]
	if ev0.ToolCallID != "call-1" {
		t.Errorf("event[0].ToolCallID = %q, want %q", ev0.ToolCallID, "call-1")
	}
	if ev0.Tool.ID != "call-1" {
		t.Errorf("event[0].Tool.ID = %q, want %q", ev0.Tool.ID, "call-1")
	}

	// Second tool call: both were empty, should get a synthetic ID
	ev1 := toolCallEvents[1]
	if ev1.ToolCallID == "" {
		t.Error("event[1].ToolCallID should not be empty (should be synthetic)")
	}
	if ev1.Tool.ID == "" {
		t.Error("event[1].Tool.ID should not be empty (should be synthetic)")
	}
	if ev1.ToolCallID != ev1.Tool.ID {
		t.Errorf("event[1].ToolCallID (%q) != event[1].Tool.ID (%q)", ev1.ToolCallID, ev1.Tool.ID)
	}
}
