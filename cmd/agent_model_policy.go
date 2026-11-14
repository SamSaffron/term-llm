package cmd

import (
	"github.com/term-llm/term-llm/internal/app/runtime"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/modelpolicy"
)

// Compatibility aliases until runner and serve command implementations move.
type modelSelection = runtime.ModelSelection
type modelSelectionInput = runtime.ModelSelectionInput

func selectRunModel(cfg *config.Config, in modelSelectionInput) (modelSelection, modelpolicy.Policy, error) {
	return runtime.SelectRunModel(cfg, in)
}
