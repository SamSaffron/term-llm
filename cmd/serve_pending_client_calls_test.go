package cmd

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func pendingClientCallIDs(calls []llm.ToolCall) []string {
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		ids = append(ids, call.ID)
	}
	return ids
}

func runPendingClientCallScenario(t *testing.T, turns ...llm.MockTurn) serveRunResult {
	t.Helper()
	provider := llm.NewMockProvider("mock")
	for _, turn := range turns {
		provider.AddTurn(turn)
	}
	tool := &serveRuntimeTestTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	engine := llm.NewEngine(provider, registry)
	rt := &serveRuntime{provider: provider, providerKey: "mock", engine: engine, defaultModel: "mock-model"}
	clientTool := llm.ToolSpec{Name: "webmcp__ping", Description: "client tool", Schema: map[string]any{"type": "object"}}
	result, err := rt.Run(context.Background(), false, false, []llm.Message{serveRuntimeTextMessage(llm.RoleUser, "go")}, llm.Request{
		Model:      "mock-model",
		Tools:      []llm.ToolSpec{tool.Spec(), clientTool},
		ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		MaxTurns:   6,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return result
}

func pendingTestCall(id, name string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{"message":"hi"}`)}
}

// Session 5319: tool-only server turns followed by client calls must still
// report the client calls, even though no text separates the turns.
func TestPendingClientCallsFollowToolOnlyServerTurns(t *testing.T) {
	result := runPendingClientCallScenario(t,
		llm.MockTurn{ToolCalls: []llm.ToolCall{pendingTestCall("call_s1", "serve_runtime_test_tool")}},
		llm.MockTurn{ToolCalls: []llm.ToolCall{pendingTestCall("call_s2", "serve_runtime_test_tool")}},
		llm.MockTurn{ToolCalls: []llm.ToolCall{pendingTestCall("call_c1", "webmcp__ping"), pendingTestCall("call_c2", "webmcp__ping")}},
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
		llm.MockTurn{ToolCalls: []llm.ToolCall{pendingTestCall("call_s1", "serve_runtime_test_tool"), pendingTestCall("call_c1", "webmcp__ping")}},
		llm.MockTurn{Text: "done"},
	)
	if got := result.PendingClientCalls(); got == nil || len(got) != 0 {
		t.Fatalf("pending client calls = %#v, want empty non-nil list", got)
	}
}

func TestPendingClientCallTrackerTurnBoundaries(t *testing.T) {
	isServer := func(name string) bool { return name == "shell" }
	call := func(turn int, id, name string) llm.Event {
		tool := llm.ToolCall{ID: id, Name: name}
		return llm.Event{Type: llm.EventToolCall, Tool: &tool, ProviderTurnIndex: turn, ProviderTurnIndexSet: true}
	}
	text := func(turn int) llm.Event {
		return llm.Event{Type: llm.EventTextDelta, Text: "x", ProviderTurnIndex: turn, ProviderTurnIndexSet: true}
	}

	cases := []struct {
		name   string
		events []llm.Event
		want   []string
	}{
		{"client calls in final turn", []llm.Event{call(0, "s", "shell"), call(1, "c1", "ping"), call(1, "c1", "ping"), call(1, "c2", "ping")}, []string{"c1", "c2"}},
		{"server tool in final turn", []llm.Event{call(0, "c1", "ping"), call(0, "s", "shell")}, []string{}},
		{"later text-only turn clears", []llm.Event{call(0, "c1", "ping"), text(1)}, []string{}},
		{"text in same turn keeps calls", []llm.Event{text(0), call(0, "c1", "ping"), text(0)}, []string{"c1"}},
		{"discarded attempt clears its calls", []llm.Event{
			call(1, "c1", "ping"),
			{Type: llm.EventAttemptDiscard, ProviderTurnIndex: 1, ProviderTurnIndexSet: true},
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
	calls := pendingClientCallsPayload([]llm.ToolCall{pendingTestCall("call_c1", "webmcp__ping")})
	if err := run.complete(map[string]any{"response": map[string]any{"id": run.id, "pending_client_calls": calls}}, llm.Usage{}, llm.Usage{}); err != nil {
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
