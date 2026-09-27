package cmd

import "github.com/samsaffron/term-llm/internal/llm"

// pendingClientCallTracker records which client (passthrough) tool calls a run
// hands back to its caller unanswered.
//
// The engine only stops on passthrough calls made by the final provider turn,
// and only when that turn deferred no server tool to its end-of-turn split;
// client calls that share a turn with such a server tool are dropped. Callers cannot reliably reconstruct
// this from the event stream, because tool-only turns emit no text boundary,
// so the server reports the list explicitly on completion.
type pendingClientCallTracker struct {
	turn       int
	turnSet    bool
	serverTool bool
	calls      []llm.ToolCall
}

func (t *pendingClientCallTracker) resetTurn() {
	t.serverTool = false
	t.calls = nil
}

func (t *pendingClientCallTracker) observe(event llm.Event, isServerTool func(string) bool) {
	if event.ProviderTurnIndexSet && (!t.turnSet || event.ProviderTurnIndex > t.turn) {
		t.turn, t.turnSet = event.ProviderTurnIndex, true
		t.resetTurn()
	}
	switch event.Type {
	case llm.EventAttemptDiscard:
		// A discarded attempt is retried as the same provider turn.
		if !event.ProviderTurnIndexSet || event.ProviderTurnIndex == t.turn {
			t.resetTurn()
		}
	case llm.EventToolCall:
		// Inline calls ran during the provider stream and never enter the
		// engine's end-of-turn split, so they neither stop the run nor cause
		// passthrough calls in their turn to be dropped.
		if event.Tool == nil || event.ToolInline {
			return
		}
		if isServerTool != nil && isServerTool(event.Tool.Name) {
			t.serverTool = true
			return
		}
		for _, existing := range t.calls {
			if existing.ID == event.Tool.ID {
				return
			}
		}
		t.calls = append(t.calls, *event.Tool)
	}
}

// pending returns the client calls awaiting results, in call order. It is
// never nil so that callers can publish an authoritative, possibly empty, list.
func (t *pendingClientCallTracker) pending() []llm.ToolCall {
	if t.serverTool || len(t.calls) == 0 {
		return []llm.ToolCall{}
	}
	return append([]llm.ToolCall(nil), t.calls...)
}

// pendingClientCallsPayload is the wire form published on response.completed.
func pendingClientCallsPayload(calls []llm.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		arguments := string(call.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		out = append(out, map[string]any{
			"call_id":   call.ID,
			"name":      call.Name,
			"arguments": arguments,
		})
	}
	return out
}

// pendingClientCallsValue reads a published pending list, native or JSON
// decoded. It returns nil when the value is not a list.
func pendingClientCallsValue(value any) []map[string]any {
	switch calls := value.(type) {
	case []map[string]any:
		return calls
	case []any:
		out := make([]map[string]any, 0, len(calls))
		for _, entry := range calls {
			if call, ok := entry.(map[string]any); ok {
				out = append(out, call)
			}
		}
		return out
	}
	return nil
}
