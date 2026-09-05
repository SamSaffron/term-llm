package chat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func commitTUIGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func commitTUIModel(t *testing.T) (*Model, string) {
	t.Helper()
	dir := t.TempDir()
	commitTUIGit(t, dir, "init", "-q")
	commitTUIGit(t, dir, "config", "user.name", "Test")
	commitTUIGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitTUIGit(t, dir, "add", "-A")
	commitTUIGit(t, dir, "commit", "-qm", "base")
	sess := &session.Session{ID: "tui-commit", CWD: dir}
	model := New(&config.Config{}, llm.NewMockProvider("mock"), nil, "", "", nil, 0, false, false, false, nil, "", "", false, "", nil, sess, false, nil, false, true, "", "", false)
	model.SetRootContext(context.Background())
	return model, dir
}
func applyCommitCmd(t *testing.T, m *Model, cmd tea.Cmd) *Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			m = applyCommitCmd(t, m, child)
		}
		return m
	}
	if _, ok := msg.(spinner.TickMsg); ok {
		return m
	}
	updated, next := m.Update(msg)
	m = updated.(*Model)
	if next != nil && m.commit != nil && m.commit.Phase != CommitEditing {
		return applyCommitCmd(t, m, next)
	}
	return m
}

func TestCommitCommandRegisteredAndComPrefixAmbiguous(t *testing.T) {
	found := false
	for _, command := range AllCommands() {
		if command.Name == "commit" {
			found = true
		}
	}
	if !found {
		t.Fatal("commit command missing")
	}
	matches := FilterCommands("com")
	var names []string
	for _, match := range matches {
		if strings.HasPrefix(match.Name, "com") {
			names = append(names, match.Name)
		}
	}
	if len(names) != 2 || names[0] != "commit" || names[1] != "compact" {
		t.Fatalf("/com matches = %v", names)
	}
}

func TestCommitEmptyIndexAutomaticallyStagesAllAndOffersManualMessage(t *testing.T) {
	m, dir := commitTUIModel(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.ExecuteCommand("/commit")
	m = updated.(*Model)
	m = applyCommitCmd(t, m, cmd)
	if m.commit == nil || m.commit.Phase != CommitEditing {
		t.Fatalf("state=%+v", m.commit)
	}
	if got := commitTUICaptureGit(t, dir, "diff", "--cached", "--name-only"); got != "base.txt" {
		t.Fatalf("staged=%q", got)
	}
	if !strings.Contains(m.commit.Error, "write a message manually") {
		t.Fatalf("error=%q", m.commit.Error)
	}
}

func TestCommitExistingIndexRequiresExplicitChoice(t *testing.T) {
	m, dir := commitTUIModel(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("staged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitTUIGit(t, dir, "add", "base.txt")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.ExecuteCommand("/commit only base")
	m = updated.(*Model)
	m = applyCommitCmd(t, m, cmd)
	if m.commit == nil || m.commit.Phase != CommitChoosing {
		t.Fatalf("state=%+v", m.commit)
	}
	updated, cmd = m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = updated.(*Model)
	m = applyCommitCmd(t, m, cmd)
	if m.commit.Phase != CommitEditing {
		t.Fatalf("phase=%s error=%s", m.commit.Phase, m.commit.Error)
	}
	if got := commitTUICaptureGit(t, dir, "diff", "--cached", "--name-only"); got != "base.txt" {
		t.Fatalf("staged-only changed index: %q", got)
	}
}
func commitTUICaptureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCommitPanelFloatsOverConversation(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.width, m.height = 100, 32
	m.commit = &CommitState{Phase: CommitChoosing}
	background := strings.Repeat(strings.Repeat("·", m.width)+"\n", m.height-1) + strings.Repeat("·", m.width)
	got := ansi.Strip(m.overlayAltScreenPanels(background, footerLayout{}))
	if lipgloss.Height(got) != m.height || lipgloss.Width(got) != m.width {
		t.Fatalf("overlay dimensions: %dx%d", lipgloss.Width(got), lipgloss.Height(got))
	}
	lines := strings.Split(got, "\n")
	if lines[0] != strings.Repeat("·", m.width) || lines[len(lines)-1] != strings.Repeat("·", m.width) {
		t.Fatal("background was replaced")
	}
	panel := ansi.Strip(m.renderCommit())
	x, y := (m.width-lipgloss.Width(panel))/2, (m.height-lipgloss.Height(panel))/2
	if !strings.HasPrefix(lines[y], strings.Repeat("·", x)+"╭") {
		t.Fatalf("panel is not centered: %q", lines[y])
	}
	if !strings.Contains(got, "Commit staged changes only") {
		t.Fatal("missing staging choice")
	}
}

func TestCommitPanelFitsAndKeepsSelectedFileVisible(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.commit = &CommitState{Phase: CommitReviewing, Selected: map[string]bool{}, Cursor: 99}
	for i := 0; i < 100; i++ {
		m.commit.Status.Untracked = append(m.commit.Status.Untracked, gitcommit.Change{Path: fmt.Sprintf("file-%03d.txt", i)})
	}
	for _, size := range [][2]int{{100, 32}, {80, 24}, {40, 20}} {
		m.width, m.height = size[0], size[1]
		panel := ansi.Strip(m.renderCommit())
		if lipgloss.Width(panel) > m.width || lipgloss.Height(panel) > m.height {
			t.Fatalf("panel overflows %v: %dx%d", size, lipgloss.Width(panel), lipgloss.Height(panel))
		}
		for _, want := range []string{"❯ ○ file-099.txt", "100 of 100 files", "enter apply", "esc cancel"} {
			if !strings.Contains(panel, want) {
				t.Fatalf("%v missing %q:\n%s", size, want, panel)
			}
		}
	}
}

func TestCommitEditorResizesWithoutLosingDraft(t *testing.T) {
	m, _ := commitTUIModel(t)
	editor := textarea.New()
	draft := "Keep my subject\n\nAnd my edited body."
	editor.SetValue(draft)
	m.commit = &CommitState{Phase: CommitEditing, Message: editor}
	for _, size := range [][2]int{{100, 32}, {40, 20}, {80, 24}} {
		m.width, m.height = size[0], size[1]
		panel := ansi.Strip(m.renderCommit())
		if lipgloss.Width(panel) > m.width || lipgloss.Height(panel) > m.height {
			t.Fatalf("editor overflows %v", size)
		}
		if m.commit.Message.Value() != draft {
			t.Fatal("resize lost draft")
		}
		if !strings.Contains(panel, "ctrl+s commit") || !strings.Contains(panel, "esc cancel") {
			t.Fatalf("missing editor controls:\n%s", panel)
		}
	}
}

func TestCommitProgressMatchesPhase(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.width, m.height = 80, 24
	m.commit = &CommitState{Phase: CommitDrafting}
	panel := ansi.Strip(m.renderCommit())
	if strings.Contains(panel, "Inspecting") || !strings.Contains(panel, "Drafting commit message…") {
		t.Fatalf("stale progress: %s", panel)
	}
}

func TestCommitPanelFitsTinyTerminalsInEveryPhase(t *testing.T) {
	m, _ := commitTUIModel(t)
	phases := []CommitPhase{CommitLoading, CommitChoosing, CommitPlanning, CommitReviewing, CommitStaging, CommitDrafting, CommitEditing, CommitCommitting, CommitError}
	for _, phase := range phases {
		for _, size := range [][2]int{{80, 12}, {40, 12}, {20, 10}, {10, 6}, {5, 4}, {2, 2}, {1, 1}} {
			m.width, m.height = size[0], size[1]
			m.commit = &CommitState{Phase: phase, Message: textarea.New(), Agent: "commit-message", AgentSource: "builtin", Intent: "A detailed request", Error: strings.Repeat("Long error detail. ", 50)}
			panel := ansi.Strip(m.renderCommit())
			if lipgloss.Width(panel) > m.width || lipgloss.Height(panel) > m.height {
				t.Fatalf("%s %v overflows (%dx%d):\n%s", phase, size, lipgloss.Width(panel), lipgloss.Height(panel), panel)
			}
			if size[0] >= 40 && phase == CommitEditing {
				for _, hint := range []string{"ctrl+s commit", "ctrl+r regenerate", "ctrl+f review files", "esc cancel"} {
					if !strings.Contains(panel, hint) {
						t.Fatalf("missing %q at %v:\n%s", hint, size, panel)
					}
				}
			}
		}
	}
}

func TestCommitPanelLongErrorIsCapped(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.width, m.height = 80, 32
	m.commit = &CommitState{Phase: CommitError, NeedsReview: true, Error: strings.Repeat("detail ", 100), Agent: "commit-message", AgentSource: "builtin"}
	panel := ansi.Strip(m.renderCommit())
	count := 0
	for _, line := range strings.Split(panel, "\n") {
		if strings.Contains(line, "detail") {
			count++
		}
	}
	if count != 3 || !strings.Contains(panel, "…") {
		t.Fatalf("error not capped at three lines:\n%s", panel)
	}
	for _, want := range []string{"Agent: commit-message (builtin)", "r refresh", "esc cancel"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("missing %q:\n%s", want, panel)
		}
	}
}

func TestCommitErrorManualMessageCopy(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.width, m.height = 80, 24
	m.commit = &CommitState{Phase: CommitError}
	panel := ansi.Strip(m.renderCommit())
	if !strings.Contains(panel, "or write a message manually") || !strings.Contains(panel, "m write message") {
		t.Fatalf("manual recovery missing:\n%s", panel)
	}
	m.commit.NeedsReview = true
	panel = ansi.Strip(m.renderCommit())
	if strings.Contains(panel, "m write message") || !strings.Contains(panel, "Refresh the checkout before continuing") {
		t.Fatalf("mandatory review copy incorrect:\n%s", panel)
	}
}

func TestCommitMouseDoesNotReachConversation(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.width, m.height = 80, 24
	m.commit = &CommitState{Phase: CommitChoosing}
	m.setTextareaValue("untouched composer")
	m.scrollOffset = 3
	for _, msg := range []tea.Msg{
		tea.MouseWheelMsg{Button: tea.MouseWheelDown},
		tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 5},
		tea.MouseClickMsg{Button: tea.MouseMiddle, X: 5, Y: 5},
	} {
		_, cmd := m.Update(msg)
		if cmd != nil || m.scrollOffset != 3 || m.textarea.Value() != "untouched composer" || m.selection.Active {
			t.Fatal("mouse event leaked through commit modal")
		}
	}
}

func TestCommitEditorBlinkRouting(t *testing.T) {
	m, _ := commitTUIModel(t)
	_, _ = m.ExecuteCommand("/commit")
	if !m.commit.Message.VirtualCursor() {
		t.Fatal("commit editor requires a virtual cursor")
	}
	// Use the actual phase transition's command: a generic textarea.Blink command
	// would initialize the background composer rather than the commit editor.
	_, cmd := m.updateCommit(commitDraftMsg{message: "My subject"})
	if cmd == nil {
		t.Fatal("editor focus did not schedule blinking")
	}
	msg := cmd()
	if _, ok := msg.(cursor.BlinkMsg); !ok {
		t.Fatalf("unexpected blink command message %T", msg)
	}
	before := m.commit.Message.View()
	_, next := m.Update(msg)
	if next == nil || m.commit.Message.View() == before {
		t.Fatal("editor did not handle its blink tick")
	}
	m.commit.Phase = CommitChoosing
	before = m.commit.Message.View()
	_, next = m.Update(cursor.BlinkMsg{})
	if next != nil || m.commit.Message.View() != before {
		t.Fatal("non-editing phase handled editor blink")
	}
}

func TestCommitPanelInlineKeepsComposer(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.altScreen = false
	m.width, m.height = 100, 32
	m.commit = &CommitState{Phase: CommitChoosing}
	m.setTextareaValue("background composer")
	view := m.View()
	panel := ansi.Strip(view.Content)
	if view.AltScreen || !strings.Contains(panel, "Git commit") || !strings.Contains(panel, "background composer") {
		t.Fatalf("inline panel replaced the UI:\n%s", panel)
	}
	if strings.Index(panel, "Git commit") > strings.Index(panel, "background composer") {
		t.Fatal("inline panel should precede the composer")
	}
}

func TestCommitSkipsChoiceUnlessStagingScopesDiffer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		staged    bool
		unstaged  bool
		untracked bool
		want      CommitPhase
	}{
		{"staged only", true, false, false, CommitEditing},
		{"unstaged only", false, true, false, CommitEditing},
		{"untracked only", false, false, true, CommitEditing},
		{"partially staged file", true, true, false, CommitChoosing},
		{"staged and untracked", true, false, true, CommitChoosing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, dir := commitTUIModel(t)
			if tc.staged {
				if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("staged\n"), 0644); err != nil {
					t.Fatal(err)
				}
				commitTUIGit(t, dir, "add", "base.txt")
			}
			if tc.unstaged {
				if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("unstaged\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.untracked {
				if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			before := commitTUICaptureGit(t, dir, "diff", "--cached")
			_, cmd := m.ExecuteCommand("/commit")
			m = applyCommitCmd(t, m, cmd)
			if m.commit == nil || m.commit.Phase != tc.want {
				t.Fatalf("expected %s, got %+v", tc.want, m.commit)
			}
			if tc.staged {
				if after := commitTUICaptureGit(t, dir, "diff", "--cached"); after != before {
					t.Fatal("existing index changed before an explicit choice")
				}
			} else if staged := commitTUICaptureGit(t, dir, "diff", "--cached", "--name-only"); staged == "" {
				t.Fatal("unstaged-only changes were not staged automatically")
			}
		})
	}
}

func TestCommitGeneratedDraftOpensAtSubjectWithoutDoubleWrapping(t *testing.T) {
	m, _ := commitTUIModel(t)
	m.width, m.height = 120, 40
	_, _ = m.ExecuteCommand("/commit")
	lines := []string{
		"Improve the native commit modal",
		"",
		"Keep the conversation visible behind a centered panel in alt-screen mode",
		"and retain the composer in inline mode. Fit content to terminal size while",
		"keeping file selection and action hints visible.",
	}
	draft := strings.Join(lines, "\n") + "\n\n" + strings.Repeat("Additional body paragraph.\n", 12)
	_, _ = m.updateCommit(commitDraftMsg{message: draft})
	panel := ansi.Strip(m.renderCommit())
	if m.commit.Message.Line() != 0 || m.commit.Message.Column() != 0 || m.commit.Message.ScrollYOffset() != 0 {
		t.Fatal("new draft did not open at the subject")
	}
	for _, line := range lines {
		if line != "" && !strings.Contains(panel, line) {
			t.Fatalf("hard-wrapped line was wrapped again: %q\n%s", line, panel)
		}
	}
	if m.commit.Message.Value() != draft || m.commit.Generated != draft || m.commit.Dirty {
		t.Fatal("presenting the draft changed its contents")
	}
	// A subsequent render should not undo intentional editor navigation.
	m.commit.Message.MoveToEnd()
	_ = m.renderCommit()
	if m.commit.Message.Line() == 0 {
		t.Fatal("render reset the user's cursor")
	}
}

func TestCommitBusyPhasesAnimateAndStopAtReview(t *testing.T) {
	for _, phase := range []CommitPhase{CommitLoading, CommitPlanning, CommitStaging, CommitDrafting, CommitCommitting} {
		t.Run(string(phase), func(t *testing.T) {
			m := newTestChatModel(false)
			m.width, m.height = 100, 24
			m.commit = &CommitState{Phase: phase}
			before := m.spinner.View()
			if !strings.Contains(ansi.Strip(m.renderCommit()), ansi.Strip(before)) {
				t.Fatal("busy panel does not display its spinner")
			}
			_, cmd := m.Update(spinner.TickMsg{ID: m.spinner.ID()})
			if cmd == nil || m.spinner.View() == before {
				t.Fatal("busy commit spinner did not advance and schedule another tick")
			}
			for _, idle := range []CommitPhase{CommitChoosing, CommitReviewing, CommitEditing, CommitError} {
				m.commit.Phase = idle
				before = m.spinner.View()
				_, cmd = m.Update(spinner.TickMsg{ID: m.spinner.ID()})
				if cmd != nil || m.spinner.View() != before {
					t.Fatalf("spinner kept running in %s", idle)
				}
			}
			m.commit = nil
			_, cmd = m.Update(spinner.TickMsg{ID: m.spinner.ID()})
			if cmd != nil {
				t.Fatal("spinner kept running after closing the modal")
			}
		})
	}
}

func TestCommitAgentOperationsStartSpinner(t *testing.T) {
	m, dir := commitTUIModel(t)
	repo, err := gitcommit.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	m.childRunner = &fakeSkillChildRunner{}
	for _, phase := range []CommitPhase{CommitPlanning, CommitDrafting} {
		m.commit = &CommitState{Repo: repo}
		var cmd tea.Cmd
		if phase == CommitPlanning {
			cmd = m.startCommitScopePlan()
		} else {
			cmd = m.startCommitDraft()
		}
		if m.commit.cancel != nil {
			m.commit.cancel()
		}
		batch, ok := cmd().(tea.BatchMsg)
		if !ok || len(batch) != 2 {
			t.Fatalf("%s did not batch spinner startup with agent work", phase)
		}
		if _, ok := batch[0]().(spinner.TickMsg); !ok {
			t.Fatalf("%s did not start a spinner tick", phase)
		}
	}
}
