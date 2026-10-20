package ask

import (
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestCompactionUsageCollectorMergesWithoutSession(t *testing.T) {
	var collector compactionUsageCollector
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collector.add(&engine.CompactionResult{Model: "  gpt-5.6-sol  ", Usage: core.Usage{
				InputTokens: 7, OutputTokens: 3, CachedInputTokens: 11, CacheWriteTokens: 2,
			}})
		}()
	}
	wg.Wait()
	stats := ui.NewSessionStats()
	collector.merge(stats)
	if stats.InputTokens != 56 || stats.OutputTokens != 24 || stats.CachedInputTokens != 88 || stats.CacheWriteTokens != 16 || stats.LLMCallCount != 8 || stats.CompactionLLMCallCount != 8 {
		t.Fatalf("compaction usage not merged into --stats: %+v", stats)
	}
	calls, _ := stats.UsageCalls()
	for _, call := range calls {
		if call.Model != "gpt-5.6-sol" {
			t.Fatalf("compaction model = %q, want normalized actual model", call.Model)
		}
	}
}
