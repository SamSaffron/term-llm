package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"

	"github.com/samsaffron/term-llm/internal/tools"
)

func wireFileRecorder(registry *tools.LocalToolRegistry, cfg *config.Config) {
	runtime.WireFileRecorder(registry, cfg)
}
