package cmd

import (
	"strings"
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

type compactionUsageEntry struct {
	model string
	usage llm.Usage
}

type compactionUsageCollector struct {
	mu     sync.Mutex
	usages []compactionUsageEntry
}

func (c *compactionUsageCollector) add(result *llm.CompactionResult) {
	if result == nil {
		return
	}
	c.mu.Lock()
	c.usages = append(c.usages, compactionUsageEntry{model: strings.TrimSpace(result.Model), usage: result.Usage})
	c.mu.Unlock()
}

func (c *compactionUsageCollector) merge(stats *ui.SessionStats) {
	if stats == nil {
		return
	}
	c.mu.Lock()
	usages := append([]compactionUsageEntry(nil), c.usages...)
	c.usages = nil
	c.mu.Unlock()
	for _, entry := range usages {
		u := entry.usage
		stats.AddCompactionUsageForModel(entry.model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	}
}
