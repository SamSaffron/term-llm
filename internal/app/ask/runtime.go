package ask

import (
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/tools"
)

// buildAskRequest projects prepared conversation/runtime values into the engine
// request. It borrows managers and does not own their cleanup.
func buildAskRequest(opts Options, cfg *config.Config, settings appserve.SessionSettings, sessionID string, messages []core.Message, engine *engine.Engine, toolMgr *tools.ToolManager, mcpManager *mcp.Manager, outputTool *tools.SetOutputTool, debug, debugRaw bool) core.Request {
	req := core.Request{SessionID: sessionID, WorkingDir: settings.BaseDir, Messages: messages, EnableToolDiscovery: mcpManager != nil, Search: settings.Search, ForceExternalSearch: bootstrap.ResolveForceExternalSearch(cfg, opts.NativeSearch, opts.NoNativeSearch), DisableExternalWebFetch: opts.NoWebFetch, ParallelToolCalls: true, MaxTurns: settings.MaxTurns, MaxOutputTokens: settings.MaxOutputTokens, Debug: debug, DebugRaw: debugRaw}
	if toolMgr != nil || mcpManager != nil || outputTool != nil {
		specs := llm.ToolSpecsForRequest(engine.Tools(), settings.Search)
		var approval *tools.ApprovalManager
		if toolMgr != nil {
			approval = toolMgr.ApprovalMgr
		}
		specs = tools.FilterToolSpecsForApprovalMode(specs, approval)
		if len(specs) > 0 {
			req.Tools = specs
			req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
		}
	}
	return req
}
