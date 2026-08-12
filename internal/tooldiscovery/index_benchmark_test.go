package tooldiscovery

import (
	"fmt"
	"testing"

	"github.com/samsaffron/term-llm/internal/mcp"
)

func BenchmarkIndexBuild10000(b *testing.B) {
	tools := benchmarkTools(10_000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buildIndexSnapshot(tools, uint64(i+1))
	}
}

func BenchmarkSearch10000(b *testing.B) {
	idx := newSearchIndex(benchmarkTools(10_000), 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		results := idx.search("merge pull request after review checks", 5, nil)
		if len(results) == 0 {
			b.Fatal("no results")
		}
	}
}

func benchmarkTools(count int) []mcp.CatalogTool {
	tools := make([]mcp.CatalogTool, count)
	for i := range tools {
		server := fmt.Sprintf("server_%02d", i%20)
		name := fmt.Sprintf("operation_%05d", i)
		description := "Read a realistic project record with review checks, pull request metadata, filters, and stable identifiers"
		if i%97 == 0 {
			name = fmt.Sprintf("merge_pull_request_%05d", i)
			description = "Merge a pull request after checking required reviews and continuous integration check runs"
		}
		fullName := server + "__" + name
		tools[i] = mcp.CatalogTool{
			ID: fullName, Name: fullName, OriginalName: name, Server: server, Description: description,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"repository": map[string]any{"type": "string"}, "pull_number": map[string]any{"type": "integer"}}},
		}
	}
	return tools
}
