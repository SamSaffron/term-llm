package chat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
)

func TestBuildTitle(t *testing.T) {
	tests := []struct {
		name string
		st   titleState
		want string
	}{
		{
			name: "attention keeps marker first",
			st:   titleState{Attention: true, Agent: "developer", Task: "Fix ctrl-c exit", Model: "fable"},
			want: "‼ Fix ctrl-c exit · developer · fable",
		},
		{
			name: "streaming does not include elapsed activity",
			st:   titleState{Agent: "developer", Task: "Fix ctrl-c exit", Model: "fable", Streaming: true, Elapsed: 12 * time.Second},
			want: "Fix ctrl-c exit · developer · fable",
		},
		{
			name: "idle includes model and agent suffix",
			st:   titleState{Agent: "developer", Task: "Fix ctrl-c exit", Model: "fable"},
			want: "Fix ctrl-c exit · developer · fable",
		},
		{
			name: "missing task falls back to term llm",
			st:   titleState{Agent: "developer", Model: "fable"},
			want: "term-llm · developer · fable",
		},
		{
			name: "missing agent omits segment",
			st:   titleState{Task: "Fix ctrl-c exit", Model: "fable"},
			want: "Fix ctrl-c exit · fable",
		},
		{
			name: "minimal fallback",
			st:   titleState{},
			want: "term-llm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildTitle(tt.st); got != tt.want {
				t.Fatalf("buildTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildTitleTruncates(t *testing.T) {
	got := buildTitle(titleState{Agent: "developer", Task: strings.Repeat("verylong ", 20), Model: "fable-medium"})
	if n := len([]rune(got)); n > terminalTitleMaxRunes {
		t.Fatalf("title length = %d, want <= %d: %q", n, terminalTitleMaxRunes, got)
	}
	if !strings.Contains(got, " · developer · fable-medium") {
		t.Fatalf("truncated title should preserve agent/model suffix: %q", got)
	}
	if strings.HasSuffix(got, "…") {
		t.Fatalf("default title should not end with streaming activity ellipsis: %q", got)
	}
}

func TestTerminalTitleModeParsing(t *testing.T) {
	tests := []struct {
		raw  string
		mode TerminalTitleMode
		ok   bool
	}{
		{"", TerminalTitleSmart, true},
		{"SMART", TerminalTitleSmart, true},
		{"basic", TerminalTitleBasic, true},
		{"off", TerminalTitleOff, true},
		{"nope", TerminalTitleSmart, false},
	}
	for _, tt := range tests {
		mode, ok := ParseTerminalTitleMode(tt.raw)
		if mode != tt.mode || ok != tt.ok {
			t.Fatalf("ParseTerminalTitleMode(%q) = (%q, %v), want (%q, %v)", tt.raw, mode, ok, tt.mode, tt.ok)
		}
	}
}

func TestTerminalTitleFormat(t *testing.T) {
	env := NewTerminalTitleEnvironment(map[string]string{"DOCKER_CONTAINER_NAME": "worker-1"})
	formatter := newTerminalTitleFormatter("[{{env.DOCKER_CONTAINER_NAME}}] {{attention}}{{agent}}/{{task}}/{{state}}/{{activity}}", env)
	got := formatter.Format(titleState{
		Attention: true,
		Agent:     "developer",
		Task:      "Fix",
		Model:     "fable",
	})
	want := "[worker-1] ‼developer/Fix/attention/attention"
	if got != want {
		t.Fatalf("formatted title = %q, want %q", got, want)
	}

	formatter = newTerminalTitleFormatter(`{{env "MISSING" | default "host"}} · {{title}}`, env)
	got = formatter.Format(titleState{Agent: "developer", Task: "Custom title", Model: "fable"})
	want = "host · Custom title · developer · fable"
	if got != want {
		t.Fatalf("formatted title with default env = %q, want %q", got, want)
	}
}

func TestTerminalTitleFormatValidation(t *testing.T) {
	if err := ValidateTerminalTitleFormat("{{agent}} · {{env.DOCKER_CONTAINER_NAME}} · {{title}}"); err != nil {
		t.Fatalf("valid title format returned error: %v", err)
	}
	if err := ValidateTerminalTitleFormat("{{unknown_placeholder}}"); err == nil {
		t.Fatal("expected unknown title format placeholder to fail validation")
	}
	if err := ValidateTerminalTitleFormat("{{.UnknownField}}"); err == nil {
		t.Fatal("expected unknown title format field to fail validation")
	}
}

func TestTerminalTitleCommandSequencesAndSanitization(t *testing.T) {
	title := "Fix\nCtrl-C\x1b\\Exit\a"
	if got := sanitizeTerminalTitle(title); got != "Fix Ctrl-C\\Exit" {
		t.Fatalf("sanitizeTerminalTitle() = %q", got)
	}
	if got, want := oscTitleSequence(title), "\x1b]2;Fix Ctrl-C\\Exit\x07"; got != want {
		t.Fatalf("osc title sequence = %q, want %q", got, want)
	}
	if got, want := tmuxWindowTitleSequence(title), "\x1bkFix Ctrl-C\\Exit\x1b\\"; got != want {
		t.Fatalf("tmux title sequence = %q, want %q", got, want)
	}

	env := NewTerminalTitleEnvironment(map[string]string{"TMUX": "/tmp/tmux"})
	snapshot := terminalTitleSnapshot{Title: title, StableTitle: title}
	if cmd := newTerminalTitleManager(TerminalTitleOff, env, false).UpdateCmd(snapshot); cmd != nil {
		t.Fatalf("off mode title command = %T, want nil", cmd)
	}

	basicRaw := strings.Join(rawStringsFromCmd(newTerminalTitleManager(TerminalTitleBasic, env, false).UpdateCmd(snapshot)), "")
	if !strings.Contains(basicRaw, "\x1b]2;Fix Ctrl-C\\Exit\x07") {
		t.Fatalf("basic mode should emit raw OSC title, got %q", basicRaw)
	}
	if strings.Contains(basicRaw, "\x1bk") {
		t.Fatalf("basic mode should not emit provider-specific window sequence: %q", basicRaw)
	}

	smartRaw := strings.Join(rawStringsFromCmd(newTerminalTitleManager(TerminalTitleSmart, env, false).UpdateCmd(snapshot)), "")
	if !strings.Contains(smartRaw, "\x1b]2;Fix Ctrl-C\\Exit\x07") || !strings.Contains(smartRaw, "\x1bkFix Ctrl-C\\Exit\x1b\\") {
		t.Fatalf("smart mode should emit raw OSC and provider-specific window sequences, got %q", smartRaw)
	}
}

func rawStringsFromCmd(cmd tea.Cmd) []string {
	msg, ok := msgFromCmd(cmd)
	if !ok {
		return nil
	}
	switch msg := msg.(type) {
	case tea.RawMsg:
		return []string{fmt.Sprint(msg.Msg)}
	case tea.BatchMsg:
		var out []string
		for _, nested := range msg {
			out = append(out, rawStringsFromCmd(nested)...)
		}
		return out
	default:
		return nil
	}
}

func msgFromCmd(cmd tea.Cmd) (tea.Msg, bool) {
	if cmd == nil {
		return nil, false
	}
	ch := make(chan tea.Msg, 1)
	go func() {
		ch <- cmd()
	}()
	select {
	case msg := <-ch:
		return msg, true
	case <-time.After(10 * time.Millisecond):
		return nil, false
	}
}

func TestGhosttyProgressProvider(t *testing.T) {
	provider := newGhosttyProgressProvider(TerminalTitleEnvironment{})
	raw := strings.Join(rawStringsFromCmd(provider.UpdateCmd(terminalTitleSnapshot{InProgress: true})), "")
	if !strings.Contains(raw, ghosttyProgressIndeterminateSequence()) {
		t.Fatalf("progress start raw = %q, want indeterminate sequence", raw)
	}
	if cmd := provider.UpdateCmd(terminalTitleSnapshot{InProgress: true}); cmd != nil {
		if raw := strings.Join(rawStringsFromCmd(cmd), ""); raw != "" {
			t.Fatalf("progress provider should not spam while refresh tick is pending, got %q", raw)
		}
	}
	if handled, cmd := provider.HandleMsg(ghosttyProgressTickMsg{}, terminalTitleSnapshot{InProgress: true}); !handled {
		t.Fatal("ghostty progress tick was not handled")
	} else if raw := strings.Join(rawStringsFromCmd(cmd), ""); !strings.Contains(raw, ghosttyProgressIndeterminateSequence()) {
		t.Fatalf("progress refresh raw = %q, want indeterminate sequence", raw)
	}
	raw = strings.Join(rawStringsFromCmd(provider.UpdateCmd(terminalTitleSnapshot{})), "")
	if !strings.Contains(raw, ghosttyProgressClearSequence()) {
		t.Fatalf("progress clear raw = %q, want clear sequence", raw)
	}
}

func TestGhosttyProgressProviderWrapsTmuxPassthrough(t *testing.T) {
	provider := newGhosttyProgressProvider(NewTerminalTitleEnvironment(map[string]string{"TMUX": "/tmp/tmux"}))
	raw := strings.Join(rawStringsFromCmd(provider.UpdateCmd(terminalTitleSnapshot{InProgress: true})), "")
	want := "\x1bPtmux;\x1b\x1b]9;4;3\x07\x1b\\"
	if !strings.Contains(raw, want) {
		t.Fatalf("tmux progress raw = %q, want passthrough %q", raw, want)
	}
}

func TestGhosttyProgressManagerDisabledByDefault(t *testing.T) {
	env := NewTerminalTitleEnvironment(map[string]string{"TERM_PROGRAM": "ghostty"})
	cmd := newTerminalTitleManager(TerminalTitleSmart, env, false).UpdateCmd(terminalTitleSnapshot{
		Title:       "Working",
		StableTitle: "Working",
		InProgress:  true,
	})
	raw := strings.Join(rawStringsFromCmd(cmd), "")
	if strings.Contains(raw, "\x1b]9;4;") {
		t.Fatalf("manager with progress disabled emitted Ghostty progress sequence: %q", raw)
	}
}

func TestGhosttyProgressManagerEnabled(t *testing.T) {
	env := NewTerminalTitleEnvironment(map[string]string{"TERM_PROGRAM": "ghostty"})
	cmd := newTerminalTitleManager(TerminalTitleSmart, env, true).UpdateCmd(terminalTitleSnapshot{
		Title:       "Working",
		StableTitle: "Working",
		InProgress:  true,
	})
	raw := strings.Join(rawStringsFromCmd(cmd), "")
	if !strings.Contains(raw, ghosttyProgressIndeterminateSequence()) {
		t.Fatalf("manager with progress enabled raw = %q, want indeterminate sequence", raw)
	}
}

func TestTmuxPassthroughSequenceEscapesAllEscapes(t *testing.T) {
	got := tmuxPassthroughSequence("\x1b]9;4;3\x07\x1b\\")
	want := "\x1bPtmux;\x1b\x1b]9;4;3\x07\x1b\x1b\\\x1b\\"
	if got != want {
		t.Fatalf("tmux passthrough = %q, want %q", got, want)
	}
}

func TestTmuxRestoreRestoresManualWindowNameAndAutomaticRename(t *testing.T) {
	callsPath, restoreExec := withFakeTmuxCommandLog(t)
	defer restoreExec()

	provider := &tmuxTitleProvider{
		windowTarget:            "@7",
		originalAutomaticRename: "off",
		originalWindowName:      "manual window",
		windowRenameAttempted:   true,
	}
	provider.Restore()

	data, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read tmux calls: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "tmux rename-window -t @7 -- manual window\n") {
		t.Fatalf("restore did not restore original manual name, calls:\n%s", got)
	}
	if !strings.Contains(got, "tmux set-window-option -t @7 automatic-rename off\n") {
		t.Fatalf("restore did not restore automatic-rename off, calls:\n%s", got)
	}
}

func TestTmuxRestoreAutomaticRenameOnDoesNotForceOldName(t *testing.T) {
	callsPath, restoreExec := withFakeTmuxCommandLog(t)
	defer restoreExec()

	provider := &tmuxTitleProvider{
		windowTarget:            "@7",
		originalAutomaticRename: "on",
		originalWindowName:      "old auto name",
		windowRenameAttempted:   true,
	}
	provider.Restore()

	data, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("read tmux calls: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "rename-window") {
		t.Fatalf("restore should not force stale automatic window name, calls:\n%s", got)
	}
	if !strings.Contains(got, "tmux set-window-option -t @7 automatic-rename on\n") {
		t.Fatalf("restore did not restore automatic-rename on, calls:\n%s", got)
	}
}

func withFakeTmuxCommandLog(t *testing.T) (string, func()) {
	t.Helper()
	callsPath := t.TempDir() + "/tmux-calls"
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmdArgs := []string{"-c", `printf '%s\n' "$*" >> "$TMUX_CALLS"`, "sh", name}
		cmdArgs = append(cmdArgs, args...)
		cmd := exec.CommandContext(ctx, "sh", cmdArgs...)
		cmd.Env = append(os.Environ(), "TMUX_CALLS="+callsPath)
		return cmd
	}
	return callsPath, func() { execCommandContext = oldExec }
}

func TestGhosttyProgressProviderFactory(t *testing.T) {
	if providers := newGhosttyProgressProviders(TerminalTitleSmart, NewTerminalTitleEnvironment(map[string]string{"TERM_PROGRAM": "ghostty"}), true); len(providers) != 1 {
		t.Fatalf("ghostty TERM_PROGRAM providers = %d, want 1", len(providers))
	}
	if providers := newGhosttyProgressProviders(TerminalTitleSmart, NewTerminalTitleEnvironment(map[string]string{"TERM_PROGRAM": "ghostty"}), false); len(providers) != 0 {
		t.Fatalf("progress disabled ghostty providers = %d, want 0", len(providers))
	}
	if providers := newGhosttyProgressProviders(TerminalTitleBasic, NewTerminalTitleEnvironment(map[string]string{"TERM_PROGRAM": "ghostty"}), true); len(providers) != 0 {
		t.Fatalf("basic mode ghostty providers = %d, want 0", len(providers))
	}
	if providers := newGhosttyProgressProviders(TerminalTitleSmart, NewTerminalTitleEnvironment(nil), true); len(providers) != 0 {
		t.Fatalf("non-ghostty providers = %d, want 0", len(providers))
	}
}

func TestViewTerminalTitleModes(t *testing.T) {
	m := newTestChatModel(true)
	m.titleMode = TerminalTitleSmart
	m.ConfigureTerminalTitleEnvironment(NewTerminalTitleEnvironment(map[string]string{"TMUX": "/tmp/tmux"}))
	view := m.View().Content
	if strings.Contains(view, "\x1b]2;") || strings.Contains(view, "\x1bk") {
		t.Fatalf("View should not embed title control sequences; they should be emitted with tea.Raw, got %q", view[:min(len(view), 40)])
	}
}

func TestViewUsesCustomTerminalTitleFormat(t *testing.T) {
	env := NewTerminalTitleEnvironment(map[string]string{"DOCKER_CONTAINER_NAME": "worker-1"})
	m := newTestChatModel(true)
	m.titleMode = TerminalTitleBasic
	m.titleFormat = "[{{env.DOCKER_CONTAINER_NAME}}] {{agent}}/{{task}}/{{model}}"
	m.agentName = "developer"
	m.modelName = "fable"
	m.sess = &session.Session{ID: "custom-title", GeneratedShortTitle: "Format titles"}
	m.ConfigureTerminalTitleEnvironment(env)

	raw := strings.Join(rawStringsFromCmd(m.terminalTitleCmd()), "")
	if !strings.Contains(raw, "\x1b]2;[worker-1] developer/Format titles/fable\x07") {
		t.Fatalf("raw title command did not include custom formatted title: %q", raw)
	}
}

func TestMaybeGenerateSessionTitleCmd(t *testing.T) {
	m := newTestChatModel(false)
	store := &mockStore{}
	m.store = store
	m.sess = &session.Session{ID: "title-session", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Fix Ctrl C Exit","long_title":"Fixing the extra Ctrl C after chat turn completion","confidence":0.92}`)
	m.messages = []session.Message{
		{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "After a turn settles, the first Ctrl-C is swallowed instead of arming exit.", Sequence: 0},
		{SessionID: m.sess.ID, Role: llm.RoleAssistant, TextContent: "I'll fix the stale stream cancel function and add tests.", Sequence: 1},
	}

	cmd := m.maybeGenerateSessionTitleCmd()
	if cmd == nil {
		t.Fatal("expected title generation command")
	}
	msg := cmd()
	updated, followup := m.Update(msg)
	m = updated.(*Model)
	if followup != nil {
		if followupMsg := followup(); followupMsg != nil {
			t.Fatalf("unexpected follow-up message: %T", followupMsg)
		}
	}

	if store.updated == nil {
		t.Fatal("expected store.Update to be called")
	}
	if got := store.updated.GeneratedShortTitle; got != "Fix Ctrl C Exit" {
		t.Fatalf("stored GeneratedShortTitle = %q", got)
	}
	if got := m.sess.GeneratedShortTitle; got != "Fix Ctrl C Exit" {
		t.Fatalf("model GeneratedShortTitle = %q", got)
	}
	if m.sess.TitleSource != session.TitleSourceGenerated {
		t.Fatalf("TitleSource = %q, want generated", m.sess.TitleSource)
	}
}

func TestTitleFallbackTickGeneratesForCurrentUntitledSession(t *testing.T) {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.sess = &session.Session{ID: "fallback-title", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Fallback Handover Title","long_title":"Fallback title from long handover prompt","confidence":0.93}`)
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "A detailed handover asks the developer agent to implement session title fallback behavior.", Sequence: 0}}

	updated, cmd := m.Update(titleFallbackTickMsg{sessionID: m.sess.ID})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("expected fallback tick to start title generation")
	}
	if !m.titleGenerationInFlight {
		t.Fatal("expected title generation to be in flight")
	}

	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	if got := m.sess.GeneratedShortTitle; got != "Fallback Handover Title" {
		t.Fatalf("GeneratedShortTitle = %q, want fallback title", got)
	}
	if cmd := m.maybeGenerateSessionTitleCmd(); cmd != nil {
		t.Fatal("stream completion title generation should no-op after fallback success")
	}
}

func TestTitleFallbackTickIgnoresStaleSession(t *testing.T) {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.sess = &session.Session{ID: "current-title-session", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Should Not Run","long_title":"Should not run for stale fallback","confidence":0.9}`)
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Current session text.", Sequence: 0}}

	updated, cmd := m.Update(titleFallbackTickMsg{sessionID: "old-title-session"})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("stale fallback tick returned unexpected command")
	}
	if m.titleGenerationAttempts != 0 {
		t.Fatalf("title generation attempts = %d, want 0", m.titleGenerationAttempts)
	}
}

func TestTitleFallbackTickNoopsWhenGeneratedOrInFlight(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Model)
	}{
		{
			name: "already generated",
			configure: func(m *Model) {
				m.sess.GeneratedShortTitle = "Already Titled"
			},
		},
		{
			name: "generation in flight",
			configure: func(m *Model) {
				m.titleGenerationSessionID = m.sess.ID
				m.titleGenerationInFlight = true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.store = &mockStore{}
			m.sess = &session.Session{ID: "fallback-noop", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
			m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Should Not Run","long_title":"Should not run fallback","confidence":0.9}`)
			m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "Current session text.", Sequence: 0}}
			tt.configure(m)

			updated, cmd := m.Update(titleFallbackTickMsg{sessionID: m.sess.ID})
			m = updated.(*Model)
			if cmd != nil {
				t.Fatal("fallback tick returned unexpected command")
			}
		})
	}
}

func TestScheduleTitleFallbackCmdRequiresUntitledSession(t *testing.T) {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.sess = &session.Session{ID: "schedule-fallback", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.fastProvider = llm.NewMockProvider("fast")
	if cmd := m.scheduleTitleFallbackCmd(); cmd == nil {
		t.Fatal("expected fallback tick command for untitled session")
	}

	m.sess.GeneratedShortTitle = "Already Titled"
	if cmd := m.scheduleTitleFallbackCmd(); cmd != nil {
		t.Fatal("did not expect fallback tick command for already titled session")
	}
}

func TestMaybeGenerateSessionTitleRetriesOnlyAfterLaterTurn(t *testing.T) {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.sess = &session.Session{ID: "retry-title-session", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":null,"long_title":null,"confidence":0}`)
	m.messages = []session.Message{{SessionID: m.sess.ID, Role: llm.RoleUser, TextContent: "hi", Sequence: 0}}

	cmd := m.maybeGenerateSessionTitleCmd()
	if cmd == nil {
		t.Fatal("expected first title generation attempt")
	}
	_, _ = m.Update(cmd())
	if cmd := m.maybeGenerateSessionTitleCmd(); cmd != nil {
		t.Fatal("should not retry without a later turn")
	}

	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse(`{"short_title":"Retry Title Works","long_title":"Generating a title after a later useful turn","confidence":0.9}`)
	m.messages = append(m.messages, session.Message{SessionID: m.sess.ID, Role: llm.RoleAssistant, TextContent: "A later useful answer.", Sequence: 1})
	if cmd := m.maybeGenerateSessionTitleCmd(); cmd == nil {
		t.Fatal("expected one retry after a later turn")
	}
}

func TestTitleGeneratedMsgIgnoresSwitchedSession(t *testing.T) {
	m := newTestChatModel(false)
	m.sess = &session.Session{ID: "new-session", Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	m.titleGenerationSessionID = "old-session"
	m.titleGenerationInFlight = true

	updated, cmd := m.Update(titleGeneratedMsg{
		sessionID:   "old-session",
		candidate:   sessiontitle.Candidate{ShortTitle: "Old Title", LongTitle: "Old title should not apply"},
		generatedAt: time.Now(),
		basisMsgSeq: 1,
	})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("stale title result returned unexpected command")
	}
	if m.titleGenerationInFlight {
		t.Fatal("stale result for generation session should clear in-flight flag")
	}
	if got := m.sess.GeneratedShortTitle; got != "" {
		t.Fatalf("switched session title = %q, want unchanged", got)
	}
}
