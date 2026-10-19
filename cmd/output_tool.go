package cmd

import (
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

func registerAgentOutputTool(cfg agents.OutputToolConfig, toolMgr *tools.ToolManager, engine *engine.Engine) *tools.SetOutputTool {
	return runtime.RegisterAgentOutputTool(cfg, toolMgr, engine)
}
