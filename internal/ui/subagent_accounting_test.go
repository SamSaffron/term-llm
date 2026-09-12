package ui

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSubagentAccountingSurvivesRemoval(t *testing.T) {
	tracker := NewSubagentTracker()
	tracker.SetMainProviderModel("anthropic", "claude-sonnet-4-20250514")
	tracker.GetOrCreate("child", "codebase")
	start := time.Unix(100, 0)
	tracker.HandleInitAt("child", "anthropic", "claude-sonnet-4-20250514", start)
	tracker.HandleToolStartAt("child", "a", "shell", "", nil, start.Add(time.Second))
	tracker.HandleToolStartAt("child", "b", "shell", "", nil, start.Add(2*time.Second))
	tracker.HandleToolEndAt("child", "a", "shell", true, start.Add(3*time.Second))
	tracker.HandleToolEndAt("child", "b", "shell", true, start.Add(4*time.Second))
	tracker.HandleUsageEvent("child", tools.SubagentEvent{InputTokens: 10, OutputTokens: 2, CachedInputTokens: 20, CacheWriteTokens: 3})
	tracker.MarkDoneAt("child", start.Add(5*time.Second))
	tracker.MarkDoneAt("child", start.Add(9*time.Second))
	tracker.Remove("child")
	tracker.HandleUsageEvent("child", tools.SubagentEvent{OutputTokens: 1})
	if tracker.GetOrCreate("child", "") != nil {
		t.Fatal("removed run resurrected")
	}
	runs := tracker.Snapshots()
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	run := runs[0]
	elapsed, toolTime := run.Timing(start.Add(time.Hour))
	if elapsed != 5*time.Second || toolTime != 3*time.Second {
		t.Fatalf("timing = %v, %v", elapsed, toolTime)
	}
	if !run.Done || run.CachedInputTokens != 20 || run.CacheWriteTokens != 3 || run.OutputTokens != 3 || len(run.UsageCalls) != 2 {
		t.Fatalf("run = %+v", run)
	}
	if run.UsageCalls[0].Model != "claude-sonnet-4-20250514" {
		t.Fatal("lost same-as-parent model")
	}
	runs[0].UsageCalls[0].Model = "changed"
	if tracker.Snapshots()[0].UsageCalls[0].Model == "changed" {
		t.Fatal("snapshot aliases accounting")
	}
}

func TestSubagentUsageDoesNotChangeParentTimingOrContext(t *testing.T) {
	s := NewSessionStats()
	s.SetModel("parent")
	s.AddUsage(10, 2, 0, 0)
	start := time.Unix(100, 0)
	s.requestStartAt(start)
	s.AddSubagentUsageForModel("child", 30, 4, 50, 6)
	if s.requestStartTime != start || s.lastInputTokens != 12 {
		t.Fatal("child altered parent timing/context")
	}
	s.DiscardUsage(10, 2, 0, 0, 1)
	calls, _ := s.UsageCalls()
	if len(calls) != 1 || !calls[0].Subagent || s.InputTokens != 30 || s.CachedInputTokens != 50 || s.CacheWriteTokens != 6 || s.OutputTokens != 4 {
		t.Fatalf("lost child usage: %+v", s)
	}
	s = NewSessionStats()
	s.AddSubagentUsageForModel("", 1, 1, 0, 0)
	if _, err := EstimateSessionStatsCost(s, "claude-sonnet-4-20250514"); err == nil {
		t.Fatal("unknown child priced as parent")
	}
}
