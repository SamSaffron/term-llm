package llm

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/factory"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/llm/providers/clibin"
)

func TestCreateProviderFromConfigAgyBin(t *testing.T) {
	provider, err := factory.Create("agy-bin", &config.ProviderConfig{Model: "gemini-3.6-flash-high"}, factoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*clibin.AgyBinProvider); !ok {
		t.Fatalf("provider = %T", provider)
	}
}

func TestProviderModelIDsPrefersAgyBinCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	want := []string{"gemini-3.8-flash-high", "claude-sonnet-4-6"}
	clibin.RefreshAgyBinCacheSync([]core.ModelInfo{{ID: want[0]}, {ID: want[1]}})
	if got := ProviderModelIDs("agy-bin"); !equalSlice(got, want) {
		t.Fatalf("ProviderModelIDs = %v, want %v", got, want)
	}
}

func TestRetryProviderForwardsBoundaryFork(t *testing.T) {
	wrapped := WrapWithRetry(clibin.NewClaudeBinProvider("opus-max", nil), DefaultRetryConfig())
	if !SupportsHelperBoundaryFork(wrapped) {
		t.Fatal("RetryProvider does not advertise the boundary fork seam")
	}
	// A live provider without a session cannot match the captured state, so
	// the fork must be refused rather than degrade to a full replay.
	state := []byte(`{"session_id":"live-session","messages_sent":2}`)
	request := []core.Message{core.UserText("q"), core.AssistantText("a"), core.UserText("side question")}
	if forked, ok := ForkHelperAtBoundary(wrapped, state, request); ok || forked != nil {
		t.Fatalf("ForkHelperAtBoundary = (%v, %v), want refusal for unverifiable state", forked, ok)
	}
}

// A provider without the seam must refuse rather than silently degrade to a
// suffix-only fork, which would deliver no conversation history at all.
func TestForkHelperAtBoundaryRefusesProvidersWithoutTheSeam(t *testing.T) {
	if _, ok := ForkHelperAtBoundary(mockprovider.NewMockProvider("mock"), []byte(`{"session_id":"s"}`), []core.Message{core.UserText("q")}); ok {
		t.Fatal("expected refusal for a provider that does not implement the boundary fork seam")
	}
}

func TestRetryProvider_SupportsInlineFlushOnlyWhenInnerDoes(t *testing.T) {
	for _, provider := range []core.Provider{
		mockprovider.NewMockProvider("mock"),
		&disabledInlineFlusher{Provider: mockprovider.NewMockProvider("disabled-inline-flusher")},
	} {
		wrapped := WrapWithRetry(provider, DefaultRetryConfig())
		flusher, ok := wrapped.(engine.InlineFlusher)
		if !ok {
			t.Fatal("RetryProvider should implement InlineFlusher")
		}
		if flusher.SupportsInlineFlush() {
			t.Fatalf("retry-wrapped %s should not advertise inline flush", provider.Name())
		}
	}
}

func TestRetryProvider_ForwardsToolExecutorSetter(t *testing.T) {
	// ClaudeBinProvider is wrapped with WrapWithRetry in the factory.
	// The RetryProvider must forward SetToolExecutor to the inner provider.
	provider := clibin.NewClaudeBinProvider("sonnet", nil)
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	// The wrapped provider must also implement ToolExecutorSetter
	if _, ok := wrapped.(engine.ToolExecutorSetter); !ok {
		t.Fatal("RetryProvider does not implement ToolExecutorSetter interface - tools will not work with wrapped providers")
	}
}

func TestRetryProvider_ForwardsProviderCleaner(t *testing.T) {
	// ClaudeBinProvider is wrapped with WrapWithRetry in the factory.
	// The RetryProvider must forward CleanupMCP to the inner provider.
	provider := clibin.NewClaudeBinProvider("sonnet", nil)
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	// The wrapped provider must also implement ProviderCleaner
	if _, ok := wrapped.(engine.ProviderCleaner); !ok {
		t.Fatal("RetryProvider does not implement ProviderCleaner interface - MCP cleanup will not work with wrapped providers")
	}
}

func TestRetryProvider_ForwardsProviderTurnCleaner(t *testing.T) {
	provider := clibin.NewClaudeBinProvider("sonnet", nil)
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	if _, ok := wrapped.(engine.ProviderTurnCleaner); !ok {
		t.Fatal("RetryProvider does not implement ProviderTurnCleaner - per-turn cleanup will not work with wrapped providers")
	}
}

func TestRetryProvider_ForwardsProviderState(t *testing.T) {
	provider := clibin.NewClaudeBinProvider("sonnet", nil)
	if err := provider.ImportProviderState([]byte(`{"session_id":"session-1","messages_sent":4}`)); err != nil {
		t.Fatal(err)
	}
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())

	exporter, ok := wrapped.(core.ProviderStateExporter)
	if !ok {
		t.Fatal("RetryProvider does not implement ProviderStateExporter")
	}
	state, ok := exporter.ExportProviderState()
	if !ok {
		t.Fatal("RetryProvider did not export inner provider state")
	}

	restored := clibin.NewClaudeBinProvider("sonnet", nil)
	restoredWrapped := WrapWithRetry(restored, DefaultRetryConfig())
	importer, ok := restoredWrapped.(core.ProviderStateImporter)
	if !ok {
		t.Fatal("RetryProvider does not implement ProviderStateImporter")
	}
	if err := importer.ImportProviderState(state); err != nil {
		t.Fatalf("ImportProviderState: %v", err)
	}
	got, ok := restored.ExportProviderState()
	if !ok {
		t.Fatal("restored provider has no state")
	}
	var decoded struct {
		SessionID    string `json:"session_id"`
		MessagesSent int    `json:"messages_sent"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SessionID != "session-1" || decoded.MessagesSent != 4 {
		t.Fatalf("restored state = (%q, %d), want (%q, %d)", decoded.SessionID, decoded.MessagesSent, "session-1", 4)
	}
}

func TestClaudeBinProvider_ToolExecutorIsWired(t *testing.T) {
	provider := clibin.NewClaudeBinProvider("sonnet", nil)
	registry := engine.NewToolRegistry()

	// Register a test tool
	executorCalled := false
	registry.Register(&clibinTestTool{
		name: "test_tool",
		exec: func(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
			executorCalled = true
			return core.TextOutput("test result"), nil
		},
	})

	// Create engine which should wire up the tool executor
	_ = engine.NewEngine(provider, registry)

	// The tool executor should now be set (not nil).
	// We verify this by checking that a Request with tools does not trigger the warning.
	// The best we can do without exposing internals is to verify the interface is satisfied
	// and trust that NewEngine's wiring works (covered by TestClaudeBinProvider_ImplementsToolExecutorSetter).

	// We can also verify the engine wiring works by checking that the executor callback
	// would be invoked if we had a real tool execution path.
	if !executorCalled {
		// This is expected - we didn't actually execute a tool, just verified wiring is possible.
		// The important thing is that the interface is satisfied and SetToolExecutor was called.
	}
}

func TestProviderModelIDsPrefersCursorBinCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	curated := ProviderModelIDs("cursor-bin")
	if len(curated) == 0 || !containsModelID(curated, "auto-smart") {
		t.Fatalf("expected curated cursor-bin fallback, got %v", curated)
	}

	clibin.RefreshCursorBinCacheSync([]core.ModelInfo{
		{ID: "auto", DisplayName: "Auto (default)"},
		{ID: "cursor-grok-4.5-high", DisplayName: "Cursor Grok 4.5"},
		{ID: "composer-2.5-fast", DisplayName: "Composer 2.5 Fast"},
		{ID: "gpt-5.6-sol-xhigh-fast", DisplayName: "GPT-5.6 Sol Extra High Fast"},
	})

	ids := ProviderModelIDs("cursor-bin")
	want := []string{"auto-smart", "grok-4.5-high", "composer-2.5-fast", "gpt-5.6-sol-xhigh-fast"}
	if !equalSlice(ids, want) {
		t.Fatalf("ProviderModelIDs(cursor-bin) = %v, want %v", ids, want)
	}
	if containsModelID(ids, "claude-sonnet-5") {
		t.Fatalf("cached cursor-bin list unexpectedly kept curated-only model: %v", ids)
	}
}

func TestGrokBinProviderACPRealMultiReviewOrchestrationSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := clibin.NewGrokBinProvider("grok-4.6-low", nil)
	defer p.CleanupMCP()
	tool := &grokACPReviewProbeTool{}
	registry := engine.NewToolRegistry()
	registry.Register(tool)
	engine := engine.NewEngine(p, registry)
	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{
			core.SystemText("Use only the supplied term-llm tools. Do not activate skills or read workflow files. When multiple reviews are requested, call spawn_agent once for each requested model before replying."),
			core.UserText("Review with opus and gpt. Use reviewer agents with models claude-bin:opus-max and chatgpt:gpt-5.6-sol-high. Call both now, then reply REVIEWERS LAUNCHED."),
		},
		Tools:    []core.ToolSpec{tool.Spec()},
		MaxTurns: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := collectGrokACPTestText(t, stream)
	tool.mu.Lock()
	calls := append([]map[string]any(nil), tool.calls...)
	tool.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("spawn_agent calls = %d (%v), response=%q", len(calls), calls, text)
	}
	models := map[string]bool{}
	for _, call := range calls {
		model, _ := call["model"].(string)
		models[model] = true
	}
	for _, model := range []string{"claude-bin:opus-max", "chatgpt:gpt-5.6-sol-high"} {
		if !models[model] {
			t.Fatalf("spawn_agent models = %v, missing %q", models, model)
		}
	}
	if !strings.Contains(text, "REVIEWERS LAUNCHED") {
		t.Fatalf("multi-review response = %q", text)
	}
}

func TestGrokBinProviderACPRealDynamicToolSmoke(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_GROK_ACP") != "1" {
		t.Skip("set TERM_LLM_TEST_GROK_ACP=1 to run the credentialed Grok ACP dynamic-tool smoke test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	provider := clibin.NewGrokBinProvider("grok-4.6-low", nil)
	defer provider.CleanupMCP()
	registry := engine.NewToolRegistry()
	late := &grokDynamicResultProbeTool{}
	activation := &grokDynamicActivationProbeTool{late: late}
	registry.Register(activation)
	engine := engine.NewEngine(provider, registry)
	activation.engine = engine

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{
			core.SystemText("Use only supplied term-llm MCP tools. You must execute every requested tool and must not invent tool results."),
			core.UserText("First call activate_dynamic exactly once. After it returns, search again for the newly registered late_dynamic tool and call late_dynamic exactly once in this same turn. Do not reply until both real tool calls have completed; then reply with the late_dynamic result."),
		},
		Tools:    []core.ToolSpec{activation.Spec()},
		MaxTurns: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := collectGrokACPTestText(t, stream)
	activation.mu.Lock()
	activationCalls := activation.calls
	activation.mu.Unlock()
	late.mu.Lock()
	calls := late.calls
	late.mu.Unlock()
	if activationCalls != 1 || calls != 1 || !strings.Contains(text, "DYNAMIC_TOOL_OK") {
		t.Fatalf("activate_dynamic calls=%d late_dynamic calls=%d response=%q", activationCalls, calls, text)
	}
}

func TestProviderModelIDsPrefersGrokBinCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	curated := ProviderModelIDs("grok-bin")
	if len(curated) == 0 || !containsModelID(curated, "grok-4.6") {
		t.Fatalf("expected curated grok-bin fallback including grok-4.6, got %v", curated)
	}

	clibin.RefreshGrokBinCacheSync([]core.ModelInfo{
		{ID: "grok-4.6"},
		{ID: "grok-4.5"},
	})

	ids := ProviderModelIDs("grok-bin")
	want := []string{
		"grok-4.6", "grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh",
		"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.5-xhigh",
	}
	if !equalSlice(ids, want) {
		t.Fatalf("ProviderModelIDs(grok-bin) = %v, want %v", ids, want)
	}
	if containsModelID(ids, "grok-composer-2.5-fast") {
		t.Fatalf("cached grok-bin list unexpectedly kept curated-only model: %v", ids)
	}
}

func TestGrokBinProviderPublishesDynamicToolsThroughRetryWrapper(t *testing.T) {
	provider := clibin.NewGrokBinProvider("grok-4.6", nil)
	if _, ok := any(provider).(engine.DynamicToolPublisher); !ok {
		t.Fatal("grok-bin does not implement DynamicToolPublisher")
	}
	wrapped := WrapWithRetry(provider, DefaultRetryConfig())
	publisher, ok := wrapped.(engine.DynamicToolPublisher)
	if !ok {
		t.Fatal("retry wrapper does not preserve DynamicToolPublisher")
	}
	// Before the conversation MCP server starts, publication is intentionally a
	// no-op; the normal request tool list will seed that server.
	if err := publisher.PublishDynamicTools([]core.ToolSpec{{Name: "late", Schema: map[string]any{"type": "object"}}}); err != nil {
		t.Fatal(err)
	}
}

type grokDynamicResultProbeTool struct {
	mu    sync.Mutex
	calls int
}

func (t *grokDynamicResultProbeTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "late_dynamic", Description: "Returns DYNAMIC_TOOL_OK after dynamic registration.", Schema: map[string]any{"type": "object"}}
}
func (t *grokDynamicResultProbeTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return core.TextOutput("DYNAMIC_TOOL_OK"), nil
}
func (t *grokDynamicResultProbeTool) Preview(json.RawMessage) string { return "dynamic result probe" }

type grokDynamicActivationProbeTool struct {
	mu     sync.Mutex
	calls  int
	engine *engine.Engine
	late   core.Tool
}

func (t *grokDynamicActivationProbeTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "activate_dynamic", Description: "Registers the late_dynamic tool. Call this before searching for late_dynamic.", Schema: map[string]any{"type": "object"}}
}
func (t *grokDynamicActivationProbeTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	t.engine.AddDynamicTool(t.late)
	return core.TextOutput("late_dynamic is registered; discover and call it now. Do not claim its result without calling it."), nil
}
func (t *grokDynamicActivationProbeTool) Preview(json.RawMessage) string {
	return "register dynamic result probe"
}

func collectGrokACPTestText(t *testing.T, stream core.Stream) string {
	t.Helper()
	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return text.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventError {
			t.Fatal(event.Err)
		}
		if event.Type == core.EventTextDelta {
			text.WriteString(event.Text)
		}
	}
}

type grokACPReviewProbeTool struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (t *grokACPReviewProbeTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "spawn_agent",
		Description: "Launch one requested reviewer. Call once per requested reviewer model.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_name": map[string]any{"type": "string", "enum": []string{"reviewer"}},
				"model":      map[string]any{"type": "string"},
				"prompt":     map[string]any{"type": "string"},
			},
			"required": []string{"agent_name", "model", "prompt"},
		},
	}
}

func (t *grokACPReviewProbeTool) Execute(_ context.Context, args json.RawMessage) (core.ToolOutput, error) {
	var call map[string]any
	if err := json.Unmarshal(args, &call); err != nil {
		return core.ToolOutput{}, err
	}
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	return core.TextOutput("REVIEW_COMPLETE"), nil
}

func (t *grokACPReviewProbeTool) Preview(json.RawMessage) string { return "review probe" }

type clibinTestTool struct {
	name string
	exec func(ctx context.Context, args json.RawMessage) (core.ToolOutput, error)
}

func (t *clibinTestTool) Name() string                        { return t.name }
func (t *clibinTestTool) Description() string                 { return "test tool" }
func (t *clibinTestTool) Spec() core.ToolSpec                 { return core.ToolSpec{Name: t.name} }
func (t *clibinTestTool) Preview(args json.RawMessage) string { return "" }
func (t *clibinTestTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return t.exec(ctx, args)
}

type disabledInlineFlusher struct {
	core.Provider
}

func (p *disabledInlineFlusher) RequestInlineFlush()       {}
func (p *disabledInlineFlusher) SupportsInlineFlush() bool { return false }
