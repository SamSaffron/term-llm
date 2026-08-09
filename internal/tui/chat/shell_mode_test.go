package chat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	m.setTextareaValue(input)
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
	m.setTextareaValue("!")
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)

	if m.directShellRun != nil || m.streaming || len(m.messages) != 0 {
		t.Fatalf("empty bang executed: run=%#v streaming=%v messages=%d", m.directShellRun, m.streaming, len(m.messages))
	}
	if m.textarea.Value() != "!" || !strings.Contains(m.footerMessage, "Type a command after !") {
		t.Fatalf("empty bang draft=%q footer=%q", m.textarea.Value(), m.footerMessage)
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

	m.setTextareaValue("! git st")
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "! git status --short" || !m.directShellComposerActive() {
		t.Fatalf("shell history completion = %q active=%v", got, m.directShellComposerActive())
	}

	m.setTextareaValue("")
	handled, _ := m.recallPreviousPrompt()
	if !handled || m.textarea.Value() != "! git status --short" || !m.directShellComposerActive() {
		t.Fatalf("shell recall = %q active=%v handled=%v", m.textarea.Value(), m.directShellComposerActive(), handled)
	}
	if strings.Contains(m.textarea.Value(), "M shell.go") {
		t.Fatalf("shell recall leaked captured output: %q", m.textarea.Value())
	}
}

func TestDirectShellMultilineHistoryIsUnambiguous(t *testing.T) {
	command := "printf one\\n\nprintf two"
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
	m.setTextareaValue("! printf blocked")
	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)

	if m.directShellRun != nil || m.textarea.Value() != "! printf blocked" {
		t.Fatalf("streaming shell submission mutated state: run=%#v draft=%q", m.directShellRun, m.textarea.Value())
	}
	if !strings.Contains(m.footerMessage, "current response") {
		t.Fatalf("streaming shell warning = %q", m.footerMessage)
	}
}

func TestDirectShellComposerAndRunningVisualTreatment(t *testing.T) {
	m := directShellTestModel(t)
	m.setTextareaValue("! echo visual")
	composer := ui.StripANSI(m.View().Content)
	if !strings.Contains(composer, "shell mode") || !strings.Contains(composer, "runs directly without tool approval") {
		t.Fatalf("shell composer treatment missing: %q", composer)
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
