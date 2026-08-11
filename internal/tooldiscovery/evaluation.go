package tooldiscovery

import "github.com/samsaffron/term-llm/internal/mcp"

// SearchCatalogue runs the production lexical index without activating tools.
// It is used by deterministic retrieval evaluation and diagnostics.
func SearchCatalogue(tools []mcp.CatalogTool, query string, limit int) []SearchResult {
	return newSearchIndex(tools, 1).search(query, limit, nil)
}
