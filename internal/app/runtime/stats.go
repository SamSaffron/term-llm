package runtime

import "github.com/samsaffron/term-llm/internal/ui"

func SetEstimatedStatsCost(stats *ui.SessionStats, model string) {
	if stats == nil {
		return
	}
	stats.ClearEstimatedCost()
	if cost, err := ui.EstimateSessionStatsCost(stats, model); err == nil {
		stats.SetEstimatedCost(cost)
	}
}
