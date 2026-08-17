package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/procutil"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminaltext"
)

func (m *Model) cmdShell(rawArgs string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	if m.streaming {
		return m.showFooterWarning("Cannot open a shell while a response is streaming.")
	}
	if m.directShellRun != nil {
		return m.showFooterWarning("Cannot open a shell while a shell-mode command is running.")
	}
	opts, err := parseShellArgs(rawArgs)
	if err != nil {
		return m.showFooterError(err.Error())
	}

	cmd, dir, err := m.interactiveShellCommand(opts)
	if err != nil {
		return m.showFooterError(err.Error())
	}

	m.clearFooterMessage()
	m.setShellTerminalHandoff(true)
	if m.completions != nil {
		m.completions.Hide()
	}
	m.selection = Selection{}

	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return shellExitMessage(dir, err)
	})
}

type shellOptions struct {
	NoRC    bool
	Command string
}

// setShellTerminalHandoff keeps the render pause and image boundary in one
// lifecycle operation. A mode-free View replaces any queued image payload
// before Bubble Tea releases the renderer; the first restored View then carries
// a complete upload/placement payload.
func (m *Model) setShellTerminalHandoff(active bool) {
	if m == nil {
		return
	}
	m.pausedForExternalUI = active
	m.externalProcessActive = active
	if !active {
		// A child process can alter either terminal screen and Kitty's image state.
		// Invalidate acknowledgements so the first restored View carries cleanup and
		// a complete upload/placement transition.
		m.resetImageUploadState()
		m.invalidateImageViewportContent()
	}
}

func parseShellArgs(rawArgs string) (shellOptions, error) {
	var opts shellOptions
	rest := strings.TrimSpace(rawArgs)
	if rest == "" {
		return opts, nil
	}

	first := strings.Fields(rest)[0]
	remainder := strings.TrimSpace(rest[len(first):])
	if first == "--no-rc" {
		opts.NoRC = true
		opts.Command = strings.TrimSpace(remainder)
		return opts, nil
	}
	if strings.HasPrefix(first, "-") {
		return opts, fmt.Errorf("unknown /shell option %q; usage: /shell [--no-rc] [command ...]", first)
	}
	opts.Command = rest
	return opts, nil
}

func (m *Model) interactiveShellCommand(opts shellOptions) (*exec.Cmd, string, error) {
	dir, err := m.interactiveShellDir()
	if err != nil {
		return nil, "", err
	}
	shellPath := interactiveShellPath()
	shellArgs, err := interactiveShellArgs(shellPath, opts.NoRC)
	if err != nil {
		return nil, "", err
	}
	if opts.Command != "" {
		shellArgs = append(shellArgs, "-c", opts.Command)
	}
	cmd := exec.Command(shellPath, shellArgs...)
	cmd.Dir = dir
	cmd.Env = interactiveShellEnv(os.Environ(), dir, m.boundWorktreeForShellEnv(), opts.NoRC)
	// Full-screen terminal programs must write straight to the terminal. In
	// alt-screen chat Bubble Tea's output is decorated to append image escape
	// sequences after rendered frames; letting vim or another interactive child
	// inherit that decorator can interleave TUI output with the child's screen.
	// Leave stdin unset so tea.ExecProcess can attach the TTY input it owns.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, dir, nil
}

func (m *Model) effectiveWorkingDir() string {
	if m == nil {
		return ""
	}
	if m.sess != nil {
		if dir := strings.TrimSpace(m.sess.WorktreeDir); dir != "" {
			return dir
		}
	}
	if m.toolMgr != nil {
		if dir := strings.TrimSpace(m.toolMgr.BaseDir()); dir != "" {
			return dir
		}
	}
	if m.sess != nil {
		return strings.TrimSpace(m.sess.CWD)
	}
	return ""
}

func (m *Model) interactiveShellDir() (string, error) {
	dir := m.effectiveWorkingDir()
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve shell working directory: %w", err)
		}
		dir = cwd
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve shell working directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("shell working directory is not accessible: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("shell working path is not a directory: %s", abs)
	}
	return abs, nil
}

func (m *Model) boundWorktreeForShellEnv() string {
	if m == nil || m.sess == nil {
		return ""
	}
	return strings.TrimSpace(m.sess.WorktreeDir)
}

func interactiveShellPath() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "sh"
}

func interactiveShellArgs(shellPath string, noRC bool) ([]string, error) {
	if !noRC {
		return nil, nil
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(shellPath)), ".exe")
	switch name {
	case "zsh":
		return []string{"-f"}, nil
	case "bash":
		return []string{"--noprofile", "--norc"}, nil
	case "fish":
		return []string{"--no-config"}, nil
	case "csh", "tcsh":
		return []string{"-f"}, nil
	case "nu", "nushell":
		return []string{"--no-config-file"}, nil
	case "sh", "dash", "ash", "ksh", "mksh", "pdksh":
		// POSIX-ish shells commonly use ENV for interactive startup. There is
		// no portable no-rc flag, so interactiveShellEnv removes ENV below.
		return nil, nil
	default:
		return nil, fmt.Errorf("/shell --no-rc is not supported for shell %q", shellPath)
	}
}

func interactiveShellEnv(environ []string, dir string, worktreeDir string, noRC bool) []string {
	out := make([]string, 0, len(environ)+3)
	for _, entry := range environ {
		if shouldDropInteractiveShellEnv(entry, noRC) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "PWD="+dir, "TERM_LLM_BASE_DIR="+dir)
	if worktreeDir = strings.TrimSpace(worktreeDir); worktreeDir != "" {
		out = append(out, "TERM_LLM_WORKTREE_DIR="+worktreeDir)
	}
	return out
}

func shouldDropInteractiveShellEnv(entry string, noRC bool) bool {
	key, _, _ := strings.Cut(entry, "=")
	switch key {
	case "PWD", "TERM_LLM_BASE_DIR", "TERM_LLM_WORKTREE_DIR":
		return true
	case "ENV", "BASH_ENV", "ZDOTDIR":
		return noRC
	default:
		return false
	}
}

func shellCompletionItems(query string) ([]Command, bool) {
	query = strings.TrimPrefix(query, "/")
	if strings.TrimSpace(query) == "" {
		return nil, false
	}
	trailingSpace := strings.HasSuffix(query, " ")
	parts := strings.Fields(query)
	if len(parts) == 0 {
		return nil, false
	}
	cmd := strings.ToLower(parts[0])
	if cmd != "shell" && cmd != "sh" {
		return nil, false
	}
	if len(parts) == 1 && !trailingSpace {
		return nil, false
	}

	prefixParts := append([]string{}, parts...)
	partial := ""
	if len(parts) > 1 && !trailingSpace {
		prefixParts = append([]string{}, parts[:len(parts)-1]...)
		partial = parts[len(parts)-1]
	}
	if partial != "" && !strings.HasPrefix(partial, "-") {
		return nil, true
	}
	for _, part := range parts[1:] {
		if part == "--no-rc" {
			return nil, true
		}
	}
	if partial != "" && !strings.HasPrefix("--no-rc", strings.ToLower(partial)) {
		return nil, true
	}
	return []Command{{
		Name:        strings.Join(append(prefixParts, "--no-rc"), " "),
		Description: "Start shell without user rc/config files",
	}}, true
}

func shellExitMessage(dir string, err error) shellExitedMsg {
	msg := shellExitedMsg{dir: dir}
	if err == nil {
		return msg
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg.exitCode = exitErr.ExitCode()
		return msg
	}
	msg.err = err
	return msg
}

const (
	directShellCaptureLimit = 64 << 10
	directShellWaitDelay    = time.Second
)

type directShellRun struct {
	generation      uint64
	sessionID       string
	command         string
	dir             string
	startedAt       time.Time
	cancel          context.CancelFunc
	cancelRequested bool
	capture         directShellCapture
}

type directShellCapture struct {
	data    []byte
	omitted int64
}

func (c *directShellCapture) append(p []byte) {
	if len(p) == 0 {
		return
	}
	remaining := directShellCaptureLimit - len(c.data)
	if remaining > 0 {
		keep := min(remaining, len(p))
		c.data = append(c.data, p[:keep]...)
		p = p[keep:]
	}
	c.omitted += int64(len(p))
}

func (c *directShellCapture) text() (string, bool) {
	if c == nil {
		return "", false
	}
	text := terminaltext.EscapeControlsPreserveLayout(string(bytes.ToValidUTF8(c.data, []byte("�"))))
	escapedOmitted := 0
	if len(text) > directShellCaptureLimit {
		cut := directShellCaptureLimit
		for cut > 0 && !utf8.ValidString(text[:cut]) {
			cut--
		}
		escapedOmitted = len(text) - cut
		text = text[:cut]
	}
	if c.omitted == 0 && escapedOmitted == 0 {
		return text, false
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if c.omitted > 0 {
		text += fmt.Sprintf("[output truncated after %d bytes; %d additional bytes omitted]", directShellCaptureLimit, c.omitted)
	} else {
		text += fmt.Sprintf("[escaped output truncated after %d bytes; %d expanded bytes omitted]", directShellCaptureLimit, escapedOmitted)
	}
	return text, true
}

type directShellOutputMsg struct {
	generation uint64
	data       []byte
	events     <-chan tea.Msg
}

type directShellDoneMsg struct {
	generation uint64
	exitCode   int
	cancelled  bool
	err        error
}

type directShellEventWriter struct {
	ctx        context.Context
	generation uint64
	events     chan tea.Msg
}

func (w *directShellEventWriter) Write(p []byte) (int, error) {
	chunk := append([]byte(nil), p...)
	select {
	case w.events <- directShellOutputMsg{generation: w.generation, data: chunk, events: w.events}:
	case <-w.ctx.Done():
		// Cancellation owns process termination. Dropping late pipe bytes prevents a
		// cancelled command from blocking while the final completion event is queued.
	}
	return len(p), nil
}

func (m *Model) directShellComposerActive() bool {
	return m != nil && m.directShellEligible
}

func directShellComposerBody(value string) (string, bool) {
	if !strings.HasPrefix(value, "!") {
		return value, false
	}
	body := strings.TrimPrefix(value, "!")
	body = strings.TrimPrefix(body, " ")
	return body, true
}

func (m *Model) setDirectShellComposerBody(body string) {
	m.directShellEligible = true
	m.textarea.SetValue(body)
	m.updateTextareaHeight()
}

func (m *Model) setDirectShellComposerValue(value string) {
	body, active := directShellComposerBody(value)
	if !active {
		body = value
	}
	m.directShellEligible = active
	m.textarea.SetValue(body)
	m.updateTextareaHeight()
}

type composerSnapshot struct {
	body      string
	shellMode bool
}

func (m *Model) captureComposerSnapshot() composerSnapshot {
	return composerSnapshot{body: m.textarea.Value(), shellMode: m.directShellComposerActive()}
}

func (m *Model) restoreComposerSnapshot(snapshot composerSnapshot) {
	m.directShellEligible = snapshot.shellMode
	m.textarea.SetValue(snapshot.body)
	m.updateTextareaHeight()
}

func (m *Model) directShellComposerValue() string {
	if m == nil {
		return ""
	}
	if !m.directShellComposerActive() {
		return m.textarea.Value()
	}
	body := m.textarea.Value()
	if body == "" {
		return "!"
	}
	return "! " + body
}

const (
	directShellResultMarker = "[term-llm shell result v1]"
	directShellStreamLabel  = "[combined stdout/stderr, in observed order]"
)

// directShellCommandFromResult recognizes the durable shell-turn format and
// returns only the executable history entry, never the captured output. The
// explicit trailing marker avoids treating an ordinary prompt as shell history,
// while the working-directory boundary supports multiline commands.
func directShellCommandFromResult(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasSuffix(text, "\n"+directShellResultMarker) || !strings.HasPrefix(text, "! ") {
		return "", false
	}
	streamBoundary := strings.LastIndex(text, "\n"+directShellStreamLabel+"\n")
	if streamBoundary < 0 {
		return "", false
	}
	boundary := strings.LastIndex(text[:streamBoundary], "\n[working directory: ")
	if boundary < 0 || !strings.HasSuffix(text[boundary:streamBoundary], "]") {
		return "", false
	}
	command := text[:boundary]
	if strings.TrimSpace(strings.TrimPrefix(command, "! ")) == "" {
		return "", false
	}
	return command, true
}

func (m *Model) directShellHistoryCompletion(prefix string) (string, bool) {
	prefix = strings.TrimSpace(prefix)
	if m == nil || !m.directShellComposerActive() {
		return "", false
	}
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role != llm.RoleUser {
			continue
		}
		command, ok := directShellCommandFromResult(m.messages[i].TextContent)
		if !ok {
			continue
		}
		body, _ := directShellComposerBody(command)
		if body != prefix && strings.HasPrefix(body, prefix) {
			return body, true
		}
	}
	return "", false
}

func (m *Model) updateDirectShellEligibilityAfterKey(oldValue, newValue string) {
	if m.directShellEligible || oldValue != "" {
		return
	}
	body, active := directShellComposerBody(newValue)
	if !active {
		return
	}
	m.setDirectShellComposerBody(body)
	m.textarea.MoveToEnd()
}

func (m *Model) startDirectShell(raw string) (tea.Model, tea.Cmd) {
	if m.streaming {
		return m.showFooterWarning("Wait for the current response to finish before running a shell command.")
	}
	if m.directShellRun != nil {
		return m.showFooterWarning("A shell command is already running. Press Esc to cancel it.")
	}
	if m.worktreeOperationBusy() {
		return m.showFooterWarning("Wait for the current worktree operation to finish before running a shell command.")
	}
	if len(m.files) > 0 || len(m.images) > 0 {
		return m.showFooterWarning("Shell mode cannot use attachments. Remove them or send a normal prompt.")
	}

	raw = m.expandedPastePlaceholders(raw)
	command := strings.TrimSpace(raw)
	if command == "" {
		return m.showFooterWarning("Type a command after !, or press Esc to leave shell mode.")
	}
	if terminaltext.EscapeControlsPreserveLayout(command) != command {
		return m.showFooterWarning("Shell commands cannot contain terminal control characters.")
	}
	dir, err := m.interactiveShellDir()
	if err != nil {
		return m.showFooterError(err.Error())
	}

	m.directShellGen++
	generation := m.directShellGen
	lifecycleCtx := m.rootContext()
	ctx, cancel := context.WithCancel(lifecycleCtx)
	events := make(chan tea.Msg, 256)
	sessionID := ""
	if m.sess != nil {
		sessionID = m.sess.ID
	}
	m.directShellRun = &directShellRun{
		generation: generation,
		sessionID:  sessionID,
		command:    command,
		dir:        dir,
		startedAt:  time.Now(),
		cancel:     cancel,
	}
	m.clearFooterMessage()
	m.resetPromptHistory()
	m.setTextareaValue("")
	m.pasteChunks = nil
	m.hideMentionPopup()
	m.selection = Selection{}
	m.resetRetainedStreamTracker()
	m.viewCache.completedStream = ""
	m.invalidateHistoryCache()
	m.scrollToBottom = true
	m.bumpContentVersion()

	return m, tea.Batch(
		m.runDirectShellCmd(ctx, lifecycleCtx, generation, command, dir, events),
		waitDirectShellEvent(events),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

func (m *Model) runDirectShellCmd(ctx, lifecycleCtx context.Context, generation uint64, command, dir string, events chan tea.Msg) tea.Cmd {
	worktreeDir := m.boundWorktreeForShellEnv()
	return func() tea.Msg {
		cmd := exec.CommandContext(ctx, interactiveShellPath(), "-c", command)
		cmd.Dir = dir
		cmd.Env = interactiveShellEnv(os.Environ(), dir, worktreeDir, false)
		cmd.WaitDelay = directShellWaitDelay

		writer := &directShellEventWriter{ctx: ctx, generation: generation, events: events}
		// Using the exact same comparable writer for both pipes makes os/exec
		// serialize writes, preserving the order in which stdout/stderr is observed.
		cmd.Stdout = writer
		cmd.Stderr = writer
		cleanup, err := procutil.PrepareCommand(cmd)
		if err == nil {
			defer cleanup()
			err = cmd.Run()
		}

		done := directShellDoneMsg{generation: generation, err: err}
		switch {
		case ctx.Err() != nil:
			done.cancelled = true
			done.exitCode = 130
		case err == nil:
			done.exitCode = 0
		default:
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				done.exitCode = exitErr.ExitCode()
				done.err = nil
			} else {
				done.exitCode = -1
			}
		}
		select {
		case events <- done:
		case <-lifecycleCtx.Done():
			// The TUI has exited and no listener can drain the bounded event queue.
			// Do not retain the completed process goroutine trying to report into it.
		}
		return nil
	}
}

func waitDirectShellEvent(events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-events
	}
}

func (m *Model) handleDirectShellOutput(msg directShellOutputMsg) (tea.Model, tea.Cmd) {
	if m.directShellRun == nil || msg.generation != m.directShellRun.generation {
		return m, nil
	}
	m.directShellRun.capture.append(msg.data)
	m.bumpContentVersion()
	return m, waitDirectShellEvent(msg.events)
}

func (m *Model) cancelDirectShell() (tea.Model, tea.Cmd) {
	if m.directShellRun == nil {
		return m, nil
	}
	if !m.directShellRun.cancelRequested {
		m.directShellRun.cancelRequested = true
		m.directShellRun.cancel()
		m.bumpContentVersion()
	}
	return m.showFooterMuted("Cancelling shell command…")
}

func (m *Model) handleDirectShellDone(msg directShellDoneMsg) (tea.Model, tea.Cmd) {
	run := m.directShellRun
	if run == nil || msg.generation != run.generation {
		return m, nil
	}
	if run.cancel != nil {
		run.cancel()
	}
	output, truncated := run.capture.text()
	m.directShellRun = nil
	m.directShellEligible = false
	m.bumpContentVersion()
	if m.sess == nil || m.sess.ID != run.sessionID {
		return m.showFooterWarning("Shell result discarded because the active session changed.")
	}
	resultText := formatDirectShellResult(run.command, run.dir, output, msg.exitCode, msg.cancelled, truncated, msg.err)
	return m.sendDirectShellResult(run.command, resultText)
}

func formatDirectShellResult(command, dir, output string, exitCode int, cancelled, truncated bool, runErr error) string {
	command = terminaltext.EscapeControlsPreserveLayout(command)
	dir = terminaltext.EscapeControlsPreserveLayout(dir)
	if strings.TrimSpace(output) == "" {
		output = "(no output)"
	} else {
		output = strings.TrimRight(output, "\n")
	}

	status := fmt.Sprintf("exit status: %d", exitCode)
	if cancelled {
		status = "cancelled by user"
	} else if runErr != nil {
		status = "failed to start: " + terminaltext.EscapeControlsPreserveLayout(runErr.Error())
	}
	if truncated {
		status += "; captured output truncated"
	}
	return fmt.Sprintf("! %s\n[working directory: %s]\n%s\n%s\n[%s]\n%s", command, dir, directShellStreamLabel, output, status, directShellResultMarker)
}

func directShellVisibleResult(content string) (string, bool) {
	content = strings.TrimSpace(content)
	markerSuffix := "\n" + directShellResultMarker
	if !strings.HasPrefix(content, "! ") || !strings.HasSuffix(content, markerSuffix) {
		return content, false
	}

	content = strings.TrimSuffix(content, markerSuffix)
	statusStart := strings.LastIndex(content, "\n[")
	if statusStart < 0 || !strings.HasSuffix(content, "]") {
		return content, false
	}
	status := content[statusStart+1:]
	body := content[:statusStart]
	streamLabel := "\n" + directShellStreamLabel + "\n"
	labelStart := strings.LastIndex(body, streamLabel)
	if labelStart < 0 {
		return content, false
	}
	workingDirectoryStart := strings.LastIndex(body[:labelStart], "\n[working directory: ")
	if workingDirectoryStart < 0 || !strings.HasSuffix(body[workingDirectoryStart:labelStart], "]") {
		return content, false
	}

	header := body[:labelStart]
	output := strings.TrimSpace(body[labelStart+len(streamLabel):])
	visible := header
	if output != "" && output != "(no output)" {
		visible += "\n" + output
	}
	visibleStatus := status
	switch {
	case status == "[exit status: 0]":
		visibleStatus = ""
	case strings.HasPrefix(status, "[exit status: 0; "):
		visibleStatus = "[" + strings.TrimSuffix(strings.TrimPrefix(status, "[exit status: 0; "), "]") + "]"
	}
	if visibleStatus != "" {
		visible += "\n" + visibleStatus
	}
	return visible, true
}

func (m *Model) sendDirectShellResult(command, content string) (tea.Model, tea.Cmd) {
	m.clearFooterMessage()
	var preSendCmds []tea.Cmd
	if cmd := m.applyPendingStreamModelSwitch(); cmd != nil {
		preSendCmds = append(preSendCmds, cmd)
	}
	m.recordCurrentModelUse()
	m.ensureContextMessages()
	m.appendPendingModelSwitchMarker()
	m.activeBranchAnchorID = lastSafeBranchMessageID(m.messages)

	userMsg := &session.Message{
		SessionID:   m.sess.ID,
		Role:        llm.RoleUser,
		Parts:       []llm.Part{{Type: llm.PartText, Text: content}},
		TextContent: content,
		CreatedAt:   time.Now(),
		Sequence:    -1,
	}
	m.messages = append(m.messages, *userMsg)
	m.invalidateHistoryCache()
	if m.store != nil {
		if err := m.store.AddMessage(context.Background(), m.sess.ID, userMsg); err == nil && userMsg.ID > 0 {
			m.messages[len(m.messages)-1].ID = userMsg.ID
			m.activeBranchAnchorID = userMsg.ID
		}
		_ = m.store.IncrementUserTurns(context.Background(), m.sess.ID)
		m.sess.UserTurns++
		if m.sess.Summary == "" {
			m.sess.Summary = session.TruncateSummary("! " + command)
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
	if cmd := m.scheduleTitleFallbackCmd(); cmd != nil {
		preSendCmds = append(preSendCmds, cmd)
	}
	if m.userMessageCount() == 1 {
		if cmd := m.maybeNameHandoverCmd("! " + command); cmd != nil {
			preSendCmds = append(preSendCmds, cmd)
		}
	}
	userDisplay, ok := directShellVisibleResult(content)
	if !ok {
		userDisplay = content
	}
	return m.beginUserResponse(content, userDisplay, preSendCmds)
}
