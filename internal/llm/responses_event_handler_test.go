package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestResponsesDoneTextCompletesPartialAndUnstreamedParts(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 4)
	send := eventSender{ctx: context.Background(), ch: events}

	_, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","output_index":0,"delta":"hel"
	}`), "response.output_text.delta", send)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done","output_index":0,
		"item":{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"hello"},
			{"type":"output_text","text":" world"}
		]}
	}`), "response.output_item.done", send)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"hel", "lo", " world"}
	for i, text := range want {
		event := <-events
		if event.Type != EventTextDelta || event.Text != text {
			t.Fatalf("event %d = %+v, want text %q", i, event, text)
		}
	}
}

func TestResponsesDoneTextMismatchDoesNotHideLaterParts(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 3)
	send := eventSender{ctx: context.Background(), ch: events}

	if _, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","output_index":0,"delta":"abc"
	}`), "response.output_text.delta", send); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done","output_index":0,
		"item":{"type":"message","role":"assistant","content":[
			{"type":"output_text","text":"abd"},
			{"type":"output_text","text":"later"}
		]}
	}`), "response.output_item.done", send); err != nil {
		t.Fatal(err)
	}

	if event := <-events; event.Type != EventTextDelta || event.Text != "abc" {
		t.Fatalf("streamed event = %+v", event)
	}
	if event := <-events; event.Type != EventTextDelta || event.Text != "later" {
		t.Fatalf("later part event = %+v, want unstreamed later part", event)
	}
	select {
	case event := <-events:
		t.Fatalf("unexpected mismatch fallback event: %+v", event)
	default:
	}
}

func TestResponsesDoneTextUsesItemIDAcrossOutputIndexDrift(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 2)
	send := eventSender{ctx: context.Background(), ch: events}

	if _, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"complete"
	}`), "response.output_text.delta", send); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done","output_index":1,
		"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"complete"}]}
	}`), "response.output_item.done", send); err != nil {
		t.Fatal(err)
	}

	if event := <-events; event.Type != EventTextDelta || event.Text != "complete" {
		t.Fatalf("streamed event = %+v", event)
	}
	select {
	case event := <-events:
		t.Fatalf("item-id reconciliation duplicated text: %+v", event)
	default:
	}
}

func TestResponsesTextBuildersDoNotAliasReusedOutputIndex(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 8)
	send := eventSender{ctx: context.Background(), ch: events}

	for _, data := range []string{
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"first"}`,
		`{"type":"response.output_text.delta","item_id":"msg_2","output_index":1,"delta":"second"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":" late"}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":" anonymous"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"first late more"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":"second anonymous"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_3","type":"message","role":"assistant","content":[{"type":"output_text","text":"third"}]}}`,
	} {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &envelope); err != nil {
			t.Fatal(err)
		}
		if _, err := handler.HandleJSONEvent([]byte(data), envelope.Type, send); err != nil {
			t.Fatal(err)
		}
	}

	for i, want := range []string{"first", "second", " late", " anonymous", " more", "third"} {
		if event := <-events; event.Type != EventTextDelta || event.Text != want {
			t.Fatalf("event %d = %+v, want %q", i, event, want)
		}
	}
	select {
	case event := <-events:
		t.Fatalf("reused output index aliased item text: %+v", event)
	default:
	}
}

func TestResponsesDoneTextDoesNotDuplicateSameItemDelta(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 2)
	send := eventSender{ctx: context.Background(), ch: events}

	_, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","output_index":0,"delta":"complete"
	}`), "response.output_text.delta", send)
	if err != nil {
		t.Fatal(err)
	}
	_, err = handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done","output_index":0,
		"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"complete"}]}
	}`), "response.output_item.done", send)
	if err != nil {
		t.Fatal(err)
	}
	if event := <-events; event.Type != EventTextDelta || event.Text != "complete" {
		t.Fatalf("text event = %+v", event)
	}
	select {
	case duplicate := <-events:
		t.Fatalf("done fallback duplicated streamed text: %+v", duplicate)
	default:
	}
}

func TestResponsesDuplicateDoneItemDoesNotRepeatSuffix(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 3)
	send := eventSender{ctx: context.Background(), ch: events}

	if _, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"first"
	}`), "response.output_text.delta", send); err != nil {
		t.Fatal(err)
	}
	done := []byte(`{
		"type":"response.output_item.done","output_index":0,
		"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"first more"}]}
	}`)
	for i := 0; i < 2; i++ {
		if _, err := handler.HandleJSONEvent(done, "response.output_item.done", send); err != nil {
			t.Fatal(err)
		}
	}

	for i, want := range []string{"first", " more"} {
		if event := <-events; event.Type != EventTextDelta || event.Text != want {
			t.Fatalf("event %d = %+v, want %q", i, event, want)
		}
	}
	select {
	case event := <-events:
		t.Fatalf("duplicate done item repeated text: %+v", event)
	default:
	}
	if len(handler.replayItems) != 1 {
		t.Fatalf("replay items = %d, want one copy of duplicate done item", len(handler.replayItems))
	}
	if len(handler.outputItems) != 1 {
		t.Fatalf("output items = %d, want one copy of duplicate done item", len(handler.outputItems))
	}
}

func TestResponsesDoneMessagePreservesHiddenVisibilityFromAddedItem(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 3)
	send := eventSender{ctx: context.Background(), ch: events}

	if completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.added","output_index":2,
		"item":{"id":"msg_worker","type":"message","agent":"/worker"}
	}`), "response.output_item.added", send); err != nil || completed {
		t.Fatalf("added item completed=%t error=%v", completed, err)
	}
	if completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","item_id":"msg_worker","output_index":3,"delta":"hidden worker "
	}`), "response.output_text.delta", send); err != nil || completed {
		t.Fatalf("delta item completed=%t error=%v", completed, err)
	}
	if completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done","output_index":3,
		"item":{"id":"msg_worker","type":"message","role":"assistant","content":[{"type":"output_text","text":"hidden worker output"}]}
	}`), "response.output_item.done", send); err != nil || completed {
		t.Fatalf("done item completed=%t error=%v", completed, err)
	}
	select {
	case event := <-events:
		t.Fatalf("hidden output emitted event: %+v", event)
	default:
	}
}

func TestResponsesDoneTextFallbackIsScopedToOutputItem(t *testing.T) {
	handler := newResponsesStreamEventHandler(&ResponsesClient{}, 0, false, "test", false, "", false)
	events := make(chan Event, 4)
	send := eventSender{ctx: context.Background(), ch: events}

	if completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_text.delta","output_index":0,"delta":"first"
	}`), "response.output_text.delta", send); err != nil || completed {
		t.Fatalf("text delta completed=%t error=%v", completed, err)
	}
	if completed, err := handler.HandleJSONEvent([]byte(`{
		"type":"response.output_item.done","output_index":1,
		"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}
	}`), "response.output_item.done", send); err != nil || completed {
		t.Fatalf("done item completed=%t error=%v", completed, err)
	}

	first := <-events
	second := <-events
	if first.Type != EventTextDelta || first.Text != "first" {
		t.Fatalf("first event = %+v", first)
	}
	if second.Type != EventTextDelta || second.Text != "second" {
		t.Fatalf("second event = %+v, want fallback text for its own output item", second)
	}
}

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
	}`), "response.completed", eventSender{ctx: context.Background()})
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
