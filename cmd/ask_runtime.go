package cmd

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
)

// buildAskRequest projects prepared conversation/runtime values into the engine
// request. It borrows managers and does not own their cleanup.
func buildAskRequest(cfg *config.Config, settings SessionSettings, sessionID string, messages []llm.Message, engine *llm.Engine, toolMgr *tools.ToolManager, mcpManager *mcp.Manager, outputTool *tools.SetOutputTool, debug, debugRaw bool) llm.Request {
	req := llm.Request{SessionID: sessionID, WorkingDir: settings.BaseDir, Messages: messages, EnableToolDiscovery: mcpManager != nil, Search: settings.Search, ForceExternalSearch: resolveForceExternalSearch(cfg, askNativeSearch, askNoNativeSearch), DisableExternalWebFetch: askNoWebFetch, ParallelToolCalls: true, MaxTurns: settings.MaxTurns, MaxOutputTokens: settings.MaxOutputTokens, Debug: debug, DebugRaw: debugRaw}
	if toolMgr != nil || mcpManager != nil || outputTool != nil {
		specs := llm.ToolSpecsForRequest(engine.Tools(), settings.Search)
		var approval *tools.ApprovalManager
		if toolMgr != nil {
			approval = toolMgr.ApprovalMgr
		}
		specs = tools.FilterToolSpecsForApprovalMode(specs, approval)
		if len(specs) > 0 {
			req.Tools = specs
			req.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		}
	}
	return req
}
