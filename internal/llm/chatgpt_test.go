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
	if chatGPTHTTPClient.Timeout != 0 {
		t.Fatalf("expected no http.Client.Timeout for ChatGPT streaming client, got %s", chatGPTHTTPClient.Timeout)
	}
}

func TestNewChatGPTProviderWithCredsDefaultsToGPT55Medium(t *testing.T) {
	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "")

	if provider.model != "gpt-5.5" {
		t.Fatalf("model = %q, want %q", provider.model, "gpt-5.5")
	}
	if provider.effort != "medium" {
		t.Fatalf("effort = %q, want %q", provider.effort, "medium")
	}
}

func TestChatGPTStream_IncludesNormalizedServiceTier(t *testing.T) {
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
