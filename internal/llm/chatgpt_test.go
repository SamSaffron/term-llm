package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestBuildResponsesInputWithInstructions_ExtractsSystem(t *testing.T) {
	t.Parallel()

	messages := []Message{
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "You are helpful."}}},
		{Role: RoleSystem, Parts: []Part{{Type: PartText, Text: "Be concise."}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Hello"}}},
	}

	instructions, input := BuildResponsesInputWithInstructions(messages)

	if instructions != "You are helpful.\n\nBe concise." {
		t.Fatalf("expected joined system instructions, got %q", instructions)
	}

	// Should only have the user message, no developer-role items
	if len(input) != 1 {
		t.Fatalf("expected 1 input item (user message only), got %d", len(input))
	}
	if input[0].Role != "user" {
		t.Fatalf("expected user role, got %q", input[0].Role)
	}
}

func TestChatGPTHTTPClient_DoesNotUseClientTimeout(t *testing.T) {
	t.Parallel()

	if chatGPTHTTPClient.Timeout != 0 {
		t.Fatalf("expected no http.Client.Timeout for ChatGPT streaming client, got %s", chatGPTHTTPClient.Timeout)
	}
}

func TestNewChatGPTProviderWithCredsDefaultsToGPT56SolMedium(t *testing.T) {
	t.Parallel()

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "")

	if provider.model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want %q", provider.model, "gpt-5.6-sol")
	}
	if provider.effort != "medium" {
		t.Fatalf("effort = %q, want %q", provider.effort, "medium")
	}
}

func TestNewChatGPTResponsesClientUsesCurrentCodexHeaders(t *testing.T) {
	t.Parallel()

	client := NewChatGPTResponsesClient(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
	})
	if got := client.ExtraHeaders["OpenAI-Beta"]; got != "" {
		t.Fatalf("legacy OpenAI-Beta header = %q, want omitted", got)
	}
	if got := client.ExtraHeaders["originator"]; got != chatGPTCodexOriginator {
		t.Fatalf("originator = %q, want %q", got, chatGPTCodexOriginator)
	}
	if got := client.ExtraHeaders["User-Agent"]; got != chatGPTCodexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, chatGPTCodexUserAgent)
	}
	if got := client.ExtraHeaders["version"]; got != chatGPTCodexClientVersion {
		t.Fatalf("version = %q, want %q", got, chatGPTCodexClientVersion)
	}
}

func TestChatGPTStream_OmitsPromptCacheKeyButKeepsSessionHeader(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var body []byte
	var sessionID string
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sessionID = req.Header.Get("session_id")
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				`data: [DONE]`,
			}, "\n"))),
			Header: make(http.Header),
		}, nil
	})}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.6-sol-medium")

	stream, err := provider.Stream(context.Background(), Request{
		SessionID: "chatgpt-session-123",
		Messages:  []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if sessionID != "chatgpt-session-123" {
		t.Fatalf("session_id header = %q, want %q", sessionID, "chatgpt-session-123")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if value, ok := payload["prompt_cache_key"]; ok {
		t.Fatalf("ChatGPT request contains prompt_cache_key = %#v", value)
	}
}

func TestChatGPTStream_CompactionAnchorDoesNotEmitPromptCacheBreakpoint(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var body []byte
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				`data: [DONE]`,
			}, "\n"))),
			Header: make(http.Header),
		}, nil
	})}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.6-sol-medium")
	messages := []Message{{
		Role:        RoleUser,
		CacheAnchor: true,
		Parts:       []Part{{Type: PartText, Text: "[Context Compaction]\nsummary"}},
	}}

	stream, err := provider.Stream(context.Background(), Request{Messages: messages})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if strings.Contains(string(body), "prompt_cache_breakpoint") {
		t.Fatalf("ChatGPT request contains unsupported prompt_cache_breakpoint: %s", body)
	}
	if !messages[0].CacheAnchor {
		t.Fatal("Stream mutated the caller's cache anchor")
	}
}

func TestChatGPTStream_IncludesNormalizedServiceTier(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var captured ResponsesRequest
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("originator"); got != chatGPTCodexOriginator {
			t.Fatalf("originator = %q, want %q", got, chatGPTCodexOriginator)
		}
		if got := req.Header.Get("User-Agent"); got != chatGPTCodexUserAgent {
			t.Fatalf("User-Agent = %q, want %q", got, chatGPTCodexUserAgent)
		}
		if got := req.Header.Get("version"); got != chatGPTCodexClientVersion {
			t.Fatalf("version = %q, want %q", got, chatGPTCodexClientVersion)
		}
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				`data: [DONE]`,
			}, "\n"))),
			Header: make(http.Header),
		}, nil
	})}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "gpt-5.5-medium")

	stream, err := provider.Stream(context.Background(), Request{
		Messages:    []Message{UserText("hello")},
		ServiceTier: "fast",
	})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if captured.ServiceTier != ServiceTierFast {
		t.Fatalf("service_tier = %q, want %q", captured.ServiceTier, ServiceTierFast)
	}
}

func TestChatGPTStream_IncludesProviderDefaultServiceTier(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var captured ResponsesRequest
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				`data: [DONE]`,
			}, "\n"))),
			Header: make(http.Header),
		}, nil
	})}

	provider := NewChatGPTProviderWithCredsAndOptions(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "gpt-5.5-medium", ChatGPTProviderOptions{ServiceTier: "fast"})

	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if captured.ServiceTier != ServiceTierFast {
		t.Fatalf("service_tier = %q, want %q", captured.ServiceTier, ServiceTierFast)
	}
}

func TestChatGPTStream_ServiceTierOverrideCanClearProviderDefault(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var captured ResponsesRequest
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				`data: [DONE]`,
			}, "\n"))),
			Header: make(http.Header),
		}, nil
	})}

	provider := NewChatGPTProviderWithCredsAndOptions(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "gpt-5.5-medium", ChatGPTProviderOptions{ServiceTier: "fast"})

	stream, err := provider.Stream(context.Background(), Request{
		Messages:       []Message{UserText("hello")},
		ServiceTierSet: true,
	})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if captured.ServiceTier != "" {
		t.Fatalf("service_tier = %q, want omitted", captured.ServiceTier)
	}
}

func TestChatGPTStream_OmitsReasoningSummaryDeliveryOverride(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var captured map[string]any
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(strings.Join([]string{
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				`data: [DONE]`,
			}, "\n"))),
			Header: make(http.Header),
		}, nil
	})}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "gpt-5.5-xhigh")

	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if streamOptions, ok := captured["stream_options"]; ok {
		t.Fatalf("stream_options = %#v, want omitted", streamOptions)
	}
}

func TestChatGPTStream_ReasoningSummaryByOutputIndex(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() {
		chatGPTHTTPClient = origClient
	}()

	sse := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_chatgpt_idx","encrypted_content":"enc_chatgpt_idx"}}`,
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":1,"delta":"summary via output index"}`,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_chatgpt_idx","encrypted_content":"enc_chatgpt_idx"}}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":1},"total_tokens":12}}}`,
		`data: [DONE]`,
	}, "\n")

	chatGPTHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(sse)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "gpt-5.2")

	stream, err := provider.Stream(context.Background(), Request{
		Model:    "gpt-5.2",
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()

	var reasoningEvent *Event
	var usageEvent *Event
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		if event.Type == EventReasoningDelta {
			ev := event
			reasoningEvent = &ev
		}
		if event.Type == EventUsage {
			ev := event
			usageEvent = &ev
		}
		if event.Type == EventDone {
			break
		}
	}

	if reasoningEvent == nil {
		t.Fatal("expected reasoning event")
	}
	if reasoningEvent.Text != "summary via output index" {
		t.Fatalf("expected reasoning summary from output_index delta, got %q", reasoningEvent.Text)
	}
	if reasoningEvent.ReasoningKind != ReasoningKindSummary {
		t.Fatalf("expected reasoning kind summary, got %q", reasoningEvent.ReasoningKind)
	}
	if reasoningEvent.ReasoningItemID != "rs_chatgpt_idx" {
		t.Fatalf("expected reasoning item id rs_chatgpt_idx, got %q", reasoningEvent.ReasoningItemID)
	}
	if reasoningEvent.ReasoningEncryptedContent != "enc_chatgpt_idx" {
		t.Fatalf("expected encrypted content enc_chatgpt_idx, got %q", reasoningEvent.ReasoningEncryptedContent)
	}
	if usageEvent == nil || usageEvent.Use == nil {
		t.Fatal("expected usage event")
	}
	if usageEvent.Use.ProviderRawInputTokens != 10 {
		t.Fatalf("provider raw input tokens = %d, want 10", usageEvent.Use.ProviderRawInputTokens)
	}
	if usageEvent.Use.ProviderTotalTokens != 12 {
		t.Fatalf("provider total tokens = %d, want 12", usageEvent.Use.ProviderTotalTokens)
	}
	if usageEvent.Use.ReasoningTokens != 1 {
		t.Fatalf("reasoning tokens = %d, want 1", usageEvent.Use.ReasoningTokens)
	}
}

func chatGPTTestCredentials() *credentials.ChatGPTCredentials {
	return &credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}
}

func chatGPTCompletedSSE(responseID string) string {
	return fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n", responseID)
}

func TestChatGPTHTTPSecondTurnMatchesReconstructedGatewayProvider(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	var captured []map[string]any
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			return nil, err
		}
		captured = append(captured, payload)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(chatGPTCompletedSSE(fmt.Sprintf("resp_%d", len(captured))))),
			Header:     make(http.Header),
		}, nil
	})}

	first := Request{Model: "gpt-5.6-sol", SessionID: "session-equivalence", Messages: []Message{UserText("first")}}
	second := Request{Model: "gpt-5.6-sol", SessionID: "session-equivalence", Messages: []Message{UserText("first"), AssistantText("answer"), UserText("second")}}

	// Direct HTTP/SSE keeps one provider/client alive across turns.
	direct := NewChatGPTProviderWithCreds(chatGPTTestCredentials(), "gpt-5.6-sol")
	stream, err := direct.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	_ = stream.Close()
	stream, err = direct.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	_ = stream.Close()

	// The gateway creates a fresh central provider for this second request.
	reconstructed := NewChatGPTProviderWithCreds(chatGPTTestCredentials(), "gpt-5.6-sol")
	stream, err = reconstructed.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	_ = stream.Close()

	if len(captured) != 3 {
		t.Fatalf("captured payloads = %d, want 3", len(captured))
	}
	if !reflect.DeepEqual(captured[1], captured[2]) {
		directJSON, _ := json.Marshal(captured[1])
		gatewayJSON, _ := json.Marshal(captured[2])
		t.Fatalf("direct/gateway HTTP second-turn payloads differ\ndirect: %s\ngateway: %s", directJSON, gatewayJSON)
	}
	for index, payload := range captured[1:] {
		if _, ok := payload["previous_response_id"]; ok {
			t.Fatalf("second-turn HTTP payload %d used previous_response_id: %#v", index, payload)
		}
		input, ok := payload["input"].([]any)
		if !ok || len(input) != 3 {
			t.Fatalf("second-turn HTTP payload %d input = %#v, want full transcript", index, payload["input"])
		}
	}
}

func TestChatGPTWebSocketDirectContinuationGatewayDifferenceAndFullHistoryFallback(t *testing.T) {
	var mu sync.Mutex
	connections := 0
	var captured []map[string]any
	capture := func(data []byte) map[string]any {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Errorf("decode WebSocket request: %v", err)
		}
		mu.Lock()
		captured = append(captured, payload)
		mu.Unlock()
		return payload
	}

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		mu.Lock()
		connections++
		connection := connections
		mu.Unlock()

		if connection == 1 {
			// Direct mode: first full turn, optimized second turn, then semantic
			// full-history retry after the connection-local parent is rejected.
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read direct first request: %v", err)
				return
			}
			capture(data)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_direct_1"}})

			_, data, err = conn.ReadMessage()
			if err != nil {
				t.Errorf("read direct continuation: %v", err)
				return
			}
			capture(data)
			_ = conn.WriteJSON(map[string]any{
				"type": "response.failed", "status": 400,
				"response": map[string]any{"error": map[string]any{"code": "previous_response_not_found", "message": "Previous response not found", "param": "previous_response_id"}},
			})

			_, data, err = conn.ReadMessage()
			if err != nil {
				t.Errorf("read direct full-history retry: %v", err)
				return
			}
			capture(data)
			_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_direct_2"}})
			return
		}

		// Gateway-equivalent reconstruction has no connection-local response ID,
		// so its second-turn transcript is complete on its first frame.
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read reconstructed request: %v", err)
			return
		}
		capture(data)
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_gateway"}})
	}))
	defer server.Close()

	newWSProvider := func() *ChatGPTProvider {
		provider := NewChatGPTProviderWithCredsAndOptions(chatGPTTestCredentials(), "gpt-5.6-sol", ChatGPTProviderOptions{UseWebSocket: true})
		provider.responsesClient = &ResponsesClient{
			BaseURL: server.URL, HTTPClient: server.Client(), UseWebSocket: true,
			WebSocketServerState: true, DisableServerState: true,
		}
		return provider
	}
	first := Request{Model: "gpt-5.6-sol", SessionID: "session-ws", Messages: []Message{UserText("first")}}
	second := Request{Model: "gpt-5.6-sol", SessionID: "session-ws", Messages: []Message{UserText("first"), AssistantText("answer"), UserText("second")}}

	direct := newWSProvider()
	stream, err := direct.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	_ = stream.Close()
	stream, err = direct.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	_ = stream.Close()

	reconstructed := newWSProvider()
	stream, err = reconstructed.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	_ = stream.Close()

	mu.Lock()
	requests := append([]map[string]any(nil), captured...)
	mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("captured WebSocket requests = %d, want 4", len(requests))
	}
	directContinuation := requests[1]
	if directContinuation["previous_response_id"] != "resp_direct_1" {
		t.Fatalf("direct continuation previous_response_id = %#v", directContinuation["previous_response_id"])
	}
	if input, ok := directContinuation["input"].([]any); !ok || len(input) != 1 {
		t.Fatalf("direct continuation input = %#v, want only new suffix", directContinuation["input"])
	}
	for name, request := range map[string]map[string]any{"direct fallback": requests[2], "gateway reconstruction": requests[3]} {
		if _, ok := request["previous_response_id"]; ok {
			t.Fatalf("%s retained previous_response_id: %#v", name, request)
		}
		if input, ok := request["input"].([]any); !ok || len(input) != 3 {
			t.Fatalf("%s input = %#v, want full transcript", name, request["input"])
		}
	}
	if !reflect.DeepEqual(requests[2], requests[3]) {
		t.Fatalf("full-history WS fallback and gateway reconstruction differ\nfallback: %#v\ngateway: %#v", requests[2], requests[3])
	}
}
