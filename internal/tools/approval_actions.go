package tools

// ApprovalGoverned reports whether a tool can ask the approval system (prompt
// or Guardian) to review an action. The Web UI uses this to decide whether
// approval status is relevant at all: a conversation that can only send text
// must not show approval warnings.
//
// Unknown names cover custom agent tools, MCP tools, and skill-provided dynamic
// tools. They execute outside the built-in path and shell checks, so they are
// treated as governed instead of silently hiding a status the user may need.
func ApprovalGoverned(name string) bool {
	descriptor := lookupBuiltin(name)
	if descriptor == nil {
		return true
	}
	switch descriptor.Policy {
	case KindEdit, KindExecute, KindImage:
		return true
	case KindRead, KindSearch:
		// Path-scoped reads and searches prompt when the path is not already
		// authorised; other read-only tools (session listings, goal reads) never do.
		return descriptor.pathCapable
	case KindAgent:
		// Spawning or queueing work runs another agent whose tools can inherit
		// this conversation's approval transport. Controlling existing children
		// (wait/continue/cancel/list) cannot.
		return descriptor.Name == SpawnAgentToolName || descriptor.Name == QueueAgentToolName
	case KindSessionState:
		return descriptor.Name == ManageWorkspaceToolName
	case KindSkill, KindInteractive:
		// Skill activation and questions are not approval-gated themselves.
		return false
	}
	return true
}

// AnyApprovalGoverned reports whether any named tool can be approval-governed.
func AnyApprovalGoverned(names []string) bool {
	for _, name := range names {
		if ApprovalGoverned(name) {
			return true
		}
	}
	return false
}
