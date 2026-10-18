package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/llm/providers/anthropic"
	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
	"github.com/samsaffron/term-llm/internal/llm/providers/gemini"
	"github.com/samsaffron/term-llm/internal/llm/providers/ollama"
	"github.com/samsaffron/term-llm/internal/llm/providers/opencompat"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
)

func TestEngineStripsConversationStartMarkersAcrossRequest(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("ok")
	engine := engine.NewEngine(provider, nil)
	first := ConversationStartMessage(time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC))
	second := ConversationStartMessage(time.Date(2026, time.January, 2, 3, 5, 0, 0, time.UTC))
	messages := []core.Message{first, core.UserText("hello"), second, core.UserText("again")}
	stream, err := engine.Stream(context.Background(), core.Request{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
	}

	requests := provider.RecordedRequests()
	if len(requests) != 1 || len(requests[0].Messages) != 4 {
		t.Fatalf("provider requests = %#v", requests)
	}
	for _, message := range requests[0].Messages {
		for _, part := range message.Parts {
			if part.Type == core.PartConversationStart {
				t.Fatalf("marker reached provider: %#v", requests[0].Messages)
			}
		}
	}
	if got := core.MessageText(requests[0].Messages[0]); got != core.MessageText(first) {
		t.Fatalf("developer text = %q, want %q", got, core.MessageText(first))
	}
	if !IsConversationStartMessage(messages[0]) || !IsConversationStartMessage(messages[2]) {
		t.Fatalf("source messages mutated: %#v", messages)
	}
}

func TestRunLoopPersistsDeduplicatedCumulativeReasoningSummary(t *testing.T) {
	events := cumulativeReasoningEvents(t)
	for i, event := range events {
		if event.Type == core.EventDone {
			events = append(events[:i], append([]core.Event{{Type: core.EventTextDelta, Text: "answer"}}, events[i:]...)...)
			break
		}
	}
	provider := &fakeProvider{script: func(call int, req core.Request) []core.Event {
		if call != 0 {
			t.Fatalf("provider called %d times, want 1", call+1)
		}
		return events
	}}
	tool := &countingTool{}
	registry := engine.NewToolRegistry()
	registry.Register(tool)
	engine := engine.NewEngine(provider, registry)

	var persisted core.Message
	engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, messages []core.Message, metrics core.TurnMetrics) error {
		if len(messages) > 0 {
			persisted = messages[len(messages)-1]
		}
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("test")},
		Tools:    []core.ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)

	if len(persisted.Parts) == 0 {
		t.Fatal("expected persisted assistant message")
	}
	textPart := persisted.Parts[0]
	if got, want := textPart.ReasoningContent, "**A**\n\n**B**\n\n**C**\n\n**D**"; got != want {
		t.Fatalf("persisted reasoning content = %q, want %q", got, want)
	}
	if got, want := strings.Join(textPart.ReasoningSummaryParts, "|"), "**A**|**B**|**C**|**D**"; got != want {
		t.Fatalf("persisted summary parts = %q, want %q", got, want)
	}
	if textPart.ReasoningItemID != "rs_3" || textPart.ReasoningEncryptedContent != "enc_3" {
		t.Fatalf("persisted reasoning metadata = id %q encrypted %q, want rs_3/enc_3", textPart.ReasoningItemID, textPart.ReasoningEncryptedContent)
	}

	var replayItems []string
	for _, part := range persisted.Parts {
		if part.Type != core.PartProviderReplay || part.ProviderReplay == nil {
			continue
		}
		var item responsesOutputItem
		if err := json.Unmarshal(part.ProviderReplay.Raw, &item); err != nil {
			t.Fatalf("decode replay item: %v", err)
		}
		replayItems = append(replayItems, fmt.Sprintf("%s:%s:%d", item.ID, item.EncryptedContent, len(item.Summary)))
	}
	if got, want := strings.Join(replayItems, "|"), "rs_0:enc_0:2|rs_1:enc_1:2|rs_2:enc_2:3|rs_3:enc_3:4"; got != want {
		t.Fatalf("persisted replay reasoning items = %q, want %q", got, want)
	}
}

func TestEngineResetConversationCallsWrappedProvider(t *testing.T) {
	t.Parallel()

	inner := &mockResettableProvider{MockProvider: mockprovider.NewMockProvider("test")}
	provider := WrapWithRetry(inner, DefaultRetryConfig())
	e := engine.NewEngine(provider, nil)

	e.ResetConversation()

	if inner.resetCalls != 1 {
		t.Errorf("expected wrapped provider ResetConversation called once, got %d", inner.resetCalls)
	}
}

func TestEngineRecoversToolCallAfterRetryWrappedStreamError(t *testing.T) {
	t.Parallel()

	streamErr := &responses.NonRecoverableStreamError{Err: errors.New("websocket disconnected after tool call")}
	tool := &countingTool{}
	registry := engine.NewToolRegistry()
	registry.Register(tool)

	inner := &recoverToolCallProvider{err: streamErr}
	provider := WrapWithRetry(inner, RetryConfig{MaxAttempts: 1, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	engine := engine.NewEngine(provider, registry)
	stream, err := engine.Stream(context.Background(), core.Request{
		Messages:   []core.Message{core.UserText("please write something")},
		Tools:      []core.ToolSpec{tool.Spec()},
		MaxTurns:   4,
		ToolChoice: core.ToolChoice{Mode: core.ToolChoiceAuto},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error = %v", err)
		}
		if event.Type == core.EventError && event.Err != nil {
			t.Fatalf("unexpected stream error event: %v", event.Err)
		}
		if event.Type == core.EventTextDelta {
			text.WriteString(event.Text)
		}
	}

	if got := tool.calls.Load(); got != 1 {
		t.Fatalf("tool executions = %d, want 1", got)
	}
	if got := text.String(); got != "recovered" {
		t.Fatalf("text = %q, want recovered", got)
	}
	if len(inner.calls) < 2 {
		t.Fatalf("provider calls = %d, want at least 2", len(inner.calls))
	}
	if countToolCalls(inner.calls[1].Messages) != 1 || countToolResults(inner.calls[1].Messages) != 1 {
		t.Fatalf("second request should include journaled tool call/result, got %#v", inner.calls[1].Messages)
	}
}

func TestConfigureContextManagementUsesOllamaRuntimeWindow(t *testing.T) {
	var showRequests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		showRequests = append(showRequests, req.Model)
		if req.Model == "qwen3.8:27b-thinking-low" {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		if req.Model != "qwen3.8:27b-thinking" {
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"capabilities":["completion","thinking","tools"],"parameters":"num_ctx 65536\nnum_predict 16384\n","model_info":{"qwen35.context_length":262144}}`)
	}))
	defer srv.Close()

	provider := ollama.New(srv.URL, "qwen3.8:27b-thinking", ollama.Options{})
	wrapped := WrapWithRetry(provider, RetryConfig{MaxAttempts: 1})
	engine := engine.NewEngine(wrapped, nil)
	engine.ConfigureContextManagement(wrapped, "chatgpt", "qwen3.8:27b-thinking-low", true)

	if got := engine.InputLimit(); got != 49152 {
		t.Fatalf("InputLimit() = %d, want 49152 (65536 num_ctx - 16384 num_predict)", got)
	}
	soft, hard, enabled := engine.CompactionThresholds()
	if !enabled || soft != 44236 || hard != 46694 {
		t.Fatalf("CompactionThresholds() = (%d, %d, %v), want (44236, 46694, true)", soft, hard, enabled)
	}
	if len(showRequests) < 2 || showRequests[0] != "qwen3.8:27b-thinking-low" {
		t.Fatalf("show requests = %v, want exact effort variant checked before base model", showRequests)
	}
}

func TestRushUsesNormalContinuationAcrossProviders(t *testing.T) {
	providers := map[string]core.Provider{
		"openai": &rp.OpenAIProvider{}, "openai-websocket": rp.NewOpenAIProviderWithOptions("", "", rp.OpenAIProviderOptions{UseWebSocket: true}),
		"compat": &opencompat.OpenAICompatProvider{}, "anthropic": &anthropic.AnthropicProvider{}, "gemini": &gemini.Provider{},
		"claude-bin": &clibin.ClaudeBinProvider{}, "cursor-bin": &clibin.CursorBinProvider{},
		"agy-bin": &clibin.AgyBinProvider{}, "grok-acp": &clibin.GrokBinProvider{},
		"custom": mockprovider.NewMockProvider("custom"), "retry": &RetryProvider{inner: mockprovider.NewMockProvider("custom")},
	}
	for name, provider := range providers {
		t.Run(name, func(t *testing.T) {
			e := engine.NewEngine(provider, nil)
			if e.SteeringAvailability().CanRush {
				t.Fatal("idle run offered Rush")
			}
			e.BeginSteeringRunForTest(true)
			e.QueueSteering(engine.QueuedSteering{ID: "guidance", Message: core.UserText("continue here"), Origin: engine.SteeringOriginUser})
			if !e.SteeringAvailability().CanRush {
				t.Fatal("provider type blocked normal stop-and-continue")
			}
			owner := engine.SteeringTransition{OperationID: "rush", Fence: 1}
			entries, err := e.FreezeSteering(owner)
			if err != nil || len(entries) != 1 {
				t.Fatalf("admission: %v, %+v", err, entries)
			}
			e.ReleaseSteeringFreeze(owner, false)
		})
	}
}

type listingProvider struct {
	*mockprovider.MockProvider
	models []core.ModelInfo
	calls  int
}

func (p *listingProvider) ListModels(context.Context) ([]core.ModelInfo, error) {
	p.calls++
	return p.models, nil
}

func TestConfigureContextManagementRefreshesCopilotModelMetadata(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	provider := &listingProvider{
		MockProvider: mockprovider.NewMockProvider("copilot"),
		models:       []core.ModelInfo{{ID: "live-copilot-model", InputLimit: 1234}},
	}
	e := engine.NewEngine(provider, nil)

	e.ConfigureContextManagement(provider, "copilot", "live-copilot-model-high", true)
	if provider.calls != 1 {
		t.Fatalf("ListModels calls = %d, want 1", provider.calls)
	}
	if got := e.InputLimit(); got != 1234 {
		t.Fatalf("InputLimit() after Copilot metadata refresh = %d, want 1234", got)
	}
	if soft, _, enabled := e.CompactionThresholds(); !enabled || soft <= 0 {
		t.Fatal("compaction disabled after Copilot metadata refresh, want enabled")
	}
}

func drainStream(t *testing.T, stream core.Stream) {
	t.Helper()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("Recv() error: %v", err)
		}
		if event.Type == core.EventError && event.Err != nil {
			t.Fatalf("stream error event: %v", event.Err)
		}
	}
}

type fakeProvider struct {
	script          func(call int, req core.Request) []core.Event
	calls           []core.Request
	capabilities    core.Capabilities
	hasCapabilities bool
	resetCalls      int
	listModels      []core.ModelInfo
	listModelsErr   error
	listModelCalls  int
}

func (p *fakeProvider) Name() string {
	return "fake"
}

func (p *fakeProvider) Credential() string {
	return "test"
}

func (p *fakeProvider) Capabilities() core.Capabilities {
	if p.hasCapabilities {
		return p.capabilities
	}
	return core.Capabilities{
		NativeWebSearch: false,
		NativeWebFetch:  false,
		ToolCalls:       true,
	}
}

func (p *fakeProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.calls = append(p.calls, req)
	call := len(p.calls) - 1
	events := p.script(call, req)
	return &sliceStream{events: events}, nil
}

func (p *fakeProvider) ResetConversation() {
	p.resetCalls++
}

func (p *fakeProvider) ListModels(ctx context.Context) ([]core.ModelInfo, error) {
	p.listModelCalls++
	return p.listModels, p.listModelsErr
}

type countingTool struct {
	calls atomic.Int64
}

func (t *countingTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "count_tool",
		Description: "Counts executions",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *countingTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	t.calls.Add(1)
	return core.TextOutput("ok"), nil
}

func (t *countingTool) Preview(args json.RawMessage) string {
	return ""
}

func countToolResults(messages []core.Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == core.PartToolResult {
				count++
			}
		}
	}
	return count
}

func countToolCalls(messages []core.Message) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == core.PartToolCall {
				count++
			}
		}
	}
	return count
}

// mockResettableProvider wraps mockprovider.MockProvider and tracks ResetConversation calls.
type mockResettableProvider struct {
	*mockprovider.MockProvider
	resetCalls int
}

func (m *mockResettableProvider) ResetConversation() {
	m.resetCalls++
}

type recoverToolCallProvider struct {
	err      error
	calls    []core.Request
	toolName string
}

func (p *recoverToolCallProvider) Name() string { return "recover-tool-call" }

func (p *recoverToolCallProvider) Credential() string { return "test" }

func (p *recoverToolCallProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true}
}

func (p *recoverToolCallProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.calls = append(p.calls, req)
	if len(p.calls) == 1 {
		toolName := p.toolName
		if toolName == "" {
			toolName = "count_tool"
		}
		return &errAfterEventsStream{
			events: []core.Event{{Type: core.EventToolCall, Tool: &core.ToolCall{ID: "call-1", Name: toolName, Arguments: json.RawMessage(`{}`)}}},
			err:    p.err,
		}, nil
	}
	return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "recovered"}, {Type: core.EventDone}}}, nil
}

// providerProjection returns the messages the engine hands the provider for
// messages, observing the provider-facing projection through Stream.
func providerProjection(t *testing.T, messages []core.Message) []core.Message {
	t.Helper()
	provider := mockprovider.NewMockProvider("projection").AddTextResponse("ok")
	stream, err := engine.NewEngine(provider, nil).Stream(context.Background(), core.Request{Messages: messages})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)
	requests := provider.RecordedRequests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	return requests[0].Messages
}

type sliceStream struct {
	events []core.Event
	index  int
}

func (s *sliceStream) Recv() (core.Event, error) {
	if s.index >= len(s.events) {
		return core.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *sliceStream) Close() error {
	return nil
}

type errAfterEventsStream struct {
	events []core.Event
	err    error
	index  int
}

func (s *errAfterEventsStream) Recv() (core.Event, error) {
	if s.index < len(s.events) {
		event := s.events[s.index]
		s.index++
		return event, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return core.Event{}, err
	}
	return core.Event{}, io.EOF
}

func (s *errAfterEventsStream) Close() error {
	return nil
}
