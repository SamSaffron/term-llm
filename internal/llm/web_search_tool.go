package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/search"
)

// WebSearchTool executes searches through a Searcher.
type WebSearchTool struct {
	searcher search.Searcher
}

func NewWebSearchTool(searcher search.Searcher) *WebSearchTool {
	return &WebSearchTool{searcher: searcher}
}

func (t *WebSearchTool) Spec() ToolSpec {
	return WebSearchToolSpec()
}

func (t *WebSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var payload struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &payload); err != nil {
		return "", fmt.Errorf("parse web_search args: %w", err)
	}
	if payload.MaxResults <= 0 {
		payload.MaxResults = 20
	}
	results, err := t.searcher.Search(ctx, payload.Query, payload.MaxResults)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No results found.", nil
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
	return strings.TrimSuffix(b.String(), "\n"), nil
}
