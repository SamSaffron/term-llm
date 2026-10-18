package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/restart"
	"strings"
	"testing"
	"time"
)

func TestEstimatedTokensUsesStructuralDeltaAfterLastAssistant(t *testing.T) {
	e := NewEngine(mockprovider.NewMockProvider("test"), nil)

	checkpoint := []core.Message{
		core.SystemText("system prompt"),
		core.UserText("hello world"),
		core.AssistantText(strings.Repeat("response from model ", 100)),
	}
	e.lastTotalTokens = 150
	e.lastMessageCount = len(checkpoint)

	msgs := append([]core.Message(nil), checkpoint...)
	msgs = append(msgs, core.UserText("follow-up question"))

	got := e.estimatedTokens(msgs)
	want := 150 + EstimateMessageTokens([]core.Message{core.UserText("follow-up question")})
	if got != want {
		t.Errorf("estimatedTokens (structural delta) = %d, want %d", got, want)
	}
}

func TestEstimatedTokens(t *testing.T) {
	e := NewEngine(mockprovider.NewMockProvider("test"), nil)

	msgs := []core.Message{
		core.SystemText("system prompt"),
		core.UserText("hello world"),
	}

	// With no API data yet, should fall back to pure heuristic
	got := e.estimatedTokens(msgs)
	want := EstimateMessageTokens(msgs)
	if got != want {
		t.Errorf("estimatedTokens (no API data) = %d, want %d", got, want)
	}

	// Simulate API response: 100 input + 50 output. The provider baseline already
	// includes the assistant response, so only structurally trailing messages after
	// the last assistant turn should be estimated heuristically.
	e.lastTotalTokens = 150
	e.lastMessageCount = 2

	// Now add an assistant response and a new user message after the API call.
	msgs = append(msgs, core.AssistantText("response from model"))
	msgs = append(msgs, core.UserText("follow-up question"))

	got = e.estimatedTokens(msgs)
	newMsgsEstimate := EstimateMessageTokens([]core.Message{msgs[len(msgs)-1]})
	want = 150 + newMsgsEstimate
	if got != want {
		t.Errorf("estimatedTokens (with API data) = %d, want %d (150 + %d)", got, want, newMsgsEstimate)
	}
}

func TestEstimatedTokensIgnoresStaleBaselineWithoutAssistantAnchor(t *testing.T) {
	e := NewEngine(mockprovider.NewMockProvider("test"), nil)

	// A provider baseline is only valid if it can be anchored to an assistant turn
	// in the current transcript. Summary-only or cleared histories should not reuse
	// a stale pre-compaction baseline.
	e.lastTotalTokens = 100
	e.lastMessageCount = 5

	msgs := []core.Message{core.UserText("short")}
	want := EstimateMessageTokens(msgs)
	got := e.estimatedTokens(msgs)
	if got != want {
		t.Errorf("estimatedTokens (stale unanchored baseline) = %d, want heuristic %d", got, want)
	}
}

func TestEstimatedTokensUsesBaselineWhenMessageCountMatches(t *testing.T) {
	e := NewEngine(mockprovider.NewMockProvider("test"), nil)
	e.lastTotalTokens = 336_000
	e.lastMessageCount = 3

	msgs := []core.Message{
		core.SystemText("system"),
		core.UserText("hello"),
		core.AssistantText("world"),
	}
	got := e.estimatedTokens(msgs)
	if got != 336_000 {
		t.Fatalf("estimatedTokens (matching baseline) = %d, want %d", got, 336_000)
	}
}

func TestReloadWaitsForActualToolAfterCallerCancellation(t *testing.T) {
	t.Parallel()
	c := &restart.Coordinator{Timeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, release, err := c.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tool := newContextIgnoringTool(1)
	engine := NewEngine(&fakeProvider{}, nil)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		defer release()
		engine.executeToolWithCancellation(ctx, tool, json.RawMessage(`{}`))
	}()
	<-tool.started
	replaced := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error { replaced <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("caller did not cancel")
	}
	select {
	case <-replaced:
		t.Fatal("replaced while actual Tool.Execute was alive")
	default:
	}
	close(tool.release)
	select {
	case <-replaced:
	case <-time.After(time.Second):
		t.Fatal("tool exit did not release ownership")
	}
}

func TestEngineApplyCompactionToolResultLimitCoversContentParts(t *testing.T) {
	engine := NewEngine(nil, nil)
	config := DefaultCompactionConfig()
	config.MaxToolResultChars = 6
	engine.SetCompaction(1000, config)

	got := engine.applyToolOutputTruncation(core.ToolOutput{
		ContentParts: []core.ToolContentPart{
			{Type: core.ToolContentPartText, Text: "abcdef"},
			{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: core.ToolContentPartText, Text: "ghijkl"},
		},
	})

	if !strings.Contains(got.Content, "6 chars truncated") {
		t.Fatalf("structured output bypassed compaction tool-result limit: %q", got.Content)
	}
	if got.ContentParts[1].Type != core.ToolContentPartImageData {
		t.Fatalf("mixed content order changed: %#v", got.ContentParts)
	}
}

func TestEngineApplyToolOutputTruncationCoversContentParts(t *testing.T) {
	engine := NewEngine(nil, nil)
	engine.SetMaxToolOutputChars(8)

	got := engine.applyToolOutputTruncation(core.ToolOutput{
		Content: "stale and unbounded",
		ContentParts: []core.ToolContentPart{
			{Type: core.ToolContentPartText, Text: "123456"},
			{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: core.ToolContentPartText, Text: "789012"},
		},
	})

	if !strings.Contains(got.Content, "4 chars truncated") {
		t.Fatalf("structured output bypassed engine truncation: %q", got.Content)
	}
	if got.ContentParts[1].Type != core.ToolContentPartImageData {
		t.Fatalf("mixed content order changed: %#v", got.ContentParts)
	}
	if got.Content != got.ContentParts[0].Text+got.ContentParts[2].Text {
		t.Fatalf("Content and ContentParts diverged: content=%q parts=%#v", got.Content, got.ContentParts)
	}
}

func TestDynamicToolPublishesImmediatelyForActiveRun(t *testing.T) {
	provider := &dynamicPublishingProvider{MockProvider: mockprovider.NewMockProvider("dynamic")}
	engine := NewEngine(provider, nil)
	runID := engine.beginToolRun()
	defer engine.endToolRun(runID)

	tool := &namedTestTool{name: "late_tool"}
	if !engine.AddDynamicToolForRun(runID, tool) {
		t.Fatal("active run rejected dynamic tool")
	}
	provider.mu.Lock()
	published := append([]core.ToolSpec(nil), provider.published...)
	provider.mu.Unlock()
	if len(published) != 1 || published[0].Name != "late_tool" {
		t.Fatalf("published tools = %#v, want late_tool immediately", published)
	}
	pending := engine.drainPendingToolSpecs(runID)
	if len(pending) != 1 || pending[0].Name != "late_tool" {
		t.Fatalf("fallback pending tools = %#v, want late_tool", pending)
	}
}

func TestDynamicToolQueueIsRunScopedAndDeduplicated(t *testing.T) {
	engine := NewEngine(mockprovider.NewMockProvider("dynamic"), nil)
	tool := &namedTestTool{name: "late_tool"}
	runOne := engine.beginToolRun()
	if !engine.AddDynamicToolForRun(runOne, tool) || !engine.AddDynamicToolForRun(runOne, tool) {
		t.Fatal("active run rejected dynamic tool")
	}
	pending := engine.drainPendingToolSpecs(runOne)
	if len(pending) != 1 || pending[0].Name != "late_tool" {
		t.Fatalf("deduplicated pending specs = %#v", pending)
	}
	if pending := engine.drainPendingToolSpecs(runOne); len(pending) != 0 {
		t.Fatalf("drained queue retained specs: %#v", pending)
	}
	if !engine.AddDynamicToolForRun(runOne, tool) {
		t.Fatal("active run rejected requeued tool")
	}
	engine.endToolRun(runOne)

	runTwo := engine.beginToolRun()
	defer engine.endToolRun(runTwo)
	if pending := engine.drainPendingToolSpecs(runTwo); len(pending) != 0 {
		t.Fatalf("cancelled/ended run leaked into later run: %#v", pending)
	}
	if engine.AddDynamicToolForRun(runOne, tool) {
		t.Fatal("ended run accepted a late activation")
	}
}
