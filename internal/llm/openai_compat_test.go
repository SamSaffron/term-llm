package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestDefaultHTTPClient_HasNoOverallTimeout(t *testing.T) {
	if defaultHTTPClient.Timeout != 0 {
		t.Fatalf("expected shared HTTP client timeout to be disabled for streaming requests, got %v", defaultHTTPClient.Timeout)
	}
}

func TestProviderHTTPClients_UseHTTP2WithLargerIdlePool(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	serverTransport := server.Client().Transport.(*http.Transport)
	clients := map[string]*http.Client{
		"default": defaultHTTPClient,
		"copilot": copilotHTTPClient,
	}
	for name, providerClient := range clients {
		t.Run(name, func(t *testing.T) {
			providerTransport, ok := providerClient.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", providerClient.Transport)
			}
			if !providerTransport.ForceAttemptHTTP2 {
				t.Fatal("HTTP/2 is not enabled")
			}
			if providerTransport.MaxIdleConnsPerHost <= http.DefaultMaxIdleConnsPerHost {
				t.Fatalf("MaxIdleConnsPerHost = %d, want more than default %d", providerTransport.MaxIdleConnsPerHost, http.DefaultMaxIdleConnsPerHost)
			}

			transport := providerTransport.Clone()
			transport.TLSClientConfig = serverTransport.TLSClientConfig.Clone()
			defer transport.CloseIdleConnections()

			response, err := (&http.Client{Transport: transport}).Get(server.URL)
			if err != nil {
				t.Fatalf("HTTP/2 request: %v", err)
			}
			defer response.Body.Close()
			if response.ProtoMajor != 2 {
				t.Fatalf("response protocol = %s, want HTTP/2", response.Proto)
			}
		})
	}
}

func TestReadSSEEvent_AllowsEventAndDataLinesWithoutSpace(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("event:error\ndata:{\"error\":{\"message\":\"boom\"}}\n\n"))

	eventType, data, eof, err := readSSEEvent(reader)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if eof {
		t.Fatal("expected event separator, not EOF")
	}
	if eventType != "error" {
		t.Fatalf("expected event type %q, got %q", "error", eventType)
	}
	if data != "{\"error\":{\"message\":\"boom\"}}" {
		t.Fatalf("expected data %q, got %q", "{\"error\":{\"message\":\"boom\"}}", data)
	}
}

func TestReadSSEEvent_StripsOnlyOptionalSingleSpaceAfterColon(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("data:  leading-space\n\n"))

	_, data, eof, err := readSSEEvent(reader)
	if err != nil {
		t.Fatalf("readSSEEvent: %v", err)
	}
	if eof {
		t.Fatal("expected event separator, not EOF")
	}
	if data != " leading-space" {
		t.Fatalf("expected data %q, got %q", " leading-space", data)
	}
}

func TestOpenAICompatListModelsParsesOpenRouterContextLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"vendor/model","name":"Vendor Model","context_length":131072,"pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "", "OpenRouter")
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1", len(models))
	}
	if models[0].InputLimit != 131072 {
		t.Fatalf("InputLimit = %d, want 131072", models[0].InputLimit)
	}
	if models[0].InputPrice != 1 || models[0].OutputPrice != 2 {
		t.Fatalf("prices = %g/%g, want 1/2", models[0].InputPrice, models[0].OutputPrice)
	}
}

func TestOpenAICompatListModelsParsesNumericPricing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"zai-org/GLM-5.2","pricing":{"prompt":1.4,"completion":4.4}}]}`)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "", "NumericCompat")
	models, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1", len(models))
	}
	if models[0].InputPrice != 1.4 || models[0].OutputPrice != 4.4 {
		t.Fatalf("prices = %g/%g, want 1.4/4.4", models[0].InputPrice, models[0].OutputPrice)
	}
}

func TestOpenAICompatStream_ExplicitReasoningParserConfigAndProviderModelEffort(t *testing.T) {
	requestCh := make(chan oaiChatRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		var got oaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestCh <- got
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "zai-org/GLM-5.2-max", "Compat")
	provider.SetReasoningParser(boolPtr(true), boolPtr(true), "enable_thinking")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
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

	select {
	case got := <-requestCh:
		if got.Model != "zai-org/GLM-5.2" {
			t.Fatalf("model = %q, want zai-org/GLM-5.2", got.Model)
		}
		if got.ReasoningEffort != "max" {
			t.Fatalf("reasoning_effort = %q, want max", got.ReasoningEffort)
		}
		if got.ParseReasoning == nil || !*got.ParseReasoning || got.IncludeReasoning == nil || !*got.IncludeReasoning {
			t.Fatalf("parse/include reasoning = %v/%v, want true/true", got.ParseReasoning, got.IncludeReasoning)
		}
		if got.ChatTemplateKwargs == nil || got.ChatTemplateKwargs["enable_thinking"] != true {
			t.Fatalf("chat_template_kwargs = %#v, want enable_thinking true", got.ChatTemplateKwargs)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request")
	}
}

func TestOpenAICompatStream_ModelConfigAliasEffortAndParser(t *testing.T) {
	requestCh := make(chan oaiChatRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got oaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestCh <- got
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "glm52-max", "Compat")
	provider.SetModelConfigs([]config.ProviderModelConfig{{
		ID:               "zai-org/GLM-5.2",
		Alias:            "glm52",
		ParseReasoning:   boolPtr(true),
		IncludeReasoning: boolPtr(true),
		ThinkingParam:    "enable_thinking",
		MaxOutputTokens:  2048,
		ReasoningEfforts: []string{"high", "max"},
	}})
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}, MaxOutputTokens: 9999})
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

	select {
	case got := <-requestCh:
		if got.Model != "zai-org/GLM-5.2" {
			t.Fatalf("model = %q, want zai-org/GLM-5.2", got.Model)
		}
		if got.ReasoningEffort != "max" {
			t.Fatalf("reasoning_effort = %q, want max", got.ReasoningEffort)
		}
		if got.MaxTokens == nil || *got.MaxTokens != 2048 {
			t.Fatalf("max_tokens = %v, want 2048", got.MaxTokens)
		}
		if got.ParseReasoning == nil || !*got.ParseReasoning || got.IncludeReasoning == nil || !*got.IncludeReasoning {
			t.Fatalf("parse/include reasoning = %v/%v, want true/true", got.ParseReasoning, got.IncludeReasoning)
		}
		if got.ChatTemplateKwargs == nil || got.ChatTemplateKwargs["enable_thinking"] != true {
			t.Fatalf("chat_template_kwargs = %#v, want enable_thinking true", got.ChatTemplateKwargs)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request")
	}
}

func TestOpenAICompatStream_AllowsLargeSSEDataLines(t *testing.T) {
	largeText := strings.Repeat("a", 1024*1024+1024)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
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

func TestOpenAICompatStream_EmitsReasoningContentDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think 1\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" think 2\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotReasoning string
	var gotKind ReasoningKind
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
		case EventReasoningDelta:
			gotReasoning += event.Text
			gotKind = event.ReasoningKind
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if gotReasoning != "think 1 think 2" {
		t.Fatalf("expected reasoning_content deltas to be emitted, got %q", gotReasoning)
	}
	if gotKind != ReasoningKindRaw {
		t.Fatalf("expected reasoning_content kind raw, got %q", gotKind)
	}
	if !sawDone {
		t.Fatal("expected EventDone")
	}
}

func TestOpenAICompatStream_SuppressesLeadingWhitespaceOnlyContentDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"\\n\\n\",\"reasoning_content\":\"thinking\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"\\n\\n\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotText string
	var gotReasoning string
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
			gotText += event.Text
		case EventReasoningDelta:
			gotReasoning += event.Text
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if gotText != "hello\n\n" {
		t.Fatalf("expected leading whitespace-only chunk to be suppressed and later whitespace preserved, got %q", gotText)
	}
	if gotReasoning != "thinking" {
		t.Fatalf("expected reasoning delta to be preserved, got %q", gotReasoning)
	}
	if !sawDone {
		t.Fatal("expected EventDone")
	}
}

func TestOpenAICompatStream_PreservesLeadingWhitespaceOnlyContentWithoutReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"  \"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotText string
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
			gotText += event.Text
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if gotText != "  hello" {
		t.Fatalf("expected leading whitespace without reasoning to be preserved, got %q", gotText)
	}
}

func TestOpenAICompatStream_HandlesMultiLineSSEPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\n"+
				"data: \"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":0},\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var gotText string
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
			gotText += event.Text
		case EventUsage:
			gotUsage = event.Use
		case EventDone:
			sawDone = true
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if gotText != "hello" {
		t.Fatalf("expected streamed text %q, got %q", "hello", gotText)
	}
	if gotUsage == nil {
		t.Fatal("expected usage event")
	}
	if gotUsage.InputTokens != 1 || gotUsage.OutputTokens != 1 || gotUsage.ProviderTotalTokens != 2 {
		t.Fatalf("unexpected usage: %+v", gotUsage)
	}
	if !sawDone {
		t.Fatal("expected EventDone")
	}
}

func TestOpenAICompatStream_HandlesMultiLineErrorEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: error\n"+
				"data: {\"error\":\n"+
				"data: {\"message\":\"split backend failure\"}}\n\n",
		)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventError:
			if event.Err == nil {
				t.Fatal("expected stream error")
			}
			if !strings.Contains(event.Err.Error(), "split backend failure") {
				t.Fatalf("expected backend error, got %v", event.Err)
			}
			return
		case EventDone:
			t.Fatal("expected EventError before EventDone")
		}
	}

	t.Fatal("expected EventError")
}

func TestOpenAICompatStream_CloseUnblocksPendingRead(t *testing.T) {
	origClient := defaultHTTPClient
	defer func() { defaultHTTPClient = origClient }()

	chunk, err := json.Marshal(oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{Content: "hello"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	bodyReader, bodyWriter := io.Pipe()
	defer bodyReader.Close()
	defer bodyWriter.Close()
	defaultHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       bodyReader,
				Request:    req,
			}, nil
		}),
	}

	provider := NewOpenAICompatProvider("https://example.test/v1", "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	sseChunk := append(append([]byte("data: "), chunk...), []byte("\n\n")...)
	writeDone := make(chan error, 1)
	go func() {
		_, err := bodyWriter.Write(sseChunk)
		writeDone <- err
	}()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv first event: %v", err)
	}
	if event.Type != EventTextDelta || event.Text != "hello" {
		t.Fatalf("first event = %+v, want text delta %q", event, "hello")
	}

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write initial SSE chunk: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream goroutine did not consume the initial SSE chunk")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- stream.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("stream.Close() error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stream.Close() blocked while the provider goroutine was waiting for more SSE bytes")
	}
}

func TestOpenAICompatStream_CloseDoesNotHangWhenConsumerStopsReceiving(t *testing.T) {
	chunk, err := json.Marshal(oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{Content: "x"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	wroteChunks := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("expected response writer to implement http.Flusher")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 64; i++ {
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
		close(wroteChunks)
		<-r.Context().Done()
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	select {
	case <-wroteChunks:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE chunks to be written")
	}

	time.Sleep(100 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- stream.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("stream.Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Close() blocked while stream goroutine was trying to emit events")
	}
}

func TestOpenAICompatStream_ReturnsErrorForMalformedFinalJSONChunk(t *testing.T) {
	firstChunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{ToolCalls: []oaiToolCall{{}}},
		}},
	}
	firstChunk.Choices[0].Delta.ToolCalls[0].ID = "call-1"
	firstChunk.Choices[0].Delta.ToolCalls[0].Function.Name = "search"
	firstChunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `{"query":"wea`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		data, err := json.Marshal(firstChunk)
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			t.Fatalf("write prefix: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			t.Fatalf("write separator: %v", err)
		}
		if _, err := w.Write([]byte(`data: {"choices":[`)); err != nil {
			t.Fatalf("write malformed chunk: %v", err)
		}
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var toolCalls []ToolCall
	var sawDone bool
	var sawError bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventToolCall:
			if event.Tool == nil {
				t.Fatal("expected tool call event to include tool")
			}
			toolCalls = append(toolCalls, *event.Tool)
		case EventDone:
			sawDone = true
		case EventError:
			sawError = true
			if event.Err == nil {
				t.Fatal("expected error event to include error")
			}
			if !strings.Contains(event.Err.Error(), "invalid JSON chunk") {
				t.Fatalf("expected invalid JSON chunk error, got %v", event.Err)
			}
		}
	}

	if !sawError {
		t.Fatal("expected EventError")
	}
	if sawDone {
		t.Fatal("did not expect EventDone")
	}
	if len(toolCalls) != 0 {
		t.Fatalf("expected no tool calls, got %d: %#v", len(toolCalls), toolCalls)
	}
}

func TestOpenAICompatStream_ReturnsErrorForPartialToolArgumentsAtEOF(t *testing.T) {
	chunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{ToolCalls: []oaiToolCall{{}}},
		}},
	}
	chunk.Choices[0].Delta.ToolCalls[0].ID = "call-1"
	chunk.Choices[0].Delta.ToolCalls[0].Function.Name = "search"
	chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `{"query":"wea`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			t.Fatalf("write prefix: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			t.Fatalf("write separator: %v", err)
		}
		// Close without [DONE]. The final decoded SSE frame is valid JSON, but the
		// accumulated function.arguments string is not complete JSON.
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var sawError bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventToolCall:
			t.Fatalf("expected partial arguments to fail before emitting tool call, got %+v", event.Tool)
		case EventDone:
			t.Fatal("expected partial arguments to fail before EventDone")
		case EventError:
			sawError = true
			if event.Err == nil {
				t.Fatal("expected error event to include error")
			}
			var incomplete *StreamIncompleteError
			if !errors.As(event.Err, &incomplete) {
				t.Fatalf("expected StreamIncompleteError for EOF before [DONE], got %T %v", event.Err, event.Err)
			}
			if !strings.Contains(event.Err.Error(), "[DONE]") {
				t.Fatalf("expected missing [DONE] error, got %v", event.Err)
			}
		}
	}
	if !sawError {
		t.Fatal("expected EventError")
	}
}

func TestOpenAICompatStream_KeepsToolCallsSeparateWhenIndexesAreOmitted(t *testing.T) {
	firstChunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{ToolCalls: []oaiToolCall{{}, {}}},
		}},
	}
	firstChunk.Choices[0].Delta.ToolCalls[0].ID = "call-1"
	firstChunk.Choices[0].Delta.ToolCalls[0].Function.Name = "search"
	firstChunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `{"query":"wea`
	firstChunk.Choices[0].Delta.ToolCalls[1].ID = "call-2"
	firstChunk.Choices[0].Delta.ToolCalls[1].Function.Name = "fetch"
	firstChunk.Choices[0].Delta.ToolCalls[1].Function.Arguments = `{"url":"https://exa`

	secondChunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta: &oaiMessage{ToolCalls: []oaiToolCall{{}, {}}},
		}},
	}
	secondChunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `ther"}`
	secondChunk.Choices[0].Delta.ToolCalls[1].Function.Arguments = `mple.com"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []oaiChatResponse{firstChunk, secondChunk} {
			data, err := json.Marshal(chunk)
			if err != nil {
				t.Fatalf("marshal chunk: %v", err)
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				t.Fatalf("write prefix: %v", err)
			}
			if _, err := w.Write(data); err != nil {
				t.Fatalf("write chunk: %v", err)
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				t.Fatalf("write separator: %v", err)
			}
		}
		if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
			t.Fatalf("write done: %v", err)
		}
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	var toolCalls []ToolCall
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch event.Type {
		case EventToolCall:
			if event.Tool == nil {
				t.Fatal("expected tool call event to include tool")
			}
			toolCalls = append(toolCalls, *event.Tool)
		case EventError:
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}

	if len(toolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %#v", len(toolCalls), toolCalls)
	}
	if toolCalls[0].ID != "call-1" || toolCalls[0].Name != "search" || string(toolCalls[0].Arguments) != `{"query":"weather"}` {
		t.Fatalf("unexpected first tool call: %#v", toolCalls[0])
	}
	if toolCalls[1].ID != "call-2" || toolCalls[1].Name != "fetch" || string(toolCalls[1].Arguments) != `{"url":"https://example.com"}` {
		t.Fatalf("unexpected second tool call: %#v", toolCalls[1])
	}
}

func TestCompatToolState_CallsStaySortedWhenIndexesArePresent(t *testing.T) {
	s := newCompatToolState()

	first := oaiToolCall{Index: intPtr(1), ID: "call-2"}
	first.Function.Name = "second"
	first.Function.Arguments = `{"value":2}`
	s.Add([]oaiToolCall{first})

	second := oaiToolCall{Index: intPtr(0), ID: "call-1"}
	second.Function.Name = "first"
	second.Function.Arguments = `{"value":1}`
	s.Add([]oaiToolCall{second})

	calls := s.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID != "call-1" || calls[1].ID != "call-2" {
		t.Fatalf("expected calls to be ordered by explicit indexes, got %#v", calls)
	}
}

func TestCompatToolState_ReusesPositionWhenIndexAppearsAfterOmission(t *testing.T) {
	s := newCompatToolState()

	first := oaiToolCall{ID: "call-1"}
	first.Function.Name = "search"
	first.Function.Arguments = `{"query":"wea`
	s.Add([]oaiToolCall{first})

	second := oaiToolCall{Index: intPtr(0)}
	second.Function.Arguments = `ther"}`
	s.Add([]oaiToolCall{second})

	calls := s.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %#v", len(calls), calls)
	}
	if calls[0].ID != "call-1" || calls[0].Name != "search" || string(calls[0].Arguments) != `{"query":"weather"}` {
		t.Fatalf("unexpected merged call: %#v", calls[0])
	}
}

func intPtr(v int) *int {
	return &v
}

func TestSplitParts_WithReasoningContent(t *testing.T) {
	// Test that splitParts correctly extracts reasoning content from parts
	parts := []Part{
		{
			Type:             PartText,
			Text:             "Hello world",
			ReasoningContent: "I need to think about this carefully",
		},
	}

	text, toolCalls, reasoning := splitParts(parts)

	if text != "Hello world" {
		t.Errorf("expected text 'Hello world', got %q", text)
	}
	if len(toolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(toolCalls))
	}
	if reasoning != "I need to think about this carefully" {
		t.Errorf("expected reasoning 'I need to think about this carefully', got %q", reasoning)
	}
}

func TestSplitParts_WithToolCallsAndReasoning(t *testing.T) {
	// Test that splitParts handles both tool calls and reasoning
	parts := []Part{
		{
			Type:             PartText,
			Text:             "Let me help you with that",
			ReasoningContent: "The user wants to list files",
		},
		{
			Type: PartToolCall,
			ToolCall: &ToolCall{
				ID:        "call-123",
				Name:      "list_files",
				Arguments: []byte(`{"path": "."}`),
			},
		},
	}

	text, toolCalls, reasoning := splitParts(parts)

	if text != "Let me help you with that" {
		t.Errorf("expected text 'Let me help you with that', got %q", text)
	}
	if len(toolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != "call-123" {
		t.Errorf("expected tool call ID 'call-123', got %q", toolCalls[0].ID)
	}
	if reasoning != "The user wants to list files" {
		t.Errorf("expected reasoning 'The user wants to list files', got %q", reasoning)
	}
}

func TestBuildCompatMessages_WithReasoningContent(t *testing.T) {
	// Test that buildCompatMessages includes reasoning_content in assistant messages
	messages := []Message{
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "What files are here?"},
			},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					Type:             PartText,
					Text:             "Let me check",
					ReasoningContent: "User wants to see directory contents",
				},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "call-456",
						Name:      "list_files",
						Arguments: []byte(`{"path": "."}`),
					},
				},
			},
		},
		{
			Role: RoleTool,
			Parts: []Part{
				{
					Type: PartToolResult,
					ToolResult: &ToolResult{
						ID:      "call-456",
						Name:    "list_files",
						Content: "file1\nfile2",
					},
				},
			},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(oaiMsgs))
	}

	// Check user message
	if oaiMsgs[0].Role != "user" {
		t.Errorf("expected first message role 'user', got %q", oaiMsgs[0].Role)
	}

	// Check assistant message with reasoning
	assistantMsg := oaiMsgs[1]
	if assistantMsg.Role != "assistant" {
		t.Errorf("expected second message role 'assistant', got %q", assistantMsg.Role)
	}
	if assistantMsg.ReasoningContent != "User wants to see directory contents" {
		t.Errorf("expected reasoning_content 'User wants to see directory contents', got %q", assistantMsg.ReasoningContent)
	}
	if len(assistantMsg.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(assistantMsg.ToolCalls))
	}

	if oaiMsgs[2].Role != "tool" {
		t.Errorf("expected third message role 'tool', got %q", oaiMsgs[2].Role)
	}
}

func TestBuildCompatMessages_NoReasoningContent(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "Hello"}},
		},
		{
			Role:  RoleAssistant,
			Parts: []Part{{Type: PartText, Text: "Hi there!"}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(oaiMsgs))
	}
	if oaiMsgs[1].ReasoningContent != "" {
		t.Errorf("expected empty reasoning_content, got %q", oaiMsgs[1].ReasoningContent)
	}
}

func TestBuildCompatMessages_ToolResultStructuredImageParts(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{{
				Type: PartToolCall,
				ToolCall: &ToolCall{
					ID:        "call-1",
					Name:      "view_image",
					Arguments: []byte(`{"path":"wow.png"}`),
				},
			}},
		},
		{
			Role: RoleTool,
			Parts: []Part{{
				Type: PartToolResult,
				ToolResult: &ToolResult{
					ID:      "call-1",
					Name:    "view_image",
					Content: "Image loaded",
					ContentParts: []ToolContentPart{
						{Type: ToolContentPartText, Text: "Image loaded"},
						{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
						{Type: ToolContentPartText, Text: "done"},
					},
				},
			}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages (assistant + tool + user multimodal), got %d", len(oaiMsgs))
	}
	if oaiMsgs[0].Role != "assistant" || len(oaiMsgs[0].ToolCalls) != 1 {
		t.Fatalf("unexpected assistant tool-call message: %#v", oaiMsgs[0])
	}
	if oaiMsgs[1].Role != "tool" || oaiMsgs[1].Content != "Image loadeddone" {
		t.Fatalf("unexpected tool message: %#v", oaiMsgs[1])
	}
	if oaiMsgs[2].Role != "user" {
		t.Fatalf("expected third message role user, got %q", oaiMsgs[2].Role)
	}
	parts, ok := oaiMsgs[2].Content.([]oaiContentPart)
	if !ok {
		t.Fatalf("expected user content []oaiContentPart, got %T", oaiMsgs[2].Content)
	}
	// Synthetic user message should contain ONLY the image part (text is
	// already in the tool result message and should not be duplicated).
	if len(parts) != 1 {
		t.Fatalf("expected 1 image-only content part, got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Fatalf("expected image_url part, got %#v", parts[0])
	}
}

func TestBuildCompatMessages_ParallelToolResultImagesStayAfterToolBlock(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "view_image", Arguments: []byte(`{"path":"one.png"}`)}},
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "call-2", Name: "read_file", Arguments: []byte(`{"path":"notes.txt"}`)}},
			},
		},
		{
			Role: RoleTool,
			Parts: []Part{
				{
					Type: PartToolResult,
					ToolResult: &ToolResult{
						ID:      "call-1",
						Name:    "view_image",
						Content: "Image loaded",
						ContentParts: []ToolContentPart{
							{Type: ToolContentPartText, Text: "Image loaded"},
							{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
						},
					},
				},
				{
					Type:       PartToolResult,
					ToolResult: &ToolResult{ID: "call-2", Name: "read_file", Content: "notes"},
				},
			},
		},
	}

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 4 {
		t.Fatalf("expected assistant + 2 contiguous tools + image user, got %d messages: %#v", len(oaiMsgs), oaiMsgs)
	}
	wantRoles := []string{"assistant", "tool", "tool", "user"}
	for i, want := range wantRoles {
		if got := oaiMsgs[i].Role; got != want {
			t.Fatalf("message %d role = %q, want %q (all messages: %#v)", i, got, want, oaiMsgs)
		}
	}
	if oaiMsgs[1].ToolCallID != "call-1" || oaiMsgs[2].ToolCallID != "call-2" {
		t.Fatalf("tool result IDs not preserved: %#v", oaiMsgs)
	}
	parts, ok := oaiMsgs[3].Content.([]oaiContentPart)
	if !ok || len(parts) != 1 || parts[0].Type != "image_url" {
		t.Fatalf("expected one trailing image part, got %#v", oaiMsgs[3].Content)
	}
}

func TestBuildCompatMessages_ToolResultImagesFlushBeforeNextConversationTurn(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{{
				Type:     PartToolCall,
				ToolCall: &ToolCall{ID: "call-1", Name: "view_image", Arguments: []byte(`{"path":"one.png"}`)},
			}},
		},
		{
			Role: RoleTool,
			Parts: []Part{{
				Type: PartToolResult,
				ToolResult: &ToolResult{
					ID:   "call-1",
					Name: "view_image",
					ContentParts: []ToolContentPart{
						{Type: ToolContentPartText, Text: "Image loaded"},
						{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
					},
				},
			}},
		},
		{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "I inspected the image."}}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Continue."}}},
	}

	oaiMsgs := buildCompatMessages(messages)
	wantRoles := []string{"assistant", "tool", "user", "assistant", "user"}
	if len(oaiMsgs) != len(wantRoles) {
		t.Fatalf("messages len = %d, want %d: %#v", len(oaiMsgs), len(wantRoles), oaiMsgs)
	}
	for i, want := range wantRoles {
		if got := oaiMsgs[i].Role; got != want {
			t.Fatalf("message %d role = %q, want %q (all messages: %#v)", i, got, want, oaiMsgs)
		}
	}
	parts, ok := oaiMsgs[2].Content.([]oaiContentPart)
	if !ok || len(parts) != 1 || parts[0].Type != "image_url" {
		t.Fatalf("expected image user message immediately after tool block, got %#v", oaiMsgs[2])
	}
}

func TestNormalizeSchemaForOpenAI_FreeFormMapProperty(t *testing.T) {
	// Regression: env parameter uses additionalProperties: {type: string} to represent
	// a free-form string map. The normalizer must not clobber it with false.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "Shell command to execute",
			},
			"env": map[string]interface{}{
				"type":                 "object",
				"description":          "Environment variables",
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}

	result := normalizeSchemaForOpenAI(schema)

	props, ok := result["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected properties map")
	}

	envSchema, ok := props["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to be a map")
	}

	// additionalProperties on env must remain a schema map, not false
	ap := envSchema["additionalProperties"]
	apMap, ok := ap.(map[string]interface{})
	if !ok {
		t.Fatalf("expected env.additionalProperties to remain a schema map, got %T (%v)", ap, ap)
	}
	if apMap["type"] != "string" {
		t.Errorf("expected env.additionalProperties.type = string, got %v", apMap["type"])
	}

	// Outer object must still have additionalProperties: false
	if result["additionalProperties"] != false {
		t.Errorf("expected outer additionalProperties = false, got %v", result["additionalProperties"])
	}

	// All properties must appear in required (OpenAI strict mode)
	required, ok := result["required"].([]string)
	if !ok {
		t.Fatal("expected required to be []string")
	}
	requiredSet := make(map[string]bool)
	for _, k := range required {
		requiredSet[k] = true
	}
	for k := range props {
		if !requiredSet[k] {
			t.Errorf("expected %q in required array, not found", k)
		}
	}
}

func TestNormalizeSchemaForOpenAI_RegularObjectGetsAdditionalPropertiesFalse(t *testing.T) {
	// Regular nested object (no additionalProperties set) should get additionalProperties: false
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"options": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"verbose": map[string]interface{}{"type": "boolean"},
				},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)

	props := result["properties"].(map[string]interface{})
	optionsSchema := props["options"].(map[string]interface{})

	if optionsSchema["additionalProperties"] != false {
		t.Errorf("expected nested object to get additionalProperties: false, got %v", optionsSchema["additionalProperties"])
	}
}

func TestNormalizeSchemaForOpenAI_MCPTypeArrayBecomesAnyOf(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"data": map[string]interface{}{
				"type":        []interface{}{"array", "null"},
				"description": "Dropped data payload",
				"items":       map[string]interface{}{},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)
	props := result["properties"].(map[string]interface{})
	dataSchema := props["data"].(map[string]interface{})

	if _, ok := dataSchema["type"]; ok {
		t.Fatalf("expected data.type to be removed in favor of anyOf, got %#v", dataSchema["type"])
	}
	anyOf, ok := dataSchema["anyOf"].([]interface{})
	if !ok || len(anyOf) != 2 {
		t.Fatalf("expected data.anyOf with 2 branches, got %#v", dataSchema["anyOf"])
	}
	arrayBranch, ok := anyOf[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected first anyOf branch to be object, got %T", anyOf[0])
	}
	if arrayBranch["type"] != "array" {
		t.Fatalf("expected first branch type array, got %v", arrayBranch["type"])
	}
	items, ok := arrayBranch["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected array branch to preserve and normalize items, got %#v", arrayBranch["items"])
	}
	if items["type"] != "string" {
		t.Fatalf("expected empty items schema to be normalized to string, got %#v", items["type"])
	}
	nullBranch, ok := anyOf[1].(map[string]interface{})
	if !ok || nullBranch["type"] != "null" {
		t.Fatalf("expected second anyOf branch to be null, got %#v", anyOf[1])
	}
}

func TestNormalizeSchemaForOpenAI_InfersInvalidMCPPropertyType(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"data": map[string]interface{}{
				"type": []interface{}{},
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"fallback": map[string]interface{}{
				"type": map[string]interface{}{"not": "a valid type"},
			},
		},
	}

	result := normalizeSchemaForOpenAI(schema)
	props := result["properties"].(map[string]interface{})
	dataSchema := props["data"].(map[string]interface{})
	if dataSchema["type"] != "array" {
		t.Fatalf("expected data type to be inferred as array, got %#v", dataSchema["type"])
	}
	fallbackSchema := props["fallback"].(map[string]interface{})
	if fallbackSchema["type"] != "string" {
		t.Fatalf("expected uninformative invalid type to fall back to string, got %#v", fallbackSchema["type"])
	}
}

func TestBuildResponsesTools_DefaultsEmptyParametersToObject(t *testing.T) {
	tools := BuildResponsesTools([]ToolSpec{{Name: "empty", Schema: map[string]interface{}{}}})
	tool := tools[0].(ResponsesTool)
	if tool.Parameters["type"] != "object" {
		t.Fatalf("expected empty tool schema to default to object parameters, got %#v", tool.Parameters)
	}
	props, ok := tool.Parameters["properties"].(map[string]interface{})
	if !ok || len(props) != 0 {
		t.Fatalf("expected empty properties map, got %#v", tool.Parameters["properties"])
	}
}

func TestBuildResponsesTools_NormalizesFreeFormMapProperty(t *testing.T) {
	specs := []ToolSpec{{
		Name:   "shell",
		Strict: true,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{"type": "string"},
				"env": map[string]interface{}{
					"type":                 "object",
					"description":          "Environment variables",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}}
	tools := BuildResponsesTools(specs)
	tool := tools[0].(ResponsesTool)
	props := tool.Parameters["properties"].(map[string]interface{})

	// command must remain unchanged
	if _, ok := props["command"]; !ok {
		t.Error("expected command to remain in strict schema")
	}

	// env must be present but transformed to an array of key/value objects
	envSchema, ok := props["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to be present and a map")
	}
	if envSchema["type"] != "array" {
		t.Errorf("expected env to be transformed to array type, got %v", envSchema["type"])
	}
	if envSchema["description"] != "Environment variables" {
		t.Errorf("expected description to be preserved, got %v", envSchema["description"])
	}
	items, ok := envSchema["items"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items to be a map")
	}
	if items["type"] != "object" {
		t.Errorf("expected env.items.type to be object, got %v", items["type"])
	}
	itemProps, ok := items["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items.properties to be a map")
	}
	if _, ok := itemProps["key"]; !ok {
		t.Error("expected env.items.properties.key to exist")
	}
	valueSchema, ok := itemProps["value"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env.items.properties.value to be a map")
	}
	if valueSchema["type"] != "string" {
		t.Errorf("expected env.items.properties.value.type to be string (original additionalProperties schema), got %v", valueSchema["type"])
	}
	if items["additionalProperties"] != false {
		t.Errorf("expected env.items.additionalProperties to be false, got %v", items["additionalProperties"])
	}
}

func TestNormalizeFreeFormMapProperties_PreservesNonStringValueType(t *testing.T) {
	// A free-form map whose values are integers, not strings.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"counts": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": map[string]interface{}{"type": "integer"},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	countsSchema := props["counts"].(map[string]interface{})
	if countsSchema["type"] != "array" {
		t.Fatalf("expected counts to be transformed to array, got %v", countsSchema["type"])
	}
	items := countsSchema["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	valueSchema := itemProps["value"].(map[string]interface{})
	if valueSchema["type"] != "integer" {
		t.Errorf("expected value type to preserve original additionalProperties type 'integer', got %v", valueSchema["type"])
	}
}

func TestNormalizeFreeFormMapProperties_TraversesItems(t *testing.T) {
	// A free-form map nested inside an array's items schema.
	schema := map[string]interface{}{
		"type": "array",
		"items": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": map[string]interface{}{"type": "string"},
				},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	items := result["items"].(map[string]interface{})
	itemProps := items["properties"].(map[string]interface{})
	envSchema, ok := itemProps["env"].(map[string]interface{})
	if !ok {
		t.Fatal("expected env to exist inside items.properties")
	}
	if envSchema["type"] != "array" {
		t.Errorf("expected env inside items to be transformed to array, got %v", envSchema["type"])
	}
}

func TestNormalizeFreeFormMapProperties_AnyOfFreeFormMap(t *testing.T) {
	// A property whose schema is an anyOf where one branch is a free-form map.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"meta": map[string]interface{}{
				"anyOf": []interface{}{
					map[string]interface{}{
						"type":                 "object",
						"additionalProperties": map[string]interface{}{"type": "string"},
					},
					map[string]interface{}{"type": "null"},
				},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	metaSchema := props["meta"].(map[string]interface{})
	anyOf := metaSchema["anyOf"].([]interface{})

	// First anyOf branch (was a free-form map) must be converted to array.
	firstBranch, ok := anyOf[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected first anyOf branch to be a map")
	}
	if firstBranch["type"] != "array" {
		t.Errorf("expected free-form map in anyOf to be converted to array, got %v", firstBranch["type"])
	}

	// Second anyOf branch (null) must be unchanged.
	secondBranch, ok := anyOf[1].(map[string]interface{})
	if !ok {
		t.Fatal("expected second anyOf branch to be a map")
	}
	if secondBranch["type"] != "null" {
		t.Errorf("expected null branch to be unchanged, got %v", secondBranch["type"])
	}
}

func TestNormalizeFreeFormMapProperties_PreservesMetadata(t *testing.T) {
	// Metadata fields beyond description (title, default) must survive conversion.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"tags": map[string]interface{}{
				"type":                 "object",
				"description":          "Tag values",
				"title":                "Tags",
				"default":              map[string]interface{}{},
				"additionalProperties": map[string]interface{}{"type": "string"},
			},
		},
	}
	result := normalizeFreeFormMapProperties(schema)
	props := result["properties"].(map[string]interface{})
	tagsSchema := props["tags"].(map[string]interface{})
	if tagsSchema["type"] != "array" {
		t.Fatalf("expected tags to be converted to array, got %v", tagsSchema["type"])
	}
	if tagsSchema["description"] != "Tag values" {
		t.Errorf("expected description to be preserved, got %v", tagsSchema["description"])
	}
	if tagsSchema["title"] != "Tags" {
		t.Errorf("expected title to be preserved, got %v", tagsSchema["title"])
	}
	if tagsSchema["default"] == nil {
		t.Error("expected default to be preserved")
	}
}

func TestBuildCompatMessages_ConvertsDanglingToolCalls(t *testing.T) {
	messages := []Message{
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "Run a tool"},
			},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Working on it"},
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
		{
			Role: RoleUser,
			Parts: []Part{
				{Type: PartText, Text: "new request"},
			},
		},
	}

	oaiMsgs := buildCompatMessages(messages)
	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(oaiMsgs))
	}

	assistant := oaiMsgs[1]
	if assistant.Role != "assistant" {
		t.Fatalf("expected assistant role, got %q", assistant.Role)
	}
	if len(assistant.ToolCalls) != 0 {
		t.Fatalf("expected dangling tool calls to be removed, got %d", len(assistant.ToolCalls))
	}
	// Orphaned tool_use is converted to a text stub so the model knows it was interrupted.
	if !strings.Contains(assistant.Content.(string), "Working on it") {
		t.Fatalf("expected original text to be preserved, got %v", assistant.Content)
	}
	if !strings.Contains(assistant.Content.(string), "[tool call interrupted") {
		t.Fatalf("expected interrupted stub in text, got %v", assistant.Content)
	}
}

func TestBuildCompatMessages_DeveloperRolePrependedToUser(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleSystem,
			Parts: []Part{{Type: PartText, Text: "You are helpful."}},
		},
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Always respond in JSON."}},
		},
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "What is 2+2?"}},
		},
		{
			Role:  RoleAssistant,
			Parts: []Part{{Type: PartText, Text: `{"answer": 4}`}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages (system, user, assistant), got %d", len(oaiMsgs))
	}
	if oaiMsgs[0].Role != "system" {
		t.Errorf("expected first message role 'system', got %q", oaiMsgs[0].Role)
	}
	userContent, ok := oaiMsgs[1].Content.(string)
	if !ok {
		t.Fatalf("expected user content to be string, got %T", oaiMsgs[1].Content)
	}
	if !strings.Contains(userContent, "<developer>") {
		t.Errorf("expected <developer> tag in user message, got %q", userContent)
	}
	if !strings.Contains(userContent, "Always respond in JSON.") {
		t.Errorf("expected developer text in user message, got %q", userContent)
	}
	if !strings.Contains(userContent, "What is 2+2?") {
		t.Errorf("expected original user text preserved, got %q", userContent)
	}
}

func TestBuildCompatMessages_TrailingDeveloperMessage(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "Hello"}},
		},
		{
			Role:  RoleAssistant,
			Parts: []Part{{Type: PartText, Text: "Hi!"}},
		},
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Be concise from now on."}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 3 {
		t.Fatalf("expected 3 messages (user, assistant, synthetic user), got %d", len(oaiMsgs))
	}
	trailing := oaiMsgs[2]
	if trailing.Role != "user" {
		t.Errorf("expected trailing message role 'user', got %q", trailing.Role)
	}
	content, ok := trailing.Content.(string)
	if !ok {
		t.Fatalf("expected trailing content to be string, got %T", trailing.Content)
	}
	if !strings.Contains(content, "<developer>") || !strings.Contains(content, "Be concise from now on.") {
		t.Errorf("expected developer text in trailing user message, got %q", content)
	}
}

func TestBuildCompatMessages_DeveloperMessageNotDropped(t *testing.T) {
	// Regression: before the fix, RoleDeveloper messages were silently dropped.
	messages := []Message{
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "You must use formal English."}},
		},
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "hey whats up"}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 1 {
		t.Fatalf("expected 1 message (merged dev+user), got %d", len(oaiMsgs))
	}
	content, ok := oaiMsgs[0].Content.(string)
	if !ok {
		t.Fatalf("expected content to be string, got %T", oaiMsgs[0].Content)
	}
	if !strings.Contains(content, "You must use formal English.") {
		t.Errorf("developer text was dropped — not found in %q", content)
	}
	if !strings.Contains(content, "hey whats up") {
		t.Errorf("user text was dropped — not found in %q", content)
	}
}

func TestBuildCompatMessages_ConsecutiveDeveloperMessagesPreserved(t *testing.T) {
	messages := []Message{
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Platform instruction."}},
		},
		{
			Role:  RoleDeveloper,
			Parts: []Part{{Type: PartText, Text: "Runtime instruction."}},
		},
		{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "Hello"}},
		},
	}

	oaiMsgs := buildCompatMessages(messages)

	if len(oaiMsgs) != 1 {
		t.Fatalf("expected 1 message (merged dev+user), got %d", len(oaiMsgs))
	}
	content, ok := oaiMsgs[0].Content.(string)
	if !ok {
		t.Fatalf("expected content to be string, got %T", oaiMsgs[0].Content)
	}
	if !strings.Contains(content, "Platform instruction.") {
		t.Errorf("first developer text was dropped — not found in %q", content)
	}
	if !strings.Contains(content, "Runtime instruction.") {
		t.Errorf("second developer text was dropped — not found in %q", content)
	}
	if !strings.Contains(content, "Hello") {
		t.Errorf("user text was dropped — not found in %q", content)
	}
}

func TestOpenAICompatProviderStreamSendsExplicitParallelToolCallsFalse(t *testing.T) {
	var got struct {
		ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
		Tools             []json.RawMessage `json:"tools,omitempty"`
		Stream            bool              `json:"stream"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewOpenAICompatProvider(ts.URL, "test-key", "test-model", "Test")
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

func TestOpenAICompatProviderStreamSendsExplicitZeroTemperatureAndTopP(t *testing.T) {
	var got struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
		Stream      bool     `json:"stream"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewOpenAICompatProvider(ts.URL, "test-key", "test-model", "Test")
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
	if !got.Stream {
		t.Fatal("expected stream=true")
	}
}

func TestOpenAICompatProviderStreamOmitsUnsetTemperatureAndTopP(t *testing.T) {
	var got struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewOpenAICompatProvider(ts.URL, "test-key", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{UserText("hello")},
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

	if got.Temperature != nil {
		t.Fatalf("expected temperature to be omitted, got %v", *got.Temperature)
	}
	if got.TopP != nil {
		t.Fatalf("expected top_p to be omitted, got %v", *got.TopP)
	}
}

func TestOpenAICompatProviderStreamSendsReasoningEffort(t *testing.T) {
	tests := []struct {
		name           string
		providerModel  string
		providerEffort string // set directly on provider
		requestModel   string // model override in Request
		requestEffort  string // req.ReasoningEffort field
		wantModel      string
		wantEffort     string
	}{
		{
			name:          "effort from provider model suffix",
			providerModel: "openai/gpt-5.4-xhigh",
			wantModel:     "openai/gpt-5.4",
			wantEffort:    "xhigh",
		},
		{
			name:          "effort from request model suffix",
			providerModel: "openai/gpt-5.4",
			requestModel:  "openai/gpt-5.4-high",
			wantModel:     "openai/gpt-5.4",
			wantEffort:    "high",
		},
		{
			name:          "no effort when not specified",
			providerModel: "openai/gpt-5.4",
			wantModel:     "openai/gpt-5.4",
			wantEffort:    "",
		},
		{
			name:           "request suffix wins over provider effort",
			providerModel:  "openai/gpt-5.4",
			providerEffort: "low",
			requestModel:   "openai/gpt-5.4-high",
			wantModel:      "openai/gpt-5.4",
			wantEffort:     "high",
		},
		{
			name:           "request reasoning_effort field wins over provider effort and suffix",
			providerModel:  "openai/gpt-5.4",
			providerEffort: "low",
			requestModel:   "openai/gpt-5.4-medium",
			requestEffort:  "high",
			wantModel:      "openai/gpt-5.4",
			wantEffort:     "high",
		},
		{
			name:          "minimal effort passes through",
			providerModel: "openai/gpt-5.4",
			requestEffort: "minimal",
			wantModel:     "openai/gpt-5.4",
			wantEffort:    "minimal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
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
			provider := NewOpenAICompatProvider(ts.URL, "test-key", actualModel, "Test")
			provider.effort = effort
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

			if got.Model != tt.wantModel {
				t.Errorf("model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.ReasoningEffort != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", got.ReasoningEffort, tt.wantEffort)
			}
		})
	}
}

func TestVLLMProviderStreamSendsThinkingControls(t *testing.T) {
	tests := []struct {
		name                string
		providerModel       string
		model               string
		effort              string
		paramOverride       string
		wantModel           string
		wantKwargs          map[string]interface{}
		wantReasoningEffort string
		wantBudget          *int
	}{
		{name: "default disables thinking", model: "Qwen/Qwen3.5-122B-A10B", wantModel: "Qwen/Qwen3.5-122B-A10B", wantKwargs: map[string]interface{}{"enable_thinking": false}},
		{name: "provider model suffix", providerModel: "Qwen/Qwen3.5-122B-A10B-high", wantModel: "Qwen/Qwen3.5-122B-A10B", wantKwargs: map[string]interface{}{"enable_thinking": true}, wantBudget: intPtr(10000)},
		{name: "low budget", model: "Qwen/Qwen3.5-122B-A10B-low", wantModel: "Qwen/Qwen3.5-122B-A10B", wantKwargs: map[string]interface{}{"enable_thinking": true}, wantBudget: intPtr(1024)},
		{name: "medium budget", effort: "medium", wantModel: "Qwen/Qwen3.5-122B-A10B", wantKwargs: map[string]interface{}{"enable_thinking": true}, wantBudget: intPtr(4096)},
		{name: "high budget", effort: "high", wantModel: "Qwen/Qwen3.5-122B-A10B", wantKwargs: map[string]interface{}{"enable_thinking": true}, wantBudget: intPtr(10000)},
		{name: "deepseek default uses thinking kwarg", providerModel: "deepseek-ai/DeepSeek-V3.1", wantModel: "deepseek-ai/DeepSeek-V3.1", wantKwargs: map[string]interface{}{"thinking": false}},
		{name: "deepseek low maps to top-level high", providerModel: "deepseek-ai/DeepSeek-V3.2-low", wantModel: "deepseek-ai/DeepSeek-V3.2", wantKwargs: map[string]interface{}{"thinking": true}, wantReasoningEffort: "high"},
		{name: "deepseek effort uses top-level high", providerModel: "deepseek-ai/DeepSeek-V3.2-high", wantModel: "deepseek-ai/DeepSeek-V3.2", wantKwargs: map[string]interface{}{"thinking": true}, wantReasoningEffort: "high"},
		{name: "deepseek max uses top-level max", providerModel: "deepseek-ai/DeepSeek-V3.2-max", wantModel: "deepseek-ai/DeepSeek-V3.2", wantKwargs: map[string]interface{}{"thinking": true}, wantReasoningEffort: "max"},
		{name: "override supports mistitled deepseek", providerModel: "cdck_qwen-high", paramOverride: "thinking", wantModel: "cdck_qwen", wantKwargs: map[string]interface{}{"thinking": true}, wantReasoningEffort: "high"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw map[string]interface{}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer ts.Close()

			providerModel := tt.providerModel
			if providerModel == "" {
				providerModel = "Qwen/Qwen3.5-122B-A10B"
			}
			provider := NewVLLMProvider(ts.URL, "test-key", providerModel, "Test vLLM")
			provider.vllmThinkingParam = tt.paramOverride
			stream, err := provider.Stream(context.Background(), Request{
				Model:           tt.model,
				Messages:        []Message{UserText("hello")},
				ReasoningEffort: tt.effort,
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

			gotEffort, effortPresent := raw["reasoning_effort"]
			if tt.wantReasoningEffort == "" {
				if effortPresent {
					t.Fatalf("reasoning_effort should be omitted, got %#v", gotEffort)
				}
			} else {
				got, ok := gotEffort.(string)
				if !ok || got != tt.wantReasoningEffort {
					t.Fatalf("reasoning_effort = %#v, want %q", gotEffort, tt.wantReasoningEffort)
				}
			}
			if got := raw["model"]; got != tt.wantModel {
				t.Fatalf("model = %#v, want %#v", got, tt.wantModel)
			}
			kwargs, ok := raw["chat_template_kwargs"].(map[string]interface{})
			if !ok {
				t.Fatalf("chat_template_kwargs missing or wrong type: %#v", raw["chat_template_kwargs"])
			}
			if len(kwargs) != len(tt.wantKwargs) {
				t.Fatalf("chat_template_kwargs = %#v, want %#v", kwargs, tt.wantKwargs)
			}
			for key, want := range tt.wantKwargs {
				if got := kwargs[key]; got != want {
					t.Fatalf("chat_template_kwargs[%q] = %#v, want %#v (all kwargs: %#v)", key, got, want, kwargs)
				}
			}
			if tt.wantBudget == nil {
				if got, ok := raw["thinking_token_budget"]; ok {
					t.Fatalf("thinking_token_budget should be omitted, got %#v", got)
				}
			} else {
				gotBudget, ok := raw["thinking_token_budget"].(float64)
				if !ok {
					t.Fatalf("thinking_token_budget missing or wrong type: %#v", raw["thinking_token_budget"])
				}
				if got := int(gotBudget); got != *tt.wantBudget {
					t.Fatalf("thinking_token_budget = %d, want %d", got, *tt.wantBudget)
				}
			}
		})
	}
}

func TestVLLMProviderReplaysReasoningWithCurrentFieldName(t *testing.T) {
	var got struct {
		Messages []struct {
			Role             string `json:"role"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	provider := NewVLLMProvider(ts.URL, "test-key", "Qwen/Qwen3.5-122B-A10B", "Test vLLM")
	stream, err := provider.Stream(context.Background(), Request{
		Messages: []Message{
			UserText("first"),
			{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "answer", ReasoningContent: "thinking trace"}}},
			UserText("second"),
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

	if len(got.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(got.Messages))
	}
	assistant := got.Messages[1]
	if assistant.Reasoning != "thinking trace" {
		t.Fatalf("assistant reasoning = %q, want thinking trace", assistant.Reasoning)
	}
	if assistant.ReasoningContent != "" {
		t.Fatalf("assistant reasoning_content should be omitted for vLLM, got %q", assistant.ReasoningContent)
	}
}

// unexpectedEOFReader wraps a reader and substitutes io.ErrUnexpectedEOF for
// the final io.EOF, mimicking Go's HTTP chunked-encoding transport when a local
// inference server (e.g. Ollama) drops the connection without a proper terminator.
type unexpectedEOFReader struct {
	r io.Reader
}

func (u *unexpectedEOFReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

type wrappedUnexpectedEOFReader struct {
	r io.Reader
}

func (u *wrappedUnexpectedEOFReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if err == io.EOF {
		return n, fmt.Errorf("wrapped EOF: %w", io.ErrUnexpectedEOF)
	}
	return n, err
}

func TestReadSSELine_TreatsUnexpectedEOFAsEOF(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantLine string
	}{
		{
			name:     "done marker without trailing newline",
			input:    "data: [DONE]",
			wantLine: "data: [DONE]",
		},
		{
			name:     "data line without trailing newline",
			input:    `data: {"choices":[]}`,
			wantLine: `data: {"choices":[]}`,
		},
		{
			name:     "empty input",
			input:    "",
			wantLine: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(&unexpectedEOFReader{r: strings.NewReader(tc.input)})
			line, eof, err := readSSELine(r)
			if err != nil {
				t.Fatalf("expected nil error for io.ErrUnexpectedEOF, got %v", err)
			}
			if !eof {
				t.Fatal("expected eof=true")
			}
			if line != tc.wantLine {
				t.Fatalf("expected line %q, got %q", tc.wantLine, line)
			}
		})
	}
}

func TestReadSSELine_TreatsWrappedUnexpectedEOFAsEOF(t *testing.T) {
	r := bufio.NewReader(&wrappedUnexpectedEOFReader{r: strings.NewReader("data: [DONE]")})
	line, eof, err := readSSELine(r)
	if err != nil {
		t.Fatalf("expected nil error for wrapped io.ErrUnexpectedEOF, got %v", err)
	}
	if !eof {
		t.Fatal("expected eof=true")
	}
	if line != "data: [DONE]" {
		t.Fatalf("expected line %q, got %q", "data: [DONE]", line)
	}
}

func TestOpenAICompatStream_ReturnsIncompleteErrorWithoutDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv text: %v", err)
	}
	if event.Type != EventTextDelta || event.Text != "hello" {
		t.Fatalf("expected text delta hello, got %+v", event)
	}
	event, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv error event: %v", err)
	}
	if event.Type != EventError {
		t.Fatalf("expected EventError, got %+v", event)
	}
	var incomplete *StreamIncompleteError
	if !errors.As(event.Err, &incomplete) {
		t.Fatalf("expected StreamIncompleteError, got %T %v", event.Err, event.Err)
	}
}

func TestOpenAICompatStream_EmitsToolCallBeforeIncompleteError(t *testing.T) {
	chunk := oaiChatResponse{
		Choices: []oaiChoice{{
			Delta:        &oaiMessage{ToolCalls: []oaiToolCall{{}}},
			FinishReason: "tool_calls",
		}},
	}
	chunk.Choices[0].Delta.ToolCalls[0].ID = "call-1"
	chunk.Choices[0].Delta.ToolCalls[0].Function.Name = "search"
	chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments = `{"query":"weather"}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("marshal chunk: %v", err)
		}
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(data)
		_, _ = w.Write([]byte("\n\n"))
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider(server.URL, "", "test-model", "Test")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hello"}}}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv tool call: %v", err)
	}
	if event.Type != EventToolCall || event.Tool == nil {
		t.Fatalf("expected EventToolCall, got %+v", event)
	}
	if event.Tool.ID != "call-1" || event.Tool.Name != "search" || string(event.Tool.Arguments) != `{"query":"weather"}` {
		t.Fatalf("unexpected tool call: %+v", event.Tool)
	}
	event, err = stream.Recv()
	if err != nil {
		t.Fatalf("recv error event: %v", err)
	}
	if event.Type != EventError {
		t.Fatalf("expected EventError, got %+v", event)
	}
	var incomplete *StreamIncompleteError
	if !errors.As(event.Err, &incomplete) {
		t.Fatalf("expected StreamIncompleteError, got %T %v", event.Err, event.Err)
	}
}
