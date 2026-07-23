package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

func TestBuildGeminiContents_DropsDanglingToolCalls(t *testing.T) {
	_, contents := buildGeminiContents([]Message{
		UserText("Run shell"),
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Working"},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "call-1",
						Name:      "shell",
						Arguments: []byte(`{"command":"sleep 10"}`),
					},
				},
			},
		},
		UserText("new request"),
	})

	if len(contents) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(contents))
	}

	assistant := contents[1]
	if assistant.Role != "model" {
		t.Fatalf("expected role model, got %q", assistant.Role)
	}

	var sawText bool
	for _, part := range assistant.Parts {
		if part.FunctionCall != nil {
			t.Fatalf("expected dangling functionCall to be removed, got %#v", part.FunctionCall)
		}
		if part.Text == "Working" {
			sawText = true
		}
	}
	if !sawText {
		t.Fatalf("expected assistant text to be preserved, got %#v", assistant.Parts)
	}
}

func TestEmitGeminiParts_StreamsTextAndToolCallsInOrder(t *testing.T) {
	thoughtSig := []byte{1, 2, 3}
	events := make(chan Event, 4)
	var lastThoughtSig []byte

	err := emitGeminiParts(eventSender{ctx: context.Background(), ch: events}, []*geminiPart{
		{Thought: true, ThoughtSignature: thoughtSig},
		{Text: "Working"},
		{Text: "..."},
		{FunctionCall: &geminiFunctionCall{
			ID:   "call_1",
			Name: "lookup",
			Args: map[string]any{"q": "weather"},
		}},
		{Text: "done"},
	}, &lastThoughtSig)
	if err != nil {
		t.Fatalf("emitGeminiParts() error = %v", err)
	}
	close(events)

	var got []Event
	for event := range events {
		got = append(got, event)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	if got[0].Type != EventTextDelta || got[0].Text != "Working..." {
		t.Fatalf("first event = %+v, want text delta %q", got[0], "Working...")
	}
	if got[1].Type != EventToolCall || got[1].Tool == nil {
		t.Fatalf("second event = %+v, want tool call", got[1])
	}
	if got[1].Tool.ID != "call_1" || got[1].Tool.Name != "lookup" || string(got[1].Tool.Arguments) != `{"q":"weather"}` {
		t.Fatalf("tool call = %+v", got[1].Tool)
	}
	if string(got[1].Tool.ThoughtSig) != string(thoughtSig) {
		t.Fatalf("tool thought signature = %v, want %v", got[1].Tool.ThoughtSig, thoughtSig)
	}
	if got[2].Type != EventTextDelta || got[2].Text != "done" {
		t.Fatalf("third event = %+v, want text delta %q", got[2], "done")
	}
	if string(lastThoughtSig) != string(thoughtSig) {
		t.Fatalf("lastThoughtSig = %v, want %v", lastThoughtSig, thoughtSig)
	}
}

func TestEmitGeminiUsage_DoesNotBlockAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := make(chan Event, 1)
	events <- Event{Type: EventTextDelta, Text: "buffer-full"}

	resp := &geminiGenerateContentResponse{
		UsageMetadata: &geminiUsageMetadata{
			PromptTokenCount:     3,
			CandidatesTokenCount: 5,
			TotalTokenCount:      8,
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- emitGeminiUsage(eventSender{ctx: ctx, ch: events}, resp)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("emitGeminiUsage() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emitGeminiUsage blocked after context cancellation")
	}
}

func TestGeminiStreamUsesMinimalRESTClient(t *testing.T) {
	var gotRequest geminiGenerateContentRequest
	provider := NewGeminiProvider("test-key", "gemini-3-flash-preview")
	provider.baseURL = "https://gemini.test/v1beta/models"
	provider.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://gemini.test/v1beta/models/gemini-3-flash-preview:streamGenerateContent?alt=sse" {
			t.Fatalf("request URL = %q", req.URL.String())
		}
		if got := req.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Fatalf("x-goog-api-key = %q", got)
		}
		if err := json.NewDecoder(req.Body).Decode(&gotRequest); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		body := strings.Join([]string{
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello "}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":1,"totalTokenCount":6}}`,
			``,
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"world"}]},"finishReason":"MAX_TOKENS"}]}`,
			``,
		}, "\n") + "\n"
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	stream, err := provider.Stream(context.Background(), Request{
		Messages:        []Message{SystemText("be brief"), UserText("hi")},
		MaxOutputTokens: 99,
		Temperature:     0,
		TemperatureSet:  true,
		TopP:            0.75,
		TopPSet:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var usage *Usage
	sawDone := false
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventTextDelta {
			text += event.Text
		}
		if event.Type == EventUsage {
			usage = event.Use
		}
		if event.Type == EventDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream did not emit EventDone")
	}
	if text != "hello world" {
		t.Fatalf("text = %q", text)
	}
	if usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if gotRequest.SystemInstruction == nil || gotRequest.SystemInstruction.Parts[0].Text != "be brief" {
		t.Fatalf("system instruction = %+v", gotRequest.SystemInstruction)
	}
	config := gotRequest.GenerationConfig
	if config == nil || config.ThinkingConfig == nil || config.ThinkingConfig.ThinkingLevel != "MINIMAL" {
		t.Fatalf("generation config = %+v", config)
	}
	if config.MaxOutputTokens != 99 || config.Temperature == nil || *config.Temperature != 0 || config.TopP == nil || *config.TopP != 0.75 {
		t.Fatalf("generation controls = %+v", config)
	}
}

func TestGeminiStreamRejectsTruncatedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}]}\n\n")
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.baseURL = server.URL
	provider.client = server.Client()
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}

	var sawText, sawError bool
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		switch event.Type {
		case EventTextDelta:
			sawText = event.Text == "partial"
		case EventDone:
			t.Fatal("truncated stream emitted EventDone")
		case EventError:
			var incomplete *StreamIncompleteError
			if !errors.As(event.Err, &incomplete) {
				t.Fatalf("error = %T %v, want StreamIncompleteError", event.Err, event.Err)
			}
			if incomplete.Transport != "Gemini SSE" || incomplete.Terminal != "candidate finishReason" {
				t.Fatalf("incomplete error = %+v", incomplete)
			}
			sawError = true
		}
	}
	if !sawText {
		t.Fatal("stream did not emit partial candidate text")
	}
	if !sawError {
		t.Fatal("truncated stream did not emit EventError")
	}
}

func TestReadGeminiSSEReportsAPIAndBlockedResponses(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "API error", data: `{"error":{"code":429,"message":"quota exhausted","status":"RESOURCE_EXHAUSTED"}}`, want: "quota exhausted"},
		{name: "blocked prompt", data: `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"unsafe prompt"}}`, want: "SAFETY: unsafe prompt"},
		{name: "blocked candidate", data: `{"candidates":[{"finishReason":"RECITATION","finishMessage":"matching text"}]}`, want: "RECITATION: matching text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := readGeminiSSE(strings.NewReader("data: "+tt.data+"\n\n"), func(*geminiGenerateContentResponse) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestReadGeminiSSEAllowsTruncationAndIgnoresSecondaryCandidates(t *testing.T) {
	payload := `{"candidates":[` +
		`{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"},` +
		`{"finishReason":"RECITATION","finishMessage":"secondary candidate"}` +
		`]}`
	called := false
	err := readGeminiSSE(strings.NewReader("data: "+payload+"\n\n"), func(*geminiGenerateContentResponse) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("readGeminiSSE() error = %v", err)
	}
	if !called {
		t.Fatal("response handler was not called")
	}
}

func TestReadGeminiSSEHandlesMultilineAndLargeEvents(t *testing.T) {
	largeText := strings.Repeat("x", 5*1024*1024)
	payload := "data: {\"candidates\":[{\"content\":{\"parts\":[\n" +
		"data: {\"text\":" + string(mustJSONMarshal(t, largeText)) + "}]},\"finishReason\":\"STOP\"}]}\n\n"
	var got string
	err := readGeminiSSE(strings.NewReader(payload), func(resp *geminiGenerateContentResponse) error {
		got = resp.Candidates[0].Content.Parts[0].Text
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != largeText {
		t.Fatalf("decoded text length = %d, want %d", len(got), len(largeText))
	}
}

func TestGeminiStreamReturnsTypedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.baseURL = server.URL
	provider.client = server.Client()
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var statusErr *providerhttp.StatusError
	if event.Type != EventError || !errors.As(event.Err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("event = %+v, want typed status 503", event)
	}
}

func TestGeminiStreamReturnsTypedSSEError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"code\":429,\"message\":\"quota exhausted\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n")
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.baseURL = server.URL
	provider.client = server.Client()
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var statusErr *providerhttp.StatusError
	if event.Type != EventError || !errors.As(event.Err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("event = %+v, want typed status 429", event)
	}
}

func TestGeminiStreamRejectsEmptyCandidateResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"totalTokenCount\":1}}\n\n")
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.baseURL = server.URL
	provider.client = server.Client()
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != EventError || event.Err == nil || !strings.Contains(event.Err.Error(), "no candidate content") {
		t.Fatalf("event = %+v, want empty-candidate error", event)
	}
}

func TestGeminiStreamCancellationInterruptsRead(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.baseURL = server.URL
	provider.client = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := provider.Stream(ctx, Request{Messages: []Message{UserText("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	recvDone := make(chan error, 1)
	go func() {
		_, recvErr := stream.Recv()
		recvDone <- recvErr
	}()
	<-started
	cancel()
	select {
	case recvErr := <-recvDone:
		if !errors.Is(recvErr, context.Canceled) {
			t.Fatalf("Recv error = %v, want context.Canceled", recvErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv did not stop after cancellation")
	}
}

func mustJSONMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
