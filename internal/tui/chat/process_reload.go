package chat

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/session"
)

// ProcessReloadMsg enters the existing /reload path from the process signal
// owner. All model/UI access remains serialized by Bubble Tea.
type ProcessReloadMsg struct{}
type processReloadTick struct{}
type processResumeMsg struct{}

type ProcessReloadState struct {
	Draft     string               `json:"draft"`
	ShellMode bool                 `json:"shell_mode,omitempty"`
	Files     []FileAttachment     `json:"files,omitempty"`
	Images    []ImageAttachment    `json:"images,omitempty"`
	Continue  bool                 `json:"continue,omitempty"`
	Pending   []llm.QueuedSteering `json:"pending,omitempty"`
}

type processReload struct {
	pending   bool
	started   time.Time
	cancelled bool
	owner     llm.SteeringTransition
	state     ProcessReloadState
}

func (m *Model) ProcessReloadState() ProcessReloadState { return m.processReload.state }
func (m *Model) SetProcessReloadState(state ProcessReloadState) {
	m.processReload.state = state
	m.branchPrefill = state.Draft
	m.processResume = state.Continue
}
func processReloadTickCmd() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return processReloadTick{} })
}

func (m *Model) processReloadBusy() bool {
	return m.streaming || m.branchContextInFlight() || m.activeSkillRunCount() > 0 || m.directShellRun != nil || m.runtimeOperations.activeCount() > 0 || (m.engine != nil && m.engine.ActiveToolExecutions() > 0) || m.mainRunManager.ActiveCount() > 0
}

func (m *Model) handleProcessReload() (tea.Model, tea.Cmd) {
	if !m.processReload.pending {
		// User Stop and failed settlement revoke continuation, but must not
		// leave the engine permanently fenced once its actual work returns.
		if m.processReload.owner.OperationID != "" {
			if m.processReloadBusy() {
				return m, processReloadTickCmd()
			}
			m.engine.ReleaseSteeringFreeze(m.processReload.owner, false)
			m.processReload.owner = llm.SteeringTransition{}
		}
		return m, nil
	}
	if !m.processReloadBusy() {
		m.processReload.state.Draft = m.expandedPastePlaceholders(m.textarea.Value())
		m.processReload.state.ShellMode = m.directShellComposerActive()
		m.processReload.state.Files = append([]FileAttachment(nil), m.files...)
		m.processReload.state.Images = append([]ImageAttachment(nil), m.images...)
		m.processReload.pending = false
		process.State("chat", "replacing", "")
		return m.cmdReload()
	}
	if time.Since(m.processReload.started) >= 10*time.Second && !m.processReload.cancelled {
		foreground := 0
		if m.streaming {
			foreground = 1
		}
		if m.mainRunManager.ActiveCount() > foreground {
			m.processReload.pending = false
			process.State("chat", "failed", "background sessions need resumable handoffs")
			return m.showFooterError("Restart deferred: background sessions are still working; nothing was cancelled.")
		}
		owner := llm.SteeringTransition{OperationID: uuid.NewString(), Fence: 1}
		pending, err := m.engine.FreezeExecutionSnapshot(owner)
		if err != nil {
			m.processReload.pending = false
			process.State("chat", "failed", err.Error())
			return m.showFooterError("Restart could not freeze execution: " + err.Error())
		}
		m.processReload.owner = owner
		m.processReload.state.Pending = pending
		m.processReload.cancelled = true
		m.processReload.state.Continue = m.streaming
		// Use ordinary interruption, including approval prompts, direct shells and
		// isolated skill/branch work. Completion accounting still gates replacement.
		_, cmd := m.cancelActiveForInterrupt()
		process.State("chat", "cancelling", "restart grace elapsed")
		return m, tea.Batch(cmd, processReloadTickCmd())
	}
	if m.processReload.cancelled && time.Since(m.processReload.started) >= 20*time.Second {
		m.processReload.pending = false
		m.processReload.state.Continue = false
		process.State("chat", "failed", "runtime work did not settle after cancellation")
		_, cmd := m.showFooterError("Restart refused: cancelled work has not finished cleanup.")
		return m, tea.Batch(cmd, processReloadTickCmd())
	}
	return m, processReloadTickCmd()
}

func (m *Model) resumeAfterProcessReload() (tea.Model, tea.Cmd) {
	state := m.processReload.state
	m.files, m.images = state.Files, state.Images
	m.restoreComposerSnapshot(composerSnapshot{body: state.Draft, shellMode: state.ShellMode})
	if !m.processResume {
		return m, nil
	}
	m.processResume = false
	notice := llm.SteeringInterruptionNotice + "\nThe process was replaced after its restart grace period. Continue the unfinished task; verify interrupted effects before repeating them. This is an internal event, not a new user message. Do not restart again."
	message := &session.Message{SessionID: m.SessionID(), Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: notice}}, TextContent: notice, CreatedAt: time.Now(), Sequence: -1}
	if m.store == nil {
		return m.showFooterError("Restart recovery requires session persistence")
	}
	if err := m.store.AddMessage(context.Background(), m.SessionID(), message); err != nil {
		return m.showFooterError(err.Error())
	}
	m.messages = append(m.messages, *message)
	for _, pending := range state.Pending {
		row := &session.Message{SessionID: m.SessionID(), Role: pending.Message.Role, Parts: pending.Message.Parts, TextContent: llm.MessageText(pending.Message), CreatedAt: time.Now(), Sequence: -1}
		if err := m.store.AddMessage(context.Background(), m.SessionID(), row); err != nil {
			return m.showFooterError(err.Error())
		}
		m.messages = append(m.messages, *row)
	}
	m.invalidateHistoryCache()
	model, cmd := m.beginUserResponse("", "", nil)
	m.files, m.images = state.Files, state.Images
	m.restoreComposerSnapshot(composerSnapshot{body: state.Draft, shellMode: state.ShellMode})
	return model, cmd
}
