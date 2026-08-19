package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

func TestChatGPTPromptCacheRetentionErrorDetection(t *testing.T) {
	t.Parallel()

	matchingBody := []byte(`{"error":{"message":"prompt_cache_retention is not supported on this model","type":"invalid_request_error","param":"prompt_cache_retention","code":"invalid_parameter"}}`)
	if !isChatGPTPromptCacheRetentionError(http.StatusBadRequest, matchingBody) {
		t.Fatal("expected prompt_cache_retention invalid_parameter response to match")
	}

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "wrong status", status: http.StatusInternalServerError, body: string(matchingBody)},
		{name: "wrong parameter", status: http.StatusBadRequest, body: `{"error":{"param":"service_tier","code":"invalid_parameter"}}`},
		{name: "wrong code", status: http.StatusBadRequest, body: `{"error":{"param":"prompt_cache_retention","code":"invalid_value"}}`},
		{name: "malformed body", status: http.StatusBadRequest, body: `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if isChatGPTPromptCacheRetentionError(tc.status, []byte(tc.body)) {
				t.Fatal("unexpected match")
			}
		})
	}
}

func TestChatGPTRetryProviderRetriesPromptCacheRetentionError(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = origClient }()

	attempts := 0
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"message":"prompt_cache_retention is not supported on this model","type":"invalid_request_error","param":"prompt_cache_retention","code":"invalid_parameter"}}`,
				)),
				Header: make(http.Header),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
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
	wrapped := WrapWithRetry(provider, RetryConfig{
		MaxAttempts: 2,
		BaseBackoff: time.Nanosecond,
		MaxBackoff:  time.Nanosecond,
	})

	stream, err := wrapped.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
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

func TestChatGPTProviderResetConversationClearsResponsesState(t *testing.T) {
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}, "gpt-5.6-luna")
	provider.responsesClient = NewChatGPTResponsesClient(provider.creds)
	provider.responsesClient.LastResponseID = "resp_previous"
	provider.responsesClient.responseStateSessionID = "benchmark-old"

	provider.ResetConversation()

	if provider.responsesClient.LastResponseID != "" || provider.responsesClient.responseStateSessionID != "" {
		t.Fatalf("responses state was not reset: id=%q session=%q", provider.responsesClient.LastResponseID, provider.responsesClient.responseStateSessionID)
	}
}

func TestChatGPTStreamOmitsUnsupportedOutputLimit(t *testing.T) {
	originalClient := chatGPTHTTPClient
	defer func() { chatGPTHTTPClient = originalClient }()

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
	}, "gpt-5.6-luna-low")
	stream, err := provider.Stream(context.Background(), Request{
		SessionID:       "benchmark-unique-request",
		Messages:        []Message{UserText("hello")},
		MaxOutputTokens: 77,
	})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if sessionID != "benchmark-unique-request" {
		t.Fatalf("session_id header = %q", sessionID)
	}
	if got, ok := payload["max_output_tokens"]; ok {
		t.Fatalf("unsupported max_output_tokens was forwarded: %v", got)
	}
	if _, ok := payload["temperature"]; ok {
		t.Fatalf("temperature should be omitted for unsupported ChatGPT benchmark control: %v", payload["temperature"])
	}
	if _, ok := payload["top_p"]; ok {
		t.Fatalf("top_p should be omitted: %v", payload["top_p"])
	}
}
