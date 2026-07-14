package ui

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/usage"
)

// EstimateSessionStatsCost prices each current-process provider request
// separately using bundled or already-cached pricing only.
func EstimateSessionStatsCost(stats *SessionStats, fallbackModel string) (float64, error) {
	if stats == nil {
		return 0, fmt.Errorf("no usage recorded")
	}
	calls, complete := stats.UsageCalls()
	if !complete {
		return 0, fmt.Errorf("resumed session has unpriced historical usage")
	}
	if len(calls) == 0 {
		return 0, fmt.Errorf("no current-process usage recorded")
	}

	fetcher := usage.NewPricingFetcher()
	var total float64
	for _, call := range calls {
		model := strings.TrimSpace(call.Model)
		if model == "" {
			model = strings.TrimSpace(fallbackModel)
		}
		if model == "" {
			return 0, fmt.Errorf("model unknown")
		}
		cost, err := fetcher.CalculateCostLocal(usage.UsageEntry{
			Model:            model,
			InputTokens:      call.InputTokens,
			OutputTokens:     call.OutputTokens,
			CacheReadTokens:  call.CachedInputTokens,
			CacheWriteTokens: call.CacheWriteTokens,
			Provider:         usage.ProviderTermLLM,
		})
		if err != nil {
			return 0, err
		}
		total += cost
	}
	return total, nil
}
