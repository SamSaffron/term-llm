package cmd

import (
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func registerAgentOutputTool(cfg agents.OutputToolConfig, toolMgr *tools.ToolManager, engine *llm.Engine) *tools.SetOutputTool {
	outputTool := tools.NewSetOutputTool(cfg.Name, cfg.Param, cfg.Description, cfg.Schema)
	if toolMgr == nil {
		engine.RegisterTool(outputTool)
		return outputTool
	}

	toolMgr.Registry.RegisterOutputTool(outputTool)
	toolMgr.SetupEngine(engine)
	return outputTool
}
