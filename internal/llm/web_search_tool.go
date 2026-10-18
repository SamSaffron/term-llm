package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/search"
)

// WebSearchTool executes searches through a Searcher.
type WebSearchTool struct {
	searcher search.Searcher
}

func NewWebSearchTool(searcher search.Searcher) *WebSearchTool {
	return &WebSearchTool{searcher: searcher}
}

func (t *WebSearchTool) Spec() core.ToolSpec {
	return WebSearchToolSpec()
}

func (t *WebSearchTool) Preview(args json.RawMessage) string {
	var payload struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &payload); err != nil || payload.Query == "" {
		return ""
	}
	return payload.Query
}

func (t *WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	var payload struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return core.ToolOutput{}, fmt.Errorf("parse web_search args: %w", err)
	}
	if payload.MaxResults <= 0 {
		payload.MaxResults = 20
	}
	results, err := t.searcher.Search(ctx, payload.Query, payload.MaxResults)
	if err != nil {
		return core.ToolOutput{}, err
	}
	if len(results) == 0 {
		return core.TextOutput("No results found."), nil
	}

	var b strings.Builder
	for _, r := range results {
		if r.URL == "" || r.Title == "" {
			continue
		}
		b.WriteString("- [")
		b.WriteString(r.Title)
		b.WriteString("](")
		b.WriteString(r.URL)
		b.WriteString(")")
		if r.Snippet != "" {
			b.WriteString(" - ")
			b.WriteString(r.Snippet)
		}
		b.WriteString("\n")
	}
	return core.TextOutput(strings.TrimSuffix(b.String(), "\n")), nil
}
