package ui

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/usage"
)

// SessionStatsCostEstimate is a per-request cost roll-up. A partial estimate is
// a lower bound: at least one request ran on a model with no known price.
type SessionStatsCostEstimate struct {
	CostUSD  float64
	Priced   int
	Unpriced int
}

// Partial reports whether some requests could not be priced.
func (e SessionStatsCostEstimate) Partial() bool { return e.Unpriced > 0 }

// EstimateSessionStatsCost prices each current-process provider request
// separately using bundled or already-cached pricing only. It fails unless
// every request could be priced, which is what single-request callers want.
func EstimateSessionStatsCost(stats *SessionStats, fallbackModel string) (float64, error) {
	estimate, err := EstimateSessionStatsCostDetailed(stats, fallbackModel)
	if err != nil {
		return 0, err
	}
	if estimate.Partial() {
		return 0, fmt.Errorf("pricing unavailable for %d of %d requests", estimate.Unpriced, estimate.Unpriced+estimate.Priced)
	}
	return estimate.CostUSD, nil
}

// EstimateSessionStatsCostDetailed prices what it can and reports the rest.
// One unknown model must not blank a whole session's cost: a lower bound with
// an explicit partial marker is far more useful than no number at all.
func EstimateSessionStatsCostDetailed(stats *SessionStats, fallbackModel string) (SessionStatsCostEstimate, error) {
	if stats == nil {
		return SessionStatsCostEstimate{}, fmt.Errorf("no usage recorded")
	}
	calls, complete := stats.UsageCalls()
	if !complete {
		return SessionStatsCostEstimate{}, fmt.Errorf("resumed session has unpriced historical usage")
	}
	if len(calls) == 0 {
		return SessionStatsCostEstimate{}, fmt.Errorf("no current-process usage recorded")
	}

	return PriceUsageCalls(calls, fallbackModel), nil
}

// PriceUsageCalls prices retained requests with a single pricing fetcher.
// Each request is priced on its own because tiered rates apply per request,
// but resolving and loading the price table happens once for the whole set
// rather than once per request.
func PriceUsageCalls(calls []UsageCall, fallbackModel string) SessionStatsCostEstimate {
	return priceUsageCalls(calls, fallbackModel, false)
}

// PriceAggregateUsage prices counters that were summed over many requests. It
// reports a floor: the per-request long-context tiers that a session may have
// paid cannot be reconstructed from a total, and applying them to the total
// would overstate every tiered model by its multiple.
func PriceAggregateUsage(model string, input, output, cacheRead, cacheWrite int) SessionStatsCostEstimate {
	return priceUsageCalls([]UsageCall{{
		Model: model, InputTokens: input, OutputTokens: output,
		CachedInputTokens: cacheRead, CacheWriteTokens: cacheWrite,
	}}, "", true)
}

func priceUsageCalls(calls []UsageCall, fallbackModel string, aggregated bool) SessionStatsCostEstimate {
	fetcher := usage.NewPricingFetcher()
	var estimate SessionStatsCostEstimate
	for _, call := range calls {
		model := strings.TrimSpace(call.Model)
		if model == "" && !call.Guardian && !call.Subagent {
			// Only the session's own turns may borrow the displayed model; a
			// helper or delegate on an unknown model is not this model.
			model = strings.TrimSpace(fallbackModel)
		}
		if model == "" {
			estimate.Unpriced++
			continue
		}
		cost, err := fetcher.CalculateCostLocal(usage.UsageEntry{
			Model:            model,
			InputTokens:      call.InputTokens,
			OutputTokens:     call.OutputTokens,
			CacheReadTokens:  call.CachedInputTokens,
			CacheWriteTokens: call.CacheWriteTokens,
			Provider:         usage.ProviderTermLLM,
			PreAggregated:    aggregated,
		})
		if err != nil {
			estimate.Unpriced++
			continue
		}
		estimate.CostUSD += cost
		estimate.Priced++
	}
	return estimate
}
