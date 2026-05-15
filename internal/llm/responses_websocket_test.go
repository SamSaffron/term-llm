package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestResponsesWebSocketURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://api.openai.com/v1/responses", "wss://api.openai.com/v1/responses"},
		{"http://localhost:8080/responses", "ws://localhost:8080/responses"},
		{"ws://localhost/responses", "ws://localhost/responses"},
		{"wss://localhost/responses", "wss://localhost/responses"},
	}
	for _, tc := range tests {
		got, err := responsesWebSocketURL(tc.in)
		if err != nil {
			t.Fatalf("responsesWebSocketURL(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("responsesWebSocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResponsesWebSocketRequestOmitsTransportFields(t *testing.T) {
	generate := false
	wsReq := newResponsesWSRequest(ResponsesRequest{
		Model:    "gpt-test",
		Input:    []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream:   true,
		Generate: &generate,
	})
	body, err := json.Marshal(wsReq)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["type"] != "response.create" {
		t.Fatalf("type = %#v", decoded["type"])
	}
	if _, ok := decoded["stream"]; ok {
		t.Fatalf("WebSocket response.create must not include stream: %s", body)
	}
	if decoded["generate"] != false {
		t.Fatalf("generate = %#v, want false", decoded["generate"])
	}
}

func TestResponsesJSONEventType(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{
			name: "top level first",
			data: `{"type":"response.output_text.delta","delta":"hello"}`,
			want: "response.output_text.delta",
		},
		{
			name: "whitespace around first field",
			data: " { \n \t \"type\" : \"response.completed\", \"response\": {\"id\": \"resp_1\"} } ",
			want: "response.completed",
		},
		{
			name: "escaped first type",
			data: `{"type":"response.output_text.\u0064elta","delta":"hello"}`,
			want: "response.output_text.delta",
		},
		{
			name: "nested type before top level",
			data: `{"item":{"type":"function_call"},"output_index":0,"type":"response.output_item.added"}`,
			want: "response.output_item.added",
		},
		{
			name: "unknown first type falls back to unmarshal semantics",
			data: `{"type":"unknown.event","type":"response.completed","response":{"id":"resp_1"}}`,
			want: "response.completed",
		},
		{
			name: "missing type",
			data: `{"delta":"hello"}`,
		},
		{
			name:    "malformed",
			data:    `{"type":`,
			wantErr: true,
		},
		{
			name:    "unknown malformed first type falls back to validation",
			data:    `{"type":"unknown.event",`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := responsesJSONEventType([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("responsesJSONEventType: %v", err)
			}
			if got != tt.want {
				t.Fatalf("type = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResponsesWebSocketPrepareClearsStalePreviousResponseID(t *testing.T) {
	client := &ResponsesClient{LastResponseID: ""}
	fullInput := []ResponsesInputItem{
		{Type: "message", Role: "user", Content: "old"},
		{Type: "message", Role: "user", Content: "new"},
	}

	prepared := client.prepareWebSocketContinuationLocked(ResponsesRequest{
		Model:              "gpt-test",
		PreviousResponseID: "resp_stale",
		Input:              []ResponsesInputItem{{Type: "message", Role: "user", Content: "new"}},
	}, func() []ResponsesInputItem { return fullInput })

	if prepared.PreviousResponseID != "" {
		t.Fatalf("PreviousResponseID = %q, want empty", prepared.PreviousResponseID)
	}
	if len(prepared.Input) != len(fullInput) || prepared.Input[0].Content != "old" || prepared.Input[1].Content != "new" {
		t.Fatalf("Input = %#v, want full input %#v", prepared.Input, fullInput)
	}
}

func TestResponsesClientStreamWebSocket(t *testing.T) {
	var handshakeCount atomic.Int32
	var gotReq map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakeCount.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != responsesWebSocketBetaHeader {
			t.Errorf("OpenAI-Beta header = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if err := json.Unmarshal(msg, &gotReq); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "hello"})
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_1",
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
				"input_tokens_details":  map[string]any{"cached_tokens": 4},
				"output_tokens_details": map[string]any{"reasoning_tokens": 1},
			},
		}})
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:      server.URL,
		UseWebSocket: true,
		GetAuthHeader: func() string {
			return "Bearer test-key"
		},
	}
	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Tools:  []any{ResponsesTool{Type: "function", Name: "tool", Parameters: map[string]any{"type": "object"}}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text string
	var usage *Usage
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventUsage:
			usage = event.Use
		case EventDone:
			if text != "hello" {
				t.Fatalf("text = %q, want hello", text)
			}
			if usage == nil || usage.InputTokens != 6 || usage.CachedInputTokens != 4 || usage.ReasoningTokens != 1 {
				t.Fatalf("usage = %+v", usage)
			}
			if handshakeCount.Load() != 1 {
				t.Fatalf("handshakes = %d, want 1", handshakeCount.Load())
			}
			if gotReq["type"] != "response.create" || gotReq["model"] != "gpt-test" {
				t.Fatalf("request fields = %#v", gotReq)
			}
			if _, ok := gotReq["stream"]; ok {
				t.Fatalf("WebSocket request must not include transport-only stream field: %#v", gotReq)
			}
			if _, ok := gotReq["tools"].([]any); !ok {
				t.Fatalf("request tools missing: %#v", gotReq)
			}
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketAuthRetryUsesFreshHeaderAndTimeout(t *testing.T) {
	var attempts atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		if attempt == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer old" {
				t.Errorf("first Authorization = %q", got)
			}
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer new" {
			t.Errorf("retry Authorization = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}})
	}))
	defer server.Close()

	token := "old"
	client := &ResponsesClient{
		BaseURL:                 server.URL,
		UseWebSocket:            true,
		WebSocketConnectTimeout: 20 * time.Millisecond,
		GetAuthHeader: func() string {
			return "Bearer " + token
		},
		OnAuthRetry: func(ctx context.Context) error {
			select {
			case <-time.After(50 * time.Millisecond):
				token = "new"
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == EventDone {
			break
		}
		if event.Type == EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestResponsesClientWebSocketFunctionCall(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup"}})
		_ = conn.WriteJSON(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": `{"q"`})
		_ = conn.WriteJSON(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": `:"x"}`})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`}})
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}})
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch event.Type {
		case EventToolCall:
			if event.Tool == nil || event.Tool.ID != "call_1" || event.Tool.Name != "lookup" || string(event.Tool.Arguments) != `{"q":"x"}` {
				t.Fatalf("tool call = %+v", event.Tool)
			}
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketConnectFailureFallsBackToHTTP(t *testing.T) {
	oldBackoff := responsesWebSocketBaseBackoff
	responsesWebSocketBaseBackoff = 0
	defer func() { responsesWebSocketBaseBackoff = oldBackoff }()

	var wsAttempts atomic.Int32
	var httpAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts.Add(1)
			http.Error(w, "no websocket", http.StatusBadGateway)
			return
		}
		httpAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n"))
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	var text string
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventDone:
			if text != "fallback" {
				t.Fatalf("text = %q, want fallback", text)
			}
			if wsAttempts.Load() != 3 {
				t.Fatalf("websocket attempts = %d, want 3", wsAttempts.Load())
			}
			if httpAttempts.Load() != 1 {
				t.Fatalf("http attempts = %d, want 1", httpAttempts.Load())
			}
			assertSecondStreamUsesHTTPFallbackOnly(t, client, &wsAttempts, &httpAttempts)
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func assertSecondStreamUsesHTTPFallbackOnly(t *testing.T, client *ResponsesClient, wsAttempts, httpAttempts *atomic.Int32) {
	t.Helper()
	wsBefore := wsAttempts.Load()
	httpBefore := httpAttempts.Load()
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "again"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("second Recv: %v", err)
		}
		switch event.Type {
		case EventDone:
			if got := wsAttempts.Load(); got != wsBefore {
				t.Fatalf("websocket attempts after fallback disable = %d, want unchanged %d", got, wsBefore)
			}
			if got := httpAttempts.Load(); got != httpBefore+1 {
				t.Fatalf("http attempts after second stream = %d, want %d", got, httpBefore+1)
			}
			return
		case EventError:
			t.Fatalf("second stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketReadFailureBeforeEventsRetriesWebSocketThenFallsBackToHTTP(t *testing.T) {
	oldBackoff := responsesWebSocketBaseBackoff
	responsesWebSocketBaseBackoff = 0
	defer func() { responsesWebSocketBaseBackoff = oldBackoff }()

	var wsAttempts atomic.Int32
	var httpAttempts atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		httpAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback after read\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n"))
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text string
	var retries int
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch event.Type {
		case EventRetry:
			retries++
		case EventTextDelta:
			text += event.Text
		case EventDone:
			if text != "fallback after read" {
				t.Fatalf("text = %q, want fallback after read", text)
			}
			if wsAttempts.Load() != 3 || httpAttempts.Load() != 1 {
				t.Fatalf("attempts ws=%d http=%d, want 3/1", wsAttempts.Load(), httpAttempts.Load())
			}
			if retries != responsesWebSocketMaxAttempts-1 {
				t.Fatalf("retry events = %d, want %d", retries, responsesWebSocketMaxAttempts-1)
			}
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketReadFailureBeforeEventsRetryCanRecover(t *testing.T) {
	oldBackoff := responsesWebSocketBaseBackoff
	responsesWebSocketBaseBackoff = 0
	defer func() { responsesWebSocketBaseBackoff = oldBackoff }()

	var wsAttempts atomic.Int32
	var httpAttempts atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			attempt := wsAttempts.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer conn.Close()
			if attempt <= 2 {
				return
			}
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "websocket retry"})
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_ws_retry"}})
			return
		}
		httpAttempts.Add(1)
		t.Fatal("HTTP fallback should not be used when the WebSocket retry succeeds")
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text string
	var retries int
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch event.Type {
		case EventRetry:
			retries++
		case EventTextDelta:
			text += event.Text
		case EventDone:
			if text != "websocket retry" {
				t.Fatalf("text = %q, want websocket retry", text)
			}
			if wsAttempts.Load() != 3 || httpAttempts.Load() != 0 {
				t.Fatalf("attempts ws=%d http=%d, want 3/0", wsAttempts.Load(), httpAttempts.Load())
			}
			if retries != 2 {
				t.Fatalf("retry events = %d, want 2", retries)
			}
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketReadFailureAfterEventsReturnsIncomplete(t *testing.T) {
	oldBackoff := responsesWebSocketBaseBackoff
	responsesWebSocketBaseBackoff = 0
	defer func() { responsesWebSocketBaseBackoff = oldBackoff }()

	var wsAttempts atomic.Int32
	var httpAttempts atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer conn.Close()
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "partial"})
			return
		}
		httpAttempts.Add(1)
		t.Fatal("HTTP fallback should not be used after visible WebSocket output")
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if event.Type != EventTextDelta || event.Text != "partial" {
		t.Fatalf("first event = %#v, want partial text", event)
	}

	event, err = stream.Recv()
	if err != nil {
		t.Fatalf("second Recv returned transport error instead of EventError: %v", err)
	}
	if event.Type != EventError || event.Err == nil {
		t.Fatalf("second event = %#v, want error event", event)
	}
	var incomplete *StreamIncompleteError
	if !errors.As(event.Err, &incomplete) {
		t.Fatalf("error type = %T, want StreamIncompleteError: %v", event.Err, event.Err)
	}
	if !strings.Contains(event.Err.Error(), "Responses WebSocket closed before response.completed") {
		t.Fatalf("incomplete stream message not actionable: %v", event.Err)
	}
	if wsAttempts.Load() != 1 || httpAttempts.Load() != 0 {
		t.Fatalf("attempts ws=%d http=%d, want 1/0", wsAttempts.Load(), httpAttempts.Load())
	}
}

func TestResponsesClientWebSocketBackoffHonorsContextCancellation(t *testing.T) {
	oldBackoff := responsesWebSocketBaseBackoff
	responsesWebSocketBaseBackoff = time.Hour
	defer func() { responsesWebSocketBaseBackoff = oldBackoff }()

	var wsAttempts atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		t.Fatal("HTTP fallback should not be reached when context is canceled during WebSocket backoff")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if event.Type != EventRetry {
		t.Fatalf("first event = %#v, want retry", event)
	}
	cancel()
	_, err = stream.Recv()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Recv after cancel = %v, want context canceled", err)
	}
	if wsAttempts.Load() != 1 {
		t.Fatalf("websocket attempts = %d, want 1", wsAttempts.Load())
	}
}

func TestResponsesClientHTTPFallbackWithWebSocketOnlyServerStateSendsFullInput(t *testing.T) {
	var httpReq map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no websocket", http.StatusBadGateway)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&httpReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n"))
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:              server.URL,
		UseWebSocket:         true,
		DisableServerState:   true,
		WebSocketServerState: true,
		LastResponseID:       "resp_ws",
	}
	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "full"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == EventDone {
			break
		}
	}
	if _, ok := httpReq["previous_response_id"]; ok {
		t.Fatalf("HTTP fallback sent previous_response_id despite DisableServerState: %#v", httpReq)
	}
}

func TestResponsesClientWebSocketPreviousResponseRejectedRetriesFullState(t *testing.T) {
	var secondRequest map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		// First stream establishes a response id.
		_, _, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read first request: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}})

		// Second stream first attempts previous_response_id.
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read incremental request: %v", err)
			return
		}
		var incremental map[string]any
		_ = json.Unmarshal(msg, &incremental)
		if incremental["previous_response_id"] != "resp_1" {
			t.Errorf("incremental previous_response_id = %#v", incremental["previous_response_id"])
		}
		_ = conn.WriteJSON(map[string]any{
			"type":   "response.failed",
			"status": 400,
			"response": map[string]any{
				"error": map[string]any{"code": "previous_response_not_found", "message": "Previous response not found", "param": "previous_response_id"},
			},
		})

		// Client should retry the same turn as full state on the same connection.
		_, msg, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read full-state retry: %v", err)
			return
		}
		_ = json.Unmarshal(msg, &secondRequest)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2"}})
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, WebSocketServerState: true, DisableServerState: true}
	for _, input := range [][]ResponsesInputItem{
		{{Type: "message", Role: "user", Content: "one"}},
		{{Type: "message", Role: "user", Content: "one"}, {Type: "message", Role: "user", Content: "two"}},
	} {
		stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: input, Stream: true}, false)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if event.Type == EventDone {
				break
			}
			if event.Type == EventError {
				t.Fatalf("stream error: %v", event.Err)
			}
		}
		_ = stream.Close()
	}
	if _, ok := secondRequest["previous_response_id"]; ok {
		t.Fatalf("full-state retry still had previous_response_id: %#v", secondRequest)
	}
	input, ok := secondRequest["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("full-state retry input = %#v, want both input items", secondRequest["input"])
	}
}

func TestResponsesClientWebSocketReusesConnectionAndPreviousResponseID(t *testing.T) {
	var handshakeCount atomic.Int32
	var requests []map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakeCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			_ = json.Unmarshal(msg, &req)
			requests = append(requests, req)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + string(rune('1'+i))}})
		}
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	for _, input := range [][]ResponsesInputItem{
		{{Type: "message", Role: "user", Content: "one"}},
		{{Type: "message", Role: "assistant", Content: "old"}, {Type: "message", Role: "user", Content: "two"}},
	} {
		stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: input, Stream: true}, false)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if event.Type == EventDone {
				break
			}
			if event.Type == EventError {
				t.Fatalf("stream error: %v", event.Err)
			}
		}
		_ = stream.Close()
	}
	if handshakeCount.Load() != 1 {
		t.Fatalf("handshakes = %d, want 1", handshakeCount.Load())
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if requests[1]["previous_response_id"] != "resp_1" {
		t.Fatalf("previous_response_id = %#v", requests[1]["previous_response_id"])
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) != 1 || !strings.Contains(toJSON(input[0]), "two") {
		t.Fatalf("second input = %#v, want only newest user item", requests[1]["input"])
	}
}

func TestResponsesClientWebSocketDoesNotReuseStateWhenRequestShapeChanges(t *testing.T) {
	var requests []map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			_ = json.Unmarshal(msg, &req)
			requests = append(requests, req)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_changed"}})
		}
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	for i, tools := range [][]any{
		{ResponsesTool{Type: "function", Name: "tool_a", Parameters: map[string]any{"type": "object"}}},
		{ResponsesTool{Type: "function", Name: "tool_b", Parameters: map[string]any{"type": "object"}}},
	} {
		stream, err := client.Stream(context.Background(), ResponsesRequest{
			Model:  "gpt-test",
			Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "turn"}},
			Tools:  tools,
			Stream: true,
		}, false)
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				t.Fatalf("Recv %d: %v", i, err)
			}
			if event.Type == EventDone {
				break
			}
			if event.Type == EventError {
				t.Fatalf("stream error: %v", event.Err)
			}
		}
		_ = stream.Close()
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatalf("previous_response_id should be omitted when tool schema changes: %#v", requests[1])
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) != 1 || !strings.Contains(toJSON(input[0]), "turn") {
		t.Fatalf("second input = %#v, want full current input", requests[1]["input"])
	}
}

func TestResponsesClientWebSocketDisableServerStateSendsFullInput(t *testing.T) {
	var requests []map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			_ = json.Unmarshal(msg, &req)
			requests = append(requests, req)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp"}})
		}
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, DisableServerState: true}
	for _, input := range [][]ResponsesInputItem{
		{{Type: "message", Role: "user", Content: "one"}},
		{{Type: "message", Role: "assistant", Content: "old"}, {Type: "message", Role: "user", Content: "two"}},
	} {
		stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: input, Stream: true}, false)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if event.Type == EventDone {
				break
			}
		}
		_ = stream.Close()
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatalf("previous_response_id sent with DisableServerState: %#v", requests[1])
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("second input = %#v, want full history", requests[1]["input"])
	}
}

func TestResponsesClientWebSocketContextCancel(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		time.Sleep(time.Second)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, WebSocketIdleTimeout: time.Second}
	stream, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("Recv error = nil, want cancellation")
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
