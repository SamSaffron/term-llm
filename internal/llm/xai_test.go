package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestXAIStreamStandard_AllowsLargeSSEDataLines(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	largeText := strings.Repeat("a", 1024*1024+1024)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		chunk, err := json.Marshal(oaiChatResponse{
			Choices: []oaiChoice{{
				Delta: &oaiMessage{Content: largeText},
			}},
		})
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte("data: ")); err != nil {
			t.Fatalf("write prefix: %v", err)
		}
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		if _, err := w.Write([]byte("\n\ndata: [DONE]\n\n")); err != nil {
			t.Fatalf("write done: %v", err)
		}
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			urlCopy := *req.URL
			cloned.URL = &urlCopy
			cloned.URL.Scheme = serverURL.Scheme
			cloned.URL.Host = serverURL.Host
			return server.Client().Transport.RoundTrip(cloned)
		}),
	}

	provider := NewXAIProvider("test-key", "test-model")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var got strings.Builder
	var sawDone bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			got.WriteString(event.Text)
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if got.String() != largeText {
		t.Fatalf("expected %d bytes of streamed text, got %d", len(largeText), got.Len())
	}
	if !sawDone {
		t.Fatal("expected EventDone")
	}
}

func TestXAIStreamWithSearch_AllowsLargeSSEDataLines(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	largeText := strings.Repeat("b", 1024*1024+1024)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		delta, err := json.Marshal(xaiResponsesEvent{
			Type:  "response.output_text.delta",
			Delta: largeText,
		})
		if err != nil {
			t.Fatalf("marshal delta: %v", err)
		}

		completed, err := json.Marshal(xaiResponsesEvent{
			Type: "response.completed",
			Response: &xaiResponsesCompletion{
				Usage: &xaiResponsesUsage{InputTokens: 10, OutputTokens: 20},
			},
		})
		if err != nil {
			t.Fatalf("marshal completed: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte("data: ")); err != nil {
			t.Fatalf("write prefix: %v", err)
		}
		if _, err := w.Write(delta); err != nil {
			t.Fatalf("write delta: %v", err)
		}
		if _, err := w.Write([]byte("\n\ndata: ")); err != nil {
			t.Fatalf("write separator: %v", err)
		}
		if _, err := w.Write(completed); err != nil {
			t.Fatalf("write completed: %v", err)
		}
		if _, err := w.Write([]byte("\n\ndata: [DONE]\n\n")); err != nil {
			t.Fatalf("write done: %v", err)
		}
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			urlCopy := *req.URL
			cloned.URL = &urlCopy
			cloned.URL.Scheme = serverURL.Scheme
			cloned.URL.Host = serverURL.Host
			return server.Client().Transport.RoundTrip(cloned)
		}),
	}

	provider := NewXAIProvider("test-key", "test-model")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
		Search:   true,
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var got strings.Builder
	var gotUsage *Usage
	var sawDone bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			got.WriteString(event.Text)
		case EventUsage:
			gotUsage = event.Use
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if got.String() != largeText {
		t.Fatalf("expected %d bytes of streamed text, got %d", len(largeText), got.Len())
	}
	if gotUsage == nil || gotUsage.InputTokens != 10 || gotUsage.OutputTokens != 20 {
		t.Fatalf("unexpected usage: %+v", gotUsage)
	}
	if !sawDone {
		t.Fatal("expected EventDone")
	}
}

func TestXAIProviderStreamSendsExplicitParallelToolCallsFalse(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	var got struct {
		ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
		Tools             []json.RawMessage `json:"tools,omitempty"`
		Stream            bool              `json:"stream"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			urlCopy := *req.URL
			cloned.URL = &urlCopy
			cloned.URL.Scheme = serverURL.Scheme
			cloned.URL.Host = serverURL.Host
			return server.Client().Transport.RoundTrip(cloned)
		}),
	}

	provider := NewXAIProvider("test-key", "test-model")
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

	drainStreamToDone(t, stream)

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

func TestXAIProviderStreamSendsExplicitZeroTemperatureAndTopP(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() {
		defaultHTTPClient = origClient
	}()

	var got struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
		Stream      bool     `json:"stream"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			cloned := req.Clone(req.Context())
			urlCopy := *req.URL
			cloned.URL = &urlCopy
			cloned.URL.Scheme = serverURL.Scheme
			cloned.URL.Host = serverURL.Host
			return server.Client().Transport.RoundTrip(cloned)
		}),
	}

	provider := NewXAIProvider("test-key", "test-model")
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

	drainStreamToDone(t, stream)

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
	if !got.Stream {
		t.Fatal("expected stream=true")
	}
}
