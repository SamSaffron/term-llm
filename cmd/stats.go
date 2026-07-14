package cmd

import (
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

func setEstimatedStatsCost(stats *ui.SessionStats, model string) {
	if stats == nil {
		return
	}
	stats.ClearEstimatedCost()
	if cost, err := ui.EstimateSessionStatsCost(stats, model); err == nil {
		stats.SetEstimatedCost(cost)
	}
}

type compactionUsageCollector struct {
	mu     sync.Mutex
	usages []llm.Usage
}

func (c *compactionUsageCollector) add(result *llm.CompactionResult) {
	if result == nil {
		return
	}
	c.mu.Lock()
	c.usages = append(c.usages, result.Usage)
	c.mu.Unlock()
}

func (c *compactionUsageCollector) merge(stats *ui.SessionStats) {
	if stats == nil {
		return
	}
	c.mu.Lock()
	usages := append([]llm.Usage(nil), c.usages...)
	c.usages = nil
	c.mu.Unlock()
	for _, u := range usages {
		stats.AddCompactionUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
}
