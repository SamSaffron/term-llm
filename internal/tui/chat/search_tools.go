package chat

import "github.com/samsaffron/term-llm/internal/llm"

func filterSearchToolSpecs(specs []llm.ToolSpec, searchEnabled bool) []llm.ToolSpec {
	if searchEnabled {
		return specs
	}
	filtered := make([]llm.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if isSearchToolName(spec.Name) {
			continue
		}
		filtered = append(filtered, spec)
	}
	return filtered
}

func isSearchToolName(name string) bool {
	return name == llm.WebSearchToolName || name == llm.ReadURLToolName
}
