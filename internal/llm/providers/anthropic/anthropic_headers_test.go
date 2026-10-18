package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicProviderSendsConfiguredHeaders(t *testing.T) {
	var got http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(minimalAnthropicSSE()))
	}))
	defer ts.Close()

	p, err := NewAnthropicProviderWithOptions("test-key", "claude-sonnet-4-6", AnthropicCredAPIKey, AnthropicProviderOptions{
		BaseURL: ts.URL,
		Headers: map[string]string{
			"anthropic-workspace-id": "wrkspc_test",
			"X-Empty":                "  ",
		},
	})
	if err != nil {
		t.Fatalf("NewAnthropicProviderWithOptions: %v", err)
	}
	stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainGoldenStream(t, stream)

	if v := got.Get("Anthropic-Workspace-Id"); v != "wrkspc_test" {
		t.Fatalf("anthropic-workspace-id = %q, want wrkspc_test", v)
	}
	if _, ok := got["X-Empty"]; ok {
		t.Fatal("empty-valued configured header was sent")
	}
	if v := got.Get("X-Api-Key"); v != "test-key" {
		t.Fatalf("x-api-key = %q", v)
	}

	// Model listing uses the same headers.
	got = nil
	_, _ = p.ListModels(context.Background())
	if v := got.Get("Anthropic-Workspace-Id"); v != "wrkspc_test" {
		t.Fatalf("list models anthropic-workspace-id = %q, want wrkspc_test", v)
	}
}

func TestAnthropicProviderRejectsInvalidHeaders(t *testing.T) {
	for name, headers := range map[string]map[string]string{
		"bad name":  {"bad header": "x"},
		"bad value": {"X-Token": "secret\r\nInjected: 1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewAnthropicProviderWithOptions("test-key", "claude-sonnet-4-6", AnthropicCredAPIKey, AnthropicProviderOptions{Headers: headers})
			if err == nil {
				t.Fatal("expected invalid header error")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks header value: %v", err)
			}
		})
	}
}

func TestAnthropicClientRefusesRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer ts.Close()

	p, err := NewAnthropicProviderWithOptions("secret-key", "claude-sonnet-4-6", AnthropicCredAPIKey, AnthropicProviderOptions{BaseURL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	events := drainGoldenStream(t, stream)
	var status interface{ HTTPStatusCode() int }
	if last := events[len(events)-1]; last.Type != core.EventError || !errors.As(last.Err, &status) || status.HTTPStatusCode() != http.StatusTemporaryRedirect {
		t.Fatalf("events = %+v, want a 307 status error", events)
	}
	if leaked {
		t.Fatal("redirect was followed with the API key")
	}
}

func TestOpenCodeInvalidBaseURLNeverFallsBackToAnthropic(t *testing.T) {
	p := newOpenCodeGoAnthropicProvider("opencode-key", "not a url", "m", nil)
	stream, err := p.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	events := drainGoldenStream(t, stream)
	if last := events[len(events)-1]; last.Type != core.EventError || !strings.Contains(last.Err.Error(), "invalid OpenCode Go base URL") {
		t.Fatalf("events = %+v, want invalid base URL error", events)
	}
}

func TestToolReplayEdgeCases(t *testing.T) {
	blocks := buildAnthropicBlocks([]core.Part{
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "t1", Name: "noop"}},
	}, true)
	use, ok := blocks[0].(*anthropicToolUseBlock)
	if !ok || string(use.Input) != "{}" {
		t.Fatalf("tool_use input = %s, want {}", use.Input)
	}
	if got := anthropicToolInput(json.RawMessage(" null ")); string(got) != "{}" {
		t.Fatalf("null tool input replayed as %s", got)
	}
	result := toolResultBlock(&core.ToolResult{ID: "t1", Name: "noop"})
	if len(result.Content) != 0 {
		t.Fatalf("empty tool output produced content blocks: %#v", result.Content)
	}
	data, _ := json.Marshal(result)
	if strings.Contains(string(data), `"content"`) || strings.Contains(string(data), `"image"`) {
		t.Fatalf("empty tool result JSON = %s", data)
	}
}

func TestOpenCodeMessagesClientRefusesRedirectsWithoutMutatingSharedClient(t *testing.T) {
	shared := &http.Client{}
	p := newOpenCodeGoAnthropicProvider("k", "https://opencode.example/zen/go/v1", "m", shared)
	if p.client.httpClient == shared || p.client.httpClient.CheckRedirect == nil {
		t.Fatal("OpenCode Messages client must be a redirect-refusing copy")
	}
	if shared.CheckRedirect != nil {
		t.Fatal("shared OpenCode HTTP client was mutated")
	}
}
