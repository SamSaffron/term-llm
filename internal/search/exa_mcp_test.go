package search

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewSearcherDefaultsToExaMCP(t *testing.T) {
	searcher, err := NewSearcher(&config.Config{})
	if err != nil {
		t.Fatalf("NewSearcher returned error: %v", err)
	}
	if _, ok := searcher.(*ExaMCPClient); !ok {
		t.Fatalf("NewSearcher returned %T, want *ExaMCPClient", searcher)
	}
}

func TestNewSearcherExaMCPDoesNotRequireAPIKey(t *testing.T) {
	cfg := &config.Config{}
	cfg.Search.Provider = "exa_mcp"

	searcher, err := NewSearcher(cfg)
	if err != nil {
		t.Fatalf("NewSearcher returned error: %v", err)
	}
	if _, ok := searcher.(*ExaMCPClient); !ok {
		t.Fatalf("NewSearcher returned %T, want *ExaMCPClient", searcher)
	}
}

func TestParseExaMCPSearchResults(t *testing.T) {
	out := `Title: First Result
URL: https://example.com/one
Published: N/A
Author: N/A
Highlights:
This is the first highlight.
More detail.

---

Title: Second Result
URL: https://example.com/two
Highlights:
Second highlight.`

	results := parseExaMCPSearchResults(out, 10)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Title != "First Result" || results[0].URL != "https://example.com/one" {
		t.Fatalf("unexpected first result: %+v", results[0])
	}
	if !strings.Contains(results[0].Snippet, "first highlight") || !strings.Contains(results[0].Snippet, "More detail") {
		t.Fatalf("unexpected snippet: %q", results[0].Snippet)
	}
}
