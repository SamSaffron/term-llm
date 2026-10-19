package runtime

import (
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

// RegisterAgentOutputTool installs an agent's structured output tool.
func RegisterAgentOutputTool(cfg agents.OutputToolConfig, toolMgr *tools.ToolManager, engine *engine.Engine) *tools.SetOutputTool {
	outputTool := tools.NewSetOutputTool(cfg.Name, cfg.Param, cfg.Description, cfg.Schema)
	if toolMgr == nil {
		engine.RegisterTool(outputTool)
		return outputTool
	}
	toolMgr.Registry.RegisterOutputTool(outputTool)
	toolMgr.SetupEngine(engine)
	return outputTool
}
