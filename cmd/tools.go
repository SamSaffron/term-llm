package cmd

import (
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/search"
)

func defaultToolRegistry() *llm.ToolRegistry {
	registry := llm.NewToolRegistry()
	registry.Register(llm.NewWebSearchTool(search.NewDuckDuckGoLite(nil)))
	return registry
}
