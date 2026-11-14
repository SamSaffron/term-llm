package cmd

import (
	"github.com/term-llm/term-llm/internal/app/runtime"
	"github.com/term-llm/term-llm/internal/config"

	"github.com/term-llm/term-llm/internal/tools"
)

func wireFileRecorder(registry *tools.LocalToolRegistry, cfg *config.Config) {
	runtime.WireFileRecorder(registry, cfg)
}
