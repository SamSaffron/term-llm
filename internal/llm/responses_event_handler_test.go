package llm

import (
	"context"
	"errors"
	"testing"
)

func TestResponsesUsageSeparatesCacheReadsAndWrites(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 1)
	completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"usage":{
				"input_tokens":1000,
				"input_tokens_details":{"cached_tokens":600,"cache_write_tokens":250},
				"output_tokens":100,
				"output_tokens_details":{"reasoning_tokens":40},
				"total_tokens":1100
			}
		}
	}`), "response.completed", eventSender{ctx: context.Background(), ch: events})
	if err != nil {
		t.Fatalf("HandleJSONEvent() error = %v", err)
	}
	if !completed {
		t.Fatal("HandleJSONEvent() completed = false")
	}
	if handler.lastUsage == nil {
		t.Fatal("lastUsage is nil")
	}
	got := *handler.lastUsage
	if got.InputTokens != 150 || got.CachedInputTokens != 600 || got.CacheWriteTokens != 250 ||
		got.OutputTokens != 100 || got.ReasoningTokens != 40 || got.ProviderRawInputTokens != 1000 || got.ProviderTotalTokens != 1100 {
		t.Fatalf("usage = %+v", got)
	}
}

func TestResponsesIncompleteReturnsErrorAfterUsage(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 4)
	completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.incomplete",
		"response":{
			"id":"resp_incomplete",
			"incomplete_details":{"reason":"max_output_tokens"},
			"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
		}
	}`), "response.incomplete", eventSender{ctx: context.Background(), ch: events})
	if completed {
		t.Fatal("HandleJSONEvent() completed = true for incomplete response")
	}
	var incompleteErr *ResponsesIncompleteError
	if !errors.As(err, &incompleteErr) {
		t.Fatalf("HandleJSONEvent() error = %T %v, want ResponsesIncompleteError", err, err)
	}
	if incompleteErr.Reason != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens", incompleteErr.Reason)
	}
	select {
	case event := <-events:
		if event.Type != EventUsage || event.Use == nil || event.Use.OutputTokens != 5 {
			t.Fatalf("final event = %+v, want usage", event)
		}
	default:
		t.Fatal("incomplete response did not emit final usage")
	}
}

func TestResponsesUsageClampsInconsistentUncachedInput(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.completed",
		"response":{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":90,"cache_write_tokens":20}}}
	}`), "response.completed", eventSender{})
	if err != nil || !completed {
		t.Fatalf("HandleJSONEvent() completed=%t error=%v", completed, err)
	}
	if handler.lastUsage == nil || handler.lastUsage.InputTokens != 0 {
		t.Fatalf("lastUsage = %+v, want clamped uncached input", handler.lastUsage)
	}
}

func TestResponsesWebSearchCallEmitsToolLifecycle(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 3)
	send := eventSender{ctx: context.Background(), ch: events}

	completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.added",
		"output_index":1,
		"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}
	}`), "response.output_item.added", send)
	if err != nil || completed {
		t.Fatalf("added event completed=%t error=%v", completed, err)
	}
	start := <-events
	if start.Type != EventToolExecStart || start.ToolCallID != "ws_1" || start.ToolName != WebSearchToolName {
		t.Fatalf("start event = %+v, want native web search tool start", start)
	}

	completed, err = handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done",
		"output_index":1,
		"item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"current Go release"}}
	}`), "response.output_item.done", send)
	if err != nil || completed {
		t.Fatalf("done item event completed=%t error=%v", completed, err)
	}
	end := <-events
	if end.Type != EventToolExecEnd || end.ToolCallID != "ws_1" || end.ToolName != WebSearchToolName || !end.ToolSuccess || end.ToolInfo != "(current Go release)" {
		t.Fatalf("end event = %+v, want successful native web search tool end", end)
	}
	activityEvent := <-events
	activity := activityEvent.ToolActivity
	if activityEvent.Type != EventToolActivity || activity == nil || activity.ID != "ws_1" || activity.Name != WebSearchToolName || activity.Info != "(current Go release)" || string(activity.Arguments) != `{"query":"current Go release"}` || activity.Status != ToolActivityCompleted {
		t.Fatalf("tool activity event = %+v", activityEvent)
	}
	if len(handler.replayItems) != 1 {
		t.Fatalf("replay items = %#v, want opaque replay separate from display activity", handler.replayItems)
	}
}

func TestResponsesWebSearchDoneWithoutAddedEmitsFailedLifecycle(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 3)

	completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done",
		"output_index":3,
		"item":{"type":"web_search_call","status":"failed","action":{"type":"open_page","url":"https://example.com"}}
	}`), "response.output_item.done", eventSender{ctx: context.Background(), ch: events})
	if err != nil || completed {
		t.Fatalf("done item event completed=%t error=%v", completed, err)
	}
	start := <-events
	end := <-events
	if start.Type != EventToolExecStart || start.ToolCallID != "web_search:3" || start.ToolInfo != "(https://example.com)" {
		t.Fatalf("start event = %+v, want fallback native web search start", start)
	}
	if end.Type != EventToolExecEnd || end.ToolCallID != start.ToolCallID || end.ToolSuccess {
		t.Fatalf("end event = %+v, want failed native web search end", end)
	}
	activityEvent := <-events
	if activityEvent.Type != EventToolActivity || activityEvent.ToolActivity == nil || activityEvent.ToolActivity.Status != ToolActivityFailed {
		t.Fatalf("activity event = %+v, want failed persisted web search activity", activityEvent)
	}
	if len(handler.replayItems) != 1 {
		t.Fatalf("replay items = %#v, want opaque failed web search replay", handler.replayItems)
	}
}
