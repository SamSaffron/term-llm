package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func withResponsesWebSocketBaseBackoff(t *testing.T, backoff time.Duration) {
	t.Helper()
	oldBackoff := responsesWebSocketBaseBackoff
	responsesWebSocketBaseBackoff = backoff
	t.Cleanup(func() { responsesWebSocketBaseBackoff = oldBackoff })
}

type marshalCountingJSONLikeValue struct {
	Calls *atomic.Int32 `json:"-"`
}

func (v marshalCountingJSONLikeValue) MarshalJSON() ([]byte, error) {
	if v.Calls != nil {
		v.Calls.Add(1)
	}
	return []byte(`{"type":"function","name":"tool","parameters":{"type":"object"}}`), nil
}

type dynamicMarshalJSONLikeValue struct {
	Version int `json:"-"`
}

func (v dynamicMarshalJSONLikeValue) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"type":"function","name":"tool","version":%d}`, v.Version)), nil
}

func TestResponsesClientForceHTTPBypassesConfiguredWebSocket(t *testing.T) {
	var sawUpgrade atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			sawUpgrade.Store(true)
			http.Error(w, "unexpected websocket", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n")
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, HTTPClient: server.Client(), UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model:     "gpt-5.6-sol",
		Input:     []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream:    true,
		ForceHTTP: true,
	}, false)
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("Recv() error = %v", recvErr)
		}
		if event.Type == EventDone {
			break
		}
	}
	if sawUpgrade.Load() {
		t.Fatal("ForceHTTP request attempted a WebSocket upgrade")
	}
}

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
		StreamOptions: &ResponsesStreamOptions{
			ReasoningSummaryDelivery: "sequential_cutoff",
		},
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
	if _, ok := decoded["service_tier"]; ok {
		t.Fatalf("empty service_tier must be omitted: %s", body)
	}
	if decoded["generate"] != false {
		t.Fatalf("generate = %#v, want false", decoded["generate"])
	}
	streamOptions, ok := decoded["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want object", decoded["stream_options"])
	}
	if got, want := streamOptions["reasoning_summary_delivery"], "sequential_cutoff"; got != want {
		t.Fatalf("reasoning_summary_delivery = %#v, want %q", got, want)
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
	}, func() []ResponsesInputItem {
		return []ResponsesInputItem{{Type: "message", Role: "user", Content: "new"}}
	}, func() []ResponsesInputItem {
		return fullInput
	})

	if prepared.PreviousResponseID != "" {
		t.Fatalf("PreviousResponseID = %q, want empty", prepared.PreviousResponseID)
	}
	if len(prepared.Input) != len(fullInput) || prepared.Input[0].Content != "old" || prepared.Input[1].Content != "new" {
		t.Fatalf("Input = %#v, want full input %#v", prepared.Input, fullInput)
	}
}

func TestResponsesWebSocketPrepareUsesContinuationWithoutFullInputRebuild(t *testing.T) {
	client := &ResponsesClient{
		LastResponseID: "resp_prev",
		wsLastRequest: &ResponsesRequest{
			Model: "gpt-test",
			Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "old"}},
		},
	}
	fullInputCalls := 0

	prepared := client.prepareWebSocketContinuationLocked(ResponsesRequest{Model: "gpt-test"}, func() []ResponsesInputItem {
		return []ResponsesInputItem{{Type: "message", Role: "user", Content: "new"}}
	}, func() []ResponsesInputItem {
		fullInputCalls++
		return []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "old"},
			{Type: "message", Role: "assistant", Content: "done"},
			{Type: "message", Role: "user", Content: "new"},
		}
	})

	if fullInputCalls != 0 {
		t.Fatalf("buildFullInput calls = %d, want 0", fullInputCalls)
	}
	if prepared.PreviousResponseID != "resp_prev" {
		t.Fatalf("PreviousResponseID = %q, want resp_prev", prepared.PreviousResponseID)
	}
	if len(prepared.Input) != 1 || prepared.Input[0].Content != "new" {
		t.Fatalf("Input = %#v, want only continuation input", prepared.Input)
	}
}

func TestResponsesWebSocketCompletionStoresLightweightLastRequest(t *testing.T) {
	var gotReq map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_next"}})
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:              server.URL,
		UseWebSocket:         true,
		WebSocketServerState: true,
		DisableServerState:   true,
		LastResponseID:       "resp_prev",
		wsLastRequest: &ResponsesRequest{
			Model:        "gpt-test",
			Instructions: "Be concise",
			Input:        []ResponsesInputItem{{Type: "message", Role: "user", Content: "old"}},
			Messages:     []Message{UserText("old")},
		},
	}
	var fullInputCalls atomic.Int32
	stream, err := client.streamWebSocketPrepared(context.Background(), ResponsesRequest{
		Model:        "gpt-test",
		Instructions: "Be concise",
		Messages: []Message{
			UserText("one"),
			AssistantText("old"),
			UserText("two"),
		},
		Stream: true,
	}, func() []ResponsesInputItem {
		return []ResponsesInputItem{{Type: "message", Role: "user", Content: "two"}}
	}, func() []ResponsesInputItem {
		fullInputCalls.Add(1)
		return []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "one"},
			{Type: "message", Role: "assistant", Content: "old"},
			{Type: "message", Role: "user", Content: "two"},
		}
	}, false, 0)
	if err != nil {
		t.Fatalf("streamWebSocketPrepared: %v", err)
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
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if fullInputCalls.Load() != 1 {
		t.Fatalf("buildFullInput calls = %d, want 1 for fresh connection", fullInputCalls.Load())
	}
	if _, ok := gotReq["previous_response_id"]; ok {
		t.Fatalf("fresh connection sent previous_response_id: %#v", gotReq)
	}
	input, ok := gotReq["input"].([]any)
	if !ok || len(input) != 3 || !strings.Contains(toJSON(input[0]), "one") || !strings.Contains(toJSON(input[2]), "two") {
		t.Fatalf("input = %#v, want full transcript on fresh connection", gotReq["input"])
	}

	client.wsMu.Lock()
	lastReq := client.wsLastRequest
	client.wsMu.Unlock()
	if lastReq == nil {
		t.Fatal("wsLastRequest = nil, want lightweight metadata")
	}
	if lastReq.Input != nil {
		t.Fatalf("wsLastRequest.Input = %#v, want nil", lastReq.Input)
	}
	if lastReq.Messages != nil {
		t.Fatalf("wsLastRequest.Messages = %#v, want nil", lastReq.Messages)
	}
	if lastReq.PreviousResponseID != "" {
		t.Fatalf("wsLastRequest.PreviousResponseID = %q, want empty", lastReq.PreviousResponseID)
	}
	if lastReq.Model != "gpt-test" || lastReq.Instructions != "Be concise" {
		t.Fatalf("wsLastRequest metadata = %#v, want model/instructions preserved", lastReq)
	}
}

func TestResponsesRequestNonInputEqual_JSONLikeTools(t *testing.T) {
	previous := ResponsesRequest{
		Model: "gpt-test",
		Tools: []any{ResponsesTool{
			Type: "function",
			Name: "tool",
			Parameters: map[string]any{
				"type":     "object",
				"required": []string{"b", "a"},
			},
		}},
		ToolChoice: map[string]any{"name": "tool", "type": "function"},
	}
	current := ResponsesRequest{
		Model: "gpt-test",
		Tools: []any{map[string]any{
			"name": "tool",
			"type": "function",
			"parameters": map[string]any{
				"required": []any{"a", "b"},
				"type":     "object",
			},
		}},
		ToolChoice: BuildResponsesToolChoice(ToolChoice{Mode: ToolChoiceName, Name: "tool"}),
	}

	if !responsesRequestNonInputEqual(previous, current) {
		t.Fatalf("expected requests to compare equal: previous=%#v current=%#v", previous, current)
	}
}

func TestResponsesRequestNonInputEqual_ComparesStreamOptions(t *testing.T) {
	previous := ResponsesRequest{
		Model: "gpt-test",
		StreamOptions: &ResponsesStreamOptions{
			ReasoningSummaryDelivery: "sequential_cutoff",
		},
	}
	current := ResponsesRequest{
		Model: "gpt-test",
		StreamOptions: &ResponsesStreamOptions{
			ReasoningSummaryDelivery: "",
		},
	}

	if responsesRequestNonInputEqual(previous, current) {
		t.Fatal("expected requests with different stream_options to compare unequal")
	}
}

func TestJSONLikeEqualForCompareUsesMarshalJSONForCustomMarshalers(t *testing.T) {
	var calls atomic.Int32
	value := marshalCountingJSONLikeValue{Calls: &calls}

	if !jsonLikeEqualForCompare(value, value) {
		t.Fatal("expected identical marshaled values to compare equal")
	}
	if calls.Load() == 0 {
		t.Fatal("expected comparison to honor MarshalJSON for custom marshalers")
	}
	if jsonLikeEqualForCompare(dynamicMarshalJSONLikeValue{Version: 1}, dynamicMarshalJSONLikeValue{Version: 2}) {
		t.Fatal("expected custom marshalers with different wire JSON to compare unequal")
	}
	if !jsonLikeEqualForCompare(dynamicMarshalJSONLikeValue{Version: 1}, map[string]any{"type": "function", "name": "tool", "version": float64(1)}) {
		t.Fatal("expected custom marshaler to compare against equivalent wire JSON")
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
		Model:       "gpt-test",
		Input:       []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Tools:       []any{ResponsesTool{Type: "function", Name: "tool", Parameters: map[string]any{"type": "object"}}},
		Stream:      true,
		ServiceTier: ServiceTierFast,
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
			if gotReq["service_tier"] != ServiceTierFast {
				t.Fatalf("service_tier = %#v, want %q", gotReq["service_tier"], ServiceTierFast)
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

func TestResponsesClientWebSocketFirstEventTimeoutIgnoresServerPingsAndFallsBackToHTTP(t *testing.T) {
	const (
		firstEventTimeout = 40 * time.Millisecond
		pingInterval      = 5 * time.Millisecond
	)

	var wsAttempts atomic.Int32
	var httpAttempts atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			httpAttempts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.output_text.delta\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n"))
			_, _ = w.Write([]byte("event: response.completed\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n"))
			return
		}

		wsAttempts.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := conn.WriteControl(websocket.PingMessage, []byte("heartbeat"), time.Now().Add(time.Second)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:                    server.URL,
		HTTPClient:                 server.Client(),
		UseWebSocket:               true,
		WebSocketIdleTimeout:       time.Second,
		WebSocketFirstEventTimeout: firstEventTimeout,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := client.Stream(ctx, ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text string
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventDone:
			if text != "fallback" {
				t.Fatalf("text = %q, want fallback", text)
			}
			if client.websocketDisabled {
				t.Fatal("transient first-event timeout permanently disabled WebSocket transport")
			}
			if got := wsAttempts.Load(); got != 1 {
				t.Fatalf("websocket attempts = %d, want 1 before direct HTTP fallback", got)
			}
			if got := httpAttempts.Load(); got != 1 {
				t.Fatalf("http attempts = %d, want 1", got)
			}
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketServerPingsKeepStreamAlive(t *testing.T) {
	const (
		idleTimeout  = 100 * time.Millisecond
		pingInterval = 20 * time.Millisecond
		pingCount    = 15
	)

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}}); err != nil {
			t.Errorf("write response.created: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "started"}); err != nil {
			t.Errorf("write initial delta: %v", err)
			return
		}

		pongs := make(chan struct{}, 1)
		conn.SetPongHandler(func(string) error {
			pongs <- struct{}{}
			return nil
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for i := 0; i < pingCount; i++ {
			time.Sleep(pingInterval)
			if err := conn.WriteControl(websocket.PingMessage, []byte("heartbeat"), time.Now().Add(time.Second)); err != nil {
				return
			}
			select {
			case <-pongs:
			case <-time.After(time.Second):
				t.Errorf("timed out waiting for pong %d", i)
				return
			}
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := &ResponsesClient{
		BaseURL:              server.URL,
		UseWebSocket:         true,
		WebSocketIdleTimeout: idleTimeout,
	}
	stream, err := client.Stream(ctx, ResponsesRequest{
		Model:          "gpt-test",
		Input:          []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream:         true,
		ForceWebSocket: true,
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
		switch event.Type {
		case EventDone:
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketParkedConnectionExpires(t *testing.T) {
	const parkedTimeout = 30 * time.Millisecond

	var handshakes atomic.Int32
	firstClosed := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		attempt := handshakes.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", attempt)}}); err != nil {
			return
		}
		if attempt == 1 {
			_, _, _ = conn.ReadMessage()
			close(firstClosed)
		}
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:                server.URL,
		UseWebSocket:           true,
		WebSocketParkedTimeout: parkedTimeout,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "first"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	drainStreamToDone(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}
	select {
	case <-firstClosed:
	case <-ctx.Done():
		t.Fatalf("parked WebSocket did not close: %v", ctx.Err())
	}

	second, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "second"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	drainStreamToDone(t, second)
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("WebSocket handshakes = %d, want reconnect after parked timeout", got)
	}
}

func TestResponsesClientWebSocketServerPingReceivesPongWhileIdle(t *testing.T) {
	const idleTimeout = 40 * time.Millisecond

	var handshakeCount atomic.Int32
	pingWhileIdle := make(chan struct{}, 1)
	pongReceived := make(chan string, 1)
	serverDone := make(chan struct{})
	var serverDoneOnce sync.Once

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer serverDoneOnce.Do(func() { close(serverDone) })
		handshakeCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read first request: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}}); err != nil {
			t.Errorf("write first completion: %v", err)
			return
		}

		<-pingWhileIdle
		conn.SetPongHandler(func(appData string) error {
			select {
			case pongReceived <- appData:
			default:
			}
			return nil
		})
		if err := conn.WriteControl(websocket.PingMessage, []byte("idle-heartbeat"), time.Now().Add(5*time.Second)); err != nil {
			t.Errorf("write idle ping: %v", err)
			return
		}

		// ReadMessage drives the server-side pong handler and then returns the
		// second response.create on the same connection.
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read second request: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2"}}); err != nil {
			t.Errorf("write second completion: %v", err)
		}
	}))
	defer server.Close()
	defer func() {
		select {
		case pingWhileIdle <- struct{}{}:
		default:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &ResponsesClient{
		BaseURL:              server.URL,
		UseWebSocket:         true,
		WebSocketIdleTimeout: idleTimeout,
	}
	first, err := client.Stream(ctx, ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "first"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	drainStreamToDone(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}

	parked := time.NewTimer(5 * idleTimeout)
	select {
	case <-parked.C:
	case <-ctx.Done():
		parked.Stop()
		t.Fatalf("waiting beyond the active-response idle timeout: %v", ctx.Err())
	}
	pingWhileIdle <- struct{}{}
	select {
	case pong := <-pongReceived:
		if pong != "idle-heartbeat" {
			t.Fatalf("pong payload = %q, want idle-heartbeat", pong)
		}
	case <-ctx.Done():
		t.Fatalf("idle ping was not answered: %v", ctx.Err())
	}

	second, err := client.Stream(ctx, ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "second"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	drainStreamToDone(t, second)
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatalf("server did not finish: %v", ctx.Err())
	}
	if got := handshakeCount.Load(); got != 1 {
		t.Fatalf("WebSocket handshakes = %d, want 1 reused connection", got)
	}
}

func TestResponsesClientWebSocketIdleCloseReconnectsNextRequest(t *testing.T) {
	var handshakeCount atomic.Int32
	closeWhileIdle := make(chan struct{}, 1)
	closeObserved := make(chan error, 1)

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshake := handshakeCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade %d: %v", handshake, err)
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read request %d: %v", handshake, err)
			return
		}
		var wireReq map[string]any
		if err := json.Unmarshal(msg, &wireReq); err != nil {
			t.Errorf("decode request %d: %v", handshake, err)
			return
		}
		if handshake == 2 {
			if previousResponseID, ok := wireReq["previous_response_id"]; ok {
				_ = conn.WriteJSON(map[string]any{
					"type":   "response.failed",
					"status": 400,
					"response": map[string]any{
						"error": map[string]any{"code": "invalid_request_error", "message": "Invalid previous_response_id"},
					},
				})
				t.Errorf("reconnected request sent connection-local previous_response_id %#v", previousResponseID)
				return
			}
			input, ok := wireReq["input"].([]any)
			if !ok || len(input) != 3 || !strings.Contains(toJSON(input[0]), "first") || !strings.Contains(toJSON(input[2]), "second") {
				t.Errorf("reconnected input = %#v, want full three-item transcript", wireReq["input"])
				return
			}
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", handshake)}}); err != nil {
			t.Errorf("write completion %d: %v", handshake, err)
			return
		}
		if handshake != 1 {
			return
		}

		<-closeWhileIdle
		if err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "idle close"), time.Now().Add(5*time.Second)); err != nil {
			t.Errorf("write idle close: %v", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, readErr := conn.ReadMessage() // Wait until the client's read pump replies to the close.
		closeObserved <- readErr
	}))
	defer server.Close()
	defer func() {
		select {
		case closeWhileIdle <- struct{}{}:
		default:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &ResponsesClient{
		BaseURL:              server.URL,
		UseWebSocket:         true,
		DisableServerState:   true,
		WebSocketServerState: true,
	}
	first, err := client.Stream(ctx, ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "first"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	drainStreamToDone(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}

	closeWhileIdle <- struct{}{}
	select {
	case closeErr := <-closeObserved:
		if !websocket.IsCloseError(closeErr, websocket.CloseNormalClosure) {
			t.Fatalf("client did not service idle close with a close reply: %v", closeErr)
		}
	case <-ctx.Done():
		t.Fatalf("idle close was not serviced: %v", ctx.Err())
	}

	second, err := client.Stream(ctx, ResponsesRequest{
		Model: "gpt-test",
		Input: []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "first"},
			{Type: "message", Role: "assistant", Content: "first response"},
			{Type: "message", Role: "user", Content: "second"},
		},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	drainStreamToDone(t, second)
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
	if got := handshakeCount.Load(); got != 2 {
		t.Fatalf("WebSocket handshakes = %d, want reconnect after idle close", got)
	}
}

func TestResponsesClientWebSocketApplicationFrameWhileIdleReconnects(t *testing.T) {
	tests := []struct {
		name  string
		frame map[string]any
	}{
		{
			name: "generic error",
			frame: map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "server_error", "message": "idle server error"},
			},
		},
		{
			name: "connection limit reached",
			frame: map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "websocket_connection_limit_reached", "message": "connection expired"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var handshakeCount atomic.Int32
			sendIdleFrame := make(chan struct{})
			idleConnectionClosed := make(chan error, 1)
			secondRequest := make(chan map[string]any, 1)

			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handshake := handshakeCount.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade %d: %v", handshake, err)
					return
				}
				defer conn.Close()

				_, msg, err := conn.ReadMessage()
				if err != nil {
					t.Errorf("read request %d: %v", handshake, err)
					return
				}
				if handshake == 1 {
					if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}}); err != nil {
						t.Errorf("write first completion: %v", err)
						return
					}
					<-sendIdleFrame
					if err := conn.WriteJSON(tt.frame); err != nil {
						t.Errorf("write idle frame: %v", err)
						return
					}
					_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, _, err = conn.ReadMessage()
					idleConnectionClosed <- err
					return
				}

				var wireReq map[string]any
				if err := json.Unmarshal(msg, &wireReq); err != nil {
					t.Errorf("decode reconnected request: %v", err)
					return
				}
				secondRequest <- wireReq
				if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2"}}); err != nil {
					t.Errorf("write second completion: %v", err)
				}
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := &ResponsesClient{
				BaseURL:              server.URL,
				UseWebSocket:         true,
				DisableServerState:   true,
				WebSocketServerState: true,
			}
			defer client.closeWebSocket()

			first, err := client.Stream(ctx, ResponsesRequest{
				Model:  "gpt-test",
				Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "first"}},
				Stream: true,
			}, false)
			if err != nil {
				t.Fatalf("first Stream: %v", err)
			}
			drainStreamToDone(t, first)
			if err := first.Close(); err != nil {
				t.Fatalf("close first stream: %v", err)
			}

			close(sendIdleFrame)
			select {
			case closeErr := <-idleConnectionClosed:
				if closeErr == nil {
					t.Fatal("idle application frame did not make the connection close")
				}
			case <-ctx.Done():
				t.Fatalf("idle application frame was not discarded: %v", ctx.Err())
			}

			second, err := client.Stream(ctx, ResponsesRequest{
				Model: "gpt-test",
				Input: []ResponsesInputItem{
					{Type: "message", Role: "user", Content: "first"},
					{Type: "message", Role: "assistant", Content: "first response"},
					{Type: "message", Role: "user", Content: "second"},
				},
				Stream: true,
			}, false)
			if err != nil {
				t.Fatalf("second Stream: %v", err)
			}
			drainStreamToDone(t, second)
			if err := second.Close(); err != nil {
				t.Fatalf("close second stream: %v", err)
			}

			var wireReq map[string]any
			select {
			case wireReq = <-secondRequest:
			case <-ctx.Done():
				t.Fatalf("reconnected request was not observed: %v", ctx.Err())
			}
			if _, ok := wireReq["previous_response_id"]; ok {
				t.Fatalf("reconnected request sent connection-local previous_response_id: %#v", wireReq)
			}
			input, ok := wireReq["input"].([]any)
			if !ok || len(input) != 3 || !strings.Contains(toJSON(input[0]), "first") || !strings.Contains(toJSON(input[2]), "second") {
				t.Fatalf("reconnected input = %#v, want full three-item transcript", wireReq["input"])
			}
			if got := handshakeCount.Load(); got != 2 {
				t.Fatalf("WebSocket handshakes = %d, want reconnect after idle application frame", got)
			}
		})
	}
}

func TestResponsesClientWebSocketTrailingFrameAfterCompletionReconnects(t *testing.T) {
	var handshakeCount atomic.Int32
	firstConnectionClosed := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshake := handshakeCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade %d: %v", handshake, err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read request %d: %v", handshake, err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", handshake)}}); err != nil {
			t.Errorf("write completion %d: %v", handshake, err)
			return
		}
		if handshake != 1 {
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"type":  "error",
			"error": map[string]any{"code": "server_error", "message": "trailing error"},
		}); err != nil {
			t.Errorf("write trailing frame: %v", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err = conn.ReadMessage()
		firstConnectionClosed <- err
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	defer client.closeWebSocket()
	request := ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "request"}},
		Stream: true,
	}

	first, err := client.Stream(ctx, request, false)
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	drainStreamToDone(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}
	select {
	case closeErr := <-firstConnectionClosed:
		if closeErr == nil {
			t.Fatal("trailing application frame did not make the connection close")
		}
	case <-ctx.Done():
		t.Fatalf("trailing application frame was not discarded: %v", ctx.Err())
	}

	second, err := client.Stream(ctx, request, false)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	drainStreamToDone(t, second)
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
	if got := handshakeCount.Load(); got != 2 {
		t.Fatalf("WebSocket handshakes = %d, want reconnect after trailing frame", got)
	}
}

func TestResponsesClientWebSocketWriteFailureReconnectSendsFullState(t *testing.T) {
	withResponsesWebSocketBaseBackoff(t, 0)

	var handshakeCount atomic.Int32
	releaseFirstConnection := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshake := handshakeCount.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade %d: %v", handshake, err)
			return
		}
		defer conn.Close()

		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read request %d: %v", handshake, err)
			return
		}
		var wireReq map[string]any
		if err := json.Unmarshal(msg, &wireReq); err != nil {
			t.Errorf("decode request %d: %v", handshake, err)
			return
		}
		if handshake == 2 {
			if previousResponseID, ok := wireReq["previous_response_id"]; ok {
				t.Errorf("write-failure reconnect sent connection-local previous_response_id %#v", previousResponseID)
				return
			}
			input, ok := wireReq["input"].([]any)
			if !ok || len(input) != 3 || !strings.Contains(toJSON(input[0]), "first") || !strings.Contains(toJSON(input[2]), "second") {
				t.Errorf("write-failure reconnect input = %#v, want full three-item transcript", wireReq["input"])
				return
			}
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", handshake)}}); err != nil {
			t.Errorf("write completion %d: %v", handshake, err)
			return
		}
		if handshake == 1 {
			<-releaseFirstConnection
		}
	}))
	defer server.Close()
	defer close(releaseFirstConnection)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &ResponsesClient{
		BaseURL:              server.URL,
		UseWebSocket:         true,
		DisableServerState:   true,
		WebSocketServerState: true,
	}
	first, err := client.Stream(ctx, ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "first"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	drainStreamToDone(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("close first stream: %v", err)
	}

	client.wsMu.Lock()
	conn := client.wsConn
	if conn != nil {
		err = conn.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "poison next write"), time.Now().Add(5*time.Second))
	}
	client.wsMu.Unlock()
	if conn == nil {
		t.Fatal("WebSocket connection was not retained after first stream")
	}
	if err != nil {
		t.Fatalf("poison WebSocket write path: %v", err)
	}

	second, err := client.Stream(ctx, ResponsesRequest{
		Model: "gpt-test",
		Input: []ResponsesInputItem{
			{Type: "message", Role: "user", Content: "first"},
			{Type: "message", Role: "assistant", Content: "first response"},
			{Type: "message", Role: "user", Content: "second"},
		},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	retries := 0
	for {
		event, recvErr := second.Recv()
		if recvErr != nil {
			t.Fatalf("second Recv: %v", recvErr)
		}
		switch event.Type {
		case EventRetry:
			retries++
		case EventDone:
			goto secondDone
		case EventError:
			t.Fatalf("second stream error: %v", event.Err)
		}
	}

secondDone:
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
	if retries != 1 {
		t.Fatalf("retry events = %d, want 1 for reused write failure", retries)
	}
	if got := handshakeCount.Load(); got != 2 {
		t.Fatalf("WebSocket handshakes = %d, want one write-failure reconnect", got)
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
	withResponsesWebSocketBaseBackoff(t, 0)

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
			if wsAttempts.Load() != int32(responsesWebSocketMaxAttempts) {
				t.Fatalf("websocket attempts = %d, want %d", wsAttempts.Load(), responsesWebSocketMaxAttempts)
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
	withResponsesWebSocketBaseBackoff(t, 0)

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
			if wsAttempts.Load() != int32(responsesWebSocketMaxAttempts) || httpAttempts.Load() != 1 {
				t.Fatalf("attempts ws=%d http=%d, want %d/1", wsAttempts.Load(), httpAttempts.Load(), responsesWebSocketMaxAttempts)
			}
			if retries != responsesWebSocketMaxRetries {
				t.Fatalf("retry events = %d, want %d", retries, responsesWebSocketMaxRetries)
			}
			return
		case EventError:
			t.Fatalf("stream error: %v", event.Err)
		}
	}
}

func TestResponsesClientWebSocketReadFailureBeforeEventsRetryCanRecover(t *testing.T) {
	withResponsesWebSocketBaseBackoff(t, 0)

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
	withResponsesWebSocketBaseBackoff(t, 0)

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

func TestResponsesClientWebSocketMalformedCompletedReturnsIncomplete(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			t.Fatal("HTTP fallback should not be used after visible WebSocket output")
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "partial"})
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":`))
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true}
	stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil || event.Type != EventTextDelta || event.Text != "partial" {
		t.Fatalf("first event = %#v, err=%v, want partial text", event, err)
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
	if !strings.Contains(event.Err.Error(), "decode Responses API response.completed event") {
		t.Fatalf("incomplete stream missing decode cause: %v", event.Err)
	}
}

func TestResponsesClientWebSocketBackoffHonorsContextCancellation(t *testing.T) {
	withResponsesWebSocketBaseBackoff(t, time.Hour)

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
	withResponsesWebSocketBaseBackoff(t, 0)

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
	tests := []struct {
		name     string
		apiError map[string]any
	}{
		{
			name: "structured previous response not found",
			apiError: map[string]any{
				"code":    "previous_response_not_found",
				"message": "Previous response not found",
				"param":   "previous_response_id",
			},
		},
		{
			name: "invalid previous response id",
			apiError: map[string]any{
				"code":    "invalid_request_error",
				"message": "Invalid previous_response_id",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secondRequest := make(chan map[string]any, 1)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer conn.Close()

				// First stream establishes a response id.
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Errorf("read first request: %v", err)
					return
				}
				if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}}); err != nil {
					t.Errorf("write first completion: %v", err)
					return
				}

				// Second stream first attempts connection-local continuation.
				_, msg, err := conn.ReadMessage()
				if err != nil {
					t.Errorf("read incremental request: %v", err)
					return
				}
				var incremental map[string]any
				if err := json.Unmarshal(msg, &incremental); err != nil {
					t.Errorf("decode incremental request: %v", err)
					return
				}
				if incremental["previous_response_id"] != "resp_1" {
					t.Errorf("incremental previous_response_id = %#v", incremental["previous_response_id"])
					return
				}
				if err := conn.WriteJSON(map[string]any{
					"type":   "response.failed",
					"status": 400,
					"response": map[string]any{
						"error": tt.apiError,
					},
				}); err != nil {
					t.Errorf("write continuation rejection: %v", err)
					return
				}

				// Client should retry the same turn as full state on this connection.
				_, msg, err = conn.ReadMessage()
				if err != nil {
					t.Errorf("read full-state retry: %v", err)
					return
				}
				var fullState map[string]any
				if err := json.Unmarshal(msg, &fullState); err != nil {
					t.Errorf("decode full-state retry: %v", err)
					return
				}
				secondRequest <- fullState
				if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_2"}}); err != nil {
					t.Errorf("write second completion: %v", err)
				}
			}))
			defer server.Close()

			client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, WebSocketServerState: true, DisableServerState: true}
			defer client.closeWebSocket()
			discoveryCall := ResponsesInputItem{Raw: json.RawMessage(`{"type":"tool_search_call","execution":"client","call_id":"search-1","status":"completed","arguments":{"query":"eta"}}`)}
			discoveryOutput := ResponsesInputItem{Raw: json.RawMessage(`{"type":"tool_search_output","execution":"client","call_id":"search-1","status":"completed","tools":[{"type":"function","name":"eta","description":"eta","defer_loading":true,"parameters":{"type":"object"}}]}`)}
			for _, input := range [][]ResponsesInputItem{
				{{Type: "message", Role: "user", Content: "one"}},
				{{Type: "message", Role: "user", Content: "one"}, discoveryCall, discoveryOutput, {Type: "message", Role: "user", Content: "two"}},
			} {
				stream, err := client.Stream(context.Background(), ResponsesRequest{Model: "gpt-test", Input: input, Stream: true}, false)
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				drainStreamToDone(t, stream)
				if err := stream.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}

			fullState := <-secondRequest
			if _, ok := fullState["previous_response_id"]; ok {
				t.Fatalf("full-state retry still had previous_response_id: %#v", fullState)
			}
			input, ok := fullState["input"].([]any)
			if !ok || len(input) != 4 {
				t.Fatalf("full-state retry input = %#v, want message plus discovery call/output plus continuation", fullState["input"])
			}
			var types []string
			for _, value := range input {
				item, _ := value.(map[string]any)
				types = append(types, fmt.Sprint(item["type"]))
			}
			if got := strings.Join(types, ","); got != "message,tool_search_call,tool_search_output,message" {
				t.Fatalf("full-state retry item types = %s", got)
			}
		})
	}
}

func TestResponsesClientWebSocketReconnectsWhenSessionIDChanges(t *testing.T) {
	var handshakeCount atomic.Int32
	var sessionIDs []string
	var requests []map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionIDs = append(sessionIDs, r.Header.Get("session_id"))
		handshake := handshakeCount.Add(1)
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
		var req map[string]any
		if err := json.Unmarshal(msg, &req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, req)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", handshake)}})
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, WebSocketServerState: true, DisableServerState: true}
	for _, sessionID := range []string{"sess-one", "sess-two"} {
		stream, err := client.Stream(context.Background(), ResponsesRequest{
			Model:     "gpt-test",
			SessionID: sessionID,
			Input:     []ResponsesInputItem{{Type: "message", Role: "user", Content: sessionID}},
			Stream:    true,
		}, false)
		if err != nil {
			t.Fatalf("Stream %s: %v", sessionID, err)
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				t.Fatalf("Recv %s: %v", sessionID, err)
			}
			if event.Type == EventDone {
				break
			}
		}
		_ = stream.Close()
	}

	if handshakeCount.Load() != 2 {
		t.Fatalf("handshakes = %d, want 2", handshakeCount.Load())
	}
	if len(sessionIDs) != 2 || sessionIDs[0] != "sess-one" || sessionIDs[1] != "sess-two" {
		t.Fatalf("session headers = %#v, want [sess-one sess-two]", sessionIDs)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if _, ok := requests[1]["previous_response_id"]; ok {
		t.Fatalf("second session request reused stale previous_response_id: %#v", requests[1])
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) != 1 || !strings.Contains(toJSON(input[0]), "sess-two") {
		t.Fatalf("second session input = %#v, want full input for sess-two", requests[1]["input"])
	}
}

func TestResponsesClientWebSocketReusesConnectionAndPreviousResponseIDWithInstructionExtraction(t *testing.T) {
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

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, WebSocketServerState: true, DisableServerState: true}
	requestsToSend := []ResponsesRequest{
		{
			Model:                           "gpt-test",
			Instructions:                    "Be concise",
			Messages:                        []Message{SystemText("Be concise"), UserText("one")},
			ExtractInstructionsFromMessages: true,
			Stream:                          true,
		},
		{
			Model:        "gpt-test",
			Instructions: "Be concise",
			Messages: []Message{
				SystemText("Be concise"),
				UserText("one"),
				AssistantText("old"),
				UserText("two"),
			},
			ExtractInstructionsFromMessages: true,
			Stream:                          true,
		},
	}
	for i, req := range requestsToSend {
		stream, err := client.Stream(context.Background(), req, false)
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
	if handshakeCount.Load() != 1 {
		t.Fatalf("handshakes = %d, want 1", handshakeCount.Load())
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if requests[0]["instructions"] != "Be concise" {
		t.Fatalf("first instructions = %#v, want Be concise", requests[0]["instructions"])
	}
	firstInput, ok := requests[0]["input"].([]any)
	if !ok || len(firstInput) != 1 || strings.Contains(toJSON(firstInput[0]), "developer") || !strings.Contains(toJSON(firstInput[0]), "one") {
		t.Fatalf("first input = %#v, want only user message without duplicated system prompt", requests[0]["input"])
	}
	if requests[1]["previous_response_id"] != "resp_1" {
		t.Fatalf("previous_response_id = %#v", requests[1]["previous_response_id"])
	}
	secondInput, ok := requests[1]["input"].([]any)
	if !ok || len(secondInput) != 1 || !strings.Contains(toJSON(secondInput[0]), "two") {
		t.Fatalf("second input = %#v, want only newest user item", requests[1]["input"])
	}
}

func TestResponsesClientWebSocketUsesPreviousResponseIDWithoutLocalBaseline(t *testing.T) {
	var gotReq map[string]any
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_next"}})
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:        server.URL,
		UseWebSocket:   true,
		LastResponseID: "resp_prev",
	}
	stream, err := client.Stream(context.Background(), ResponsesRequest{
		Model: "gpt-test",
		Input: []ResponsesInputItem{
			{Type: "message", Role: "assistant", Content: "old"},
			{Type: "message", Role: "user", Content: "new"},
		},
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

	if gotReq["previous_response_id"] != "resp_prev" {
		t.Fatalf("previous_response_id = %#v, want resp_prev", gotReq["previous_response_id"])
	}
	input, ok := gotReq["input"].([]any)
	if !ok || len(input) != 1 || !strings.Contains(toJSON(input[0]), "new") {
		t.Fatalf("input = %#v, want only continuation input", gotReq["input"])
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
