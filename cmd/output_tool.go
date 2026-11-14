package cmd

import (
	"github.com/term-llm/term-llm/internal/agents"
	"github.com/term-llm/term-llm/internal/app/runtime"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/tools"
)

func registerAgentOutputTool(cfg agents.OutputToolConfig, toolMgr *tools.ToolManager, engine *engine.Engine) *tools.SetOutputTool {
	return runtime.RegisterAgentOutputTool(cfg, toolMgr, engine)
}
