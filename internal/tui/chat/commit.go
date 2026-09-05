package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/commitworkflow"
	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/terminaltext"
	"github.com/samsaffron/term-llm/internal/ui"
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
	editor.Prompt = ""
	editor.SetVirtualCursor(true)
	editorStyles := editor.Styles()
	editorStyles.Focused.Base = lipgloss.NewStyle()
	editorStyles.Focused.CursorLine = lipgloss.NewStyle()
	editorStyles.Focused.Placeholder = lipgloss.NewStyle().Foreground(m.styles.Theme().Muted)
	editorStyles.Blurred = editorStyles.Focused
	editor.SetStyles(editorStyles)
	editor.SetHeight(8)
	editor.ShowLineNumbers = false
	editor.SetWidth(m.commitBodyWidth())
	original := "/commit"
	if strings.TrimSpace(intent) != "" {
		original += " " + intent
	}
	m.commit = &CommitState{Phase: CommitLoading, Intent: intent, OriginalComposer: original, Message: editor}
	m.setTextareaValue("")
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	coordinator := m.commitMutationCoordinator
	return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
		repo, err := gitcommit.OpenWithCoordinator(ctx, dir, coordinator)
		if err != nil {
			return commitInspectMsg{err: err}
		}
		state, err := repo.Inspect(ctx)
		return commitInspectMsg{repo: repo, state: state, err: err}
	})
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
				return m, state.Message.Focus()
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
			if value.state.TotalUnstaged == 0 && value.state.TotalUntracked == 0 {
				// Everything and staged-only are identical. An explicit scope
				// request still needs planning, but not a redundant staging choice.
				if strings.TrimSpace(state.Intent) != "" {
					return m, m.startCommitScopePlan()
				}
				return m, m.startCommitDraft()
			}
			state.Phase = CommitChoosing
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
			return m, state.Message.Focus()
		}
		state.Generated = value.message
		state.Message.SetValue(value.message)
		state.Message.MoveToBegin()
		state.Dirty = false
		state.NeedsReview = false
		state.ConfirmOverwrite = false
		state.Error = ""
		return m, state.Message.Focus()
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
			return m, s.Message.Focus()
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
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		state, err := s.Repo.Inspect(ctx)
		return commitInspectMsg{repo: s.Repo, state: state, err: err}
	})
}
func (m *Model) startCommitStage(mode gitcommit.StageMode, paths []string) tea.Cmd {
	s := m.commit
	s.Phase = CommitStaging
	s.Error = ""
	ctx := m.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	request := gitcommit.StageRequest{Mode: mode, Paths: paths, StatusToken: s.Status.StatusToken}
	expected := s.Status.Fingerprint
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		next, err := s.Repo.Stage(ctx, request, expected)
		return commitStageMsg{state: next, err: err}
	})
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
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		proposal, meta, err := commitworkflow.New().PlanScope(ctx, request)
		return commitScopeMsg{proposal: proposal, meta: meta, err: err}
	})
}
func (m *Model) startCommitDraft() tea.Cmd {
	s := m.commit
	s.Phase = CommitDrafting
	s.Error = ""
	if m.childRunner == nil {
		s.Phase = CommitEditing
		s.Error = "Commit message agent is unavailable; write a message manually."
		return s.Message.Focus()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	request := commitworkflow.Request{ParentSessionID: m.SessionID(), CheckoutDir: s.Repo.CheckoutRoot(), AgentName: m.commitAgentName(), Intent: s.Intent, ScopeSummary: s.ScopeSummary, ExpectedFingerprint: s.Status.Fingerprint, Runner: m.childRunner}
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		message, meta, err := commitworkflow.New().DraftMessage(ctx, request)
		return commitDraftMsg{message: message, meta: meta, err: err}
	})
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
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		result, err := s.Repo.Commit(context.Background(), message, expected)
		return commitDoneMsg{result: result, err: err}
	})
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

// commitBodyWidth leaves room for the border, padding, and terminal margins.
// An 80-cell editor accommodates conventional 72–76-column hard-wrapped bodies
// plus the textarea cursor cell, avoiding a second wrap of their final words.
func (m *Model) commitBodyWidth() int {
	if m.width <= 0 {
		return 80
	}
	if m.width < 12 {
		return max(1, m.width-2)
	}
	return min(80, m.width-10)
}

func (m *Model) renderCommit() string {
	s := m.commit
	if s == nil {
		return ""
	}
	width := m.commitBodyWidth()
	theme := m.styles.Theme()
	title := lipgloss.NewStyle().Bold(true).Foreground(theme.Primary)
	muted := lipgloss.NewStyle().Foreground(theme.Muted)
	selected := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	// Reserve chrome and help before allocating space to the editor or file list.
	height := m.height
	if height <= 0 {
		height = 32
	}
	paddingY, paddingX := 1, 2
	if height < 16 {
		paddingY = 0
	}
	if m.width > 0 && m.width < 12 {
		paddingX = 0
	}
	bodyHeight := max(1, height-2-2*paddingY)
	border := lipgloss.RoundedBorder()
	if (m.width > 0 && m.width < 3) || height < 3 {
		border = lipgloss.Border{}
		paddingY, paddingX = 0, 0
		width = max(1, m.width)
		bodyHeight = height
	}
	line := func(text string) string { return ansi.Truncate(terminaltext.SanitizeSingleLine(text), width, "…") }
	rows := []string{title.Render("Git commit")}
	branch := s.Status.Branch
	if s.Status.Detached {
		branch = "detached HEAD"
	}
	if branch != "" {
		rows = append(rows, line(branch))
	}
	rows = append(rows, muted.Render(line(fmt.Sprintf("%d staged · %d unstaged · %d untracked", s.Status.TotalStaged, s.Status.TotalUnstaged, s.Status.TotalUntracked))))
	if strings.TrimSpace(s.Intent) != "" {
		rows = append(rows, muted.Render(line("Request: "+s.Intent)))
	}
	rows = append(rows, "")
	help := "esc cancel"
	var body []string
	switch s.Phase {
	case CommitLoading:
		body = []string{m.spinner.View() + " Inspecting checkout…"}
	case CommitPlanning:
		body = []string{m.spinner.View() + " Planning which files to include…"}
	case CommitStaging:
		body = []string{m.spinner.View() + " Staging selected changes…"}
		help = "Please wait · staging cannot be cancelled"
	case CommitDrafting:
		body = []string{m.spinner.View() + " Drafting commit message…"}
	case CommitCommitting:
		body = []string{m.spinner.View() + " Committing changes…", muted.Render("Running Git hooks and signing")}
		help = "Please wait · commit cannot be cancelled"
	case CommitChoosing:
		body = []string{"You already have staged changes.", "", selected.Render("e") + "  Commit everything", selected.Render("s") + "  Commit staged changes only"}
		if strings.TrimSpace(s.Intent) != "" {
			body = append(body, selected.Render("f")+"  Follow request and review files")
		}
	case CommitReviewing:
		help = "↑/↓ move · space toggle · enter apply\na all files · r retry · esc cancel"
	case CommitEditing:
		help = "ctrl+s commit · ctrl+r regenerate\nctrl+f review files · esc cancel"
	case CommitError:
		body = []string{"Refresh the checkout before continuing."}
		help = "r refresh · esc cancel"
		if !s.NeedsReview {
			body = []string{"Refresh the checkout or write a message manually."}
			help = "r refresh · m write message · esc cancel"
		}
	}
	// Wrap between shortcuts, never between a key and its action.
	var helpLines []string
	for _, group := range strings.Split(help, "\n") {
		current := ""
		for _, hint := range strings.Split(group, " · ") {
			if current != "" && lipgloss.Width(current+" · "+hint) > width {
				helpLines = append(helpLines, ansi.Truncate(current, width, "…"))
				current = ""
			}
			if current != "" {
				current += " · "
			}
			current += hint
		}
		helpLines = append(helpLines, ansi.Truncate(current, width, "…"))
	}

	var notices []string
	if s.Phase == CommitReviewing && s.Proposal.Summary != "" {
		notices = append(notices, muted.Render(line(s.Proposal.Summary)))
	}
	if s.Agent != "" {
		notices = append(notices, muted.Render(line("Agent: "+s.Agent+commitAgentSourceLabel(s.AgentSource))))
	}
	optionalNotices := len(notices)
	if s.Error != "" {
		// Keep confirmations readable without allowing hook/agent errors to grow the panel.
		errorLines := strings.Split(ansi.Wrap(terminaltext.SanitizeSingleLine(s.Error), width, ""), "\n")
		if len(errorLines) > 3 {
			errorLines = append(errorLines[:2], ansi.Truncate(errorLines[2], max(1, width-1), "")+"…")
		}
		for _, text := range errorLines {
			notices = append(notices, lipgloss.NewStyle().Foreground(theme.Warning).Render(text))
		}
	}
	// Discard optional metadata before sacrificing the active editor/list or help.
	minimumBody := len(body)
	if s.Phase == CommitEditing {
		minimumBody = 2
	}
	if s.Phase == CommitReviewing {
		minimumBody = 3
	}
	for len(rows)+len(notices)+minimumBody+len(helpLines)+1 > bodyHeight && len(rows) > 1 {
		rows = rows[:len(rows)-1]
	}
	for len(rows)+len(notices)+minimumBody+len(helpLines)+1 > bodyHeight && len(notices) > 0 {
		if optionalNotices > 0 {
			notices = notices[1:]
			optionalNotices--
		} else {
			notices = notices[:len(notices)-1]
			if len(notices) > 0 {
				last := len(notices) - 1
				notices[last] = lipgloss.NewStyle().Foreground(theme.Warning).Render(ansi.Truncate(ansi.Strip(notices[last]), max(0, width-1), "") + "…")
			}
		}
	}
	available := max(1, bodyHeight-len(rows)-len(notices)-len(helpLines)-1)
	switch s.Phase {
	case CommitEditing:
		s.Message.SetWidth(width)
		s.Message.SetHeight(max(1, min(8, available-1)))
		body = append([]string{muted.Render("Commit message")}, strings.Split(s.Message.View(), "\n")...)
	case CommitReviewing:
		paths := m.commitSelectablePaths()
		start, end := ui.VisibleRange(len(paths), s.Cursor, max(1, available-2))
		body = []string{muted.Render(line(fmt.Sprintf("Files to include · %d selected", countSelectedCommitPaths(paths, s.Selected))))}
		for i := start; i < end; i++ {
			mark := "○"
			if s.Selected[paths[i]] {
				mark = "●"
			}
			text := line("  " + mark + " " + paths[i])
			if i == s.Cursor {
				text = selected.Render(line("❯ " + mark + " " + paths[i]))
			}
			body = append(body, text)
		}
		if len(paths) == 0 {
			body = append(body, muted.Render("No changed files"))
		} else {
			body = append(body, muted.Render(line(fmt.Sprintf("%d–%d of %d files", start+1, end, len(paths)))))
		}
	}
	for _, text := range body {
		rows = append(rows, ansi.Truncate(text, width, "…"))
	}
	rows = append(rows, notices...)
	// Reserve the footer even on short terminals. If the entire help cannot fit,
	// retain its first action and final cancellation/wait hint where possible.
	if len(helpLines) >= bodyHeight {
		if bodyHeight == 1 {
			helpLines = helpLines[len(helpLines)-1:]
		} else {
			helpLines = append(helpLines[:bodyHeight-1], helpLines[len(helpLines)-1])
		}
	}
	room := bodyHeight - len(helpLines)
	if len(rows) > room {
		rows = rows[:room]
	}
	if len(rows) < room {
		rows = append(rows, "")
	}
	for _, hint := range helpLines {
		rows = append(rows, muted.Render(hint))
	}
	// Truncate every row before Lip Gloss can soft-wrap it beyond the height budget.
	for i := range rows {
		rows[i] = ansi.Truncate(rows[i], width, "…")
	}
	frameWidth := width + 2*paddingX
	if border.Top != "" {
		frameWidth += 2
	}
	return lipgloss.NewStyle().Border(border).BorderForeground(theme.Border).
		Padding(paddingY, paddingX).Width(frameWidth).Render(strings.Join(rows, "\n"))
}

func countSelectedCommitPaths(paths []string, selected map[string]bool) int {
	count := 0
	for _, path := range paths {
		if selected[path] {
			count++
		}
	}
	return count
}

func commitAgentSourceLabel(source string) string {
	if source == "" {
		return ""
	}
	return " (" + source + ")"
}

func (m *Model) commitBusy() bool {
	if m.commit == nil {
		return false
	}
	switch m.commit.Phase {
	case CommitLoading, CommitPlanning, CommitStaging, CommitDrafting, CommitCommitting:
		return true
	default:
		return false
	}
}
