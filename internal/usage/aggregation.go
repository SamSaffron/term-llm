package usage

import (
	"sort"
	"time"
)

// AggregateDaily aggregates usage entries by day
func AggregateDaily(entries []UsageEntry) []DailyUsage {
	if len(entries) == 0 {
		return nil
	}

	// Group by date
	byDate := make(map[string]*DailyUsage)
	for _, e := range entries {
		date := e.Timestamp.Format("2006-01-02")
		daily, ok := byDate[date]
		if !ok {
			daily = &DailyUsage{
				Date:    date,
				Entries: []UsageEntry{},
			}
			byDate[date] = daily
		}

		daily.InputTokens += e.InputTokens
		daily.OutputTokens += e.OutputTokens
		daily.CacheWriteTokens += e.CacheWriteTokens
		daily.CacheReadTokens += e.CacheReadTokens
		daily.ReasoningTokens += e.ReasoningTokens
		daily.TotalCost += e.CostUSD
		daily.Entries = append(daily.Entries, e)

		// Track unique models
		found := false
		for _, m := range daily.ModelsUsed {
			if m == e.Model {
				found = true
				break
			}
		}
		if !found && e.Model != "" {
			daily.ModelsUsed = append(daily.ModelsUsed, e.Model)
		}
	}

	// Convert to slice and sort by date
	result := make([]DailyUsage, 0, len(byDate))
	for _, daily := range byDate {
		result = append(result, *daily)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Date < result[j].Date
	})

	return result
}

// GetModelBreakdown returns token usage broken down by model for a set of entries
func GetModelBreakdown(entries []UsageEntry) []ModelBreakdown {
	byModel := make(map[string]*ModelBreakdown)

	for _, e := range entries {
		model := e.Model
		if model == "" {
			model = "unknown"
		}

		mb, ok := byModel[model]
		if !ok {
			mb = &ModelBreakdown{Model: model}
			byModel[model] = mb
		}

		mb.InputTokens += e.InputTokens
		mb.OutputTokens += e.OutputTokens
		mb.CacheWriteTokens += e.CacheWriteTokens
		mb.CacheReadTokens += e.CacheReadTokens
		mb.ReasoningTokens += e.ReasoningTokens
		mb.Cost += e.CostUSD
	}

	result := make([]ModelBreakdown, 0, len(byModel))
	for _, mb := range byModel {
		result = append(result, *mb)
	}

	// Sort by total tokens descending
	sort.Slice(result, func(i, j int) bool {
		iTotal := result[i].InputTokens + result[i].OutputTokens
		jTotal := result[j].InputTokens + result[j].OutputTokens
		return iTotal > jTotal
	})

	return result
}

// ProviderBreakdown represents usage breakdown by provider
type ProviderBreakdown struct {
	Provider         string
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	ReasoningTokens  int
	Cost             float64
}

// GetProviderBreakdown returns token usage broken down by provider for a set of entries
func GetProviderBreakdown(entries []UsageEntry) []ProviderBreakdown {
	byProvider := make(map[string]*ProviderBreakdown)

	for _, e := range entries {
		provider := e.Provider
		if provider == "" {
			provider = "unknown"
		}

		pb, ok := byProvider[provider]
		if !ok {
			pb = &ProviderBreakdown{Provider: provider}
			byProvider[provider] = pb
		}

		pb.InputTokens += e.InputTokens
		pb.OutputTokens += e.OutputTokens
		pb.CacheWriteTokens += e.CacheWriteTokens
		pb.CacheReadTokens += e.CacheReadTokens
		pb.ReasoningTokens += e.ReasoningTokens
		pb.Cost += e.CostUSD
	}

	result := make([]ProviderBreakdown, 0, len(byProvider))
	for _, pb := range byProvider {
		result = append(result, *pb)
	}

	// Sort by cost descending
	sort.Slice(result, func(i, j int) bool {
		return result[i].Cost > result[j].Cost
	})

	return result
}

// CountProviders returns the number of unique providers in the entries
func CountProviders(entries []UsageEntry) int {
	providers := make(map[string]bool)
	for _, e := range entries {
		providers[e.Provider] = true
	}
	return len(providers)
}

// CalculateTotals calculates total usage across all daily entries
func CalculateTotals(daily []DailyUsage) DailyUsage {
	var total DailyUsage
	total.Date = "Total"
	modelSet := make(map[string]bool)

	for _, d := range daily {
		total.InputTokens += d.InputTokens
		total.OutputTokens += d.OutputTokens
		total.CacheWriteTokens += d.CacheWriteTokens
		total.CacheReadTokens += d.CacheReadTokens
		total.ReasoningTokens += d.ReasoningTokens
		total.TotalCost += d.TotalCost
		total.Entries = append(total.Entries, d.Entries...)

		for _, m := range d.ModelsUsed {
			modelSet[m] = true
		}
	}

	for m := range modelSet {
		total.ModelsUsed = append(total.ModelsUsed, m)
	}
	sort.Strings(total.ModelsUsed)

	return total
}

// LoadAllUsage loads usage data from all providers
func LoadAllUsage() LoadResult {
	var result LoadResult

	// Load from all providers
	claude := LoadClaudeUsage()
	result.Merge(claude)

	codex := LoadCodexUsage()
	result.Merge(codex)

	gemini := LoadGeminiUsage()
	result.Merge(gemini)

	termllm := LoadTermLLMUsage()
	result.Merge(termllm)

	// Sort all entries by timestamp
	sort.Slice(result.Entries, func(i, j int) bool {
		return result.Entries[i].Timestamp.Before(result.Entries[j].Timestamp)
	})

	return result
}

// CalculateCosts uses the pricing fetcher to calculate costs for entries without pre-calculated costs
func CalculateCosts(entries []UsageEntry, fetcher *PricingFetcher) []UsageEntry {
	result := make([]UsageEntry, len(entries))
	copy(result, entries)

	for i := range result {
		// Skip if already has a cost
		if result[i].CostUSD > 0 {
			continue
		}

		cost, err := fetcher.CalculateCost(result[i])
		if err == nil {
			result[i].CostUSD = cost
		}
	}

	return result
}

// DefaultDateRange returns the default date range (last 7 days)
func DefaultDateRange() (since, until time.Time) {
	now := time.Now()
	until = time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())
	since = until.AddDate(0, 0, -6) // 7 days including today
	since = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, since.Location())
	return since, until
}

// ParseDateYYYYMMDD parses a date in YYYYMMDD format
func ParseDateYYYYMMDD(s string) (time.Time, error) {
	return time.Parse("20060102", s)
}
