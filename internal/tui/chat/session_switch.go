package chat

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
)

// SessionSwitchRequest carries relaunch state into a session switch: the
// target session plus any branch draft/notes/first-message handoff.
type SessionSwitchRequest struct {
	SessionID       string
	TargetLabel     string
	TargetNumber    int64
	BranchPrefill   string
	BranchPathNotes *BranchPathNotesRequest
	BranchAutoSend  string
}

type sessionTransition struct {
	request             SessionSwitchRequest
	startedAt           time.Time
	sourceComposer      composerSnapshot
	sourceFiles         []FileAttachment
	sourceImages        []ImageAttachment
	sourceSelectedImage int
	sourcePastes        map[int]string
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

func (m *Model) newSessionTransition(request SessionSwitchRequest) *sessionTransition {
	return &sessionTransition{
		request:             request,
		startedAt:           time.Now(),
		sourceComposer:      m.captureComposerSnapshot(),
		sourceFiles:         append([]FileAttachment(nil), m.files...),
		sourceImages:        cloneBranchImages(m.images),
		sourceSelectedImage: m.selectedImage,
		sourcePastes:        cloneBranchPasteChunks(m.pasteChunks),
	}
}

func (m *Model) restoreSessionTransitionSource(transition *sessionTransition) {
	if transition == nil {
		return
	}
	m.restoreComposerSnapshot(transition.sourceComposer)
	m.files = transition.sourceFiles
	m.images = transition.sourceImages
	m.selectedImage = transition.sourceSelectedImage
	m.pasteChunks = transition.sourcePastes
}

// beginSessionSwitch navigates to another session. With a switcher installed
// the visible model is replaced inside the running program, so the terminal
// never leaves the alternate screen (no relaunch flash). Otherwise it uses the
// historical quit-and-relaunch path driven by RequestedResumeSessionID.
func (m *Model) beginSessionSwitch(request SessionSwitchRequest) (tea.Model, tea.Cmd) {
	if m.sessionSwitcher == nil {
		prefill := request.BranchPrefill
		if m.sessionTransition != nil {
			prefill = m.textarea.Value()
		}
		m.pendingBranchPrefill = prefill
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
	transitionActive := m.sessionTransition != nil
	if transitionActive {
		m.sessionTransition.request = request
	} else {
		m.sessionTransition = m.newSessionTransition(request)
		m.restoreComposerSnapshot(composerSnapshot{body: request.BranchPrefill})
		m.files = nil
		m.images = nil
		m.selectedImage = -1
		m.pasteChunks = nil
	}
	if m.completions != nil {
		m.completions.Hide()
	}
	m.hideMentionPopup()
	m.bumpContentVersion()
	// Detach now: an active run keeps executing under the manager and retains
	// its events, so a failed switch can re-subscribe with replay. Branch creation
	// already detached before doing storage work, so do not discard that retained
	// presentation a second time.
	if !transitionActive {
		m.detachMainRun()
	}
	switcher := m.sessionSwitcher
	switchCmd := func() tea.Msg {
		next, err := switcher(request)
		return sessionSwitchedMsg{model: next, err: err}
	}
	_, footerCmd := m.showFooterMuted("Switching session…")
	return m, tea.Batch(switchCmd, footerCmd, m.spinner.Tick)
}

func (m *Model) failSessionTransition(message, tone string) (tea.Model, tea.Cmd) {
	m.sessionSwitchPending = false
	transition := m.sessionTransition
	m.sessionTransition = nil
	if transition != nil && m.textarea.Value() == transition.request.BranchPrefill && len(m.files) == 0 && len(m.images) == 0 && len(m.pasteChunks) == 0 {
		m.restoreSessionTransitionSource(transition)
	}
	m.bumpContentVersion()
	reattach := m.attachMainRun(m.SessionID())
	if m.mainRunManager != nil && m.program != nil {
		m.AttachMainRunUISink(m.program.Send)
	}
	_, footerCmd := m.showFooterMessageWithTone(message, tone)
	return m, tea.Batch(reattach, footerCmd)
}

func (m *Model) handleSessionSwitched(msg sessionSwitchedMsg) (tea.Model, tea.Cmd) {
	m.sessionSwitchPending = false
	transitionDraft := m.captureComposerSnapshot()
	transitionFiles := append([]FileAttachment(nil), m.files...)
	transitionImages := cloneBranchImages(m.images)
	transitionSelectedImage := m.selectedImage
	transitionPastes := cloneBranchPasteChunks(m.pasteChunks)
	m.sessionTransition = nil
	m.bumpContentVersion()
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
	// The transition composer is the target session's composer. It starts with
	// any branch prefill and may be edited while the full runtime is built, so it
	// must win over the original relaunch handoff during the replacement's Init.
	next.branchPrefill = ""
	transitionPayload := &pendingBranchSend{
		content:       transitionDraft.body,
		composer:      transitionDraft,
		files:         transitionFiles,
		images:        transitionImages,
		selectedImage: transitionSelectedImage,
		pasteChunks:   transitionPastes,
	}
	if next.branchAutoSend != "" && next.branchPathNotesRequest == nil {
		// Init must keep the explicit /thread message in the composer until its
		// auto-send command runs. Preserve anything drafted during hydration and
		// restore it immediately after that send instead of overwriting either.
		next.transitionAutoSendDraft = transitionPayload
		next.files = nil
		next.images = nil
		next.selectedImage = -1
		next.pasteChunks = nil
	} else {
		next.restoreComposerSnapshot(transitionDraft)
		next.files = transitionFiles
		next.images = transitionImages
		next.selectedImage = transitionSelectedImage
		next.pasteChunks = transitionPastes
	}
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
