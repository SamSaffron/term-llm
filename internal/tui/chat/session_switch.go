package chat

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// SessionSwitchRequest carries relaunch state into a session switch: the
// target session plus any branch draft/notes/first-message handoff.
type SessionSwitchRequest struct {
	SessionID       string
	BranchPrefill   string
	BranchPathNotes *BranchPathNotesRequest
	BranchAutoSend  string
}

// SessionSwitcher builds a fully wired replacement model for the requested
// session against the already-running Bubble Tea program. It executes on a
// command goroutine, so implementations may perform blocking setup work.
type SessionSwitcher func(request SessionSwitchRequest) (*Model, error)

// SetSessionSwitcher enables in-process session switching. Without a switcher
// the model falls back to quitting so the caller can relaunch the program.
func (m *Model) SetSessionSwitcher(switcher SessionSwitcher) {
	m.sessionSwitcher = switcher
}

type sessionSwitchedMsg struct {
	model *Model
	err   error
}

// beginSessionSwitch navigates to another session. With a switcher installed
// the visible model is replaced inside the running program, so the terminal
// never leaves the alternate screen (no relaunch flash). Otherwise it uses the
// historical quit-and-relaunch path driven by RequestedResumeSessionID.
func (m *Model) beginSessionSwitch(request SessionSwitchRequest) (tea.Model, tea.Cmd) {
	if m.sessionSwitcher == nil {
		m.pendingBranchPrefill = request.BranchPrefill
		m.pendingBranchPathNotes = request.BranchPathNotes
		m.pendingBranchAutoSend = request.BranchAutoSend
		m.pendingResumeSessionID = request.SessionID
		m.detachMainRun()
		m.quitting = true
		return m, m.quitCmd()
	}
	if m.sessionSwitchPending {
		return m.showFooterWarning("A session switch is already in progress.")
	}
	m.sessionSwitchPending = true
	// Detach now: an active run keeps executing under the manager and retains
	// its events, so a failed switch can re-subscribe with replay.
	m.detachMainRun()
	switcher := m.sessionSwitcher
	switchCmd := func() tea.Msg {
		next, err := switcher(request)
		return sessionSwitchedMsg{model: next, err: err}
	}
	_, footerCmd := m.showFooterMuted("Switching session…")
	return m, tea.Batch(switchCmd, footerCmd)
}

func (m *Model) handleSessionSwitched(msg sessionSwitchedMsg) (tea.Model, tea.Cmd) {
	m.sessionSwitchPending = false
	if msg.err != nil || msg.model == nil {
		// This model stays visible; restore both its stream subscription and its
		// interactive prompt destination. beginSessionSwitch detached both before
		// setup started so an outgoing background run could proceed independently.
		reattach := m.attachMainRun(m.SessionID())
		if m.mainRunManager != nil && m.program != nil {
			m.AttachMainRunUISink(m.program.Send)
		}
		err := msg.err
		if err == nil {
			err = fmt.Errorf("switch produced no session")
		}
		_, footerCmd := m.showFooterError(fmt.Sprintf("Switch session: %v", err))
		return m, tea.Batch(reattach, footerCmd)
	}
	next := msg.model
	next.program = m.program
	// Attach the interactive-prompt sink only now that this model is the one
	// the program renders: retained prompts from a background run of the
	// target session must reach the replacement, not the outgoing model.
	// AttachUISink flushes those prompts asynchronously, so attaching from the
	// update loop cannot deadlock on Program.Send.
	if next.mainRunManager != nil {
		status := next.mainRunManager.Status(next.SessionID())
		if status.RunID != "" {
			next.mainRunManager.Visit(next.SessionID(), status.RunID)
		}
	}
	if next.mainRunManager != nil && next.program != nil {
		next.AttachMainRunUISink(next.program.Send)
	}
	// Bubble Tea reports terminal size only to the model it started with. Apply
	// the current dimensions before returning the replacement so even its first
	// frame is fitted. Sending a synthetic WindowSizeMsg through the Program can
	// trigger an unnecessary alternate-screen clear and reintroduce a flash.
	if m.width > 0 && m.height > 0 {
		next.applyWindowSize(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	}
	return next, next.Init()
}
