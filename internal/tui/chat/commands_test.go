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
	tea "charm.land/bubbletea/v2"
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
	sessions        map[string]*session.Session
	getErr          error
	messages        map[string][]session.Message
	summaries       []session.SessionSummary
	msgErr          error
	updated         *session.Session
	updateErr       error
	created         []*session.Session
	createErr       error
	added           []session.Message
	addErr          error
	currentID       string
	setCurrentErr   error
	deleted         []string
	deleteErr       error
	statusUpdates   []statusUpdate
	updateStatusErr error
	compacted       []session.Message
	compactSession  string
	compactErr      error
}

type statusUpdate struct {
	id     string
	status session.SessionStatus
}

func (s *mockStore) Get(_ context.Context, id string) (*session.Session, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
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

func (s *mockStore) GetMessagesFrom(_ context.Context, sessionID string, fromSeq int) ([]session.Message, error) {
	if s.msgErr != nil {
		return nil, s.msgErr
	}
	var filtered []session.Message
	for _, msg := range s.messages[sessionID] {
		if msg.Sequence >= fromSeq {
			filtered = append(filtered, msg)
		}
	}
	return filtered, nil
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

func (s *mockStore) Create(_ context.Context, sess *session.Session) error {
	if s.createErr != nil {
		return s.createErr
	}
	if sess.ID == "" {
		sess.ID = session.NewID()
	}
	if s.sessions == nil {
		s.sessions = make(map[string]*session.Session)
	}
	cp := *sess
	s.created = append(s.created, &cp)
	s.sessions[sess.ID] = sess
	return nil
}

func (s *mockStore) ensureMessages() {
	if s.messages == nil {
		s.messages = make(map[string][]session.Message)
	}
}

func (s *mockStore) AddMessage(_ context.Context, sessionID string, msg *session.Message) error {
	if s.addErr != nil {
		return s.addErr
	}
	s.ensureMessages()
	msg.SessionID = sessionID
	if msg.Sequence < 0 {
		msg.Sequence = len(s.messages[sessionID])
	}
	cp := *msg
	s.added = append(s.added, cp)
	s.messages[sessionID] = append(s.messages[sessionID], cp)
	return nil
}

func (s *mockStore) SetCurrent(_ context.Context, sessionID string) error {
	if s.setCurrentErr != nil {
		return s.setCurrentErr
	}
	s.currentID = sessionID
	return nil
}

func (s *mockStore) Delete(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, id)
	if s.sessions != nil {
		delete(s.sessions, id)
	}
	delete(s.messages, id)
	return nil
}

func (s *mockStore) UpdateStatus(_ context.Context, id string, status session.SessionStatus) error {
	if s.updateStatusErr != nil {
		return s.updateStatusErr
	}
	s.statusUpdates = append(s.statusUpdates, statusUpdate{id: id, status: status})
	return nil
}

func (s *mockStore) CompactMessages(_ context.Context, sessionID string, messages []session.Message) error {
	if s.compactErr != nil {
		return s.compactErr
	}
	s.ensureMessages()
	startSeq := len(s.messages[sessionID])
	for i := range messages {
		messages[i].SessionID = sessionID
		messages[i].Sequence = startSeq + i
	}
	if sess := s.sessions[sessionID]; sess != nil {
		sess.CompactionSeq = startSeq
	}
	s.compactSession = sessionID
	s.compacted = append([]session.Message(nil), messages...)
	s.messages[sessionID] = append(s.messages[sessionID], messages...)
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

func TestCmdReload_QuitsWithoutPrintingStatus(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.sess = &session.Session{ID: "sess-reload-1"}

	result, cmd := m.cmdReload()
	rm := result.(*Model)

	if !rm.quitting {
		t.Fatal("expected /reload to request quit")
	}
	if !rm.WantsReload() {
		t.Fatal("expected /reload to mark reload requested")
	}
	if got := rm.ReloadSessionID(); got != "sess-reload-1" {
		t.Fatalf("ReloadSessionID() = %q, want %q", got, "sess-reload-1")
	}
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	if _, isBatch := cmd().(tea.BatchMsg); isBatch {
		t.Fatal("expected /reload to quit without printing a status line")
	}
}

func TestCmdCompress_StartDoesNotPrintStatusLine(t *testing.T) {
	m := newTestChatModel(false)
	m.messages = []session.Message{
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "hello"}},
			TextContent: "hello",
			CreatedAt:   time.Now(),
		},
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "hi"}},
			TextContent: "hi",
			CreatedAt:   time.Now(),
		},
	}

	result, cmd := m.cmdCompress()
	rm := result.(*Model)

	if !rm.streaming {
		t.Fatal("expected compaction to enter streaming mode")
	}
	if got := rm.phase; got != "Compacting" {
		t.Fatalf("phase = %q, want %q", got, "Compacting")
	}
	if cmd == nil {
		t.Fatal("expected compaction start command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected compaction to return a batch command, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("expected compaction start batch to avoid print command, got %d entries", len(batch))
	}
}

func TestUpdate_CompactDone_CancelUsesMutedFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(compactDoneMsg{err: context.Canceled})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Compaction cancelled." {
		t.Fatalf("expected compact cancel footer message, got %q", got)
	}
	if got := rm.footerMessageTone; got != "muted" {
		t.Fatalf("expected compact cancel footer tone muted, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected compact cancel footer clear command")
	}
}

func TestUpdate_CompactDone_SuccessUsesFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(compactDoneMsg{result: &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("summary")}}})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Conversation compacted." {
		t.Fatalf("expected compact success footer message, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected compact success footer clear command")
	}
}

func TestUpdate_CompactDone_PersistErrorDoesNotMutateMemory(t *testing.T) {
	store := &mockStore{compactErr: errors.New("disk full")}
	m := newTestChatModel(false)
	m.store = store
	m.sess = &session.Session{ID: "sess-compact-fail"}
	m.streaming = true
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("old user"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("old assistant"), 1),
	}
	oldMessages := append([]session.Message(nil), m.messages...)

	result, cmd := m.Update(compactDoneMsg{result: &llm.CompactionResult{NewMessages: []llm.Message{llm.UserText("summary")}}})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Compaction finished, but saving failed: disk full" {
		t.Fatalf("expected compact persist error footer message, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected compact persist error footer clear command")
	}
	if rm.compactionIdx != 0 {
		t.Fatalf("compactionIdx changed on persist failure: got %d want 0", rm.compactionIdx)
	}
	if len(rm.messages) != len(oldMessages) {
		t.Fatalf("messages changed on persist failure: got %d want %d", len(rm.messages), len(oldMessages))
	}
	for i := range oldMessages {
		if rm.messages[i].TextContent != oldMessages[i].TextContent || rm.messages[i].SessionID != oldMessages[i].SessionID {
			t.Fatalf("message %d changed on persist failure: got %#v want %#v", i, rm.messages[i], oldMessages[i])
		}
	}
}

func TestCmdHandover_StartDoesNotPrintStatusLine(t *testing.T) {
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.sess = &session.Session{ID: "sess-handover-start", CreatedAt: time.Now()}
	m.messages = []session.Message{
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "please continue"}},
			TextContent: "please continue",
			CreatedAt:   time.Now(),
		},
		{
			SessionID:   m.sess.ID,
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Working on it."}},
			TextContent: "Working on it.",
			CreatedAt:   time.Now(),
		},
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return &agents.Agent{Name: name, SystemPrompt: "You are target."}, nil
	}

	result, cmd := m.cmdHandover([]string{"@target"})
	rm := result.(*Model)

	if !rm.streaming {
		t.Fatal("expected handover generation to enter streaming mode")
	}
	if got := rm.phase; got != "Handover" {
		t.Fatalf("phase = %q, want %q", got, "Handover")
	}
	if cmd == nil {
		t.Fatal("expected handover start command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected handover to return a batch command, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("expected handover start batch to avoid print command, got %d entries", len(batch))
	}
}

func TestStartHandoverScriptHandover_DoesNotPrintStatusLine(t *testing.T) {
	m := newTestChatModel(false)
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target.", HandoverScript: "./handover.sh"}

	result, cmd := m.startHandoverScriptHandover(targetAgent, "source", targetAgent, "", false, "")
	rm := result.(*Model)

	if !rm.streaming {
		t.Fatal("expected script-backed handover to enter streaming mode")
	}
	if got := rm.phase; got != "Handover" {
		t.Fatalf("phase = %q, want %q", got, "Handover")
	}
	if cmd == nil {
		t.Fatal("expected script-backed handover command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected script-backed handover to return a batch command, got %T", cmd())
	}
	if len(batch) != 3 {
		t.Fatalf("expected script-backed handover batch to avoid print command, got %d entries", len(batch))
	}
}

func TestUpdate_HandoverDone_CancelUsesMutedFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(handoverDoneMsg{err: context.Canceled, agentName: "target"})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover cancelled." {
		t.Fatalf("expected handover cancel footer message, got %q", got)
	}
	if got := rm.footerMessageTone; got != "muted" {
		t.Fatalf("expected handover cancel footer tone muted, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected handover cancel footer clear command")
	}
}

func TestUpdate_HandoverDone_ErrorUsesFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, cmd := m.Update(handoverDoneMsg{err: errors.New("boom"), agentName: "target"})
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed: boom" {
		t.Fatalf("expected handover error footer message, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected handover error footer clear command")
	}
}

func TestExecuteHandover_ResolveErrorUsesFooterMessage(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.config = &config.Config{}
	m.pendingHandover = &handoverDoneMsg{agentName: "target"}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return nil, errors.New("missing target")
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed to resolve target agent: missing target" {
		t.Fatalf("expected executeHandover resolve error in footer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected executeHandover error footer clear command")
	}
}

func TestExecuteHandover_CreateErrorUsesFooterMessage(t *testing.T) {
	store := &mockStore{createErr: errors.New("disk full")}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-handover-persist"}
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target."}
	expectedResult := llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name)
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    expectedResult,
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed to persist: disk full" {
		t.Fatalf("expected executeHandover persist error in footer, got %q", got)
	}
	if got := rm.footerMessageTone; got != "error" {
		t.Fatalf("expected executeHandover persist error tone error, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected executeHandover persist error footer clear command")
	}
	if len(store.compacted) != 0 {
		t.Fatalf("handover must not compact source session, compacted %d messages", len(store.compacted))
	}
	if len(store.statusUpdates) != 0 {
		t.Fatalf("source session should not be marked complete on create failure, got %#v", store.statusUpdates)
	}
	if rm.pendingHandover == nil {
		t.Fatal("expected pending handover to remain available for retry")
	}
}

func TestExecuteHandover_CreatesNewIsolatedSessionAndRequestsResume(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	oldSess := &session.Session{
		ID:          "old-session",
		Provider:    "Old Provider",
		ProviderKey: "old-provider",
		Model:       "old-model",
		Agent:       "source",
		Search:      false,
		Tools:       "read_file",
		MCP:         "old-mcp",
		CreatedAt:   time.Now().Add(-time.Hour),
	}
	m.sess = oldSess
	m.providerName = "Old Provider"
	m.providerKey = "old-provider"
	m.modelName = "old-model"
	m.messages = []session.Message{
		*session.NewMessage(oldSess.ID, llm.UserText("old user content"), 0),
		*session.NewMessage(oldSess.ID, llm.AssistantText("old assistant content"), 1),
	}
	oldMessages := append([]session.Message(nil), m.messages...)

	targetAgent := &agents.Agent{
		Name:          "target",
		SystemPrompt:  "You are target.",
		Provider:      "gemini",
		Model:         "gemini-2.5-pro",
		Search:        true,
		DefaultPrompt: "Continue from handover.",
		Tools:         agents.ToolsConfig{Enabled: []string{"read_file", "edit_file"}},
		MCP:           []agents.MCPConfig{{Name: "server-a"}, {Name: "server-b"}},
	}
	expectedResult := llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name)
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    expectedResult,
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		if name != "target" {
			t.Fatalf("resolver called with %q, want target", name)
		}
		return targetAgent, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if cmd == nil {
		t.Fatal("expected executeHandover to quit for relaunch")
	}
	if !rm.quitting {
		t.Fatal("expected model to be quitting for handover relaunch")
	}
	if len(store.created) != 1 {
		t.Fatalf("expected exactly one new session, got %d", len(store.created))
	}
	newSess := store.created[0]
	if newSess.ID == "" || newSess.ID == oldSess.ID {
		t.Fatalf("expected a fresh session ID distinct from %q, got %q", oldSess.ID, newSess.ID)
	}
	if got := rm.RequestedResumeSessionID(); got != newSess.ID {
		t.Fatalf("resume session ID = %q, want new session %q", got, newSess.ID)
	}
	if store.currentID != newSess.ID {
		t.Fatalf("current session = %q, want %q", store.currentID, newSess.ID)
	}
	if newSess.Agent != "target" {
		t.Fatalf("new session agent = %q, want target", newSess.Agent)
	}
	if !newSess.Search {
		t.Fatal("expected target search setting on new session")
	}
	if newSess.Tools != "read_file,edit_file" {
		t.Fatalf("new session tools = %q, want read_file,edit_file", newSess.Tools)
	}
	if newSess.MCP != "server-a,server-b" {
		t.Fatalf("new session MCP = %q, want server-a,server-b", newSess.MCP)
	}
	if newSess.ProviderKey != "gemini" || newSess.Model != "gemini-2.5-pro" {
		t.Fatalf("new provider/model = %q/%q, want gemini/gemini-2.5-pro", newSess.ProviderKey, newSess.Model)
	}
	if newSess.Provider == "Old Provider" || newSess.Provider == "" {
		t.Fatalf("new provider label = %q, want target provider label", newSess.Provider)
	}
	if newSess.Status != session.StatusActive {
		t.Fatalf("new session status = %q, want active", newSess.Status)
	}
	if newSess.CompactionSeq != -1 {
		t.Fatalf("new session compaction seq = %d, want -1", newSess.CompactionSeq)
	}
	if len(store.statusUpdates) != 1 || store.statusUpdates[0].id != oldSess.ID || store.statusUpdates[0].status != session.StatusComplete {
		t.Fatalf("expected source session to be marked complete, got %#v", store.statusUpdates)
	}
	if len(store.compacted) != 0 || store.compactSession != "" {
		t.Fatalf("handover must not compact any session, compactSession=%q compacted=%d", store.compactSession, len(store.compacted))
	}
	if len(store.added) != len(expectedResult.NewMessages) {
		t.Fatalf("expected %d reconstructed messages to be added, got %d", len(expectedResult.NewMessages), len(store.added))
	}
	for i, msg := range store.added {
		if msg.SessionID != newSess.ID {
			t.Fatalf("added message %d session ID = %q, want %q", i, msg.SessionID, newSess.ID)
		}
	}
	if store.added[0].Role != llm.RoleSystem || store.added[0].TextContent != targetAgent.SystemPrompt {
		t.Fatalf("first added message = role %q text %q, want target system prompt", store.added[0].Role, store.added[0].TextContent)
	}
	if !strings.Contains(store.added[1].TextContent, "handover doc") || !strings.Contains(store.added[1].TextContent, "@source -> @target") {
		t.Fatalf("handover message missing document/source-target prefix: %q", store.added[1].TextContent)
	}
	if len(rm.messages) != len(oldMessages) {
		t.Fatalf("old in-memory messages length changed: got %d want %d", len(rm.messages), len(oldMessages))
	}
	for i := range oldMessages {
		if rm.messages[i].SessionID != oldMessages[i].SessionID || rm.messages[i].TextContent != oldMessages[i].TextContent {
			t.Fatalf("old in-memory message %d changed: got %#v want %#v", i, rm.messages[i], oldMessages[i])
		}
	}
	if oldSess.Agent != "source" || oldSess.ProviderKey != "old-provider" || oldSess.Model != "old-model" || oldSess.Tools != "read_file" || oldSess.MCP != "old-mcp" {
		t.Fatalf("source session metadata was mutated: %#v", oldSess)
	}
	if rm.pendingHandover != nil {
		t.Fatal("expected successful handover to clear pending handover")
	}
}

func TestExecuteHandover_ProviderOverrideStoredOnNewSession(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "old-session", ProviderKey: "old-provider", Model: "old-model"}
	m.providerName = "Old Provider"
	m.providerKey = "old-provider"
	m.modelName = "old-model"
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target.", Provider: "gemini", Model: "gemini-2.5-pro"}
	m.pendingHandover = &handoverDoneMsg{
		agentName:   "target",
		providerStr: "openai:gpt-5",
		result:      llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name),
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	result, _ := m.executeHandover()
	rm := result.(*Model)

	if rm.footerMessage != "" {
		t.Fatalf("unexpected footer error: %s", rm.footerMessage)
	}
	if len(store.created) != 1 {
		t.Fatalf("expected one created session, got %d", len(store.created))
	}
	newSess := store.created[0]
	if newSess.ProviderKey != "openai" || newSess.Model != "gpt-5" {
		t.Fatalf("new provider/model = %q/%q, want openai/gpt-5", newSess.ProviderKey, newSess.Model)
	}
	if newSess.Provider == "Old Provider" || !strings.Contains(newSess.Provider, "openai") {
		t.Fatalf("new provider label = %q, want openai label", newSess.Provider)
	}
}

func TestExecuteHandover_AddMessageErrorUsesFooterMessage(t *testing.T) {
	store := &mockStore{addErr: errors.New("write failed")}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "old-session"}
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "You are target."}
	m.pendingHandover = &handoverDoneMsg{
		agentName: "target",
		result:    llm.HandoverFromFile("handover doc", targetAgent.SystemPrompt, "source", targetAgent.Name),
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return targetAgent, nil
	}

	result, cmd := m.executeHandover()
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Handover failed to persist: write failed" {
		t.Fatalf("expected add-message error in footer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected footer clear command")
	}
	if rm.quitting {
		t.Fatal("should not quit when message persistence fails")
	}
	if store.currentID != "" {
		t.Fatalf("should not set current session on failed handover, got %q", store.currentID)
	}
	if len(store.statusUpdates) != 0 {
		t.Fatalf("source session should not be marked complete on failed handover, got %#v", store.statusUpdates)
	}
	if len(store.created) != 1 {
		t.Fatalf("expected target session to have been created before add failure, got %d", len(store.created))
	}
	if len(store.deleted) != 1 || store.deleted[0] != store.created[0].ID {
		t.Fatalf("expected failed target session cleanup, deleted=%#v created=%q", store.deleted, store.created[0].ID)
	}
	if rm.pendingHandover == nil {
		t.Fatal("expected pending handover to remain available for retry")
	}
}

func TestShowSystemMessage_SingleLineUsesTransientFooter(t *testing.T) {
	m := newCmdTestModel(&mockStore{})
	m.setTextareaValue("/search")

	result, cmd := m.showSystemMessage("Web search enabled.")
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Web search enabled." {
		t.Fatalf("expected footer message %q, got %q", "Web search enabled.", got)
	}
	if got := rm.textarea.Value(); got != "" {
		t.Fatalf("expected showSystemMessage to clear the composer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected transient footer clear command")
	}

	updated, _ := rm.Update(footerMessageClearMsg{Seq: rm.footerMessageSeq})
	cleared := updated.(*Model)
	if cleared.footerMessage != "" {
		t.Fatalf("expected footer message to clear after timer, got %q", cleared.footerMessage)
	}
}

func TestShowSystemMessage_SingleLineStripsSimpleMarkdownForFooter(t *testing.T) {
	m := newCmdTestModel(&mockStore{})

	result, _ := m.showSystemMessage("Approved directory: `/tmp/demo`")
	rm := result.(*Model)

	if got := rm.footerMessage; got != "Approved directory: /tmp/demo" {
		t.Fatalf("expected footer markdown to be stripped, got %q", got)
	}
}

func TestShowSystemMessage_MultilineFallsBackToScrollback(t *testing.T) {
	m := newCmdTestModel(&mockStore{})

	result, cmd := m.showSystemMessage("## Help\n\nUse `/help` for commands.")
	rm := result.(*Model)

	if got := rm.footerMessage; got != "" {
		t.Fatalf("expected multiline system message to avoid footer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected multiline system message to return a print command")
	}
}

func TestShowSystemMessage_NewFooterMessageReplacesOlderTimer(t *testing.T) {
	m := newCmdTestModel(&mockStore{})

	result, _ := m.showSystemMessage("First message")
	rm := result.(*Model)
	firstSeq := rm.footerMessageSeq

	result, _ = rm.showSystemMessage("Second message")
	rm = result.(*Model)
	secondSeq := rm.footerMessageSeq
	if secondSeq <= firstSeq {
		t.Fatalf("expected footer message sequence to advance, got first=%d second=%d", firstSeq, secondSeq)
	}

	updated, _ := rm.Update(footerMessageClearMsg{Seq: firstSeq})
	stillVisible := updated.(*Model)
	if got := stillVisible.footerMessage; got != "Second message" {
		t.Fatalf("expected stale timer to preserve latest footer message, got %q", got)
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
	m.setTextareaValue("/model debug:fast")

	result, cmd := m.switchModel("debug:fast")
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
	if got := rm.textarea.Value(); got != "" {
		t.Fatalf("expected switchModel to clear the composer, got %q", got)
	}
	if cmd == nil {
		t.Fatal("expected switchModel success to show a model-switch notice")
	}
	if !strings.Contains(rm.footerMessage, "Switched model to debug:fast") {
		t.Fatalf("expected footer model-switch notice, got %q", rm.footerMessage)
	}
}

func TestSwitchModel_WithExistingHistoryPersistsModelSwapEventMarker(t *testing.T) {
	store := &mockStore{}
	m := newCmdTestModel(store)
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "sess-model-switch-marker", Provider: "old", ProviderKey: "old", Model: "old-model"}
	m.providerName = "old"
	m.providerKey = "old"
	m.modelName = "old-model"
	m.engine = llm.NewEngine(llm.NewMockProvider("old"), nil)
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.UserText("hello"), 0)}
	m.altScreen = true
	m.viewCache.completedStream = "cached previous assistant"
	m.viewCache.historyValid = true

	result, _ := m.switchModel("debug:fast")
	rm := result.(*Model)
	if len(rm.messages) != 2 {
		t.Fatalf("messages len = %d, want user + event marker", len(rm.messages))
	}
	markerMsg := rm.messages[1]
	if markerMsg.Role != llm.RoleEvent {
		t.Fatalf("marker role = %q, want event", markerMsg.Role)
	}
	marker, ok := llm.ParseModelSwapMarker(markerMsg.ToLLMMessage())
	if !ok {
		t.Fatalf("failed to parse model-swap marker: %#v", markerMsg)
	}
	if marker.FromProvider != "old" || marker.FromModel != "old-model" || marker.ToProvider != "debug" || marker.ToModel != "fast" || marker.Status != "started" {
		t.Fatalf("unexpected marker: %#v", marker)
	}
	if len(store.added) != 1 || store.added[0].Role != llm.RoleEvent {
		t.Fatalf("expected store AddMessage event marker, got %#v", store.added)
	}
	if rm.viewCache.completedStream != "" {
		t.Fatalf("expected model switch to clear completed stream cache, got %q", rm.viewCache.completedStream)
	}
	if rm.viewCache.historyValid {
		t.Fatal("expected model switch to invalidate history cache")
	}
	built := rm.buildMessages()
	for _, msg := range built {
		if msg.Role == llm.RoleEvent {
			t.Fatalf("buildMessages included event marker in provider context: %#v", built)
		}
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
	if got := rm.RequestedResumeSessionID(); got == "" || got == "sess-handover-confirm" {
		t.Fatalf("RequestedResumeSessionID() = %q, want a fresh target session", got)
	}
	if store.currentID != rm.RequestedResumeSessionID() {
		t.Fatalf("current session = %q, want requested resume session %q", store.currentID, rm.RequestedResumeSessionID())
	}
	if got := rm.RequestedHandoverAutoSend(); got != "review changes" {
		t.Fatalf("RequestedHandoverAutoSend() = %q, want %q", got, "review changes")
	}
	if len(store.created) != 1 {
		t.Fatalf("expected one fresh handover session to be created, got %d", len(store.created))
	}
	if store.created[0].ID != rm.RequestedResumeSessionID() {
		t.Fatalf("created session ID = %q, want resume session %q", store.created[0].ID, rm.RequestedResumeSessionID())
	}
	if len(store.compacted) != 0 {
		t.Fatalf("handover must not compact the source session, compacted %d messages", len(store.compacted))
	}
	if len(store.added) == 0 {
		t.Fatal("expected handover messages to be added to fresh session")
	}
	var combined strings.Builder
	for _, msg := range store.added {
		if msg.SessionID != rm.RequestedResumeSessionID() {
			t.Fatalf("persisted message session ID = %q, want %q", msg.SessionID, rm.RequestedResumeSessionID())
		}
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
		if processHasExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for process %d to exit", pid)
}

func processHasExited(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil {
		return errors.Is(err, syscall.ESRCH)
	}
	if runtime.GOOS == "linux" {
		state, ok := linuxProcState(pid)
		return ok && state == 'Z'
	}
	return false
}

func linuxProcState(pid int) (byte, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	return procStatState(data)
}

func procStatState(data []byte) (byte, bool) {
	stat := string(data)
	end := strings.LastIndex(stat, ")")
	if end == -1 {
		return 0, false
	}
	rest := strings.TrimSpace(stat[end+1:])
	if rest == "" {
		return 0, false
	}
	return rest[0], true
}

func TestProcStatState(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want byte
		ok   bool
	}{
		{name: "running", stat: "123 (sleep) S 1 2 3", want: 'S', ok: true},
		{name: "zombie", stat: "123 (sleep) Z 1 2 3", want: 'Z', ok: true},
		{name: "command with paren", stat: "123 (odd)name) R 1 2 3", want: 'R', ok: true},
		{name: "missing close paren", stat: "123 sleep S 1 2 3", ok: false},
		{name: "missing state", stat: "123 (sleep)", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := procStatState([]byte(tt.stat))
			if ok != tt.ok || got != tt.want {
				t.Fatalf("procStatState(%q) = %q, %v; want %q, %v", tt.stat, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestStreamDoneReloadRespectsCompactionSeq(t *testing.T) {
	sessionID := "sess-compacted-reload"
	store := &mockStore{
		sessions: map[string]*session.Session{
			sessionID: {ID: sessionID, CompactionSeq: 2},
		},
		messages: map[string][]session.Message{
			sessionID: {
				*session.NewMessage(sessionID, llm.UserText("old user"), 0),
				*session.NewMessage(sessionID, llm.AssistantText("old assistant"), 1),
				*session.NewMessage(sessionID, llm.SystemText("post compact system"), 2),
				*session.NewMessage(sessionID, llm.UserText("post compact summary"), 3),
			},
		},
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = store.sessions[sessionID]
	m.streaming = true
	m.messages = append([]session.Message(nil), store.messages[sessionID]...)
	m.compactionIdx = 2

	result, _ := m.Update(streamEventMsg{event: ui.DoneEvent(42)})
	rm := result.(*Model)

	if len(rm.messages) != 4 {
		t.Fatalf("expected full scrollback after reload, got %d", len(rm.messages))
	}
	if rm.messages[0].Sequence != 0 || rm.messages[3].Sequence != 3 {
		t.Fatalf("unexpected reloaded scrollback sequences: got first=%d last=%d want 0,3", rm.messages[0].Sequence, rm.messages[3].Sequence)
	}
	if rm.compactionIdx != 2 {
		t.Fatalf("compactionIdx after scrollback reload = %d, want 2", rm.compactionIdx)
	}
	built := rm.buildMessagesForStream()
	if len(built) != 2 {
		t.Fatalf("expected next request to include 2 compacted messages, got %d", len(built))
	}
}

func TestStreamDoneRefreshFailureDoesNotReloadFullCompactedHistory(t *testing.T) {
	sessionID := "sess-compacted-refresh-fail"
	store := &mockStore{
		getErr: errors.New("db temporarily unavailable"),
		sessions: map[string]*session.Session{
			sessionID: {ID: sessionID, CompactionSeq: -1},
		},
		messages: map[string][]session.Message{
			sessionID: {
				*session.NewMessage(sessionID, llm.UserText("old user"), 0),
				*session.NewMessage(sessionID, llm.AssistantText("old assistant"), 1),
				*session.NewMessage(sessionID, llm.SystemText("post compact system"), 2),
				*session.NewMessage(sessionID, llm.UserText("post compact summary"), 3),
			},
		},
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = store.sessions[sessionID]
	m.streaming = true
	m.messages = append([]session.Message(nil), store.messages[sessionID]...)
	m.compactionIdx = 2

	result, _ := m.Update(streamEventMsg{event: ui.DoneEvent(42)})
	rm := result.(*Model)

	if len(rm.messages) != 4 {
		t.Fatalf("expected in-memory history to be preserved on refresh failure, got %d", len(rm.messages))
	}
	if rm.compactionIdx != 2 {
		t.Fatalf("compactionIdx after refresh failure = %d, want 2", rm.compactionIdx)
	}
	built := rm.buildMessagesForStream()
	if len(built) != 2 {
		t.Fatalf("expected next request to keep using compacted in-memory window, got %d messages", len(built))
	}
	if got := rm.footerMessage; !strings.Contains(got, "Session refresh failed after compaction") {
		t.Fatalf("expected refresh failure footer, got %q", got)
	}
}
