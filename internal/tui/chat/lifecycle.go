package chat

// LifecycleReporter receives the coarse status that a terminal host needs to
// coordinate an interactive chat. Implementations must return promptly because
// reports are emitted from Bubble Tea's event loop.
type LifecycleReporter interface {
	Report(state, sessionID string)
}

// SetLifecycleReporter attaches an optional host integration. The current
// state is emitted straight away so a newly opened chat is visible as idle
// before the user submits a prompt. Passing nil detaches the current reporter.
func (m *Model) SetLifecycleReporter(reporter LifecycleReporter) {
	m.lifecycleReporter = reporter
	m.lifecycleReported = false
	m.lifecycleState = ""
	m.lifecycleSession = ""
	m.reportLifecycleState()
}

func (m *Model) reportLifecycleState() {
	if m.lifecycleReporter == nil {
		return
	}
	state := m.currentLifecycleState()
	sessionID := sessionIDOf(m.sess)
	if m.lifecycleReported && m.lifecycleState == state && m.lifecycleSession == sessionID {
		return
	}
	m.lifecycleReported = true
	m.lifecycleState = state
	m.lifecycleSession = sessionID
	m.lifecycleReporter.Report(state, sessionID)
}

func (m *Model) currentLifecycleState() string {
	if m.approvalModel != nil || m.askUserModel != nil || (m.pausedForExternalUI && !m.externalProcessActive) {
		return "blocked"
	}
	if m.streaming || m.directShellRun != nil || m.externalProcessActive || m.worktreeOperationBusy() {
		return "working"
	}
	return "idle"
}
