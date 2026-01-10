package cmd

import (
	"log"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/search"
)

func defaultToolRegistry(cfg *config.Config) *llm.ToolRegistry {
	registry := llm.NewToolRegistry()
	searcher, err := search.NewSearcher(cfg)
	if err != nil {
		log.Printf("Warning: search provider error: %v, falling back to DuckDuckGo", err)
		searcher = search.NewDuckDuckGoLite(nil)
	}
	registry.Register(llm.NewWebSearchTool(searcher))
	registry.Register(llm.NewReadURLTool())
	return registry
}
