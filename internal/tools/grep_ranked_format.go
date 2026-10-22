package tools

import "github.com/samsaffron/term-llm/internal/greprerank"

// Ranked matches must not pass through formatGrepResults: it regroups by file
// and caps ten blocks/file, both of which would silently destroy rank order.
func formatRankedGrepResults(matches []GrepMatch, note func(displayed int) string) string {
	return formatRankedGrepResultsWithBudget(matches, note, maxOutputBytes)
}

func formatRankedGrepResultsWithBudget(matches []GrepMatch, note func(displayed int) string, budget int) string {
	items := make([]greprerank.Item, len(matches))
	for i, m := range matches {
		items[i] = greprerank.Item{Path: m.FilePath, Block: m.Context}
	}
	return greprerank.FormatRanked(items, note, budget)
}
