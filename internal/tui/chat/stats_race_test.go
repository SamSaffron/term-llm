package chat

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

// This intentionally produces compaction notifications on a separate goroutine
// while applying every stats mutation serially through Update, matching Bubble
// Tea's runtime ownership model. Run under -race to guard against reintroducing
// direct callback-goroutine SessionStats mutation.
func TestCompactionAppliedMessagesMutateStatsOnlyThroughUpdate(t *testing.T) {
	m := newTestChatModel(false)
	m.stats = ui.NewSessionStats()
	m.stats.SetModel("main-model")
	m.stats.RequestStart()
	m.stats.ObserveOutput()

	messages := make(chan compactionAppliedMsg)
	go func() {
		defer close(messages)
		for range 100 {
			messages <- compactionAppliedMsg{
				generation: m.streamGeneration,
				model:      " compact-model ",
				usage:      llm.Usage{InputTokens: 2, OutputTokens: 1},
			}
		}
	}()
	for msg := range messages {
		m.queueCompactionForUI(msg)
		if _, cmd := m.Update(streamEventMsg{event: ui.PhaseEvent(llm.PhaseCompactingResumeTask), generation: m.streamGeneration}); cmd == nil {
			t.Fatal("compaction resume phase did not schedule the stream listener")
		}
	}

	if m.stats.CompactionLLMCallCount != 100 || m.stats.LLMCallCount != 100 {
		t.Fatalf("compaction counts = %d/%d, want 100/100", m.stats.CompactionLLMCallCount, m.stats.LLMCallCount)
	}
	calls, _ := m.stats.UsageCalls()
	if len(calls) != 100 || calls[0].Model != "compact-model" {
		t.Fatalf("compaction calls = %#v", calls)
	}

	// The pending main request remains intact after all compaction messages.
	m.stats.AddUsage(5, 2, 0, 0)
	calls, _ = m.stats.UsageCalls()
	if calls[len(calls)-1].Model != "main-model" || !calls[len(calls)-1].ObservedOutput {
		t.Fatalf("pending main call was clobbered: %+v", calls[len(calls)-1])
	}
}
