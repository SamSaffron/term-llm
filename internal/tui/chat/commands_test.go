package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestAllCommandsIncludesCompact(t *testing.T) {
	commands := AllCommands()
	found := false
	for _, cmd := range commands {
		if cmd.Name == "compact" {
			found = true
			if cmd.Usage != "/compact" {
				t.Errorf("compact usage = %q, want /compact", cmd.Usage)
			}
			break
		}
	}
	if !found {
		t.Error("AllCommands() should include 'compact' command")
	}
}

func TestFilterCommandsMatchesCompact(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"compact", true},
		{"comp", true},
		{"compa", true},
		{"xyz", false},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			results := FilterCommands(tt.query)
			found := false
			for _, cmd := range results {
				if cmd.Name == "compact" {
					found = true
					break
				}
			}
			if found != tt.want {
				t.Errorf("FilterCommands(%q) found compact = %v, want %v", tt.query, found, tt.want)
			}
		})
	}
}

func TestAllCommandsRemovesLoadAndKeepsResume(t *testing.T) {
	commands := AllCommands()

	hasLoad := false
	hasResume := false
	for _, cmd := range commands {
		if cmd.Name == "load" {
			hasLoad = true
		}
		if cmd.Name == "resume" {
			hasResume = true
		}
	}

	if hasLoad {
		t.Error("AllCommands() should not include 'load'")
	}
	if !hasResume {
		t.Error("AllCommands() should include 'resume'")
	}
}

// mockStore implements session.Store for testing resume behavior.
type mockStore struct {
	session.NoopStore
	sessions   map[string]*session.Session
	messages   map[string][]session.Message
	summaries  []session.SessionSummary
	msgErr     error
	updated    *session.Session
	updateErr  error
	compacted  []session.Message
	compactErr error
}

func (s *mockStore) Get(_ context.Context, id string) (*session.Session, error) {
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return nil, nil
}

func (s *mockStore) GetByPrefix(_ context.Context, prefix string) (*session.Session, error) {
	if sess, ok := s.sessions[prefix]; ok {
		return sess, nil
	}
	return nil, nil
}

func (s *mockStore) GetMessages(_ context.Context, sessionID string, _, _ int) ([]session.Message, error) {
	if s.msgErr != nil {
		return nil, s.msgErr
	}
	return s.messages[sessionID], nil
}

func (s *mockStore) List(_ context.Context, _ session.ListOptions) ([]session.SessionSummary, error) {
	return s.summaries, nil
}

func (s *mockStore) Update(_ context.Context, sess *session.Session) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updated = sess
	return nil
}

func (s *mockStore) CompactMessages(_ context.Context, _ string, messages []session.Message) error {
	if s.compactErr != nil {
		return s.compactErr
	}
	s.compacted = append([]session.Message(nil), messages...)
	return nil
}

// newCmdTestModel creates a minimal Model suitable for testing command functions.
func newCmdTestModel(store session.Store) *Model {
	styles := ui.DefaultStyles()
	ta := textarea.New()
	return &Model{
		width:    80,
		height:   24,
		textarea: ta,
		styles:   styles,
		dialog:   NewDialogModel(styles),
		store:    store,
	}
}

func TestCmdResume_DirectResumeRequestsRelaunch(t *testing.T) {
	sessionID := "sess-resume-1"
	sess := &session.Session{ID: sessionID, Number: 1, Name: "my session"}
	msgs := []session.Message{
		{ID: 1, SessionID: sessionID, Role: "user", TextContent: "hello"},
		{ID: 2, SessionID: sessionID, Role: "assistant", TextContent: "hi"},
	}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
		messages: map[string][]session.Message{sessionID: msgs},
	}
	m := newCmdTestModel(store)
	result, _ := m.cmdResume([]string{sessionID})
	rm := result.(*Model)

	if !rm.quitting {
		t.Fatal("expected cmdResume to request chat relaunch via quit")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
	if len(rm.messages) != 0 {
		t.Errorf("expected no in-place message load, got %d messages", len(rm.messages))
	}
	if len(msgs) == 0 {
		t.Fatal("test fixture expected non-empty source messages")
	}
}

func TestCmdResume_DoesNotMutateViewStateInPlace(t *testing.T) {
	sessionID := "sess-cache-bug"
	sess := &session.Session{ID: sessionID, Number: 2}
	msgs := []session.Message{
		{ID: 1, SessionID: sessionID},
		{ID: 2, SessionID: sessionID},
	}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
		messages: map[string][]session.Message{sessionID: msgs},
	}
	m := newCmdTestModel(store)

	// Simulate stale cache with the same message count as the incoming session.
	// Before the fix, historyValid stays true because the count check is a false positive.
	m.viewCache.historyValid = true
	m.viewCache.historyMsgCount = len(msgs) // same count — the bug
	m.viewCache.contentVersion = 5

	result, _ := m.cmdResume([]string{sessionID})
	rm := result.(*Model)

	if !rm.viewCache.historyValid {
		t.Error("expected in-place view cache to be untouched because resume now relaunches chat")
	}
	if rm.viewCache.contentVersion != 5 {
		t.Errorf("expected contentVersion to remain unchanged, got %d", rm.viewCache.contentVersion)
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestCmdResume_NoArgs_OpensEmbeddedSessionsBrowser(t *testing.T) {
	sessionID := "sess-resume-picker-1"
	store := &mockStore{
		summaries: []session.SessionSummary{
			{
				ID:           sessionID,
				Number:       7,
				Name:         "session seven",
				MessageCount: 3,
				Model:        "claude-sonnet-20250101",
				Summary:      "Discussed release notes and rollout checks",
				UpdatedAt:    time.Now().Add(-5 * time.Minute),
			},
		},
	}
	m := newCmdTestModel(store)
	m.setTextareaValue("draft note")

	result, _ := m.cmdResume([]string{})
	rm := result.(*Model)

	if !rm.resumeBrowserMode {
		t.Fatal("expected resume browser mode to be active")
	}
	if rm.resumeBrowserModel == nil {
		t.Fatal("expected embedded resume browser model to be initialized")
	}
	if rm.dialog.IsOpen() {
		t.Fatal("expected generic dialog to remain closed")
	}
	if got := rm.textarea.Value(); got != "draft note" {
		t.Fatalf("expected draft input to be preserved, got %q", got)
	}
}

func TestUpdateCompletions_ModelCommandShowsModelMatches(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestChatModel(false)
	m.config = &config.Config{DefaultProvider: "anthropic"}
	m.completions.Show()
	m.setTextareaValue("/model claude-sonnet")

	m.updateCompletions()

	if len(m.completions.filtered) == 0 {
		t.Fatal("expected /model completions to include matching models")
	}

	for _, item := range m.completions.filtered {
		if strings.HasPrefix(item.Name, "model anthropic:claude-sonnet") {
			return
		}
	}

	t.Fatalf("expected an anthropic claude-sonnet completion, got %#v", m.completions.filtered)
}

func TestUpdateCompletions_ModelCommandFallbackFuzzyMatchesProviderAndModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestChatModel(false)
	m.config = &config.Config{DefaultProvider: "chatgpt"}
	m.completions.Show()
	m.setTextareaValue("/model chat5.4")

	m.updateCompletions()

	if len(m.completions.filtered) == 0 {
		t.Fatal("expected fuzzy /model completions to include matching models")
	}

	for _, item := range m.completions.filtered {
		if item.Name == "model chatgpt:gpt-5.4" {
			return
		}
	}

	t.Fatalf("expected fuzzy completion to include chatgpt:gpt-5.4, got %#v", m.completions.filtered)
}

func TestUpdateCompletions_HandoverModelOverrideUsesSameProviderModelMatcher(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := newTestChatModel(false)
	m.config = &config.Config{DefaultProvider: "chatgpt"}
	m.agentLister = func(cfg *config.Config) ([]string, error) {
		return []string{"developer"}, nil
	}
	m.completions.Show()
	m.setTextareaValue("/handover @developer chat5.4")

	m.updateCompletions()

	if len(m.completions.filtered) == 0 {
		t.Fatal("expected /handover completions to include matching provider:model overrides")
	}

	for _, item := range m.completions.filtered {
		if item.Name == "handover @developer chatgpt:gpt-5.4" {
			return
		}
	}

	t.Fatalf("expected handover completion to include chatgpt:gpt-5.4, got %#v", m.completions.filtered)
}

func TestResolveProviderModelArg_FuzzyMatchSharedByCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	resolved, ok := resolveProviderModelArg("chat5.4", &config.Config{DefaultProvider: "chatgpt"}, "")
	if !ok {
		t.Fatal("expected shorthand provider/model query to resolve")
	}
	if resolved != "chatgpt:gpt-5.4" {
		t.Fatalf("resolveProviderModelArg() = %q, want %q", resolved, "chatgpt:gpt-5.4")
	}
}

func TestResolveProviderModelArg_PrefersRecentModels(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if err := config.RecordModelUse("openai:gpt-5.4"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}

	resolved, ok := resolveProviderModelArg("gpt-5.4", &config.Config{DefaultProvider: "chatgpt"}, "")
	if !ok {
		t.Fatal("expected gpt-5.4 to resolve")
	}
	if resolved != "openai:gpt-5.4" {
		t.Fatalf("resolveProviderModelArg() = %q, want %q", resolved, "openai:gpt-5.4")
	}
}

func TestProviderModelCompletionItems_PrefersRecentModels(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if err := config.RecordModelUse("openai:gpt-5.4"); err != nil {
		t.Fatalf("RecordModelUse: %v", err)
	}

	items := providerModelCompletionItems("model ", "gpt-5.4", &config.Config{DefaultProvider: "chatgpt"})
	if len(items) == 0 {
		t.Fatal("expected provider model completions")
	}
	if items[0].Name != "model openai:gpt-5.4" {
		t.Fatalf("first completion = %q, want %q", items[0].Name, "model openai:gpt-5.4")
	}
}

func TestSendMessage_RecordsCurrentModelUse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	m := newTestChatModel(false)
	m.providerKey = "claude-bin"
	m.modelName = "opus"

	_, _ = m.sendMessage("hello")
	config.FlushModelHistoryAsync()

	history, err := config.LoadModelHistory()
	if err != nil {
		t.Fatalf("LoadModelHistory: %v", err)
	}
	if len(history) == 0 || history[0].Model != "claude-bin:opus" {
		t.Fatalf("expected claude-bin:opus in model history, got %v", history)
	}
}

func TestSwitchModel_UpdatesSessionMetadata(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{
		ID:       "sess-model-switch-1",
		Provider: "OpenAI (old-model)",
		Model:    "old-model",
	}
	m.providerName = "OpenAI (old-model)"
	m.modelName = "old-model"
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)

	result, _ := m.switchModel("debug:fast")
	rm := result.(*Model)

	if rm.sess.Provider != "debug:fast" {
		t.Fatalf("expected session provider to be updated to %q, got %q", "debug:fast", rm.sess.Provider)
	}
	if rm.sess.Model != "fast" {
		t.Fatalf("expected session model to be updated to %q, got %q", "fast", rm.sess.Model)
	}
	if store.updated == nil {
		t.Fatal("expected switchModel to persist session changes")
	}
}

func TestResumeFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"just now", now.Add(-30 * time.Second), "just now"},
		{"minutes", now.Add(-5 * time.Minute), "5m ago"},
		{"hours", now.Add(-2 * time.Hour), "2h ago"},
		{"days", now.Add(-2 * 24 * time.Hour), "2d ago"},
		{"old date", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), "Jan 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resumeFormatAge(tt.t)
			if got != tt.want {
				t.Errorf("resumeFormatAge() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCmdHandover_TargetScriptIsDeferredUntilConfirmation(t *testing.T) {
	agentDir := t.TempDir()
	scriptPath := filepath.Join(agentDir, "handover.sh")
	markerPath := filepath.Join(agentDir, "marker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'generated handover'\nprintf 'script warning' >&2\ntouch marker\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	targetAgent := &agents.Agent{
		Name:           "target",
		SystemPrompt:   "You are target.",
		HandoverScript: "./handover.sh",
		Source:         agents.SourceLocal,
		SourcePath:     agentDir,
	}

	m := newCmdTestModel(&mockStore{})
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-preview", CreatedAt: time.Now()}
	m.agentName = "source"
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetYoloMode(true)
	m.SetHandoverApprovalManager(mgr)

	result, cmd := m.cmdHandover([]string{"@target"})
	if cmd == nil {
		t.Fatal("expected preview command")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("script should not run before preview, stat err = %v", err)
	}

	updated, _ := result.(*Model).Update(cmd())
	rm := updated.(*Model)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("script should remain deferred until confirmation, stat err = %v", err)
	}
	if rm.pendingHandover == nil || rm.handoverPreview == nil {
		t.Fatal("expected pending handover preview to be shown")
	}
	if !strings.Contains(rm.pendingHandover.result.Document, "will run only after you confirm") {
		t.Fatalf("expected deferred-script preview, got %q", rm.pendingHandover.result.Document)
	}
}

func TestHandoverScriptCmd_UsesAgentSourcePathAndPersistsResult(t *testing.T) {
	agentDir := t.TempDir()
	scriptPath := filepath.Join(agentDir, "handover.sh")
	markerPath := filepath.Join(agentDir, "marker")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf 'generated handover'\nprintf 'script warning' >&2\ntouch marker\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	targetAgent := &agents.Agent{
		Name:           "target",
		SystemPrompt:   "You are target.",
		DefaultPrompt:  "  review changes  ",
		HandoverScript: "./handover.sh",
		Source:         agents.SourceLocal,
		SourcePath:     agentDir,
	}
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-confirm", CreatedAt: time.Now()}
	m.agentName = "source"
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetYoloMode(true)
	m.SetHandoverApprovalManager(mgr)
	m.pendingHandover = &handoverDoneMsg{
		result:    llm.HandoverFromFile("placeholder", targetAgent.SystemPrompt, "source", targetAgent.Name),
		agentName: targetAgent.Name,
	}

	msg := handoverScriptCmd(context.Background(), mgr, targetAgent, "source", targetAgent, "", true, "please focus on tests")()
	updated, quitCmd := m.Update(msg)
	rm := updated.(*Model)

	if quitCmd == nil {
		t.Fatal("expected handover to request quit after confirmation")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected deferred script to run after confirmation: %v", err)
	}
	if !rm.quitting {
		t.Fatal("expected model to request restart after confirmed handover")
	}
	if got := rm.RequestedResumeSessionID(); got != "sess-handover-confirm" {
		t.Fatalf("RequestedResumeSessionID() = %q, want %q", got, "sess-handover-confirm")
	}
	if got := rm.RequestedHandoverAutoSend(); got != "review changes" {
		t.Fatalf("RequestedHandoverAutoSend() = %q, want %q", got, "review changes")
	}
	if len(store.compacted) == 0 {
		t.Fatal("expected compacted handover messages to be persisted")
	}
	var combined strings.Builder
	for _, msg := range store.compacted {
		if msg.TextContent == "" {
			continue
		}
		combined.WriteString(msg.TextContent)
		combined.WriteString("\n")
	}
	text := combined.String()
	if !strings.Contains(text, "generated handover") {
		t.Fatalf("expected script output in persisted messages, got %q", text)
	}
	if strings.Contains(text, "script warning") {
		t.Fatalf("expected stderr to be excluded from persisted messages, got %q", text)
	}
	if !strings.Contains(text, "Additional Instructions") || !strings.Contains(text, "please focus on tests") {
		t.Fatalf("expected additional instructions in persisted messages, got %q", text)
	}
}

func TestRunHandoverScript_CancelKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancellation test is unix-specific")
	}

	agentDir := t.TempDir()
	scriptPath := filepath.Join(agentDir, "handover.sh")
	childPIDPath := filepath.Join(agentDir, "child.pid")
	script := "#!/bin/sh\nchild_pid_file=\"$1\"\n(while :; do sleep 1; done) &\necho $! > \"$child_pid_file\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	agent := &agents.Agent{
		Name:       "target",
		Source:     agents.SourceLocal,
		SourcePath: agentDir,
	}
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetYoloMode(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := runHandoverScript(ctx, mgr, agent, fmt.Sprintf("./handover.sh %q", childPIDPath))
		errCh <- err
	}()

	childPID := waitForHandoverChildPID(t, childPIDPath)
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runHandoverScript error = %v, want context.Canceled", err)
	}
	waitForProcessExit(t, childPID)
}

func waitForHandoverChildPID(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pidText := strings.TrimSpace(string(data))
			if pidText != "" {
				pid, convErr := strconv.Atoi(pidText)
				if convErr != nil {
					t.Fatalf("parse child pid %q: %v", pidText, convErr)
				}
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for child pid file %s", path)
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err != nil && errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for process %d to exit", pid)
}
