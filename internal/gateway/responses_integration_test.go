package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	openairesponses "github.com/openai/openai-go/responses"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

type responsesFixtureProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	events   [][]llm.Event
	errors   []error
	started  chan struct{}
	canceled chan struct{}
}

func (*responsesFixtureProvider) Name() string       { return "responses-fixture" }
func (*responsesFixtureProvider) Credential() string { return "mock" }
func (*responsesFixtureProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true, SupportsToolChoice: true}
}
func (p *responsesFixtureProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	index := len(p.requests) - 1
	var events []llm.Event
	if index < len(p.events) {
		events = append([]llm.Event(nil), p.events[index]...)
	}
	var terminalErr error
	if index < len(p.errors) {
		terminalErr = p.errors[index]
	}
	started := p.started
	canceled := p.canceled
	p.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
		return &responsesBlockingStream{ctx: ctx, canceled: canceled}, nil
	}
	return &responsesSliceStream{events: events, terminalErr: terminalErr}, nil
}
func (p *responsesFixtureProvider) recorded() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

type responsesSliceStream struct {
	events      []llm.Event
	index       int
	terminalErr error
}

func (s *responsesSliceStream) Recv() (llm.Event, error) {
	if s.index >= len(s.events) {
		if s.terminalErr != nil {
			err := s.terminalErr
			s.terminalErr = nil
			return llm.Event{}, err
		}
		return llm.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*responsesSliceStream) Close() error { return nil }

type responsesBlockingStream struct {
	ctx      context.Context
	canceled chan struct{}
	once     sync.Once
}

func (s *responsesBlockingStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	s.once.Do(func() {
		if s.canceled != nil {
			close(s.canceled)
		}
	})
	return llm.Event{}, s.ctx.Err()
}
func (s *responsesBlockingStream) Close() error {
	s.once.Do(func() {
		if s.canceled != nil {
			close(s.canceled)
		}
	})
	return nil
}

func TestResponsesOfficialOpenAIClientsNonStreamingStreamingAndModels(t *testing.T) {
	provider := &responsesFixtureProvider{events: [][]llm.Event{
		{
			{Type: llm.EventTextDelta, Text: "hello "},
			{Type: llm.EventTextDelta, Text: "client"},
			{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 7, CachedInputTokens: 2, OutputTokens: 3, ProviderTotalTokens: 12}},
			{Type: llm.EventDone},
		},
		{
			{Type: llm.EventTextDelta, Text: "streamed"},
			{Type: llm.EventDone},
		},
	}}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	client := openai.NewClient(option.WithAPIKey(fixture.token), option.WithBaseURL(fixture.server.URL+"/v1"))

	response, err := client.Responses.New(t.Context(), openairesponses.ResponseNewParams{
		Model:        "remote/model-a",
		Input:        openairesponses.ResponseNewParamsInputUnion{OfString: openai.String("hello")},
		Instructions: openai.String("official instructions"),
		Metadata:     map[string]string{"source": "official-client"},
		Temperature:  openai.Float(0.25),
		TopP:         openai.Float(0.75),
	})
	if err != nil {
		t.Fatalf("official Responses client: %v", err)
	}
	if got := response.OutputText(); got != "hello client" {
		t.Fatalf("official client output = %q", got)
	}
	if response.Usage.InputTokens != 9 || response.Usage.InputTokensDetails.CachedTokens != 2 || response.Usage.OutputTokens != 3 || response.Usage.TotalTokens != 12 {
		t.Fatalf("official client usage = %+v", response.Usage)
	}
	assertOfficialResponsesDocument(t, *response)
	if response.Instructions.AsString() != "official instructions" || response.Metadata["source"] != "official-client" || response.Temperature != 0.25 || response.TopP != 0.75 || response.ToolChoice.AsToolChoiceMode() != "auto" || len(response.Tools) != 0 || !response.ParallelToolCalls {
		t.Fatalf("official client required response fields = %+v", response)
	}
	stream := client.Responses.NewStreaming(t.Context(), openairesponses.ResponseNewParams{
		Model: "remote/model-a",
		Input: openairesponses.ResponseNewParamsInputUnion{OfString: openai.String("stream")},
	})
	defer stream.Close()
	var streamTypes []string
	var streamedText strings.Builder
	var sawCreated, sawInProgress, sawCompleted, sawTextDone bool
	for stream.Next() {
		current := stream.Current()
		streamTypes = append(streamTypes, current.Type)
		switch event := current.AsAny().(type) {
		case openairesponses.ResponseCreatedEvent:
			sawCreated = true
			assertOfficialResponsesDocument(t, event.Response)
			assertOfficialResponsesDefaults(t, event.Response)
			if !event.JSON.Response.Valid() || !event.JSON.SequenceNumber.Valid() || !event.JSON.Type.Valid() {
				t.Fatalf("created event JSON validity = %+v", event.JSON)
			}
		case openairesponses.ResponseInProgressEvent:
			sawInProgress = true
			assertOfficialResponsesDocument(t, event.Response)
			assertOfficialResponsesDefaults(t, event.Response)
		case openairesponses.ResponseTextDeltaEvent:
			streamedText.WriteString(event.Delta)
			if !event.JSON.Logprobs.Valid() || event.Logprobs == nil {
				t.Fatalf("text delta logprobs missing: raw=%s", event.RawJSON())
			}
		case openairesponses.ResponseTextDoneEvent:
			sawTextDone = true
			if !event.JSON.Logprobs.Valid() || event.Logprobs == nil || event.Text != "streamed" {
				t.Fatalf("text done event = %+v raw=%s", event, event.RawJSON())
			}
		case openairesponses.ResponseCompletedEvent:
			sawCompleted = true
			assertOfficialResponsesDocument(t, event.Response)
			assertOfficialResponsesDefaults(t, event.Response)
			if event.Response.OutputText() != "streamed" || event.Response.Status != "completed" {
				t.Fatalf("typed completed response = %+v", event.Response)
			}
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("official streaming Responses client: %v", err)
	}
	if !sawCreated || !sawInProgress || !sawTextDone || !sawCompleted || streamedText.String() != "streamed" || !responsesEventTypesContain(streamTypes, "response.output_text.delta") || len(streamTypes) == 0 || streamTypes[len(streamTypes)-1] != "response.completed" {
		t.Fatalf("official streaming events = types:%v text:%q typed:%t/%t/%t/%t", streamTypes, streamedText.String(), sawCreated, sawInProgress, sawTextDone, sawCompleted)
	}
	page, err := client.Models.List(t.Context())
	if err != nil {
		t.Fatalf("official models client: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID != "remote/model-a" {
		t.Fatalf("official models = %+v", page.Data)
	}

	requests := provider.recorded()
	if len(requests) != 2 || requests[0].Model != "model-a" || len(requests[0].Messages) != 2 || llm.MessageText(requests[0].Messages[0]) != "official instructions" || llm.MessageText(requests[0].Messages[1]) != "hello" || llm.MessageText(requests[1].Messages[0]) != "stream" {
		t.Fatalf("translated official requests = %+v", requests)
	}
	fixture.usage.mu.Lock()
	defer fixture.usage.mu.Unlock()
	if len(fixture.usage.records) != 2 || fixture.usage.records[0].ClientID != fixture.client.ID || fixture.usage.records[0].ProviderKey != "remote" || fixture.usage.records[0].InputTokens != 7 || fixture.usage.records[0].CachedInputTokens != 2 {
		t.Fatalf("Responses usage attribution = %+v", fixture.usage.records)
	}
}

func TestResponsesDiscourseShapedStreamRejectsPreOutputProviderFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "authentication",
			err:        providerhttp.NewStatusErrorString("OpenAI", http.StatusUnauthorized, "401 Unauthorized", nil, `api_key=super-secret-provider-key`),
			wantStatus: http.StatusUnauthorized,
			wantCode:   "provider_api_key_unauthenticated",
		},
		{
			name:       "rate limit",
			err:        &llm.RateLimitError{Message: "raw provider quota detail"},
			wantStatus: http.StatusTooManyRequests,
			wantCode:   "provider_rate_limited",
		},
		{
			name:       "upstream",
			err:        providerhttp.NewStatusErrorString("OpenAI", http.StatusServiceUnavailable, "503 Service Unavailable", nil, `raw upstream body /srv/private`),
			wantStatus: http.StatusBadGateway,
			wantCode:   "provider_upstream_failure",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := &responsesFixtureProvider{errors: []error{tc.err}}
			fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
			fixture.gateway.cfg.UpstreamRetryAttempts = 1
			response := doResponsesRequest(t, fixture, map[string]any{
				"model": "remote/model-a", "input": "hello", "stream": true,
			})
			if response.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				t.Fatalf("pre-output failure status = %d body=%s, want %d", response.StatusCode, body, tc.wantStatus)
			}
			if output, err := discourseShapedResponsesStream(response); err == nil {
				t.Fatalf("Discourse-shaped client accepted failed stream as successful output %q", output)
			} else if !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("Discourse-shaped error = %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestResponsesCommittedStreamUsesStandardErrorAndFailedEvents(t *testing.T) {
	provider := &responsesFixtureProvider{
		events: [][]llm.Event{{{Type: llm.EventTextDelta, Text: "partial"}}},
		errors: []error{providerhttp.NewStatusErrorString("OpenAI", http.StatusServiceUnavailable, "503 Service Unavailable", nil, `raw body super-secret-provider-key /srv/private`)},
	}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	fixture.gateway.cfg.UpstreamRetryAttempts = 1
	response := doResponsesRequest(t, fixture, map[string]any{
		"model": "remote/model-a", "input": "hello", "stream": true,
		"instructions": "keep this", "metadata": map[string]string{"client": "discourse"},
		"temperature": 0.4, "top_p": 0.6,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("committed stream status = %d: %s", response.StatusCode, body)
	}
	events := readResponsesEvents(t, response.Body)
	assertResponsesEventTypes(t, events, "response.created", "response.output_text.delta", "error", "response.failed")
	errorEvent := findResponsesEvent(t, events, "error")
	if _, nested := errorEvent["error"]; nested || errorEvent["code"] != "provider_upstream_failure" || errorEvent["message"] == "" {
		t.Fatalf("Responses error event shape = %#v", errorEvent)
	}
	if _, present := errorEvent["param"]; !present {
		t.Fatalf("Responses error event omitted required param: %#v", errorEvent)
	}
	failed := findResponsesEvent(t, events, "response.failed")
	failedResponse := failed["response"].(map[string]any)
	for _, field := range []string{"id", "object", "created_at", "status", "error", "incomplete_details", "instructions", "metadata", "model", "output", "parallel_tool_calls", "temperature", "tool_choice", "tools", "top_p"} {
		if _, present := failedResponse[field]; !present {
			t.Fatalf("failed response omitted %q: %#v", field, failedResponse)
		}
	}
	apiError := failedResponse["error"].(map[string]any)
	if failedResponse["status"] != "failed" || apiError["code"] != "provider_upstream_failure" || apiError["message"] == "" {
		t.Fatalf("failed response shape = %#v", failedResponse)
	}
	encoded, _ := json.Marshal(events)
	for _, secret := range []string{"super-secret-provider-key", "raw body", "/srv/private"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("stream error leaked central diagnostic %q: %s", secret, encoded)
		}
	}
}

func TestResponsesDiscourseStreamingReasoningMultimodalAndFunctionCall(t *testing.T) {
	provider := &responsesFixtureProvider{events: [][]llm.Event{{
		{Type: llm.EventReasoningDelta, Text: "**Thinking**", ReasoningKind: llm.ReasoningKindSummary, ReasoningItemID: "rs_provider", ReasoningEncryptedContent: "ENC", ReasoningSummaryParts: []string{"**Thinking**"}, ReasoningFinal: true},
		{Type: llm.EventTextDelta, Text: "answer"},
		{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "call_external", Name: "echo", Arguments: json.RawMessage(`{"string":"hello"}`)}},
		{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 6, ReasoningTokens: 2}},
		{Type: llm.EventDone},
	}}}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	payload := map[string]any{
		"model": "remote/model-a", "stream": true, "max_output_tokens": 123,
		"reasoning": map[string]any{"summary": "auto", "effort": "high"},
		"include":   []string{"reasoning.encrypted_content"}, "temperature": 0.2, "top_p": 0.8,
		"service_tier": "priority", "parallel_tool_calls": true,
		"input": []any{
			map[string]any{"role": "developer", "content": "policy"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "inspect"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aW1n"},
				map[string]any{"type": "input_file", "filename": "notes.txt", "file_data": "data:text/plain;base64,ZmlsZQ=="},
			}},
		},
		"tools":       []any{map[string]any{"type": "function", "name": "echo", "description": "echo text", "parameters": map[string]any{"type": "object", "properties": map[string]any{"string": map[string]any{"type": "string"}}, "required": []string{"string"}}}},
		"tool_choice": map[string]any{"type": "function", "name": "echo"},
	}
	response := doResponsesRequest(t, fixture, payload)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("stream response = %d %s: %s", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	events := readResponsesEvents(t, response.Body)
	assertResponsesEventTypes(t, events,
		"response.created", "response.in_progress", "response.output_item.added",
		"response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.output_item.done", "response.output_item.added", "response.output_text.delta",
		"response.output_item.done", "response.output_item.added", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done", "response.completed",
	)
	completed := findResponsesEvent(t, events, "response.completed")
	completedResponse := completed["response"].(map[string]any)
	output := completedResponse["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("completed output = %#v", output)
	}
	reasoning := output[0].(map[string]any)
	message := output[1].(map[string]any)
	function := output[2].(map[string]any)
	if reasoning["id"] != "rs_provider" || reasoning["encrypted_content"] != "ENC" || message["type"] != "message" || function["id"] == function["call_id"] || function["call_id"] != "call_external" {
		t.Fatalf("completed items = %#v", output)
	}
	addedIDs := make(map[string]any)
	for _, event := range events {
		if event["type"] != "response.output_item.added" {
			continue
		}
		item := event["item"].(map[string]any)
		addedIDs[item["type"].(string)] = item["id"]
	}
	if addedIDs["reasoning"] != reasoning["id"] || addedIDs["message"] != message["id"] || addedIDs["function_call"] != function["id"] {
		t.Fatalf("unstable stream item IDs: added=%#v completed=%#v", addedIDs, output)
	}
	usage := completedResponse["usage"].(map[string]any)
	if usage["input_tokens"] != float64(14) || usage["output_tokens"] != float64(6) || usage["total_tokens"] != float64(20) {
		t.Fatalf("completed usage = %#v", usage)
	}

	requests := provider.recorded()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	request := requests[0]
	if request.ReasoningEffort != "high" || request.MaxOutputTokens != 123 || !request.TemperatureSet || !request.TopPSet || request.ServiceTier != "priority" || !request.ParallelToolCalls || len(request.Tools) != 1 || request.Tools[0].Name != "echo" || request.ToolChoice.Mode != llm.ToolChoiceName {
		t.Fatalf("translated request options = %+v", request)
	}
	properties, schemaOK := request.Tools[0].Schema["properties"].(map[string]interface{})
	if !schemaOK || properties["string"] == nil {
		t.Fatalf("translated function schema = %#v", request.Tools[0].Schema)
	}
	if len(request.Messages) != 2 || len(request.Messages[1].Parts) != 3 || request.Messages[1].Parts[1].ImageData == nil || request.Messages[1].Parts[1].ImageData.Base64 != "aW1n" || request.Messages[1].Parts[2].FileData == nil || request.Messages[1].Parts[2].FileData.Filename != "notes.txt" {
		t.Fatalf("translated multimodal input = %+v", request.Messages)
	}
}

func TestResponsesNonStreamingReasoningAndFunctionCall(t *testing.T) {
	provider := &responsesFixtureProvider{events: [][]llm.Event{{
		{Type: llm.EventReasoningDelta, Text: "summary", ReasoningKind: llm.ReasoningKindSummary, ReasoningItemID: "rs_nonstream", ReasoningEncryptedContent: "ENC", ReasoningFinal: true},
		{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "call_nonstream", Name: "echo", Arguments: json.RawMessage(`{"value":1}`)}},
		{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 3, OutputTokens: 2, ReasoningTokens: 1}},
		{Type: llm.EventDone},
	}}}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	response := doResponsesRequest(t, fixture, map[string]any{
		"model": "remote/model-a", "input": "call a function",
		"tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
	})
	defer response.Body.Close()
	var document map[string]any
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	output := document["output"].([]any)
	if response.StatusCode != http.StatusOK || len(output) != 2 || output[0].(map[string]any)["id"] != "rs_nonstream" || output[1].(map[string]any)["call_id"] != "call_nonstream" {
		t.Fatalf("nonstream reasoning/function response = %d %#v", response.StatusCode, document)
	}
}

func TestResponsesStatelessDiscourseFunctionOutputAndReasoningReplay(t *testing.T) {
	provider := &responsesFixtureProvider{events: [][]llm.Event{{{Type: llm.EventTextDelta, Text: "continued"}, {Type: llm.EventDone}}}}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	payload := map[string]any{
		"model": "remote/model-a",
		"input": []any{
			map[string]any{"type": "reasoning", "id": "rs_old", "encrypted_content": "ENC_OLD", "summary": []any{map[string]any{"type": "summary_text", "text": "old summary"}}},
			map[string]any{"type": "message", "id": "msg_old", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "I will call echo"}}},
			// Discourse omits the optional output item id when provider metadata is absent.
			map[string]any{"type": "function_call", "call_id": "call_old", "name": "echo", "arguments": `{"string":"old"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_old", "output": "tool result"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
	}
	response := doResponsesRequest(t, fixture, payload)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("continuation status = %d: %s", response.StatusCode, body)
	}
	requests := provider.recorded()
	if len(requests) != 1 || len(requests[0].Messages) != 4 {
		t.Fatalf("continuation request = %+v", requests)
	}
	if replay := requests[0].Messages[0].Parts[0].ProviderReplay; replay == nil || !bytes.Contains(replay.Raw, []byte(`"encrypted_content":"ENC_OLD"`)) {
		t.Fatalf("reasoning replay = %+v", requests[0].Messages[0].Parts)
	}
	if replay := requests[0].Messages[1].Parts[0].ProviderReplay; replay == nil || !bytes.Contains(replay.Raw, []byte(`"id":"msg_old"`)) {
		t.Fatalf("assistant message replay = %+v", requests[0].Messages[1].Parts)
	}
	if replay := requests[0].Messages[2].Parts[0].ProviderReplay; replay == nil || bytes.Contains(replay.Raw, []byte(`"id"`)) || !bytes.Contains(replay.Raw, []byte(`"call_id":"call_old"`)) {
		t.Fatalf("function replay without provider item id = %+v", requests[0].Messages[2].Parts)
	}
	if call := requests[0].Messages[2].Parts[1].ToolCall; call == nil || call.ID != "call_old" {
		t.Fatalf("function replay = %+v", requests[0].Messages[2].Parts)
	}
	if result := requests[0].Messages[3].Parts[0].ToolResult; result == nil || result.ID != "call_old" || result.Content != "tool result" {
		t.Fatalf("function output = %+v", requests[0].Messages[3].Parts)
	}
}

func TestResponsesFunctionCallHistoryPreservesSuppliedItemID(t *testing.T) {
	message, wireErr := decodeResponsesFunctionCall(json.RawMessage(`{"type":"function_call","id":"fc_provider","call_id":"call_provider","name":"echo","arguments":"{}"}`), 0)
	if wireErr != nil {
		t.Fatal(wireErr)
	}
	if len(message.Parts) != 2 || message.Parts[0].ProviderReplay == nil || !bytes.Contains(message.Parts[0].ProviderReplay.Raw, []byte(`"id":"fc_provider"`)) || message.Parts[1].ToolCall == nil || message.Parts[1].ToolCall.ID != "call_provider" {
		t.Fatalf("function history with supplied item ID = %+v", message.Parts)
	}
}

func TestResponsesNamespacePolicyErrorsAndAllowedModels(t *testing.T) {
	if provider, model, err := splitResponsesModel("openrouter/moonshotai/kimi-k2"); err != nil || provider != "openrouter" || model != "moonshotai/kimi-k2" {
		t.Fatalf("first-slash namespace = %q/%q err=%v", provider, model, err)
	}
	provider := &responsesFixtureProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	unauthorizedBody, _ := json.Marshal(map[string]any{"model": "remote/model-a", "input": "hello"})
	unauthorizedRequest, _ := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/responses", bytes.NewReader(unauthorizedBody))
	unauthorizedResponse, err := http.DefaultClient.Do(unauthorizedRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertResponsesError(t, unauthorizedResponse, http.StatusUnauthorized, "gateway_client_unauthorized")
	for _, model := range []string{"", "model-a", "/model-a", "remote/"} {
		response := doResponsesRequest(t, fixture, map[string]any{"model": model, "input": "hello"})
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || body["error"].(map[string]any)["code"] != "invalid_model_namespace" {
			t.Fatalf("bad namespace %q = %d %#v", model, response.StatusCode, body)
		}
	}
	unknown := doResponsesRequest(t, fixture, map[string]any{"model": "missing/model-a", "input": "hello"})
	assertResponsesError(t, unknown, http.StatusNotFound, "unknown_provider")
	unknownModel := doResponsesRequest(t, fixture, map[string]any{"model": "remote/not-configured", "input": "hello"})
	assertResponsesError(t, unknownModel, http.StatusNotFound, "unknown_model")

	_, deniedToken, err := fixture.clients.Add("denied-token", Policy{AllowProviders: []string{"other"}})
	if err != nil {
		t.Fatal(err)
	}
	requestBody, _ := json.Marshal(map[string]any{"model": "remote/model-a", "input": "hello"})
	req, _ := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer "+deniedToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertResponsesError(t, response, http.StatusForbidden, "policy_denied")

	modelsReq, _ := http.NewRequest(http.MethodGet, fixture.server.URL+"/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer "+deniedToken)
	modelsResponse, err := http.DefaultClient.Do(modelsReq)
	if err != nil {
		t.Fatal(err)
	}
	defer modelsResponse.Body.Close()
	var models struct {
		Object string `json:"object"`
		Data   []any  `json:"data"`
	}
	if err := json.NewDecoder(modelsResponse.Body).Decode(&models); err != nil || models.Object != "list" || len(models.Data) != 0 {
		t.Fatalf("denied models = %+v err=%v", models, err)
	}
}

func TestResponsesModelsListOnlyPolicyAllowedNamespaced(t *testing.T) {
	provider := &responsesFixtureProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	remote := fixture.central.Providers["remote"]
	remote.Models = []string{"model-a", "model-b"}
	fixture.central.Providers["remote"] = remote
	fixture.central.Providers["other"] = config.ProviderConfig{Type: config.ProviderTypeOpenAI, Model: "other-model", Models: []string{"other-model"}, APIKey: "configured"}
	_, token, err := fixture.clients.Add("model-filter", Policy{AllowProviders: []string{"remote"}, AllowModels: []string{"remote:model-a"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, fixture.server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "remote/model-a" || list.Data[0].Provider != "remote" {
		t.Fatalf("policy-filtered models = %d %+v", response.StatusCode, list)
	}
}

func TestResponsesRejectsHostedToolsAndInlineCLIToolLoops(t *testing.T) {
	provider := &responsesFixtureProvider{}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	hosted := doResponsesRequest(t, fixture, map[string]any{"model": "remote/model-a", "input": "hello", "tools": []any{map[string]any{"type": "web_search"}}})
	assertResponsesError(t, hosted, http.StatusBadRequest, "unsupported_tool_type")
	previousState := doResponsesRequest(t, fixture, map[string]any{"model": "remote/model-a", "input": "hello", "previous_response_id": "resp_forged"})
	assertResponsesError(t, previousState, http.StatusBadRequest, "unsupported_field")
	unknownField := doResponsesRequest(t, fixture, map[string]any{"model": "remote/model-a", "input": "hello", "gateway_tools": true})
	assertResponsesError(t, unknownField, http.StatusBadRequest, "unsupported_field")

	inline := &inlineProvider{}
	cliFixture := newGatewayFixture(t, config.ProviderTypeGrokBin, inline, time.Second)
	incompatible := doResponsesRequest(t, cliFixture, map[string]any{
		"model": "remote/model-a", "input": "hello",
		"tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
	})
	assertResponsesError(t, incompatible, http.StatusBadRequest, "incompatible_tool_request")
	inline.mu.Lock()
	defer inline.mu.Unlock()
	if inline.response.Result.Content != "" || inline.response.Err != nil {
		t.Fatalf("inline provider tool loop unexpectedly ran: %+v", inline.response)
	}
}

func TestResponsesDisconnectCancelsProviderAndRecordsCanceledUsage(t *testing.T) {
	provider := &responsesFixtureProvider{started: make(chan struct{}), canceled: make(chan struct{})}
	fixture := newGatewayFixture(t, config.ProviderTypeOpenAI, provider, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{"model": "remote/model-a", "input": "wait", "stream": true})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fixture.server.URL+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fixture.token)
	req.Header.Set("Content-Type", "application/json")
	result := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = bufio.NewReader(resp.Body).ReadString('\n')
			_ = resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case <-provider.canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP disconnect did not cancel provider")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("HTTP request did not return after cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for {
		fixture.usage.mu.Lock()
		if len(fixture.usage.records) > 0 {
			record := fixture.usage.records[len(fixture.usage.records)-1]
			fixture.usage.mu.Unlock()
			if record.ErrorCode != "canceled" {
				t.Fatalf("canceled usage = %+v", record)
			}
			break
		}
		fixture.usage.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("canceled usage was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertOfficialResponsesDocument(t *testing.T, response openairesponses.Response) {
	t.Helper()
	checks := []struct {
		name  string
		valid bool
	}{
		{"id", response.JSON.ID.Valid()},
		{"created_at", response.JSON.CreatedAt.Valid()},
		{"error", response.JSON.Error.Raw() != ""},
		{"incomplete_details", response.JSON.IncompleteDetails.Raw() != ""},
		{"instructions", response.JSON.Instructions.Raw() != ""},
		{"metadata", response.JSON.Metadata.Valid()},
		{"model", response.JSON.Model.Valid()},
		{"object", response.JSON.Object.Valid()},
		{"output", response.JSON.Output.Valid()},
		{"parallel_tool_calls", response.JSON.ParallelToolCalls.Valid()},
		{"temperature", response.JSON.Temperature.Valid()},
		{"tool_choice", response.JSON.ToolChoice.Valid()},
		{"tools", response.JSON.Tools.Valid()},
		{"top_p", response.JSON.TopP.Valid()},
	}
	for _, check := range checks {
		if !check.valid {
			t.Fatalf("official Responses document omitted required field %q: %s", check.name, response.RawJSON())
		}
	}
	if response.ID == "" || response.Object != "response" || response.Model != "remote/model-a" || response.Output == nil || response.Metadata == nil || response.Tools == nil {
		t.Fatalf("official typed Responses document fields = %+v", response)
	}
}

func assertOfficialResponsesDefaults(t *testing.T, response openairesponses.Response) {
	t.Helper()
	if response.JSON.Instructions.Raw() != "null" || len(response.Metadata) != 0 || response.Temperature != 1 || response.TopP != 1 || response.ToolChoice.AsToolChoiceMode() != "auto" || len(response.Tools) != 0 || !response.ParallelToolCalls {
		t.Fatalf("official Responses defaults = %+v raw=%s", response, response.RawJSON())
	}
}

func responsesEventTypesContain(types []string, want string) bool {
	for _, eventType := range types {
		if eventType == want {
			return true
		}
	}
	return false
}

func doResponsesRequest(t *testing.T, fixture *gatewayFixture, payload any) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, fixture.server.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fixture.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func discourseShapedResponsesStream(response *http.Response) (string, error) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			return "", fmt.Errorf("Responses HTTP %d: invalid error body: %w", response.StatusCode, err)
		}
		return "", fmt.Errorf("Responses HTTP %d: %s: %s", response.StatusCode, body.Error.Code, body.Error.Message)
	}
	var output strings.Builder
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Type == "response.output_text.delta" {
			output.WriteString(event.Delta)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	// Discourse's Net::HTTP edge trusts the HTTP status and consumes text deltas;
	// it cannot reinterpret an HTTP 200 with no deltas as an upstream rejection.
	return output.String(), nil
}

func readResponsesEvents(t *testing.T, reader io.Reader) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") || line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-Discourse SSE line %q", line)
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode SSE event: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func assertResponsesEventTypes(t *testing.T, events []map[string]any, required ...string) {
	t.Helper()
	positions := make(map[string][]int)
	for i, event := range events {
		positions[event["type"].(string)] = append(positions[event["type"].(string)], i)
	}
	last := -1
	for _, eventType := range required {
		found := -1
		for _, position := range positions[eventType] {
			if position > last {
				found = position
				break
			}
		}
		if found < 0 {
			t.Fatalf("event %q not found after position %d; events=%#v", eventType, last, events)
		}
		last = found
	}
}

func findResponsesEvent(t *testing.T, events []map[string]any, eventType string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["type"] == eventType {
			return event
		}
	}
	t.Fatalf("event %q not found", eventType)
	return nil
}

func assertResponsesError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status || body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("Responses error = %d %+v, want %d/%s", response.StatusCode, body.Error, status, code)
	}
	for _, forbidden := range []string{"super-secret-provider-key", filepath.Clean(t.TempDir())} {
		if forbidden != "" && strings.Contains(body.Error.Message, forbidden) {
			t.Fatalf("unsafe error message = %q", body.Error.Message)
		}
	}
}
