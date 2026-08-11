package tooldiscovery

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tooldiscovery/evalfixture"
)

func TestMultiOperationQueryRetrievesDistinctRequiredTools(t *testing.T) {
	var tools []mcp.CatalogTool
	for _, domain := range evalfixture.Domains() {
		for _, definition := range domain.Tools {
			name := domain.Name + "__" + definition.Name
			tools = append(tools, mcp.CatalogTool{ID: name, Name: name, OriginalName: definition.Name, Server: domain.Name, Description: definition.Description})
		}
	}
	results := newSearchIndex(tools, 1).search("pull request reviews checks merge github gitlab", 10, nil)
	for _, result := range results {
		t.Logf("%s %.3f", result.ID, result.Score)
	}
	required := map[string]bool{"source_control__get_pull_request": true, "source_control__get_reviews": true, "source_control__get_check_runs": true, "source_control__merge_pull_request": true}
	hits := 0
	for _, result := range results[:min(5, len(results))] {
		if required[result.ID] {
			hits++
		}
	}
	if hits < 4 {
		t.Fatalf("top five contained %d/4 required tools", hits)
	}
}
