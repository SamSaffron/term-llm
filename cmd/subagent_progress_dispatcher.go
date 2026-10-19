package cmd

import (
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/tools"
)

func newSubagentProgressDispatcher(send func(string, tools.SubagentEvent)) *runtime.SubagentProgressDispatcher {
	return runtime.NewSubagentProgressDispatcher(send)
}
