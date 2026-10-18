package responsesproviders

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUseResponsesAPI(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		// GPT-5 models should use Responses API
		{"gpt-5", true},
		{"gpt-5.1", true},
		{"gpt-5.2", true},
		{"gpt-5.2-high", true},
		{"GPT-5.2", true}, // Case insensitive

		// Codex models should use Responses API
		{"gpt-5.2-codex", true},
		{"gpt-5.1-codex-max", true},
		{"codex-5.2", true},

		// Reasoning models should use Responses API
		{"o1", true},
		{"o1-mini", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4", true},

		// Older models should use Chat Completions
		{"gpt-4.1", false},
		{"gpt-4o", false},
		{"claude-sonnet-4", false},
		{"claude-opus-4.5", false},
		{"gemini-3-pro", false},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			result := useResponsesAPI(tc.model)
			if result != tc.expected {
				t.Errorf("useResponsesAPI(%q) = %v, want %v", tc.model, result, tc.expected)
			}
		})
	}
}

func TestOpenAIProviderStream_OmitsPromptCacheKeyButKeepsSessionHeader(t *testing.T) {
	type capturedRequest struct {
		SessionID         string
		HasPromptCacheKey bool
	}

	captured := make(chan capturedRequest, 1)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload map[string]any
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)
			_, hasPromptCacheKey := payload["prompt_cache_key"]

			captured <- capturedRequest{
				SessionID:         r.Header.Get("session_id"),
				HasPromptCacheKey: hasPromptCacheKey,
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_openai\"}}\n\n",
				)),
			}, nil
		}),
	}

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-5.2",
		responsesClient: &ResponsesClient{
			BaseURL:       "https://example.test/v1/responses",
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    httpClient,
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Model:     "gpt-5.2",
		SessionID: "openai-session-1",
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		},
	})
	if err != nil {
		t.Fatalf("openai stream failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	select {
	case req := <-captured:
		if req.SessionID != "openai-session-1" {
			t.Fatalf("expected session_id header 'openai-session-1', got %q", req.SessionID)
		}
		if req.HasPromptCacheKey {
			t.Fatal("OpenAI request contains prompt_cache_key")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request capture")
	}
}

func TestCopilotStreamResponses_UsesSessionIDForResponsesCaching(t *testing.T) {
	type capturedRequest struct {
		SessionID      string
		PromptCacheKey string
	}

	captured := make(chan capturedRequest, 1)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload struct {
				PromptCacheKey string `json:"prompt_cache_key"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &payload)

			captured <- capturedRequest{
				SessionID:      r.Header.Get("session_id"),
				PromptCacheKey: payload.PromptCacheKey,
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_copilot\"}}\n\n",
				)),
			}, nil
		}),
	}

	provider := &CopilotProvider{
		model:        "gpt-5.2",
		apiBaseURL:   "https://api.githubcopilot.com",
		sessionToken: "copilot-session-token",
		responsesClient: &ResponsesClient{
			BaseURL:       "https://example.test/v1/responses",
			GetAuthHeader: func() string { return "Bearer copilot-session-token" },
			HTTPClient:    httpClient,
		},
	}

	stream, err := provider.streamResponses(context.Background(), Request{
		Model:     "gpt-5.2",
		SessionID: "copilot-session-1",
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}},
		},
	}, "gpt-5.2")
	if err != nil {
		t.Fatalf("copilot stream failed: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	select {
	case req := <-captured:
		if req.SessionID != "copilot-session-1" {
			t.Fatalf("expected session_id header 'copilot-session-1', got %q", req.SessionID)
		}
		if req.PromptCacheKey != "copilot-session-1" {
			t.Fatalf("expected prompt_cache_key 'copilot-session-1', got %q", req.PromptCacheKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request capture")
	}
}
