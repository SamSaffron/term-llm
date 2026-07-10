package llm

import (
	"context"
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
