package chat

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

// PassiveCommandMsg tells the process host that a command only waits for a UI
// event. It has no operation effects and must not keep an idle process alive.
type PassiveCommandMsg struct{ Command tea.Cmd }

func PassiveCommand(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg { return PassiveCommandMsg{cmd} }
}
func (m *Model) passiveCommand(cmd tea.Cmd) tea.Cmd {
	if !m.reloadEnabled {
		return cmd
	}
	return PassiveCommand(cmd)
}
func (m *Model) presentationTick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return m.passiveCommand(tea.Tick(d, fn))
}

// ReloadState preserves presentation and an optional settled engine boundary.
// Messages are included only for --no-session runtimes.
type ReloadState struct {
	Continuation *llm.Continuation
	SessionID    string
	Provider     string
	Model        string
	Draft        string
	ShellMode    bool
	Files        []FileAttachment
	Images       []ImageAttachment
	SideVisible  bool
	SideDraft    string
	SideHistory  []sidequestion.Entry
	SideQuestion string
	SideResponse string
	Meta         *session.Session  `json:",omitempty"`
	Messages     []session.Message `json:",omitempty"`
}
type ReloadInspectMsg struct {
	Context context.Context
	Reply   chan<- ReloadInspection
}
type ReloadInspection struct {
	State ReloadState
	Err   error
}

func (m *Model) EnableProcessReload()       { m.reloadEnabled = true }
func (m *Model) ProcessReloadEnabled() bool { return m.reloadEnabled }

// ReloadBusy reuses mutation guards and real completion channels, rather than
// equating a rendered stream-done message with settled background work.
func (m *Model) ReloadBusy() bool {
	if m.transcriptMutationBusy() || m.directShellRun != nil || m.commitBusy() || m.sessionTransition != nil || m.runtimeOperations.activeCount() != 0 || len(m.autoSendQueue) != 0 || m.autoSendPending || m.queuedBranchSend != nil {
		return true
	}
	if m.mainRunManager.UnsettledCount() != 0 {
		return true
	}
	for _, done := range []<-chan struct{}{m.streamDone, m.sideQuestion.Done} {
		if done != nil {
			select {
			case <-done:
			default:
				return true
			}
		}
	}
	return m.engine != nil && m.engine.ActiveToolExecutions() != 0
}

func (m *Model) inspectReload(msg ReloadInspectMsg) {
	result := ReloadInspection{}
	switch {
	case msg.Context.Err() != nil:
		result.Err = msg.Context.Err()
	case m.ReloadBusy():
		result.Err = errors.New("TUI still owns unfinished work")
	default:
		result.State = ReloadState{Continuation: m.reloadContinuation, SessionID: m.SessionID(), Provider: m.providerKey, Model: m.modelName, Draft: m.expandedPastePlaceholders(m.textarea.Value()), ShellMode: m.directShellComposerActive(), Files: append([]FileAttachment(nil), m.files...), Images: append([]ImageAttachment(nil), m.images...), SideVisible: m.sideQuestion.Visible, SideDraft: m.sideQuestion.Composer.Value(), SideHistory: append([]sidequestion.Entry(nil), m.sideQuestion.History...), SideQuestion: m.sideQuestion.Question, SideResponse: m.sideQuestion.Response.String()}
		if m.store == nil {
			result.State.Meta = m.sess
			result.State.Messages = append([]session.Message(nil), m.messages...)
		}
	}
	select {
	case msg.Reply <- result:
	case <-msg.Context.Done():
	}
}

func (m *Model) RestoreReloadState(state ReloadState) {
	m.reloadContinuation = state.Continuation
	if state.Meta != nil && m.store == nil {
		m.sess = state.Meta
		m.messages = state.Messages
		m.invalidateHistoryCache()
	}
	m.branchPrefill = state.Draft
	m.files, m.images = state.Files, state.Images
	m.restoreComposerSnapshot(composerSnapshot{body: state.Draft, shellMode: state.ShellMode})
	m.ensureSideComposer()
	m.sideQuestion.Visible = state.SideVisible
	m.sideQuestion.Composer.SetValue(state.SideDraft)
	m.sideQuestion.History = state.SideHistory
	m.sideQuestion.Question = state.SideQuestion
	m.sideQuestion.Response.Reset()
	m.sideQuestion.Response.WriteString(state.SideResponse)
}

func (m *Model) reloadBlocksInput(msg tea.Msg) bool {
	if !m.reloadEnabled || !restart.Default.Draining() {
		return false
	}
	switch key := msg.(type) {
	case tea.KeyPressMsg:
		// Controls for already-admitted work remain usable during drain.
		return key.String() != "esc" && key.String() != "ctrl+c" && m.approvalModel == nil && m.askUserModel == nil
	case tea.PasteMsg, tea.MouseMsg:
		return true
	}
	return false
}

// ReloadResumeMsg runs only on the UI event loop, after failed-exec rollback or
// replacement initialization. It does not send a new user message.
type ReloadResumeMsg struct{}

func (m *Model) suspendForReload(saved *llm.Continuation) (tea.Model, tea.Cmd) {
	m.reloadContinuation = saved
	m.streaming = false
	m.mainRunViewComplete = true
	m.releaseStreamCancelFunc()
	m.clearStreamCallbacks()
	m.restoreSkillAllowedTools()
	m.setStreamCancelRequested(false)
	m.resetRetainedStreamTracker()
	m.currentResponse.Reset()
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}
	m.smoothTickPending = false
	if m.store != nil {
		if saved.DiscardPartial {
			messages := make([]session.Message, 0, len(saved.Request.Messages))
			for i, message := range saved.Request.Messages {
				messages = append(messages, *session.NewMessage(m.SessionID(), message, i))
			}
			if err := m.store.ReplaceMessages(context.Background(), m.SessionID(), messages); err != nil {
				return m.showFooterError(err.Error())
			}
		}
		if err := m.reloadMessagesFromStore(context.Background()); err != nil {
			return m.showFooterError(err.Error())
		}
	} else {
		m.messages = nil
		for _, message := range saved.Request.Messages {
			if message.Role != llm.RoleSystem {
				m.messages = append(m.messages, *session.NewMessage(m.SessionID(), message, -1))
			}
		}
	}
	m.invalidateHistoryCache()
	m.phase = "Restarting"
	wait := m.passiveCommand(func() tea.Msg {
		if restart.Default.WaitReady(m.rootContext()) != nil {
			return nil
		}
		return ReloadResumeMsg{}
	})
	return m, tea.Batch(wait, m.mainRunStatusCmd())
}

func (m *Model) resumeAfterReload() (tea.Model, tea.Cmd) {
	if m.reloadContinuation == nil || m.streaming {
		return m, nil
	}
	m.streaming = true
	// The tracker will contain only post-reload events. Keep the persisted
	// assistant/tool prefix authoritative, as with a partial run reattachment.
	m.mainRunViewComplete = false
	m.mainRunID = ""
	m.mainRunLastSeq = 0
	m.mainRunSubscription++
	m.mainRunReplay = nil
	m.mainRunLive = nil
	m.mainRunCoalescer = nil
	m.streamStartTime = time.Now()
	m.phase = "Resuming"
	return m, tea.Batch(m.startStream(""), m.tickEvery(), m.passiveCommand(m.spinner.Tick))
}
