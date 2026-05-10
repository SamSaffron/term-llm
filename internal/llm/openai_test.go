package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseModelEffort(t *testing.T) {
	tests := []struct {
		in         string
		wantModel  string
		wantEffort string
	}{
		{"gpt-5.4-mini", "gpt-5.4-mini", ""},
		{"gpt-5.4", "gpt-5.4", ""},
		{"gpt-5.4-high", "gpt-5.4", "high"},
		{"gpt-5.4-xhigh", "gpt-5.4", "xhigh"},
		{"gpt-5.4-medium", "gpt-5.4", "medium"},
		{"gpt-5.4-low", "gpt-5.4", "low"},
		{"gpt-5.4-minimal", "gpt-5.4", "minimal"},
		{"gpt-5.4-max", "gpt-5.4", "max"},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			gotModel, gotEffort := ParseModelEffort(tt.in)
			if gotModel != tt.wantModel || gotEffort != tt.wantEffort {
				t.Errorf("ParseModelEffort(%q) = (%q, %q), want (%q, %q)",
					tt.in, gotModel, gotEffort, tt.wantModel, tt.wantEffort)
			}
		})
	}
}

func TestOpenAIProviderStreamUsesMessagesForContinuationRequests(t *testing.T) {
	var got struct {
		PreviousResponseID string               `json:"previous_response_id,omitempty"`
		Input              []ResponsesInputItem `json:"input"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_next\"}}\n\n"))
	}))
	defer ts.Close()

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-4.1",
		responsesClient: &ResponsesClient{
			BaseURL:        ts.URL,
			GetAuthHeader:  func() string { return "Bearer test-key" },
			HTTPClient:     ts.Client(),
			LastResponseID: "resp_prev",
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{
			SystemText("Be concise"),
			UserText("old question"),
			AssistantText("old answer"),
			UserText("new question"),
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if got.PreviousResponseID != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want %q", got.PreviousResponseID, "resp_prev")
	}
	if len(got.Input) != 1 {
		t.Fatalf("expected only latest user item, got %d items: %+v", len(got.Input), got.Input)
	}
	if got.Input[0].Type != "message" || got.Input[0].Role != "user" || got.Input[0].Content != "new question" {
		t.Fatalf("unexpected continuation input: %+v", got.Input[0])
	}
}

func TestOpenAIProviderStreamSendsExplicitParallelToolCallsFalse(t *testing.T) {
	var got struct {
		ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
		Tools             []json.RawMessage `json:"tools,omitempty"`
		Stream            bool              `json:"stream"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-4.1",
		responsesClient: &ResponsesClient{
			BaseURL:       ts.URL,
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    ts.Client(),
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
		Tools: []ToolSpec{{
			Name:        "echo",
			Description: "Echo input",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"text": map[string]interface{}{"type": "string"},
				},
			},
		}},
		ParallelToolCalls: false,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if got.ParallelToolCalls == nil {
		t.Fatal("expected parallel_tool_calls to be sent explicitly")
	}
	if *got.ParallelToolCalls {
		t.Fatalf("expected parallel_tool_calls=false, got %v", *got.ParallelToolCalls)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got.Tools))
	}
	if !got.Stream {
		t.Fatal("expected stream=true")
	}
}

func TestOpenAIProviderStreamSendsExplicitZeroTemperatureAndTopP(t *testing.T) {
	var got struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-4.1",
		responsesClient: &ResponsesClient{
			BaseURL:       ts.URL,
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    ts.Client(),
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Messages:       []Message{UserText("hello")},
		Temperature:    0,
		TemperatureSet: true,
		TopP:           0,
		TopPSet:        true,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}

	if got.Temperature == nil {
		t.Fatal("expected temperature=0 to be sent explicitly")
	}
	if *got.Temperature != 0 {
		t.Fatalf("expected temperature=0, got %v", *got.Temperature)
	}
	if got.TopP == nil {
		t.Fatal("expected top_p=0 to be sent explicitly")
	}
	if *got.TopP != 0 {
		t.Fatalf("expected top_p=0, got %v", *got.TopP)
	}
}

func TestOpenAIProviderStreamReasoningEffortPrecedence(t *testing.T) {
	tests := []struct {
		name           string
		providerModel  string
		providerEffort string
		requestModel   string
		requestEffort  string
		wantEffort     string
	}{
		{
			name:          "provider suffix sets effort",
			providerModel: "gpt-5.4-xhigh",
			wantEffort:    "xhigh",
		},
		{
			name:           "request suffix overrides provider effort",
			providerModel:  "gpt-5.4",
			providerEffort: "low",
			requestModel:   "gpt-5.4-high",
			wantEffort:     "high",
		},
		{
			name:           "request reasoning_effort field wins over provider effort and suffix",
			providerModel:  "gpt-5.4",
			providerEffort: "low",
			requestModel:   "gpt-5.4-medium",
			requestEffort:  "high",
			wantEffort:     "high",
		},
		{
			name:          "minimal effort passes through",
			providerModel: "gpt-5.4",
			requestEffort: "minimal",
			wantEffort:    "minimal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				Reasoning *ResponsesReasoning `json:"reasoning,omitempty"`
			}

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer ts.Close()

			actualModel, effort := ParseModelEffort(tt.providerModel)
			provider := &OpenAIProvider{
				apiKey: "test-key",
				model:  actualModel,
				effort: effort,
				responsesClient: &ResponsesClient{
					BaseURL:       ts.URL,
					GetAuthHeader: func() string { return "Bearer test-key" },
					HTTPClient:    ts.Client(),
				},
			}
			if tt.providerEffort != "" {
				provider.effort = tt.providerEffort
			}

			stream, err := provider.Stream(context.Background(), Request{
				Model:           tt.requestModel,
				Messages:        []Message{UserText("hello")},
				ReasoningEffort: tt.requestEffort,
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			defer stream.Close()

			for {
				ev, err := stream.Recv()
				if err != nil {
					t.Fatalf("Recv() error = %v", err)
				}
				if ev.Type == EventDone {
					break
				}
			}

			if got.Reasoning == nil {
				t.Fatal("expected reasoning block to be sent")
			}
			if got.Reasoning.Effort != tt.wantEffort {
				t.Errorf("reasoning.effort = %q, want %q", got.Reasoning.Effort, tt.wantEffort)
			}
		})
	}
}
