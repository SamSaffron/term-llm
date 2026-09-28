package tools

import "context"

type agentApprovalScopeKey struct{}

func ContextWithAgentApprovalScope(ctx context.Context, manager *ApprovalManager) context.Context {
	return context.WithValue(ctx, agentApprovalScopeKey{}, manager)
}

func AgentApprovalScopeFromContext(ctx context.Context) *ApprovalManager {
	manager, _ := ctx.Value(agentApprovalScopeKey{}).(*ApprovalManager)
	return manager
}
