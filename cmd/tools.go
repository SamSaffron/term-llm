package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

func defaultToolRegistry(cfg *config.Config) *engine.ToolRegistry {
	return runtime.DefaultToolRegistry(cfg)
}

func newEngine(provider core.Provider, cfg *config.Config) *engine.Engine {
	return runtime.NewEngine(provider, cfg)
}

func buildToolConfig(toolsFlag string, readDirs, writeDirs, shellAllow []string, cfg *config.Config) tools.ToolConfig {
	return runtime.BuildToolConfig(toolsFlag, readDirs, writeDirs, shellAllow, cfg)
}

func filterOutTools(enabled []string, exclude ...string) []string {
	return runtime.FilterOutTools(enabled, exclude...)
}
