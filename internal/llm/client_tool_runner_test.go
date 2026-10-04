package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// testClientToolRunner owns the tools in run; each answers a call.
type testClientToolRunner struct {
	run map[string]func(context.Context, ToolCall) (ToolOutput, error)
}

func (r testClientToolRunner) OwnsClientTool(name string) bool { return r.run[name] != nil }

func (r testClientToolRunner) RunClientTool(ctx context.Context, call ToolCall) (ToolOutput, error) {
	return r.run[call.Name](ctx, call)
}

var inlineCapabilities = Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}

func clientToolCall(id, name string) ToolCall {
	return ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{"message":"hi"}`)}
}

// drainEngineStream reads a stream to its end and returns the last tool exec end.
func drainEngineStream(t *testing.T, stream Stream) *Event {
	t.Helper()
	var execEnd *Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return execEnd
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventToolExecEnd {
			execEnd = &event
		}
	}
}

// An inline-loop provider blocks on every tool call, so a call to a declared
// client tool must be answered while it waits rather than passed through.
func TestEngineAnswersInlineClientToolCallsThroughTheContextRunner(t *testing.T) {
	runner := testClientToolRunner{run: map[string]func(context.Context, ToolCall) (ToolOutput, error){
		"webmcp__ping": func(ctx context.Context, call ToolCall) (ToolOutput, error) {
			if CallIDFromContext(ctx) != call.ID {
				return ToolOutput{}, errors.New("call id missing from context")
			}
			return TextOutput("pong:" + string(call.Arguments)), nil
		},
		"webmcp__broken": func(context.Context, ToolCall) (ToolOutput, error) {
			return ToolOutput{}, errors.New("the device did not run its tools in time")
		},
		"webmcp__unoffered": func(context.Context, ToolCall) (ToolOutput, error) {
			return TextOutput("must not run"), nil
		},
	}}
	offered := []ToolSpec{
		{Name: "webmcp__ping", Schema: map[string]any{"type": "object"}},
		{Name: "webmcp__broken", Schema: map[string]any{"type": "object"}},
		{Name: "webmcp__other", Schema: map[string]any{"type": "object"}},
	}
	for _, testCase := range []struct {
		name      string
		runner    ClientToolRunner
		call      ToolCall
		wantText  string
		wantError string
	}{
		{name: "declared tool", runner: runner, call: clientToolCall("call_1", "webmcp__ping"), wantText: `pong:{"message":"hi"}`},
		{name: "runner failure", runner: runner, call: clientToolCall("call_2", "webmcp__broken"), wantError: "did not run its tools in time"},
		{name: "not the runner's tool", runner: runner, call: clientToolCall("call_3", "webmcp__other"), wantError: "tool not registered"},
		// A nested run inherits the runner but never offered the tool.
		{name: "owned but not offered by this request", runner: runner, call: clientToolCall("call_4", "webmcp__unoffered"), wantError: "tool not registered"},
		{name: "no runner", call: clientToolCall("call_5", "webmcp__ping"), wantError: "tool not registered"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			provider := NewMockProvider("inline").WithCapabilities(inlineCapabilities)
			provider.AddTurn(MockTurn{ToolCalls: []ToolCall{testCase.call}, InlineTools: true, InlineText: "done"})
			engine := NewEngine(provider, NewToolRegistry())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := engine.Stream(ContextWithClientToolRunner(ctx, testCase.runner), Request{
				Messages: []Message{UserText("ping my phone")}, Tools: offered, MaxTurns: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			execEnd := drainEngineStream(t, stream)
			result, ok := provider.InlineToolResult(testCase.call.ID)
			if !ok {
				t.Fatal("the inline call was never answered")
			}
			if testCase.wantError != "" {
				if result.Err == nil || !strings.Contains(result.Err.Error(), testCase.wantError) {
					t.Fatalf("result error = %v, want %q", result.Err, testCase.wantError)
				}
				if execEnd == nil || execEnd.ToolSuccess {
					t.Fatalf("tool exec end = %+v, want a failure", execEnd)
				}
				return
			}
			if result.Err != nil || result.Result.Content != testCase.wantText {
				t.Fatalf("result = %+v, want %q", result, testCase.wantText)
			}
			if execEnd == nil || !execEnd.ToolSuccess || execEnd.ToolOutput != testCase.wantText {
				t.Fatalf("tool exec end = %+v", execEnd)
			}
			if got := len(provider.RecordedRequests()); got != 1 {
				t.Fatalf("provider turns = %d, want 1", got)
			}
		})
	}
}

// A client tool runs like a registered one: it counts as an actual tool
// execution, which steering waits out before abandoning an attempt.
func TestEngineCountsInlineClientToolAsActiveExecution(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	runner := testClientToolRunner{run: map[string]func(context.Context, ToolCall) (ToolOutput, error){
		"webmcp__ping": func(ctx context.Context, _ ToolCall) (ToolOutput, error) {
			close(entered)
			select {
			case <-release:
				return TextOutput("pong"), nil
			case <-ctx.Done():
				return ToolOutput{}, ctx.Err()
			}
		},
	}}
	provider := NewMockProvider("inline").WithCapabilities(inlineCapabilities)
	provider.AddTurn(MockTurn{ToolCalls: []ToolCall{clientToolCall("call_1", "webmcp__ping")}, InlineTools: true})
	engine := NewEngine(provider, NewToolRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := engine.Stream(ContextWithClientToolRunner(ctx, runner), Request{
		Messages: []Message{UserText("ping")}, Tools: []ToolSpec{{Name: "webmcp__ping"}}, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	drained := make(chan *Event, 1)
	go func() { drained <- drainEngineStream(t, stream) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the runner was never called")
	}
	if got := engine.ActiveToolExecutions(); got != 1 {
		t.Fatalf("active tool executions while the device runs = %d, want 1", got)
	}
	close(release)
	<-drained
	if got := engine.ActiveToolExecutions(); got != 0 {
		t.Fatalf("active tool executions after = %d, want 0", got)
	}
}

// A provider that ends its turn on a call still passes the call through, even
// with a runner installed: the caller answers it in its next request.
func TestEngineLeavesNonInlineClientToolCallsForTheCaller(t *testing.T) {
	runner := testClientToolRunner{run: map[string]func(context.Context, ToolCall) (ToolOutput, error){
		"webmcp__ping": func(context.Context, ToolCall) (ToolOutput, error) {
			t.Error("the runner answered a passthrough call")
			return ToolOutput{}, nil
		},
	}}
	provider := NewMockProvider("passthrough")
	provider.AddTurn(MockTurn{ToolCalls: []ToolCall{clientToolCall("call_1", "webmcp__ping")}})
	engine := NewEngine(provider, NewToolRegistry())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := engine.Stream(ContextWithClientToolRunner(ctx, runner), Request{
		Messages: []Message{UserText("ping")}, Tools: []ToolSpec{{Name: "webmcp__ping"}}, MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if execEnd := drainEngineStream(t, stream); execEnd != nil {
		t.Fatalf("passthrough call was executed: %+v", execEnd)
	}
	if got := len(provider.RecordedRequests()); got != 1 {
		t.Fatalf("provider turns = %d, want 1", got)
	}
}
