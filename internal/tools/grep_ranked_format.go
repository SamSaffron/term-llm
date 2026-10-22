package tools

import "github.com/samsaffron/term-llm/internal/greprerank"

// Ranked matches must not pass through formatGrepResults: it regroups by file
// and caps ten blocks/file, both of which would silently destroy rank order.
func formatRankedGrepResults(matches []GrepMatch, note func(displayed int) string) string {
	items := make([]greprerank.Item, len(matches))
	for i, m := range matches {
		items[i] = greprerank.Item{Path: m.FilePath, Block: m.Context}
	}
	return greprerank.FormatRanked(items, note, maxOutputBytes)
}
