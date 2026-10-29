package engine

import (
	"context"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// A provider that manages its own context reports occupancy for display. The
// engine must surface and forward it without enabling any context management.
func TestReportedContextIsDisplayOnly(t *testing.T) {
	t.Parallel()
	for _, withTools := range []bool{false, true} {
		t.Run(map[bool]string{false: "simple", true: "agentic"}[withTools], func(t *testing.T) {
			t.Parallel()
			testReportedContextIsDisplayOnly(t, withTools)
		})
	}
}

func testReportedContextIsDisplayOnly(t *testing.T, withTools bool) {
	provider := &fakeProvider{
		capabilities:    core.Capabilities{ManagesOwnContext: true, ToolCalls: withTools},
		hasCapabilities: true,
		script: func(call int, req core.Request) []core.Event {
			return []core.Event{
				{Type: core.EventContextUsage, Context: &core.ContextUsage{UsedTokens: 25514}},
				{Type: core.EventTextDelta, Text: "hi"},
				{Type: core.EventContextUsage, Context: &core.ContextUsage{UsedTokens: 25670, Window: 1_000_000}},
				{Type: core.EventUsage, Use: &core.Usage{InputTokens: 6, CachedInputTokens: 63699, CacheWriteTokens: 13071, OutputTokens: 184}},
				{Type: core.EventDone},
			}
		},
	}
	e := NewEngine(provider, nil)
	e.ConfigureContextManagement(provider, "claude-bin", "opus", true)

	req := core.Request{Model: "opus", Messages: []core.Message{core.UserText("hi")}}
	if withTools {
		tool := &countingSearchTool{}
		e.RegisterTool(tool)
		req.Tools = []core.ToolSpec{tool.Spec()}
	}
	stream, err := e.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	forwarded := 0
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv() error: %v", err)
		}
		if ev.Type == core.EventContextUsage {
			forwarded++
		}
	}
	if forwarded != 2 {
		t.Fatalf("forwarded %d context events, want 2", forwarded)
	}
	if got, want := e.ContextUsage(), (core.ContextUsage{UsedTokens: 25670, Window: 1_000_000}); got != want {
		t.Fatalf("ContextUsage() = %+v, want %+v", got, want)
	}
	if got := e.InputLimit(); got != 0 {
		t.Fatalf("InputLimit() = %d, want 0 for a self-managed provider", got)
	}
	if _, _, enabled := e.CompactionThresholds(); enabled {
		t.Fatal("compaction enabled for a self-managed provider")
	}
	// The summed per-turn billing usage must not become a context baseline.
	if got := e.LastTotalTokens(); got != 0 {
		t.Fatalf("LastTotalTokens() = %d, want 0", got)
	}

	// Re-configuring the same provider/model (every serve request does) keeps
	// the reading; switching models drops it.
	e.ConfigureContextManagement(provider, "claude-bin", "opus", true)
	if got := e.ContextUsage().UsedTokens; got != 25670 {
		t.Fatalf("ContextUsage().UsedTokens after same-model configure = %d, want 25670", got)
	}
	e.ConfigureContextManagement(provider, "claude-bin", "sonnet", true)
	if got := e.ContextUsage(); got != (core.ContextUsage{}) {
		t.Fatalf("ContextUsage() after model switch = %+v, want empty", got)
	}
}

// The provider owns window retention. A reading with an unknown window (a
// mid-run model change) must not inherit the previous model's window.
func TestReportedContextDoesNotInheritWindow(t *testing.T) {
	t.Parallel()
	e := NewEngine(&fakeProvider{}, nil)
	e.observeReportedContext(core.Event{Type: core.EventContextUsage, Context: &core.ContextUsage{UsedTokens: 25670, Window: 1_000_000}})
	e.observeReportedContext(core.Event{Type: core.EventContextUsage, Context: &core.ContextUsage{UsedTokens: 26003}})
	if got, want := e.ContextUsage(), (core.ContextUsage{UsedTokens: 26003}); got != want {
		t.Fatalf("ContextUsage() = %+v, want %+v", got, want)
	}
}
