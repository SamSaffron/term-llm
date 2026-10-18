package responsesproviders

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"

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
