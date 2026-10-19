package cmd

import "context"

func (rt *serveRuntime) CloseContext(ctx context.Context) {
	if rt.agentOwner != nil && rt.spawnRunner != nil && len(rt.spawnRunner.OutstandingAgentIDs()) > 0 {
		rt.agentOwner.Adopt(rt, nil)
		return
	}
	rt.closeContext(ctx, false)
}

// These methods expose only child lifecycle operations to runtime.AgentHostOwner.
func (rt *serveRuntime) HasOutstandingAgents() bool {
	return rt.spawnRunner != nil && len(rt.spawnRunner.OutstandingAgentIDs()) > 0
}
func (rt *serveRuntime) DrainOwned(ctx context.Context) error       { return rt.spawnRunner.Drain(ctx) }
func (rt *serveRuntime) ShutdownOwned(ctx context.Context) error    { return rt.spawnRunner.Shutdown(ctx) }
func (rt *serveRuntime) CloseOwned(ctx context.Context, drain bool) { rt.closeContext(ctx, drain) }
