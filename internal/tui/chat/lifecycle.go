package chat

import "github.com/samsaffron/term-llm/internal/lifecycle"

// LifecycleSnapshot returns the current foreground lifecycle without mutating
// the model or publishing it. The Bubble Tea host decides which model is
// visible and therefore which snapshot has authority.
func (m *Model) LifecycleSnapshot() lifecycle.Snapshot {
	snapshot := lifecycle.Snapshot{State: lifecycle.Idle}
	if m == nil {
		return snapshot
	}
	snapshot.SessionID = sessionIDOf(m.sess)
	if m.sess != nil {
		snapshot.CWD = m.effectiveWorkingDir()
	}

	// Interactive blockers have precedence over concurrent work. A paused
	// external UI is blocked only while Bubble Tea is not intentionally handing
	// the terminal to a child process.
	if message, ok := m.blockingPromptDetail(); ok {
		snapshot.State = lifecycle.Blocked
		snapshot.Message = message
		return snapshot
	}
	if m.pausedForExternalUI && !m.externalProcessActive {
		snapshot.State = lifecycle.Blocked
		snapshot.Message = "Waiting for external input"
		return snapshot
	}

	switch {
	case m.streaming:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Generating response"
	case m.directShellRun != nil:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Running shell command"
	case m.externalProcessActive:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Running external process"
	case m.worktreeOperationBusy():
		snapshot.State = lifecycle.Working
		snapshot.Message = "Running worktree operation"
	case m.sideQuestion.Running:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Answering side question"
	case m.branchContextInFlight():
		snapshot.State = lifecycle.Working
		snapshot.Message = "Preparing branch context"
	case m.sessionTransition != nil || m.sessionSwitchPending:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Preparing session switch"
	case m.activeSkillRunCount() > 0:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Running skill"
	case m.transcriptMutationInFlight:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Updating transcript"
	case m.shareInFlight:
		snapshot.State = lifecycle.Working
		snapshot.Message = "Sharing session"
	}
	return snapshot
}

func (m *Model) blockingPromptActive() bool {
	_, active := m.blockingPromptDetail()
	return active
}

func (m *Model) blockingPromptDetail() (string, bool) {
	if m == nil {
		return "", false
	}
	switch {
	case m.approvalModel != nil:
		return "Waiting for approval", true
	case m.askUserModel != nil:
		return "Waiting for user input", true
	case m.handoverPreview != nil:
		return "Waiting for handover confirmation", true
	default:
		return "", false
	}
}
