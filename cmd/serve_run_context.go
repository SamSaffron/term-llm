package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

// prepareRunContext binds platform-owned context values and normalizes the
// provider runtime identity. Cancellation remains owned by runOnce.
func (rt *serveRuntime) prepareRunContext(ctx context.Context, collaboration tools.CollaborativeShellRunBinding, req *llm.Request) (context.Context, string, string) {
	if rt.toolMgr != nil && rt.toolMgr.Registry != nil {
		ctx = tools.ContextWithCollaborativeShellRunBinding(ctx, collaboration)
	}
	askUser := rt.askUserFunc
	if askUser == nil {
		switch rt.platform {
		case "", "web":
			askUser = rt.awaitAskUser
		case "telegram", "jobs":
			platform := rt.platform
			askUser = func(context.Context, []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
				return nil, fmt.Errorf("ask_user is not available on %s sessions", platform)
			}
		}
	}
	if askUser != nil {
		ctx = tools.ContextWithAskUserUIFunc(ctx, askUser)
	}
	if rt.platform == "web" {
		if callback := rt.subagentProgressCallback(); callback != nil {
			ctx = tools.ContextWithSubagentEventCallback(ctx, callback)
		}
		if strings.TrimSpace(req.SessionID) != "" {
			ctx = tools.ContextWithQueueAgentOrigin(ctx, tools.QueueAgentOriginContext{Origin: tools.QueueAgentOriginWeb, SessionID: req.SessionID})
		}
	}
	model, effort := strings.TrimSpace(req.Model), strings.TrimSpace(req.ReasoningEffort)
	if model == "" {
		model = strings.TrimSpace(rt.defaultModel)
	}
	model, effort = normalizeProviderModelEffort(runtimeProviderKey(rt), model, effort)
	if model != "" {
		req.Model = model
	}
	req.ReasoningEffort = effort
	return ctx, model, effort
}
