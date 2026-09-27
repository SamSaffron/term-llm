package cmd

import "github.com/samsaffron/term-llm/internal/llm"

// pendingClientCallTracker records which client (passthrough) tool calls a run
// hands back to its caller unanswered.
//
// The engine only stops on passthrough calls made by the final provider turn,
// and only when that turn called no server tool; client calls that share a
// turn with a server tool are dropped. Callers cannot reliably reconstruct
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
		if event.Tool == nil {
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
