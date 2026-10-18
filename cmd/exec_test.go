package cmd

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestEnvEnabled(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset", value: "", want: false},
		{name: "one", value: "1", want: true},
		{name: "true", value: "true", want: true},
		{name: "true-caps", value: "TRUE", want: true},
		{name: "yes", value: "yes", want: true},
		{name: "y", value: "y", want: true},
		{name: "spaced", value: "  yes  ", want: true},
		{name: "zero", value: "0", want: false},
		{name: "false", value: "false", want: false},
		{name: "no", value: "no", want: false},
		{name: "garbage", value: "nope", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(allowAutoRunEnv, tt.value)
			if got := envEnabled(allowAutoRunEnv); got != tt.want {
				t.Fatalf("envEnabled(%q)=%v, want %v", allowAutoRunEnv, got, tt.want)
			}
		})
	}
}

func TestExecRunSinkAccountsGuardianUsage(t *testing.T) {
	stats := ui.NewSessionStats()
	sink := &execRunSink{stats: stats}
	usage := core.Usage{InputTokens: 11, OutputTokens: 3, CachedInputTokens: 7, CacheWriteTokens: 2}

	sink.GuardianEvent(tools.GuardianEvent{Model: "guardian-model", Usage: usage})

	calls, _ := stats.UsageCalls()
	if stats.InputTokens != 11 || stats.OutputTokens != 3 || stats.CachedInputTokens != 7 || stats.CacheWriteTokens != 2 || len(calls) != 1 || !calls[0].Guardian || calls[0].Model != "guardian-model" {
		t.Fatalf("exec guardian stats = %+v calls=%+v", stats, calls)
	}
}

func TestExecRunSinkDiscardsRetryAttemptUsageAndPerformance(t *testing.T) {
	stats := ui.NewSessionStats()
	stats.SetModel("gpt-5.6-sol")
	sink := &execRunSink{stats: stats}
	sink.Event(core.Event{Type: core.EventTextDelta, Text: "discard me"})
	sink.Event(core.Event{Type: core.EventUsage, Use: &core.Usage{InputTokens: 10, OutputTokens: 50}})
	sink.Event(core.Event{Type: core.EventAttemptDiscard})
	sink.Event(core.Event{Type: core.EventTextDelta, Text: "keep me"})
	sink.Event(core.Event{Type: core.EventUsage, Use: &core.Usage{InputTokens: 2, OutputTokens: 3}})
	calls, _ := stats.UsageCalls()
	if stats.InputTokens != 2 || stats.OutputTokens != 3 || len(calls) != 1 || calls[0].OutputTokens != 3 {
		t.Fatalf("exec stats retained discarded attempt: stats=%+v calls=%+v", stats, calls)
	}
}

func TestExecRunSinkEndsToolTiming(t *testing.T) {
	stats := ui.NewSessionStats()
	sink := &execRunSink{stats: stats}
	sink.Event(core.Event{Type: core.EventToolExecStart, ToolName: "shell"})
	time.Sleep(time.Millisecond)
	sink.Event(core.Event{Type: core.EventToolExecEnd, ToolName: "shell", ToolSuccess: true})
	toolTime := stats.ToolTime
	time.Sleep(time.Millisecond)
	stats.Finalize()

	if toolTime <= 0 {
		t.Fatal("expected completed exec tool to record tool time")
	}
	if stats.ToolTime != toolTime {
		t.Fatalf("tool time continued after end: before=%s after=%s", toolTime, stats.ToolTime)
	}
}
