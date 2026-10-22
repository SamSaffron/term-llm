package tools

import (
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func (t *GrepTool) Spec() core.ToolSpec {
	spec := core.ToolSpec{
		Name:        GrepToolName,
		Description: "Search file contents using regex patterns (RE2 syntax). Returns matches with context.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Regular expression pattern to search for (RE2 syntax)",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "File or directory to search in (defaults to current directory)",
				},
				"include": map[string]interface{}{
					"type":        "string",
					"description": "Glob filter for files, e.g., '*.go' or '*.{js,ts}'",
				},
				"exclude": map[string]interface{}{
					"type":        "string",
					"description": "Glob pattern for paths to exclude, e.g. 'vendor/**' or '**/*_test.go'",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Ripgrep file type filter, e.g. 'go', 'ts', or 'rb'",
				},
				"context_lines": map[string]interface{}{
					"type":        "integer",
					"description": "Lines of context around each match (default: 2)",
					"default":     2,
				}, "files_with_matches": map[string]interface{}{
					"type":        "boolean",
					"description": "Return only filenames containing matches, not the match lines (default: false)",
				},
				"multiline": map[string]interface{}{
					"type":        "boolean",
					"description": "Allow regex matches to span line boundaries (default: false)",
				},
				"max_results": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results (default: 100)",
					"default":     100,
				}},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
	if t.reranker != nil {
		spec.Description += " Set intent to rerank and filter matches by relevance."
		spec.Schema["properties"].(map[string]interface{})["intent"] = map[string]interface{}{
			"type":        "string",
			"description": "What you are trying to find (e.g. 'where session DB writes happen'). When set, matches are reranked by relevance and incidental matches dropped. Omit for plain grep. Ignored with files_with_matches.",
		}
	}
	return spec
}

func (t *GrepTool) Preview(args json.RawMessage) string {
	var a GrepArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Pattern == "" {
		return ""
	}
	pattern := a.Pattern
	if len(pattern) > 30 {
		pattern = pattern[:27] + "..."
	}
	result := fmt.Sprintf("/%s/", pattern)
	if a.Path != "" {
		result += " in " + a.Path
	}
	if a.Include != "" {
		result += " (" + a.Include + ")"
	}
	if a.Type != "" {
		result += " type:" + a.Type
	}
	if a.Exclude != "" {
		result += " exclude:" + a.Exclude
	}
	if a.Multiline {
		result += " multiline"
	}
	return result
}
