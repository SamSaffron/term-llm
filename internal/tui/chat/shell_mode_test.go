package chat

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func directShellTestModel(t *testing.T) *Model {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	t.Setenv("SHELL", shell)
	m := newTestChatModel(true)
	m.width = 100
	m.height = 30
	return m
}

func startDirectShellTest(t *testing.T, m *Model, input string) (tea.BatchMsg, <-chan tea.Msg) {
	t.Helper()
	m.setDirectShellComposerValue(input)
	updated, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if cmd == nil || m.directShellRun == nil {
		t.Fatalf("direct shell did not start: cmd=%v run=%#v footer=%q", cmd != nil, m.directShellRun, m.footerMessage)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) < 2 {
		t.Fatalf("start command = %T, want batch with process and listener", cmd())
	}
	processDone := make(chan tea.Msg, 1)
	go func() { processDone <- batch[0]() }()
	return batch, processDone
}

func directShellCmdMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil direct-shell listener command")
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for direct-shell event")
		return nil
	}
}

func pumpDirectShellTest(t *testing.T, m *Model, listener tea.Cmd, onOutput func(string)) tea.Cmd {
	t.Helper()
	for {
		msg := directShellCmdMsg(t, listener)
		switch typed := msg.(type) {
		case directShellOutputMsg:
			updated, next := m.Update(typed)
			m = updated.(*Model)
			if onOutput != nil {
				output, _ := m.directShellRun.capture.text()
				onOutput(output)
			}
			listener = next
		case directShellDoneMsg:
			updated, followup := m.Update(typed)
			m = updated.(*Model)
			return followup
		default:
			t.Fatalf("unexpected direct-shell event %T", msg)
		}
	}
}

func TestDirectShellSuccessPersistsResultAndStartsModelResponse(t *testing.T) {
	m := directShellTestModel(t)
	store := &mockStore{}
	m.store = store

	batch, processDone := startDirectShellTest(t, m, "! printf success")
	followup := pumpDirectShellTest(t, m, batch[1], nil)
	if msg := directShellCmdMsg(t, func() tea.Msg { return <-processDone }); msg != nil {
		t.Fatalf("process command returned %T, want nil", msg)
	}

	if m.directShellRun != nil || !m.streaming || followup == nil {
		t.Fatalf("completion state: run=%#v streaming=%v followup=%v", m.directShellRun, m.streaming, followup != nil)
	}
	if len(store.added) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(store.added))
	}
	got := store.added[0]
	if got.Role != llm.RoleUser || !strings.Contains(got.TextContent, "! printf success") ||
		!strings.Contains(got.TextContent, "success") || !strings.Contains(got.TextContent, "exit status: 0") {
		t.Fatalf("persisted shell result = %#v", got)
	}
	if len(m.messages) != 1 || m.messages[0].TextContent != got.TextContent {
		t.Fatalf("in-memory shell result = %#v, persisted=%#v", m.messages, got)
	}

	followBatch, ok := followup().(tea.BatchMsg)
	if !ok || len(followBatch) == 0 {
		t.Fatalf("model follow-up command = %T, want batch", followup())
	}
	_ = followBatch[0]()
	provider := m.provider.(*llm.MockProvider)
	requests := provider.RecordedRequests()
	if len(requests) != 1 || len(requests[0].Messages) == 0 {
		t.Fatalf("model follow-up requests = %#v", requests)
	}
	if contextText := llm.MessageText(requests[0].Messages[len(requests[0].Messages)-1]); contextText != got.TextContent {
		t.Fatalf("model context shell result = %q, want %q", contextText, got.TextContent)
	}
}

func TestDirectShellNonzeroExitStillStartsModelResponse(t *testing.T) {
	m := directShellTestModel(t)
	batch, _ := startDirectShellTest(t, m, "! printf failure >&2; exit 7")
	followup := pumpDirectShellTest(t, m, batch[1], nil)

	if !m.streaming || followup == nil || len(m.messages) != 1 {
		t.Fatalf("nonzero completion did not start model: streaming=%v followup=%v messages=%d", m.streaming, followup != nil, len(m.messages))
	}
	text := m.messages[0].TextContent
	if !strings.Contains(text, "failure") || !strings.Contains(text, "exit status: 7") {
		t.Fatalf("nonzero result = %q", text)
	}
}

func TestDirectShellLaunchFailureStillStartsModelResponse(t *testing.T) {
	m := directShellTestModel(t)
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	batch, _ := startDirectShellTest(t, m, "! echo unreachable")
	followup := pumpDirectShellTest(t, m, batch[1], nil)

	if !m.streaming || followup == nil || len(m.messages) != 1 {
		t.Fatalf("launch failure did not start model: streaming=%v followup=%v messages=%d", m.streaming, followup != nil, len(m.messages))
	}
	if text := m.messages[0].TextContent; !strings.Contains(text, "failed to start:") || !strings.Contains(text, "missing-shell") {
		t.Fatalf("launch failure result = %q", text)
	}
}

func TestDirectShellStreamsCombinedOutputInObservedOrder(t *testing.T) {
	m := directShellTestModel(t)
	batch, _ := startDirectShellTest(t, m, "! printf first; sleep 0.05; printf second >&2; sleep 0.05; printf third")
	var snapshots []string
	pumpDirectShellTest(t, m, batch[1], func(output string) {
		if m.streaming {
			t.Fatal("model started before shell completion")
		}
		if m.directShellRun == nil {
			t.Fatal("shell run cleared while output was still arriving")
		}
		snapshots = append(snapshots, output)
	})

	if len(snapshots) < 2 {
		t.Fatalf("live output snapshots = %#v, want multiple events", snapshots)
	}
	text := m.messages[0].TextContent
	first, second, third := strings.Index(text, "first"), strings.Index(text, "second"), strings.Index(text, "third")
	if first < 0 || second <= first || third <= second {
		t.Fatalf("combined output order lost: %q", text)
	}
}

func TestDirectShellCancellationPersistsAndStartsModelResponse(t *testing.T) {
	m := directShellTestModel(t)
	batch, processDone := startDirectShellTest(t, m, "! printf started; sleep 30")

	msg := directShellCmdMsg(t, batch[1])
	output, ok := msg.(directShellOutputMsg)
	if !ok {
		t.Fatalf("first event = %T, want output", msg)
	}
	updated, listener := m.Update(output)
	m = updated.(*Model)
	updated, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if m.directShellRun == nil || !m.directShellRun.cancelRequested {
		t.Fatalf("cancel state = %#v", m.directShellRun)
	}
	pumpDirectShellTest(t, m, listener, nil)
	_ = directShellCmdMsg(t, func() tea.Msg { return <-processDone })

	if !m.streaming || m.directShellRun != nil || len(m.messages) != 1 {
		t.Fatalf("cancel completion state: streaming=%v run=%#v messages=%d", m.streaming, m.directShellRun, len(m.messages))
	}
	text := m.messages[0].TextContent
	if !strings.Contains(text, "started") || !strings.Contains(text, "cancelled by user") {
		t.Fatalf("cancelled result = %q", text)
	}
}

func TestDirectShellEmptyBangStaysInComposer(t *testing.T) {
	m := directShellTestModel(t)
	m.setDirectShellComposerValue("!")
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)

	if m.directShellRun != nil || m.streaming || len(m.messages) != 0 {
		t.Fatalf("empty bang executed: run=%#v streaming=%v messages=%d", m.directShellRun, m.streaming, len(m.messages))
	}
	if m.textarea.Value() != "" || !m.directShellComposerActive() || !strings.Contains(m.footerMessage, "Type a command after !") {
		t.Fatalf("empty shell body=%q active=%v footer=%q", m.textarea.Value(), m.directShellComposerActive(), m.footerMessage)
	}
}

func TestDirectShellTypingBangEntersAndBackspaceLeavesMode(t *testing.T) {
	m := directShellTestModel(t)
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: '!', Text: "!"})
	m = updated.(*Model)
	if !m.directShellComposerActive() || m.textarea.Value() != "" {
		t.Fatalf("typed bang state: active=%v body=%q", m.directShellComposerActive(), m.textarea.Value())
	}

	updated, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = updated.(*Model)
	if m.directShellComposerActive() || m.textarea.Value() != "" {
		t.Fatalf("backspace did not leave shell mode: active=%v body=%q", m.directShellComposerActive(), m.textarea.Value())
	}
}

func TestDirectShellRejectsTerminalControlsInCommand(t *testing.T) {
	m := directShellTestModel(t)
	m.directShellEligible = true
	updated, cmd := m.startDirectShell("printf '\x1b'")
	m = updated.(*Model)
	if m.directShellRun != nil || !m.directShellComposerActive() {
		t.Fatalf("control-bearing command state: cmd=%v run=%#v active=%v", cmd != nil, m.directShellRun, m.directShellComposerActive())
	}
	if !strings.Contains(m.footerMessage, "control characters") {
		t.Fatalf("control-bearing command warning = %q", m.footerMessage)
	}
}

func TestDirectShellPasteEntersShellMode(t *testing.T) {
	m := directShellTestModel(t)
	updated, _ := m.Update(tea.PasteMsg{Content: "! printf pasted"})
	m = updated.(*Model)
	if !m.directShellComposerActive() {
		t.Fatal("pasted leading bang did not activate shell mode")
	}
	updated, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if m.directShellRun == nil || cmd == nil || m.streaming {
		t.Fatalf("pasted bang did not start shell command: run=%#v cmd=%v streaming=%v", m.directShellRun, cmd != nil, m.streaming)
	}
	m.directShellRun.cancel()
}

func TestDirectShellPasteCannotConvertExistingDraft(t *testing.T) {
	m := directShellTestModel(t)
	m.setTextareaValue("draft")
	m.directShellEligible = false
	m.textarea.MoveToBegin()
	updated, _ := m.Update(tea.PasteMsg{Content: "! "})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "! draft" || m.directShellComposerActive() {
		t.Fatalf("paste into existing draft = %q active=%v", got, m.directShellComposerActive())
	}
}

func TestDirectShellHistoryCompletionAndRecallStripOutput(t *testing.T) {
	m := directShellTestModel(t)
	result := formatDirectShellResult("git status --short", "/tmp/project", "M shell.go", 0, false, false, nil)
	m.messages = append(m.messages, *session.NewMessage(m.sess.ID, llm.UserText(result), -1))

	m.setDirectShellComposerValue("! git st")
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "git status --short" || !m.directShellComposerActive() {
		t.Fatalf("shell history completion = %q active=%v", got, m.directShellComposerActive())
	}

	m.setTextareaValue("")
	handled, _ := m.recallPreviousPrompt()
	if !handled || m.textarea.Value() != "git status --short" || !m.directShellComposerActive() {
		t.Fatalf("shell recall = %q active=%v handled=%v", m.textarea.Value(), m.directShellComposerActive(), handled)
	}
	if strings.Contains(m.textarea.Value(), "M shell.go") {
		t.Fatalf("shell recall leaked captured output: %q", m.textarea.Value())
	}
}

func TestDirectShellVisibleResultHidesInternalMetadata(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		exitCode  int
		cancelled bool
		truncated bool
		runErr    error
		want      []string
		notWant   []string
	}{
		{
			name:     "successful output",
			output:   "file-a\nfile-b\n",
			exitCode: 0,
			want:     []string{"! ls", "[working directory: /tmp/project]", "file-a\nfile-b"},
			notWant:  []string{"combined stdout/stderr", "exit status: 0", directShellResultMarker},
		},
		{
			name:     "successful empty output",
			exitCode: 0,
			want:     []string{"! true", "[working directory: /tmp/project]"},
			notWant:  []string{"(no output)", "exit status: 0", directShellResultMarker},
		},
		{
			name:      "successful truncated output",
			output:    "partial output\n",
			exitCode:  0,
			truncated: true,
			want:      []string{"partial output", "[captured output truncated]"},
			notWant:   []string{"exit status: 0", "combined stdout/stderr", directShellResultMarker},
		},
		{
			name:     "nonzero exit",
			output:   "failed\n",
			exitCode: 7,
			want:     []string{"failed", "[exit status: 7]"},
			notWant:  []string{"combined stdout/stderr", directShellResultMarker},
		},
		{
			name:      "cancelled",
			output:    "started\n",
			exitCode:  130,
			cancelled: true,
			want:      []string{"started", "[cancelled by user]"},
			notWant:   []string{"combined stdout/stderr", directShellResultMarker},
		},
		{
			name:     "launch failure",
			exitCode: -1,
			runErr:   errors.New("missing shell"),
			want:     []string{"[failed to start: missing shell]"},
			notWant:  []string{"(no output)", "combined stdout/stderr", directShellResultMarker},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := "ls"
			if tt.name == "successful empty output" {
				command = "true"
			}
			structured := formatDirectShellResult(command, "/tmp/project", tt.output, tt.exitCode, tt.cancelled, tt.truncated, tt.runErr)
			visible, ok := directShellVisibleResult(structured)
			if !ok {
				t.Fatalf("structured result was not recognized: %q", structured)
			}
			for _, want := range tt.want {
				if !strings.Contains(visible, want) {
					t.Fatalf("visible result missing %q: %q", want, visible)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(visible, notWant) {
					t.Fatalf("visible result contains %q: %q", notWant, visible)
				}
			}
		})
	}
}

func TestDirectShellVisibleMessagesKeepStructuredContentInternal(t *testing.T) {
	structured := formatDirectShellResult("ls", "/tmp/project", "file-a", 0, false, false, nil)
	messages := []session.Message{{Role: llm.RoleUser, TextContent: structured, Parts: []llm.Part{{Type: llm.PartText, Text: structured}}}}
	visible := visibleDirectShellMessages(messages)

	if strings.Contains(visible[0].TextContent, directShellResultMarker) || strings.Contains(visible[0].TextContent, "exit status: 0") {
		t.Fatalf("visible message leaked metadata: %q", visible[0].TextContent)
	}
	if messages[0].TextContent != structured || llm.MessageText(messages[0].ToLLMMessage()) != structured {
		t.Fatal("visible transformation mutated durable/model content")
	}
}

func TestDirectShellHistoryRecallDoesNotArmOrdinaryBangPrompt(t *testing.T) {
	m := directShellTestModel(t)
	m.messages = append(m.messages, *session.NewMessage(m.sess.ID, llm.UserText("!important: explain this"), -1))

	handled, _ := m.recallPreviousPrompt()
	if !handled || m.directShellComposerActive() || m.textarea.Value() != "!important: explain this" {
		t.Fatalf("ordinary bang recall: handled=%v active=%v value=%q", handled, m.directShellComposerActive(), m.textarea.Value())
	}

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if m.directShellRun != nil || !m.streaming || len(m.messages) != 2 {
		t.Fatalf("ordinary recalled prompt executed: run=%#v streaming=%v messages=%d", m.directShellRun, m.streaming, len(m.messages))
	}
}

func TestComposerSnapshotPreservesShellModeWithoutArmingPlainBangDraft(t *testing.T) {
	m := directShellTestModel(t)
	m.setTextareaValue("!plain draft")
	plain := m.captureComposerSnapshot()
	m.setTextareaValue("")
	m.restoreComposerSnapshot(plain)
	if m.directShellComposerActive() || m.textarea.Value() != "!plain draft" {
		t.Fatalf("plain snapshot armed shell mode: active=%v value=%q", m.directShellComposerActive(), m.textarea.Value())
	}

	m.setDirectShellComposerValue("! echo shell")
	shell := m.captureComposerSnapshot()
	m.setTextareaValue("")
	m.restoreComposerSnapshot(shell)
	if !m.directShellComposerActive() || m.textarea.Value() != "echo shell" {
		t.Fatalf("shell snapshot lost mode: active=%v value=%q", m.directShellComposerActive(), m.textarea.Value())
	}
}

func TestDirectShellMultilineHistoryIsUnambiguous(t *testing.T) {
	command := "printf one\\n\nprintf '[working directory: fake]\\n'\nprintf '[combined stdout/stderr, in observed order]\\n'\nprintf two"
	result := formatDirectShellResult(command, "/tmp/project", "one\ntwo", 0, false, false, nil)
	got, ok := directShellCommandFromResult(result)
	if !ok || got != "! "+command {
		t.Fatalf("multiline shell history = %q, %v; want %q", got, ok, "! "+command)
	}
	if _, ok := directShellCommandFromResult("! harmless\n[working directory: prose]"); ok {
		t.Fatal("ordinary prompt was misclassified as shell history")
	}
}

func TestDirectShellNormalPromptsContainingBangDoNotExecute(t *testing.T) {
	for _, input := range []string{"explain ! printf normal", " ! printf leading-space"} {
		m := directShellTestModel(t)
		m.setTextareaValue(input)
		updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = updated.(*Model)
		if m.directShellRun != nil || !m.streaming || len(m.messages) != 1 {
			t.Fatalf("normal prompt %q entered shell mode: run=%#v streaming=%v messages=%d", input, m.directShellRun, m.streaming, len(m.messages))
		}
		if got := m.messages[0].TextContent; got != strings.TrimSpace(input) {
			t.Fatalf("normal prompt text = %q, want %q", got, strings.TrimSpace(input))
		}
	}
}

func TestDirectShellUsesEffectiveWorkingDirectory(t *testing.T) {
	m := directShellTestModel(t)
	worktree := t.TempDir()
	m.sess = &session.Session{ID: "shell-cwd", CWD: t.TempDir(), WorktreeDir: worktree}
	batch, _ := startDirectShellTest(t, m, "! pwd; printf marker > cwd-marker")
	pumpDirectShellTest(t, m, batch[1], nil)

	if !strings.Contains(m.messages[0].TextContent, worktree) {
		t.Fatalf("shell result did not report effective cwd: %q", m.messages[0].TextContent)
	}
	if _, err := os.Stat(filepath.Join(worktree, "cwd-marker")); err != nil {
		t.Fatalf("command did not execute in worktree: %v", err)
	}
}

func TestDirectShellDoesNotExecuteDuringStreaming(t *testing.T) {
	m := directShellTestModel(t)
	m.streaming = true
	m.setDirectShellComposerValue("! printf blocked")
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)

	if m.directShellRun != nil || m.textarea.Value() != "printf blocked" || !m.directShellComposerActive() {
		t.Fatalf("streaming shell submission mutated state: run=%#v draft=%q", m.directShellRun, m.textarea.Value())
	}
	if !strings.Contains(m.footerMessage, "current response") {
		t.Fatalf("streaming shell warning = %q", m.footerMessage)
	}
}

func TestDirectShellComposerAndRunningVisualTreatment(t *testing.T) {
	m := directShellTestModel(t)
	m.setDirectShellComposerValue("! echo visual")
	composerRaw := m.buildFooterLayout().view
	composer := ui.StripANSI(composerRaw)
	redPrompt := lipgloss.NewStyle().Foreground(m.styles.Theme().Error).Bold(true).Render("! ")
	redStatus := lipgloss.NewStyle().Foreground(m.styles.Theme().Error).Render("shell mode")
	if !strings.Contains(composerRaw, redPrompt) {
		t.Fatalf("shell composer prompt is not red: %q", composerRaw)
	}
	if !strings.Contains(composerRaw, redStatus) {
		t.Fatalf("shell mode status is not red: %q", composerRaw)
	}
	if strings.Contains(composer, "runs directly without tool approval") {
		t.Fatalf("shell composer still renders redundant banner: %q", composer)
	}
	if !strings.Contains(composer, "shell mode") {
		t.Fatalf("shell mode missing from status line: %q", composer)
	}
	composerLines := strings.Split(composer, "\n")
	foundShellPrompt := false
	for _, line := range composerLines {
		if strings.TrimSpace(line) == "! echo visual" {
			foundShellPrompt = true
			break
		}
	}
	if !foundShellPrompt || strings.Contains(composer, "❯ ! echo visual") {
		t.Fatalf("shell composer did not replace the normal prompt with bang: %q", composer)
	}

	m.setDirectShellComposerValue("!echo compact")
	compact := ui.StripANSI(m.buildFooterLayout().view)
	if !strings.Contains(compact, "! echo compact") || strings.Contains(compact, "!echo compact") {
		t.Fatalf("shell composer did not guarantee space after bang: %q", compact)
	}

	m.setDirectShellComposerValue("! printf 'done!'\nprintf second")
	multiline := ui.StripANSI(m.buildFooterLayout().view)
	if !strings.Contains(multiline, "done!") || !strings.Contains(multiline, "printf second") {
		t.Fatalf("multiline shell composer corrupted command text: %q", multiline)
	}

	m.setTextareaValue("normal prompt")
	normal := ui.StripANSI(m.buildFooterLayout().view)
	if !strings.Contains(normal, "❯ normal prompt") {
		t.Fatalf("normal composer prompt was not restored after shell mode: %q", normal)
	}

	m.directShellRun = &directShellRun{command: "echo visual", dir: "/tmp", startedAt: time.Now()}
	m.setTextareaValue("")
	m.bumpContentVersion()
	running := ui.StripANSI(m.View().Content)
	if !strings.Contains(running, "! echo visual") || !strings.Contains(running, "Esc cancels") || !strings.Contains(running, "waiting for output") {
		t.Fatalf("running shell treatment missing: %q", running)
	}
}

func TestDirectShellCaptureIsBoundedAndReadable(t *testing.T) {
	var capture directShellCapture
	capture.append([]byte(strings.Repeat("x", directShellCaptureLimit+123)))
	text, truncated := capture.text()
	if !truncated || len(capture.data) != directShellCaptureLimit || capture.omitted != 123 {
		t.Fatalf("capture bounds: truncated=%v kept=%d omitted=%d", truncated, len(capture.data), capture.omitted)
	}
	if !strings.Contains(text, "123 additional bytes omitted") {
		t.Fatalf("capture marker = %q", text[len(text)-100:])
	}

	capture = directShellCapture{}
	capture.append([]byte(strings.Repeat("\x01", directShellCaptureLimit)))
	text, truncated = capture.text()
	if !truncated || len(text) > directShellCaptureLimit+200 || !strings.Contains(text, "escaped output truncated") {
		t.Fatalf("escaped capture bounds: truncated=%v len=%d suffix=%q", truncated, len(text), text[max(0, len(text)-100):])
	}
}

func TestDirectShellCompletionDoesNotPersistIntoChangedSession(t *testing.T) {
	m := directShellTestModel(t)
	m.directShellRun = &directShellRun{generation: 1, sessionID: "old-session", command: "echo old", dir: "/tmp"}
	m.sess.ID = "new-session"

	updated, _ := m.handleDirectShellDone(directShellDoneMsg{generation: 1, exitCode: 0})
	m = updated.(*Model)
	if m.directShellRun != nil || len(m.messages) != 0 || !strings.Contains(m.footerMessage, "active session changed") {
		t.Fatalf("cross-session completion: run=%#v messages=%d footer=%q", m.directShellRun, len(m.messages), m.footerMessage)
	}
}

func TestDirectShellLateOutputDoesNotRearmListener(t *testing.T) {
	m := directShellTestModel(t)
	updated, cmd := m.handleDirectShellOutput(directShellOutputMsg{generation: 1, data: []byte("late"), events: make(chan tea.Msg)})
	m = updated.(*Model)
	if cmd != nil || m.directShellRun != nil {
		t.Fatalf("late output rearmed listener: cmd=%v run=%#v", cmd != nil, m.directShellRun)
	}
}

func TestDirectShellCompletionDoesNotBlockAfterTUIExit(t *testing.T) {
	m := directShellTestModel(t)
	commandCtx, cancelCommand := context.WithCancel(context.Background())
	defer cancelCommand()
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	events := make(chan tea.Msg, 1)
	events <- directShellOutputMsg{}
	cancelLifecycle()

	done := make(chan struct{})
	dir := t.TempDir()
	go func() {
		_ = m.runDirectShellCmd(commandCtx, lifecycleCtx, 1, "true", dir, events)()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shell completion blocked on a full event queue after TUI exit")
	}
}

func TestDirectShellRunningOwnsPromptHistory(t *testing.T) {
	m := directShellTestModel(t)
	m.messages = append(m.messages, *session.NewMessage(m.sess.ID, llm.UserText("older prompt"), -1))
	m.directShellRun = &directShellRun{cancel: func() {}}

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "" || m.promptHistory.active {
		t.Fatalf("running shell allowed prompt history: draft=%q active=%v", got, m.promptHistory.active)
	}
}
