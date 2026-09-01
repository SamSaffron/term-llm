package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/commitworkflow"
	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

type CommitPhase string

const (
	CommitLoading    CommitPhase = "loading"
	CommitChoosing   CommitPhase = "choosing_scope"
	CommitPlanning   CommitPhase = "planning_scope"
	CommitReviewing  CommitPhase = "reviewing_scope"
	CommitStaging    CommitPhase = "staging"
	CommitDrafting   CommitPhase = "drafting_message"
	CommitEditing    CommitPhase = "editing"
	CommitCommitting CommitPhase = "committing"
	CommitError      CommitPhase = "error"
)

type CommitState struct {
	Phase             CommitPhase
	Intent            string
	OriginalComposer  string
	Repo              *gitcommit.Repository
	Status            gitcommit.RepositoryState
	Proposal          commitworkflow.ScopeProposal
	Selected          map[string]bool
	Cursor            int
	Message           textarea.Model
	Generated         string
	Dirty             bool
	ConfirmRegenerate bool
	ConfirmOverwrite  bool
	NeedsReview       bool
	Error             string
	Info              string
	Agent             string
	AgentSource       string
	ScopeSummary      string
	RetryAfterFailure bool
	RetryError        string
	cancel            context.CancelFunc
}
type commitInspectMsg struct {
	repo  *gitcommit.Repository
	state gitcommit.RepositoryState
	err   error
}
type commitStageMsg struct {
	state gitcommit.RepositoryState
	err   error
}
type commitScopeMsg struct {
	proposal commitworkflow.ScopeProposal
	meta     commitworkflow.ChildRunMetadata
	err      error
}
type commitDraftMsg struct {
	message string
	meta    commitworkflow.ChildRunMetadata
	err     error
}
type commitDoneMsg struct {
	result gitcommit.CommitResult
	err    error
}

func (m *Model) SetCommitMutationCoordinator(coordinator gitcommit.MutationCoordinator) {
	m.commitMutationCoordinator = coordinator
}
func (m *Model) cmdCommit(intent string) (tea.Model, tea.Cmd) {
	if m.commit != nil || m.streaming || m.activeSkillRunCount() > 0 || m.directShellRun != nil || m.worktreeOperationBusy() {
		return m, m.footerErrorCmd("Cannot start /commit while the checkout is busy.")
	}
	if m.sess == nil {
		return m, m.footerErrorCmd("Start or resume a session before using /commit.")
	}
	dir := m.effectiveWorkingDir()
	if strings.TrimSpace(dir) == "" {
		return m, m.footerErrorCmd("The active session has no working directory.")
	}
	editor := textarea.New()
	editor.Placeholder = "Commit subject\n\nOptional body"
	editor.SetWidth(maxInt(40, m.width-12))
	editor.SetHeight(8)
	editor.ShowLineNumbers = false
	original := "/commit"
	if strings.TrimSpace(intent) != "" {
		original += " " + intent
	}
	m.commit = &CommitState{Phase: CommitLoading, Intent: intent, OriginalComposer: original, Message: editor, Info: "Inspecting the active checkout…"}
	m.setTextareaValue("")
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	coordinator := m.commitMutationCoordinator
	return m, func() tea.Msg {
		repo, err := gitcommit.OpenWithCoordinator(ctx, dir, coordinator)
		if err != nil {
			return commitInspectMsg{err: err}
		}
		state, err := repo.Inspect(ctx)
		return commitInspectMsg{repo: repo, state: state, err: err}
	}
}
func (m *Model) footerErrorCmd(text string) tea.Cmd { _, cmd := m.showFooterError(text); return cmd }

func (m *Model) updateCommit(msg tea.Msg) (tea.Model, tea.Cmd) {
	state := m.commit
	if state == nil {
		return m, nil
	}
	switch value := msg.(type) {
	case commitInspectMsg:
		if value.err != nil {
			if state.RetryAfterFailure || (state.NeedsReview && strings.TrimSpace(state.Message.Value()) != "") {
				state.RetryAfterFailure = false
				state.Phase = CommitError
				state.Error = "Could not refresh repository status: " + value.err.Error() + ". The commit message remains preserved."
				return m, nil
			}
			m.setTextareaValue(state.OriginalComposer)
			m.commit = nil
			return m, m.footerErrorCmd(value.err.Error())
		}
		state.Repo = value.repo
		reviewed := state.Status.Fingerprint
		state.Status = value.state
		if state.RetryAfterFailure {
			state.RetryAfterFailure = false
			if gitcommit.FingerprintsEqual(reviewed, value.state.Fingerprint) && len(value.state.Staged) > 0 {
				state.Phase = CommitEditing
				state.NeedsReview = false
				state.Error = state.RetryError + " The staged state is unchanged; the message was preserved for retry."
				state.RetryError = ""
				state.Message.Focus()
				return m, textarea.Blink
			}
			state.NeedsReview = true
			state.RetryError = ""
		}
		if state.NeedsReview {
			state.Phase = CommitReviewing
			state.Cursor = 0
			state.Selected = map[string]bool{}
			for _, change := range value.state.Staged {
				state.Selected[change.Path] = true
			}
			state.Error = "Repository state refreshed. Review the files before replacing the preserved message."
			return m, nil
		}
		if len(value.state.Staged) > 0 {
			state.Phase = CommitChoosing
			state.Info = "Choose deliberately; no staging choice is preselected."
			return m, nil
		}
		if strings.TrimSpace(state.Intent) != "" {
			return m, m.startCommitScopePlan()
		}
		return m, m.startCommitStage(gitcommit.StageAll, nil)
	case commitStageMsg:
		if value.err != nil {
			state.Status = value.state
			state.Phase = CommitError
			state.NeedsReview = true
			state.Error = value.err.Error() + " Refresh and review repository status before continuing."
			return m, nil
		}
		state.Status = value.state
		return m, m.startCommitDraft()
	case commitScopeMsg:
		state.cancel = nil
		state.Agent = value.meta.AgentName
		state.AgentSource = value.meta.AgentSource
		if value.err != nil {
			state.Phase = CommitReviewing
			state.Error = "Scope planning failed: " + value.err.Error()
			state.Selected = map[string]bool{}
			return m, nil
		}
		state.Proposal = value.proposal
		state.ScopeSummary = value.proposal.Summary
		if value.proposal.Mode == commitworkflow.ScopeAll {
			return m, m.startCommitStage(gitcommit.StageAll, nil)
		}
		state.Selected = map[string]bool{}
		if value.proposal.Mode == commitworkflow.ScopeSelected {
			for _, path := range value.proposal.IncludePaths {
				state.Selected[path] = true
			}
		}
		state.Phase = CommitReviewing
		state.Cursor = 0
		if value.proposal.Mode == commitworkflow.ScopeNeedsManual {
			state.Error = "The request cannot be represented safely at whole-file granularity. Choose files manually, commit everything, retry, or cancel."
		}
		return m, nil
	case commitDraftMsg:
		state.cancel = nil
		state.Agent = value.meta.AgentName
		state.AgentSource = value.meta.AgentSource
		state.Phase = CommitEditing
		if value.err != nil {
			state.Error = "Message generation failed; write a message manually: " + value.err.Error()
			state.Message.Focus()
			return m, textarea.Blink
		}
		state.Generated = value.message
		state.Message.SetValue(value.message)
		state.Dirty = false
		state.NeedsReview = false
		state.ConfirmOverwrite = false
		state.Error = ""
		state.Message.Focus()
		return m, textarea.Blink
	case commitDoneMsg:
		if value.err != nil {
			state.Phase = CommitError
			state.NeedsReview = true
			state.RetryError = value.err.Error()
			if gitcommit.IsKind(value.err, gitcommit.ErrUncertain) {
				state.Error = value.err.Error() + " The outcome is uncertain; do not retry blindly. Refresh and inspect the repository."
				return m, nil
			}
			if gitcommit.IsKind(value.err, gitcommit.ErrStale) {
				state.RetryError = ""
				state.Error = value.err.Error() + " Refresh and review the repository before retrying."
				return m, nil
			}
			state.RetryAfterFailure = true
			state.Error = value.err.Error() + " Refreshing repository status before retrying with the preserved message."
			return m, m.startCommitInspect()
		}
		m.commit = nil
		text := fmt.Sprintf("Committed %s %s", value.result.ShortOID, value.result.Subject)
		if value.result.TreeChanged {
			return m, m.footerWarningCmd(text + " — warning: a hook changed the committed tree after review.")
		}
		return m, m.footerSuccessCmd(text)
	case tea.PasteMsg:
		if state.Phase == CommitEditing {
			var cmd tea.Cmd
			state.Message, cmd = state.Message.Update(value)
			state.Dirty = state.Message.Value() != state.Generated
			return m, cmd
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.handleCommitKey(value)
	}
	return m, nil
}
func (m *Model) footerSuccessCmd(text string) tea.Cmd {
	_, cmd := m.showFooterSuccess(text)
	return cmd
}
func (m *Model) footerWarningCmd(text string) tea.Cmd {
	_, cmd := m.showFooterWarning(text)
	return cmd
}

func (m *Model) handleCommitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.commit
	if s == nil {
		return m, nil
	}
	key := msg.String()
	if key == "esc" {
		if s.Phase == CommitCommitting || s.Phase == CommitStaging {
			if s.Phase == CommitCommitting {
				s.Error = "Git commit is already running and must reach a known result."
			} else {
				s.Error = "Git staging is already running and must reach a known result."
			}
			return m, nil
		}
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
			s.Error = "Cancelling agent run…"
			return m, nil
		}
		persisted := s.Phase != CommitLoading && s.Phase != CommitChoosing && s.Phase != CommitPlanning
		m.commit = nil
		if persisted {
			return m, m.footerErrorCmd("Commit cancelled. Any confirmed staging changes remain in the index.")
		}
		return m, nil
	}
	switch s.Phase {
	case CommitChoosing:
		switch key {
		case "e", "E":
			if !m.confirmCommitMessageOverwrite("E") {
				return m, nil
			}
			return m, m.startCommitStage(gitcommit.StageAll, nil)
		case "s", "S":
			if !m.confirmCommitMessageOverwrite("S") {
				return m, nil
			}
			return m, m.startCommitDraft()
		case "f", "F":
			if strings.TrimSpace(s.Intent) != "" {
				if !m.confirmCommitMessageOverwrite("F") {
					return m, nil
				}
				return m, m.startCommitScopePlan()
			}
		}
	case CommitReviewing:
		paths := m.commitSelectablePaths()
		if len(paths) == 0 {
			s.Cursor = 0
		} else if s.Cursor >= len(paths) {
			s.Cursor = len(paths) - 1
		} else if s.Cursor < 0 {
			s.Cursor = 0
		}
		switch key {
		case "up", "k":
			if s.Cursor > 0 {
				s.Cursor--
			}
		case "down", "j":
			if s.Cursor < len(paths)-1 {
				s.Cursor++
			}
		case " ":
			if len(paths) > 0 {
				s.Selected[paths[s.Cursor]] = !s.Selected[paths[s.Cursor]]
			}
		case "enter":
			if !m.confirmCommitMessageOverwrite("Enter") {
				return m, nil
			}
			var selected []string
			for _, p := range paths {
				if s.Selected[p] {
					selected = append(selected, p)
				}
			}
			return m, m.startCommitStage(gitcommit.StageExactSelection, selected)
		case "a", "A":
			if !m.confirmCommitMessageOverwrite("A") {
				return m, nil
			}
			return m, m.startCommitStage(gitcommit.StageAll, nil)
		case "r", "R":
			if !m.confirmCommitMessageOverwrite("R") {
				return m, nil
			}
			return m, m.startCommitScopePlan()
		}
	case CommitEditing:
		if key == "ctrl+s" {
			return m, m.startCommitCommit()
		}
		if key == "ctrl+r" {
			if s.Dirty && !s.ConfirmRegenerate {
				s.ConfirmRegenerate = true
				s.Error = "Press Ctrl+R again to replace your edited message."
				return m, nil
			}
			s.ConfirmRegenerate = false
			return m, m.startCommitDraft()
		}
		if key == "ctrl+f" {
			s.Phase = CommitReviewing
			s.Cursor = 0
			s.NeedsReview = strings.TrimSpace(s.Message.Value()) != ""
			s.ConfirmOverwrite = false
			s.Selected = map[string]bool{}
			for _, change := range s.Status.Staged {
				s.Selected[change.Path] = true
			}
			return m, nil
		}
		var cmd tea.Cmd
		s.Message, cmd = s.Message.Update(msg)
		s.Dirty = s.Message.Value() != s.Generated
		return m, cmd
	case CommitError:
		if key == "r" || key == "R" {
			return m, m.startCommitInspect()
		}
		if (key == "m" || key == "M") && !s.NeedsReview {
			s.Phase = CommitEditing
			s.Message.Focus()
			return m, textarea.Blink
		}
	}
	return m, nil
}

func (m *Model) confirmCommitMessageOverwrite(action string) bool {
	s := m.commit
	if s == nil || !s.NeedsReview || strings.TrimSpace(s.Message.Value()) == "" {
		return true
	}
	if !s.ConfirmOverwrite {
		s.ConfirmOverwrite = true
		s.Error = fmt.Sprintf("This will regenerate and replace the preserved message. Press %s again to continue.", action)
		return false
	}
	s.ConfirmOverwrite = false
	s.NeedsReview = false
	return true
}

func (m *Model) startCommitInspect() tea.Cmd {
	s := m.commit
	s.Phase = CommitLoading
	s.Error = ""
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		state, err := s.Repo.Inspect(ctx)
		return commitInspectMsg{repo: s.Repo, state: state, err: err}
	}
}
func (m *Model) startCommitStage(mode gitcommit.StageMode, paths []string) tea.Cmd {
	s := m.commit
	s.Phase = CommitStaging
	s.Error = ""
	s.Info = "Staging selected content…"
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	request := gitcommit.StageRequest{Mode: mode, Paths: paths, StatusToken: s.Status.StatusToken}
	expected := s.Status.Fingerprint
	return func() tea.Msg {
		next, err := s.Repo.Stage(ctx, request, expected)
		return commitStageMsg{state: next, err: err}
	}
}
func (m *Model) startCommitScopePlan() tea.Cmd {
	s := m.commit
	if m.childRunner == nil {
		s.Phase = CommitReviewing
		s.Error = "Commit scope agent is unavailable. Select whole files manually or commit everything."
		s.Selected = map[string]bool{}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.Phase = CommitPlanning
	s.Error = ""
	request := commitworkflow.Request{ParentSessionID: m.SessionID(), CheckoutDir: s.Repo.CheckoutRoot(), AgentName: m.commitAgentName(), Intent: s.Intent, ExpectedFingerprint: s.Status.Fingerprint, ExpectedStatusToken: s.Status.StatusToken, Runner: m.childRunner}
	return func() tea.Msg {
		proposal, meta, err := commitworkflow.New().PlanScope(ctx, request)
		return commitScopeMsg{proposal: proposal, meta: meta, err: err}
	}
}
func (m *Model) startCommitDraft() tea.Cmd {
	s := m.commit
	s.Phase = CommitDrafting
	s.Error = ""
	if m.childRunner == nil {
		s.Phase = CommitEditing
		s.Error = "Commit message agent is unavailable; write a message manually."
		s.Message.Focus()
		return textarea.Blink
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	request := commitworkflow.Request{ParentSessionID: m.SessionID(), CheckoutDir: s.Repo.CheckoutRoot(), AgentName: m.commitAgentName(), Intent: s.Intent, ScopeSummary: s.ScopeSummary, ExpectedFingerprint: s.Status.Fingerprint, Runner: m.childRunner}
	return func() tea.Msg {
		message, meta, err := commitworkflow.New().DraftMessage(ctx, request)
		return commitDraftMsg{message: message, meta: meta, err: err}
	}
}
func (m *Model) startCommitCommit() tea.Cmd {
	s := m.commit
	message := s.Message.Value()
	if strings.TrimSpace(message) == "" || strings.TrimSpace(strings.SplitN(message, "\n", 2)[0]) == "" {
		s.Error = "A non-empty subject is required."
		return nil
	}
	s.Phase = CommitCommitting
	s.Error = ""
	expected := s.Status.Fingerprint
	return func() tea.Msg {
		result, err := s.Repo.Commit(context.Background(), message, expected)
		return commitDoneMsg{result: result, err: err}
	}
}
func (m *Model) commitAgentName() string {
	if m.config == nil {
		return "commit-message"
	}
	return m.config.Commit.EffectiveMessageAgent()
}
func (m *Model) commitSelectablePaths() []string {
	if m.commit == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, list := range [][]gitcommit.Change{m.commit.Status.Staged, m.commit.Status.Unstaged, m.commit.Status.Untracked} {
		for _, change := range list {
			if _, ok := seen[change.Path]; !ok {
				seen[change.Path] = struct{}{}
				paths = append(paths, change.Path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

func (m *Model) renderCommit() string {
	s := m.commit
	if s == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("Git commit\n\n")
	fmt.Fprintf(&b, "Branch: %s", s.Status.Branch)
	if s.Status.Detached {
		b.WriteString("detached HEAD")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Staged %d · Unstaged %d · Untracked %d\n", s.Status.TotalStaged, s.Status.TotalUnstaged, s.Status.TotalUntracked)
	switch s.Phase {
	case CommitLoading, CommitPlanning, CommitStaging, CommitDrafting, CommitCommitting:
		b.WriteString("\n" + s.Info)
		if s.Phase == CommitPlanning {
			b.WriteString("Planning whole-file scope…")
		}
		if s.Phase == CommitDrafting {
			b.WriteString("Drafting message…")
		}
		if s.Phase == CommitCommitting {
			b.WriteString("Running Git hooks and signing; this cannot be cancelled…")
		}
	case CommitChoosing:
		b.WriteString("\nExisting staged changes require an explicit choice:\n  [E] Commit everything\n  [S] Commit staged only\n")
		if strings.TrimSpace(s.Intent) != "" {
			b.WriteString("  [F] Follow request with reviewed scope\n")
		}
	case CommitReviewing:
		b.WriteString("\nIncluded in this commit (Space toggles):\n")
		paths := m.commitSelectablePaths()
		for i, path := range paths {
			marker := " "
			if s.Selected[path] {
				marker = "x"
			}
			cursor := " "
			if i == s.Cursor {
				cursor = ">"
			}
			fmt.Fprintf(&b, "%s [%s] %s\n", cursor, marker, terminaltext.SanitizeSingleLine(path))
		}
		if s.Proposal.Summary != "" {
			b.WriteString("\nPlanner: " + terminaltext.SanitizeSingleLine(s.Proposal.Summary) + "\n")
		}
		b.WriteString("Enter apply · A everything · R retry · Esc cancel\n")
	case CommitEditing:
		b.WriteString("\nEditable message:\n")
		b.WriteString(s.Message.View())
		b.WriteString("\nCtrl+S commit · Ctrl+R regenerate · Ctrl+F review files · Esc cancel\n")
	case CommitError:
		if s.NeedsReview {
			b.WriteString("\nPress R to refresh and review the repository.\n")
		} else {
			b.WriteString("\nPress R to refresh or M to enter a message manually.\n")
		}
	}
	if s.Agent != "" {
		fmt.Fprintf(&b, "\nAgent: %s (%s)\n", terminaltext.SanitizeSingleLine(s.Agent), terminaltext.SanitizeSingleLine(s.AgentSource))
	}
	if s.Error != "" {
		b.WriteString("\nError: " + terminaltext.SanitizeSingleLine(s.Error) + "\n")
	}
	return b.String()
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
