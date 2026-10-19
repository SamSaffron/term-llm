package chat

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
)

// A fresh session needs a fresh runtime too: mutating the old Model in place
// would change the session underneath its detached children and wake coordinator.
func (m *Model) beginFreshSessionSwitch() (tea.Model, tea.Cmd) {
	if m.branchContextInFlight() {
		return m.showFooterWarning("Wait for path notes to finish, or press Esc to cancel them before starting a new session.")
	}
	if m.sessionSwitchPending {
		return m.showFooterWarning("A session switch is already in progress.")
	}
	m.pauseGoalForLocalAction("paused because a new session was started")
	now := time.Now()
	next := &session.Session{ID: session.NewID(), Provider: m.providerName, ProviderKey: m.providerKey, Model: m.modelName,
		Mode: session.ModeChat, Origin: session.OriginTUI, Agent: m.agentName, CWD: m.effectiveWorkingDir(),
		CreatedAt: now, UpdatedAt: now, Search: m.searchEnabled, Tools: m.toolsStr, MCP: m.mcpStr,
		ApprovalMode: sessionApprovalModeFromTools(m.requestedApprovalMode)}
	persistNewTUISession(m.rootContext(), m.store, next)
	return m.beginSessionSwitch(SessionSwitchRequest{SessionID: next.ID})
}
