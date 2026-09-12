package chat

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestSubagentStatsTotalsAndCompletedDetails(t *testing.T) {
	m := newTestChatModel(true)
	m.subagentTracker = ui.NewSubagentTracker()
	store := &mockStore{}
	m.store = store
	m.sess = &session.Session{ID: "parent"}
	start := time.Unix(100, 0)
	send := func(event tools.SubagentEvent) { m.Update(SubagentProgressMsg{CallID: "child", Event: event}) }
	send(tools.SubagentEvent{Type: tools.SubagentEventInit, Model: "claude-sonnet-4-20250514", Timestamp: start})
	send(tools.SubagentEvent{Type: tools.SubagentEventUsage, InputTokens: 10, OutputTokens: 2, CachedInputTokens: 20, CacheWriteTokens: 3})
	if m.stats.InputTokens != 10 || m.stats.CachedInputTokens != 20 || m.stats.CacheWriteTokens != 3 || m.stats.OutputTokens != 2 {
		t.Fatalf("stats = %+v", m.stats)
	}
	if len(store.metricUpdates) != 1 || m.sess.InputTokens != 10 {
		t.Fatalf("metrics = %+v", store.metricUpdates)
	}
	if !strings.Contains(m.renderStatsModal(), "running") {
		t.Fatal("missing running run")
	}
	send(tools.SubagentEvent{Type: tools.SubagentEventDone, Timestamp: start.Add(5 * time.Second)})
	m.subagentTracker.Remove("child")
	modal := m.renderStatsModal()
	for _, want := range []string{"Subagent models · this process", "5.0s", "Est. cost"} {
		if !strings.Contains(modal, want) {
			t.Fatalf("missing %q in %s", want, modal)
		}
	}
	calls, _ := m.stats.UsageCalls()
	if len(calls) != 1 || !calls[0].Subagent || calls[0].Model != "claude-sonnet-4-20250514" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestSkillRunUsageIncludedInSessionTotals(t *testing.T) {
	m := newTestChatModel(true)
	m.subagentTracker = ui.NewSubagentTracker()
	store := &mockStore{}
	m.store = store
	m.sess = &session.Session{ID: "parent"}
	m.skillRuns = map[string]*skillRunState{"skill": {ID: "skill", Status: "running", TrackerCallID: "child"}}
	send := func(event tools.SubagentEvent) {
		m.handleSkillRunProgress(skillRunProgressMsg{RunID: "skill", Event: event})
	}
	send(tools.SubagentEvent{Type: tools.SubagentEventInit, Model: "child-model"})
	send(tools.SubagentEvent{Type: tools.SubagentEventUsage, InputTokens: 10, OutputTokens: 2, CachedInputTokens: 20, CacheWriteTokens: 3})
	send(tools.SubagentEvent{Type: tools.SubagentEventGuardian, Guardian: &tools.GuardianEvent{Model: "guardian-model", Usage: llm.Usage{InputTokens: 5, OutputTokens: 1}}})
	calls, _ := m.stats.UsageCalls()
	if len(calls) != 2 || calls[0].Model != "child-model" || !calls[0].Subagent || !calls[1].Guardian {
		t.Fatalf("calls = %+v", calls)
	}
	if m.stats.InputTokens != 15 || m.stats.OutputTokens != 3 || m.stats.CachedInputTokens != 20 || m.stats.CacheWriteTokens != 3 {
		t.Fatalf("stats = %+v", m.stats)
	}
	if len(store.metricUpdates) != 2 || m.sess.InputTokens != 15 {
		t.Fatalf("persisted = %+v, session = %+v", store.metricUpdates, m.sess)
	}
	runs := m.subagentTracker.Snapshots()
	if len(runs) != 1 || runs[0].InputTokens != m.stats.InputTokens || len(runs[0].UsageCalls) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestLateSubagentGuardianUsageRetainedWithoutResurrection(t *testing.T) {
	m := newTestChatModel(true)
	m.subagentTracker = ui.NewSubagentTracker()
	m.Update(SubagentProgressMsg{CallID: "child", Event: tools.SubagentEvent{Type: tools.SubagentEventInit, Model: "child-model"}})
	m.subagentTracker.Remove("child")
	m.Update(SubagentProgressMsg{CallID: "child", Event: tools.SubagentEvent{Type: tools.SubagentEventGuardian, Guardian: &tools.GuardianEvent{ToolCallID: "nested", Model: "guardian-model", Usage: llm.Usage{InputTokens: 5, OutputTokens: 1, CachedInputTokens: 3, CacheWriteTokens: 2}}}})
	runs := m.subagentTracker.Snapshots()
	if len(runs) != 1 || len(runs[0].UsageCalls) != 1 || !runs[0].UsageCalls[0].Guardian || runs[0].InputTokens != 5 || runs[0].CachedInputTokens != 3 || runs[0].CacheWriteTokens != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	if m.stats.InputTokens != 5 || m.stats.LLMCallCount != 1 {
		t.Fatalf("stats = %+v", m.stats)
	}
	if m.subagentTracker.GetOrCreate("child", "") != nil || m.subagentTracker.HasActive() {
		t.Fatal("completed run resurrected")
	}
	if got := m.subagentTracker.ResolvedModel("child"); got != "child-model" {
		t.Fatalf("retained model = %q", got)
	}
}

func TestSubagentStatsCompactTable(t *testing.T) {
	original := statsCostEstimator
	statsCostEstimator = func(_ string, stats *ui.SessionStats) (float64, error) {
		calls, _ := stats.UsageCalls()
		if len(calls) > 0 && calls[0].Model == "priced-model" && calls[0].OutputTokens != 1 {
			return 0.0068, nil
		}
		return 0, errors.New("unpriced")
	}
	t.Cleanup(func() { statsCostEstimator = original })
	for _, width := range []int{104, 80, 56} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newTestChatModel(true)
			m.dialog.SetSize(width, 40)
			m.subagentTracker = ui.NewSubagentTracker()
			p := m.subagentTracker.GetOrCreate("child", "developer")
			p.Prompt = "Run sleep 1 then return a lengthy description that must never enter the table"
			start := time.Unix(100, 0)
			m.subagentTracker.HandleInitAt("child", "anthropic", "priced-model", start)
			m.subagentTracker.HandleUsageEvent("child", tools.SubagentEvent{InputTokens: 12000, CachedInputTokens: 4600, OutputTokens: 157})
			m.subagentTracker.HandleUsageEvent("child", tools.SubagentEvent{Model: "priced-model", OutputTokens: 1})
			m.subagentTracker.HandleToolStartAt("child", "shell", "shell", "", nil, start.Add(time.Second))
			m.subagentTracker.HandleToolEndAt("child", "shell", "shell", true, start.Add(5900*time.Millisecond))
			m.subagentTracker.MarkDoneAt("child", start.Add(10700*time.Millisecond))
			var b strings.Builder
			m.renderSubagentStats(&b)
			out := b.String()
			for _, want := range []string{"priced-model", "Time", "Tools", "10.7s", "4.9s", "≥$", "≥ partial cost"} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q:\n%s", want, out)
				}
			}
			for _, absent := range []string{p.Prompt, "finished", "Tokens:", "Estimates use", "Run details", "Known estimated"} {
				if strings.Contains(out, absent) {
					t.Fatalf("unexpected prose %q:\n%s", absent, out)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if ansi.StringWidth(line) > m.dialog.contentWidth()-4 {
					t.Fatalf("line exceeds available width: %q", line)
				}
			}
			if len(strings.Split(strings.TrimSpace(out), "\n")) != 4 {
				t.Fatalf("expected heading, columns, row, legend:\n%s", out)
			}
			t.Log("\n" + out)
		})
	}
}

func TestSubagentStatsOmitsEmptySection(t *testing.T) {
	m := newTestChatModel(true)
	var b strings.Builder
	m.renderSubagentStats(&b)
	if b.Len() != 0 {
		t.Fatalf("empty subagent section adds noise: %q", b.String())
	}
}

func TestSubagentStatsGroupsRunsByBillingModel(t *testing.T) {
	original := statsCostEstimator
	statsCostEstimator = func(_ string, stats *ui.SessionStats) (float64, error) {
		return float64(stats.InputTokens) * 0.001, nil
	}
	t.Cleanup(func() { statsCostEstimator = original })
	m := newTestChatModel(true)
	m.dialog.SetSize(104, 40)
	m.subagentTracker = ui.NewSubagentTracker()
	start := time.Unix(100, 0)
	for _, id := range []string{"developer", "reviewer"} {
		m.subagentTracker.GetOrCreate(id, id)
		m.subagentTracker.HandleInitAt(id, "provider", "shared-model", start)
		m.subagentTracker.HandleUsageEvent(id, tools.SubagentEvent{Model: "shared-model", InputTokens: 100, OutputTokens: 10})
		m.subagentTracker.MarkDoneAt(id, start.Add(5*time.Second))
	}
	m.subagentTracker.HandleGuardianEvent("reviewer", tools.GuardianEvent{Model: "guardian-model", Usage: llm.Usage{InputTokens: 50, OutputTokens: 5}})
	var b strings.Builder
	m.renderSubagentStats(&b)
	out := b.String()
	if strings.Count(out, "shared-model") != 1 || strings.Count(out, "guardian-model") != 1 {
		t.Fatalf("models not grouped:\n%s", out)
	}
	for _, want := range []string{"10.0s", "200", "20", "$0.2000", "$0.0500"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	for _, name := range []string{"developer", "reviewer"} {
		if strings.Contains(out, name) {
			t.Fatalf("agent leaked into model breakdown:\n%s", out)
		}
	}
}
