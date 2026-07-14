package chat

import (
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestCmdQuitPrintsSummaryWhenIdle(t *testing.T) {
	oldEstimator := statsCostEstimator
	called := false
	statsCostEstimator = func(string, *ui.SessionStats) (float64, error) {
		called = true
		return 0.25, nil
	}
	t.Cleanup(func() { statsCostEstimator = oldEstimator })

	m := newTestChatModel(false)
	m.showStats = true
	m.stats = ui.NewSessionStats()
	m.stats.AddUsage(2, 1, 0, 0)
	_, cmd := m.cmdQuit()
	if !called {
		t.Fatal("idle /quit did not produce the stats summary")
	}
	if cmd == nil {
		t.Fatal("idle /quit returned no quit command")
	}
}

func TestCmdQuitDoesNotSummarizeCancellingStream(t *testing.T) {
	oldEstimator := statsCostEstimator
	statsCostEstimator = func(string, *ui.SessionStats) (float64, error) {
		t.Fatal("/quit must not estimate incomplete active-stream stats")
		return 0, nil
	}
	t.Cleanup(func() { statsCostEstimator = oldEstimator })

	m := newTestChatModel(false)
	m.showStats = true
	m.streaming = true
	m.stats = ui.NewSessionStats()
	m.stats.AddUsage(1, 1, 0, 0)
	_, _ = m.cmdQuit()
}

func TestSeedStatsFromSessionIncludesPersistedCompactions(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{LLMTurns: 3, CompactionCount: 2}
	m.seedStatsFromSession()
	if m.stats.LLMCallCount != 5 {
		t.Fatalf("resumed LLM calls = %d, want 5", m.stats.LLMCallCount)
	}
}

func TestExitStatsSummaryUsesBalancedOutputAndOmitsUnavailableCost(t *testing.T) {
	oldEstimator := statsCostEstimator
	statsCostEstimator = func(string, *ui.SessionStats) (float64, error) {
		return 0, errors.New("pricing unavailable")
	}
	t.Cleanup(func() { statsCostEstimator = oldEstimator })

	m := newTestChatModel(false)
	m.showStats = true
	m.stats = ui.NewSessionStats()
	m.stats.AddUsage(1200, 300, 400, 50)

	out := m.exitStatsSummary()
	for _, want := range []string{"Stats:", "1.2K in + 400 cached + 50 cache write → 300 out", "1 call"} {
		if !strings.Contains(out, want) {
			t.Fatalf("exit stats missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "$") {
		t.Fatalf("unavailable cost should be omitted: %s", out)
	}
}
