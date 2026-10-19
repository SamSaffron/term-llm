package chat

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func filterSearchToolSpecs(specs []core.ToolSpec, searchEnabled bool) []core.ToolSpec {
	if searchEnabled {
		return specs
	}
	filtered := make([]core.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if isSearchToolName(spec.Name) {
			continue
		}
		filtered = append(filtered, spec)
	}
	return filtered
}

func isSearchToolName(name string) bool {
	return name == core.WebSearchToolName || name == core.ReadURLToolName
}
