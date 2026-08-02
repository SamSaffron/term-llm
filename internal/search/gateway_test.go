package search

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestGatewayExplicitFalsePreservesLocalSearch(t *testing.T) {
	route := false
	cfg := &config.Config{
		Gateway: config.GatewayConfig{URL: "https://gateway.invalid", Search: &route},
		Search:  config.SearchConfig{Provider: "duckduckgo"},
	}
	searcher, err := NewSearcher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := searcher.(*DuckDuckGoLite); !ok {
		t.Fatalf("explicit gateway.search false created %T, want local DuckDuckGo", searcher)
	}
}
