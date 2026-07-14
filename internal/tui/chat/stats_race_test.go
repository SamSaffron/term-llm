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
func TestCompactionUsageMessagesMutateStatsOnlyThroughUpdate(t *testing.T) {
	m := newTestChatModel(false)
	m.stats = ui.NewSessionStats()
	m.stats.SetModel("main-model")
	m.stats.RequestStart()
	m.stats.ObserveOutput()

	messages := make(chan compactionUsageMsg)
	go func() {
		defer close(messages)
		for range 100 {
			messages <- compactionUsageMsg{
				model: " compact-model ",
				usage: llm.Usage{InputTokens: 2, OutputTokens: 1},
			}
		}
	}()
	for msg := range messages {
		if _, cmd := m.Update(msg); cmd != nil {
			t.Fatal("compaction usage message unexpectedly returned a command")
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
