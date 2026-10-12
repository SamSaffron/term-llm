package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/openaiapi"
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

func TestOpenAIProviderStreamIncludesProviderDefaultServiceTier(t *testing.T) {
	var got ResponsesRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}))
	defer ts.Close()

	provider := &OpenAIProvider{
		apiKey:      "test-key",
		model:       "gpt-5.4",
		serviceTier: ServiceTierFast,
		responsesClient: &ResponsesClient{
			BaseURL:       ts.URL,
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    ts.Client(),
		},
	}

	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if got.ServiceTier != ServiceTierFast {
		t.Fatalf("service_tier = %q, want %q", got.ServiceTier, ServiceTierFast)
	}
}

func TestOpenAIProviderStreamServiceTierOmittedByDefaultAndClearOverride(t *testing.T) {
	var got ResponsesRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}))
	defer ts.Close()

	provider := &OpenAIProvider{
		apiKey:      "test-key",
		model:       "gpt-5.4",
		serviceTier: ServiceTierFast,
		responsesClient: &ResponsesClient{
			BaseURL:       ts.URL,
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    ts.Client(),
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Messages:       []Message{UserText("hello")},
		ServiceTierSet: true,
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if got.ServiceTier != "" {
		t.Fatalf("service_tier = %q, want omitted", got.ServiceTier)
	}
}

func TestOpenAIProviderStreamMapsGPT56AdvancedResponsesControls(t *testing.T) {
	var got ResponsesRequest
	var beta string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		beta = r.Header.Get("OpenAI-Beta")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}))
	defer ts.Close()

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-5.6-sol",
		responsesOptions: ResponsesOptions{
			ReasoningMode:    "standard",
			ReasoningContext: "all_turns",
			MultiAgent: MultiAgentOptions{
				Enabled:                true,
				EnabledSet:             true,
				MaxConcurrentSubagents: 6,
			},
			ProgrammaticToolCalling: ProgrammaticToolCallingOptions{
				Enabled:    true,
				EnabledSet: true,
				Tools:      []string{"shell"},
			},
			PromptCache: PromptCacheOptions{Mode: "implicit", TTL: "30m"},
		},
		responsesClient: &ResponsesClient{
			BaseURL:       ts.URL,
			GetAuthHeader: func() string { return "Bearer test-key" },
			HTTPClient:    ts.Client(),
		},
	}

	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
		Tools: []ToolSpec{{
			Name:   "shell",
			Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		Responses: &ResponsesOptions{
			ReasoningMode:    "pro",
			ReasoningContext: "current_turn",
			MultiAgent:       MultiAgentOptions{MaxConcurrentSubagents: 2},
			PromptCache:      PromptCacheOptions{Mode: "explicit"},
		},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if got.Reasoning == nil || got.Reasoning.Mode != "pro" || got.Reasoning.Context != "current_turn" || got.Reasoning.Summary != "" {
		t.Fatalf("reasoning = %+v", got.Reasoning)
	}
	if got.MultiAgent == nil || !got.MultiAgent.Enabled || got.MultiAgent.MaxConcurrentSubagents != 2 {
		t.Fatalf("multi_agent = %+v", got.MultiAgent)
	}
	if got.PromptCacheOptions == nil || got.PromptCacheOptions.Mode != "explicit" || got.PromptCacheOptions.TTL != "30m" {
		t.Fatalf("prompt_cache_options = %+v", got.PromptCacheOptions)
	}
	if beta != "responses_multi_agent=v1" {
		t.Fatalf("OpenAI-Beta = %q", beta)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %#v", got.Tools)
	}
	tool, ok := got.Tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v", got.Tools[0])
	}
	callers, ok := tool["allowed_callers"].([]any)
	if !ok || len(callers) != 1 || callers[0] != "programmatic" {
		t.Fatalf("allowed_callers = %#v", tool["allowed_callers"])
	}
	marker, ok := got.Tools[1].(map[string]any)
	if !ok || marker["type"] != "programmatic_tool_calling" {
		t.Fatalf("PTC marker tool = %#v", got.Tools[1])
	}
}

func clearOpenAIEndpointEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{openaiapi.EnvBaseURL, openaiapi.EnvOrganization, openaiapi.EnvProject} {
		t.Setenv(key, "")
	}
}

func TestOpenAIProviderListModels(t *testing.T) {
	clearOpenAIEndpointEnv(t)
	origClient := defaultHTTPClient
	t.Cleanup(func() { defaultHTTPClient = origClient })

	var gotAuth string
	defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != "https://api.openai.com/v1/models" {
			t.Fatalf("request = %s %s", r.Method, r.URL)
		}
		gotAuth = r.Header.Get("Authorization")
		body := `{"object":"list","data":[{"id":"gpt-5.2","object":"model","created":1700000000,"owned_by":"system"},{"id":"text-embedding-3-small","object":"model","created":1600000000,"owned_by":"system"}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}

	models, err := NewOpenAIProvider("test-key", "gpt-5.2").ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if len(models) != 2 || models[0].ID != "gpt-5.2" || models[0].Created != 1700000000 || models[1].ID != "text-embedding-3-small" {
		t.Fatalf("models = %#v", models)
	}
	if models[0].InputLimit != InputLimitForModel("gpt-5.2") {
		t.Fatalf("input limit = %d", models[0].InputLimit)
	}
}

func TestOpenAIProviderListModelsReturnsStatusError(t *testing.T) {
	origClient := defaultHTTPClient
	t.Cleanup(func() { defaultHTTPClient = origClient })

	defaultHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Retry-After": []string{"3"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)),
			Request:    r,
		}, nil
	})}

	_, err := NewOpenAIProvider("test-key", "gpt-5.2").ListModels(context.Background())
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests || statusErr.Header.Get("Retry-After") != "3" {
		t.Fatalf("err = %v, want 429 HTTPStatusError with headers", err)
	}
}

func TestOpenAIProviderEndpointAppliesToListModelsAndStream(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		cfg      config.ProviderConfig
		wantBase string
		wantOrg  string
		wantProj string
		wantKey  string
	}{
		{
			name:     "environment",
			env:      map[string]string{openaiapi.EnvBaseURL: "%s/env/v1", openaiapi.EnvOrganization: "org-env", openaiapi.EnvProject: "proj-env"},
			wantBase: "/env/v1",
			wantOrg:  "org-env",
			wantProj: "proj-env",
		},
		{
			name: "config base_url and headers win",
			env:  map[string]string{openaiapi.EnvBaseURL: "%s/env/v1", openaiapi.EnvOrganization: "org-env", openaiapi.EnvProject: "proj-env"},
			cfg: config.ProviderConfig{
				BaseURL: "%s/config/v1",
				Headers: map[string]string{"openai-organization": "org-config", "X-Proxy-Key": "proxy"},
			},
			wantBase: "/config/v1",
			wantOrg:  "org-config",
			wantProj: "proj-env",
			wantKey:  "proxy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type seen struct{ path, org, project, proxyKey, beta string }
			var requests []seen
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, seen{r.URL.Path, r.Header.Get("OpenAI-Organization"), r.Header.Get("OpenAI-Project"), r.Header.Get("X-Proxy-Key"), r.Header.Get("OpenAI-Beta")})
				if strings.HasSuffix(r.URL.Path, "/models") {
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.2"}]}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
			}))
			defer server.Close()
			for _, key := range []string{openaiapi.EnvBaseURL, openaiapi.EnvOrganization, openaiapi.EnvProject} {
				value := tt.env[key]
				if strings.Contains(value, "%s") {
					value = strings.Replace(value, "%s", server.URL, 1)
				}
				t.Setenv(key, value)
			}
			cfg := tt.cfg
			cfg.BaseURL = strings.Replace(cfg.BaseURL, "%s", server.URL, 1)
			origClient := defaultHTTPClient
			t.Cleanup(func() { defaultHTTPClient = origClient })
			defaultHTTPClient = server.Client()

			provider := NewOpenAIProviderWithOptions("test-key", "gpt-5.2", OpenAIProviderOptions{Endpoint: cfg.OpenAIEndpoint()})
			if _, err := provider.ListModels(context.Background()); err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			for {
				ev, err := stream.Recv()
				if err != nil {
					t.Fatalf("Recv: %v", err)
				}
				if ev.Type == EventDone {
					break
				}
			}
			stream.Close()

			want := []seen{
				{tt.wantBase + "/models", tt.wantOrg, tt.wantProj, tt.wantKey, ""},
				{tt.wantBase + "/responses", tt.wantOrg, tt.wantProj, tt.wantKey, ""},
			}
			if len(requests) != len(want) {
				t.Fatalf("requests = %#v", requests)
			}
			for i := range want {
				if requests[i] != want[i] {
					t.Fatalf("request %d = %#v, want %#v", i, requests[i], want[i])
				}
			}
		})
	}
}

func TestOpenAIProviderWebSocketUsesEndpointHeaders(t *testing.T) {
	clearOpenAIEndpointEnv(t)
	type handshake struct{ path, org, project, beta string }
	handshakes := make(chan handshake, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case handshakes <- handshake{r.URL.Path, r.Header.Get("OpenAI-Organization"), r.Header.Get("OpenAI-Project"), r.Header.Get("OpenAI-Beta")}:
		default:
		}
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
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_1"}})
	}))
	defer server.Close()

	provider := NewOpenAIProviderWithOptions("test-key", "gpt-5.2", OpenAIProviderOptions{
		UseWebSocket: true,
		Endpoint: openaiapi.Endpoint{
			BaseURL: server.URL + "/v1",
			// Lowercase names mirror how config keys are loaded.
			Headers: map[string]string{"openai-organization": "org-ws", "openai-project": "proj-ws", "openai-beta": "custom=v1"},
		},
	})
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		ev, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == EventDone {
			break
		}
	}
	got := <-handshakes
	if got != (handshake{"/v1/responses", "org-ws", "proj-ws", composeBetaHeader("custom=v1", responsesWebSocketBetaHeader)}) {
		t.Fatalf("handshake = %#v", got)
	}
}

func TestCreateOpenAIProviderFromConfigUsesBaseURLAndHeaders(t *testing.T) {
	clearOpenAIEndpointEnv(t)
	t.Setenv(openaiapi.EnvProject, "proj-env")
	provider, err := createProviderFromConfig("openai", &config.ProviderConfig{
		ResolvedAPIKey: "test-key",
		Model:          "gpt-5.2",
		BaseURL:        "https://gateway.example/v1/",
		Headers:        map[string]string{"OpenAI-Organization": "org-config"},
	})
	if err != nil {
		t.Fatalf("createProviderFromConfig: %v", err)
	}
	openai, ok := provider.(*OpenAIProvider)
	if !ok {
		t.Fatalf("provider = %T", provider)
	}
	want := openaiapi.Endpoint{
		BaseURL: "https://gateway.example/v1",
		Headers: map[string]string{"OpenAI-Organization": "org-config", "OpenAI-Project": "proj-env"},
	}
	if !reflect.DeepEqual(openai.endpoint, want) {
		t.Fatalf("endpoint = %#v, want %#v", openai.endpoint, want)
	}
}

func TestOpenAIProviderRequestEncryptedReasoning(t *testing.T) {
	for _, tc := range []struct {
		name        string
		option      *bool
		wantInclude bool
	}{
		{name: "default", wantInclude: true},
		{name: "enabled", option: boolPtr(true), wantInclude: true},
		{name: "disabled", option: boolPtr(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []ResponsesRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload ResponsesRequest
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests = append(requests, payload)
				if len(requests) == 2 {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"previous_response_id not found"}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_saved\",\"output\":[]}}\n\n"))
			}))
			defer server.Close()

			// Exercise the custom type: openai configuration path, not just the constructor.
			created, err := createProviderFromConfig("muse_wonder", &config.ProviderConfig{
				Type: config.ProviderTypeOpenAI, APIKey: "test-key", Model: "muse-wonder",
				BaseURL: server.URL, RequestEncryptedReasoning: tc.option,
			})
			if err != nil {
				t.Fatal(err)
			}
			provider := created.(*OpenAIProvider)
			messages := []Message{UserText("hello")}
			stream, err := provider.Stream(context.Background(), Request{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			drainStreamToDone(t, stream)
			stream.Close()

			// Existing or unsolicited encrypted reasoning must still survive full-history replay.
			messages = append(messages, Message{Role: RoleAssistant, Parts: []Part{{
				Type: PartText, Text: "answer", ReasoningItemID: "rs_saved", ReasoningEncryptedContent: "enc_saved",
			}}}, UserText("follow up"))
			stream, err = provider.Stream(context.Background(), Request{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			drainStreamToDone(t, stream)
			stream.Close()

			if len(requests) != 3 {
				t.Fatalf("requests = %d, want initial, continuation, and full-history retry", len(requests))
			}
			for i, req := range requests {
				want := []string(nil)
				if tc.wantInclude {
					want = []string{"reasoning.encrypted_content"}
				}
				if !reflect.DeepEqual(req.Include, want) {
					t.Errorf("request %d include = %v, want %v", i, req.Include, want)
				}
			}
			if requests[0].PreviousResponseID != "" || requests[1].PreviousResponseID != "resp_saved" || requests[2].PreviousResponseID != "" {
				t.Fatalf("unexpected continuation state: %q, %q, %q", requests[0].PreviousResponseID, requests[1].PreviousResponseID, requests[2].PreviousResponseID)
			}
			if len(requests[1].Input) != 1 {
				t.Fatalf("continuation input = %v, want only latest user message", requests[1].Input)
			}
			var found bool
			for _, item := range requests[2].Input {
				if item.Type == "reasoning" && item.EncryptedContent == "enc_saved" {
					found = true
				}
			}
			if !found {
				t.Fatal("full-history retry lost saved encrypted reasoning")
			}
		})
	}
}
