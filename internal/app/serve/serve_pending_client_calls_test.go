package serve

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

func pendingClientCallIDs(calls []core.ToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.ID)
	}
	return ids
}

func runPendingClientCallScenario(t *testing.T, turns ...mockprovider.MockTurn) serveRunResult {
	t.Helper()
	provider := mockprovider.NewMockProvider("mock")
	for _, turn := range turns {
		provider.AddTurn(turn)
	}
	tool := &serveRuntimeTestTool{}
	registry := engine.NewToolRegistry()
	registry.Register(tool)
	engine := engine.NewEngine(provider, registry)
	rt := &serveRuntime{provider: provider, providerKey: "mock", engine: engine, defaultModel: "mock-model"}
	clientTool := core.ToolSpec{Name: "webmcp__ping", Description: "client tool", Schema: map[string]any{"type": "object"}}
	result, err := rt.Run(context.Background(), false, false, []core.Message{serveRuntimeTextMessage(core.RoleUser, "go")}, core.Request{
		Model:      "mock-model",
		Tools:      []core.ToolSpec{tool.Spec(), clientTool},
		ToolChoice: core.ToolChoice{Mode: core.ToolChoiceAuto},
		MaxTurns:   6,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return result
}

// inlineThenPassthroughProvider runs a server tool inline (with a
// ToolResponse channel, as CLI-bridge providers do) and then, in the same
// provider turn, asks for a passthrough client call.
type inlineThenPassthroughProvider struct{ calls int }

type pendingTestStream struct{ events chan core.Event }

func (s *pendingTestStream) Recv() (core.Event, error) {
	event, ok := <-s.events
	if !ok {
		return core.Event{}, io.EOF
	}
	return event, nil
}
func (s *pendingTestStream) Close() error { return nil }

func (p *inlineThenPassthroughProvider) Name() string       { return "inline-then-passthrough" }
func (p *inlineThenPassthroughProvider) Credential() string { return "test" }
func (p *inlineThenPassthroughProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true}
}
func (p *inlineThenPassthroughProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.calls++
	stream := &pendingTestStream{events: make(chan core.Event)}
	first := p.calls == 1
	go func() {
		defer close(stream.events)
		send := func(event core.Event) bool {
			select {
			case stream.events <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !first {
			_ = send(core.Event{Type: core.EventTextDelta, Text: "unexpected extra turn"})
			return
		}
		response := make(chan core.ToolExecutionResponse, 1)
		server := pendingTestCall("call_inline", "serve_runtime_test_tool")
		if !send(core.Event{Type: core.EventToolCall, ToolCallID: server.ID, ToolName: server.Name, Tool: &server, ToolResponse: response}) {
			return
		}
		select {
		case <-response:
		case <-ctx.Done():
			return
		}
		client := pendingTestCall("call_c1", "webmcp__ping")
		if !send(core.Event{Type: core.EventToolCall, ToolCallID: client.ID, ToolName: client.Name, Tool: &client}) {
			return
		}
		_ = send(core.Event{Type: core.EventDone})
	}()
	return stream, nil
}

// An inline server call never enters the engine's end-of-turn split, so a
// passthrough call later in the same turn still stops the run.
func TestPendingClientCallsSurviveInlineServerToolInSameTurn(t *testing.T) {
	provider := &inlineThenPassthroughProvider{}
	tool := &serveRuntimeTestTool{}
	registry := engine.NewToolRegistry()
	registry.Register(tool)
	engine := engine.NewEngine(provider, registry)
	rt := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: engine, defaultModel: "mock-model"}
	clientTool := core.ToolSpec{Name: "webmcp__ping", Description: "client tool", Schema: map[string]any{"type": "object"}}
	result, err := rt.Run(context.Background(), false, false, []core.Message{serveRuntimeTextMessage(core.RoleUser, "go")}, core.Request{
		Model:      "mock-model",
		Tools:      []core.ToolSpec{tool.Spec(), clientTool},
		ToolChoice: core.ToolChoice{Mode: core.ToolChoiceAuto},
		MaxTurns:   4,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want the run to stop on the passthrough call", provider.calls)
	}
	if got, want := pendingClientCallIDs(result.PendingClientCalls()), []string{"call_c1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending client calls = %v, want %v", got, want)
	}
}

func pendingTestCall(id, name string) core.ToolCall {
	return core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{"message":"hi"}`)}
}

// Session 5319: tool-only server turns followed by client calls must still
// report the client calls, even though no text separates the turns.
func TestPendingClientCallsFollowToolOnlyServerTurns(t *testing.T) {
	result := runPendingClientCallScenario(t,
		mockprovider.MockTurn{ToolCalls: []core.ToolCall{pendingTestCall("call_s1", "serve_runtime_test_tool")}},
		mockprovider.MockTurn{ToolCalls: []core.ToolCall{pendingTestCall("call_s2", "serve_runtime_test_tool")}},
		mockprovider.MockTurn{ToolCalls: []core.ToolCall{pendingTestCall("call_c1", "webmcp__ping"), pendingTestCall("call_c2", "webmcp__ping")}},
	)
	if got, want := pendingClientCallIDs(result.PendingClientCalls()), []string{"call_c1", "call_c2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pending client calls = %v, want %v", got, want)
	}
	payload := pendingClientCallsPayload(result.PendingClientCalls())
	if len(payload) != 2 || payload[0]["name"] != "webmcp__ping" || payload[0]["arguments"] != `{"message":"hi"}` {
		t.Fatalf("payload = %#v", payload)
	}
}

// Client calls sharing a turn with a server tool are dropped by the engine, so
// they must not be reported once the run moves on.
func TestPendingClientCallsExcludeCallsDroppedWithServerTools(t *testing.T) {
	result := runPendingClientCallScenario(t,
		mockprovider.MockTurn{ToolCalls: []core.ToolCall{pendingTestCall("call_s1", "serve_runtime_test_tool"), pendingTestCall("call_c1", "webmcp__ping")}},
		mockprovider.MockTurn{Text: "done"},
	)
	if got := result.PendingClientCalls(); got == nil || len(got) != 0 {
		t.Fatalf("pending client calls = %#v, want empty non-nil list", got)
	}
}

func TestPendingClientCallTrackerTurnBoundaries(t *testing.T) {
	isServer := func(name string) bool { return name == "shell" }
	call := func(turn int, id, name string) core.Event {
		tool := core.ToolCall{ID: id, Name: name}
		return core.Event{Type: core.EventToolCall, Tool: &tool, ProviderTurnIndex: turn, ProviderTurnIndexSet: true}
	}
	inline := func(turn int, id, name string) core.Event {
		event := call(turn, id, name)
		event.ToolInline = true
		return event
	}
	text := func(turn int) core.Event {
		return core.Event{Type: core.EventTextDelta, Text: "x", ProviderTurnIndex: turn, ProviderTurnIndexSet: true}
	}

	cases := []struct {
		name   string
		events []core.Event
		want   []string
	}{
		{"client calls in final turn", []core.Event{call(0, "s", "shell"), call(1, "c1", "ping"), call(1, "c1", "ping"), call(1, "c2", "ping")}, []string{"c1", "c2"}},
		{"inline server tool does not drop same-turn calls", []core.Event{inline(0, "s", "shell"), call(0, "c1", "ping")}, []string{"c1"}},
		{"server tool in final turn", []core.Event{call(0, "c1", "ping"), call(0, "s", "shell")}, []string{}},
		{"later text-only turn clears", []core.Event{call(0, "c1", "ping"), text(1)}, []string{}},
		{"text in same turn keeps calls", []core.Event{text(0), call(0, "c1", "ping"), text(0)}, []string{"c1"}},
		{"discarded attempt clears its calls", []core.Event{
			call(1, "c1", "ping"),
			{Type: core.EventAttemptDiscard, ProviderTurnIndex: 1, ProviderTurnIndexSet: true},
			call(1, "c2", "ping"),
		}, []string{"c2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tracker pendingClientCallTracker
			for _, event := range tc.events {
				tracker.observe(event, isServer)
			}
			if got := pendingClientCallIDs(tracker.pending()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pending = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResponseRunSnapshotCarriesPendingClientCalls(t *testing.T) {
	run := newResponseRun("resp-pending-client", "sess-pending-client", "", "test", time.Now().Unix(), nil)
	run.finalRevReader = func() (int64, error) { return 1, nil }
	calls := pendingClientCallsPayload([]core.ToolCall{pendingTestCall("call_c1", "webmcp__ping")})
	if err := run.complete(map[string]any{"response": map[string]any{"id": run.id, "pending_client_calls": calls}}, core.Usage{}, core.Usage{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	snapshot := run.snapshot()
	if snapshot["status"] != "completed" {
		t.Fatalf("status = %v, want completed", snapshot["status"])
	}
	if got, ok := snapshot["pending_client_calls"].([]map[string]any); !ok || !reflect.DeepEqual(got, calls) {
		t.Fatalf("snapshot pending_client_calls = %#v, want %#v", snapshot["pending_client_calls"], calls)
	}
}

func TestPendingClientCallsValueAcceptsDecodedJSON(t *testing.T) {
	native := pendingClientCallsPayload([]core.ToolCall{pendingTestCall("call_c1", "webmcp__ping")})
	encoded, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	got := pendingClientCallsValue(decoded)
	if len(got) != 1 || got[0]["call_id"] != "call_c1" || got[0]["name"] != "webmcp__ping" {
		t.Fatalf("decoded pending calls = %#v", got)
	}
	if empty := pendingClientCallsValue([]any{}); empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v, want empty non-nil", empty)
	}
	if pendingClientCallsValue(nil) != nil || pendingClientCallsValue("x") != nil {
		t.Fatal("non-list values must yield nil")
	}
}
